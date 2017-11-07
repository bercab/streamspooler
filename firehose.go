package firehosePool

import (
	"log"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/firehose"
)

const (
	maxConnectionsTries = 3
	connectionRetry     = 5 * time.Second
	connectTimeout      = 15 * time.Second
	errorsFrame         = 10 * time.Second
	maxErrors           = 10 // Limit of errors to restart the connection

)

func (srv *Server) failure() {
	srv.Lock()
	defer srv.Unlock()

	if srv.reseting || srv.exiting {
		return
	}

	if time.Now().Sub(srv.lastError) > errorsFrame {
		srv.errors = 0
	}

	srv.errors++
	srv.lastError = time.Now()
	log.Printf("Firehose: %d errors detected", srv.errors)

	if srv.errors > maxErrors {
		go srv.retry()
	}
}

func (srv *Server) retry() {
	srv.Lock()
	defer srv.Unlock()

	if srv.failing {
		return
	}

	srv.failing = true
	tries := 0

	for {
		if srv.clientsReset() == nil {
			return
		}

		tries++
		log.Printf("Firehose ERROR: %d attempts to connect to kinens", tries)

		if tries >= maxConnectionsTries {
			time.Sleep(connectionRetry * 2)
		} else {
			time.Sleep(connectionRetry)
		}
	}
}

func (srv *Server) clientsReset() (err error) {

	if srv.exiting {
		return nil
	}

	srv.reseting = true
	defer func() { srv.reseting = false }()

	log.Printf("Firehose Reload config to the stream %s", srv.cfg.StreamName)

	var sess *session.Session

	if srv.cfg.Profile != "" {
		sess, err = session.NewSessionWithOptions(session.Options{Profile: srv.cfg.Profile})
	} else {
		sess, err = session.NewSession()
	}

	if err != nil {
		log.Printf("Firehose ERROR: session: %s", err)

		srv.errors++
		srv.lastError = time.Now()
		return err
	}

	srv.awsSvc = firehose.New(sess, &aws.Config{Region: aws.String(srv.cfg.Region)})
	stream := &firehose.DescribeDeliveryStreamInput{
		DeliveryStreamName: &srv.cfg.StreamName,
	}

	var l *firehose.DescribeDeliveryStreamOutput
	l, err = srv.awsSvc.DescribeDeliveryStream(stream)
	if err != nil {
		log.Printf("Firehose ERROR: describe stream: %s", err)

		srv.errors++
		srv.lastError = time.Now()
		return err
	}

	log.Printf("Firehose Connected to %s (%s) status %s",
		*l.DeliveryStreamDescription.DeliveryStreamName,
		*l.DeliveryStreamDescription.DeliveryStreamARN,
		*l.DeliveryStreamDescription.DeliveryStreamStatus)

	srv.lastConnection = time.Now()
	srv.errors = 0
	srv.failing = false

	currClients := len(srv.clients)

	// No changes in the number of clients
	if currClients == srv.cfg.Workers {
		return nil
	}

	// If the config define lower number than the active clients remove the difference
	if currClients > srv.cfg.Workers {
		for i := currClients; i > srv.cfg.Workers; i-- {
			k := i - 1
			srv.clients[k].Exit()
			srv.clients = append(srv.clients[:k], srv.clients[k+1:]...)
		}
		atomic.StoreInt64(&clientCount, int64(len(srv.clients)))
	} else {
		// If the config define higher number than the active clients start new clients
		for i := currClients; i < srv.cfg.Workers; i++ {
			srv.clients = append(srv.clients, NewClient(srv))
		}
	}
	atomic.StoreInt64(&clientCount, int64(len(srv.clients)))

	return nil
}
