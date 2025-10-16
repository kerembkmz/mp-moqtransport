package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	schedulers "multipath-moq/Schedulers"
	"os"
	"time"

	"github.com/mengelbart/moqtransport"
	"github.com/mengelbart/moqtransport/quicmoq"
	"github.com/quic-go/quic-go"
)

const (
	appName = "multipath-moq"
)

var usg = `%s demonstrates multipath Media over QUIC Transport.
The publisher opens two QUIC connections (simulating WiFi and LTE) and 
sends objects using the selected scheduling between the connections.
The subscriber receives data from both connections and logs which path each object arrives from.

Usage of %s:
`

type options struct {
	certFile  string
	keyFile   string
	addr      string
	server    bool
	publish   bool
	subscribe bool
	namespace string
	trackname string
	port1     int
	port2     int
}

func parseOptions(fs *flag.FlagSet, args []string) (*options, error) {
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, usg, appName, appName)
		fmt.Fprintf(os.Stderr, "%s [options]\n\noptions:\n", appName)
		fs.PrintDefaults()
	}

	opts := options{}
	fs.StringVar(&opts.certFile, "cert", "localhost.pem", "TLS certificate file (only used for server)")
	fs.StringVar(&opts.keyFile, "key", "localhost-key.pem", "TLS key file (only used for server)")
	fs.StringVar(&opts.addr, "addr", "localhost", "listen or connect address")
	fs.BoolVar(&opts.server, "server", false, "run as server (subscriber)")
	fs.BoolVar(&opts.publish, "publish", false, "publish video frames")
	fs.BoolVar(&opts.subscribe, "subscribe", false, "subscribe to video frames")
	fs.StringVar(&opts.namespace, "namespace", "multipath", "Namespace to publish/subscribe")
	fs.StringVar(&opts.trackname, "trackname", "video", "Track to publish/subscribe")
	fs.IntVar(&opts.port1, "port1", 8080, "First port (simulating WiFi)")
	fs.IntVar(&opts.port2, "port2", 8081, "Second port (simulating LTE)")
	err := fs.Parse(args[1:])
	return &opts, err
}

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	opts, err := parseOptions(fs, args)

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if opts.server {
		return runServer(opts)
	}
	return runClient(opts)
}

func runServer(opts *options) error {
	log.Printf("=== MULTIPATH SUBSCRIBER SERVER ===")
	log.Printf("Starting subscriber that will listen on two ports:")
	log.Printf("Port %d (WiFi)", opts.port1)
	log.Printf("Port %d (LTE)", opts.port2)
	log.Printf("===================================")

	tlsConfig, err := generateTLSConfigWithCertAndKey(opts.certFile, opts.keyFile)
	if err != nil {
		log.Printf("failed to generate TLS config from cert file and key, generating in memory certs: %v", err)
		tlsConfig, err = generateTLSConfig()
		if err != nil {
			log.Fatal(err)
		}
	}

	// Create the multipath subscriber
	subscriber := NewMultiPathSubscriber([]string{opts.namespace}, opts.trackname)

	// Create loggers for server connections
	wifiServerLogger := quicmoq.NewQuicLoggerWithDefaults("subscriber-WiFi")
	wifiServerLogger.SetLogLevel(quicmoq.LogLevelInfo)
	lteServerLogger := quicmoq.NewQuicLoggerWithDefaults("subscriber-LTE")
	lteServerLogger.SetLogLevel(quicmoq.LogLevelInfo)

	// (WiFi simulation listener)
	listener1, err := quic.ListenAddr(fmt.Sprintf("%s:%d", opts.addr, opts.port1), tlsConfig, &quic.Config{
		EnableDatagrams: true,
		Tracer:          wifiServerLogger.CreateConnectionTracer(),
	})
	if err != nil {
		return fmt.Errorf("failed to start WiFi listener: %v", err)
	}
	log.Printf("WiFi listener started on %s:%d", opts.addr, opts.port1)

	// (LTE simulation listener)
	listener2, err := quic.ListenAddr(fmt.Sprintf("%s:%d", opts.addr, opts.port2), tlsConfig, &quic.Config{
		EnableDatagrams: true,
		Tracer:          lteServerLogger.CreateConnectionTracer(),
	})
	if err != nil {
		return fmt.Errorf("failed to start LTE listener: %v", err)
	}
	log.Printf("LTE listener started on %s:%d", opts.addr, opts.port2)

	// handle connection 1
	go func() {
		for {
			conn, err := listener1.Accept(context.Background())
			if err != nil {
				log.Printf("WiFi listener error: %v", err)
				return
			}
			log.Printf("WiFi connection accepted from %s", conn.RemoteAddr())

			moqConn := quicmoq.NewServer(conn)
			if err := subscriber.AddConnection(moqConn, "WiFi"); err != nil {
				log.Printf("Failed to add WiFi connection: %v", err)
			}
		}
	}()

	// handle connection 2
	go func() {
		for {
			conn, err := listener2.Accept(context.Background())
			if err != nil {
				log.Printf("LTE listener error: %v", err)
				return
			}
			log.Printf("LTE connection accepted from %s", conn.RemoteAddr())

			moqConn := quicmoq.NewServer(conn)
			if err := subscriber.AddConnection(moqConn, "LTE"); err != nil {
				log.Printf("Failed to add LTE connection: %v", err)
			}
		}
	}()

	log.Printf("Multipath subscriber ready. Waiting for publisher connections...")

	// Keep the server running
	select {}
}

func runClient(opts *options) error {
	log.Printf("=== MULTIPATH PUBLISHER CLIENT ===")
	log.Printf("Starting publisher that will connect via two paths:")
	log.Printf("Path 1: %s:%d (WiFi)", opts.addr, opts.port1)
	log.Printf("Path 2: %s:%d (LTE)", opts.addr, opts.port2)
	log.Printf("===================================")

	// Setting the scheduler (path selector)
	currentSelector := schedulers.NewRoundRobinSelector()
	log.Printf("Starting with %s selector", currentSelector.GetName())

	publisher := NewMultiPathPublisher([]string{opts.namespace}, opts.trackname, currentSelector)

	// connection 1
	wifiLogger := quicmoq.NewQuicLoggerWithDefaults("publisher-WiFi")
	wifiLogger.SetLogLevel(quicmoq.LogLevelInfo)
	conn1, err := dialQUICWithLogger(context.Background(), fmt.Sprintf("%s:%d", opts.addr, opts.port1), wifiLogger)
	if err != nil {
		return fmt.Errorf("failed to connect via WiFi: %v", err)
	}
	log.Printf("Connected via WiFi path")

	// connection 2
	lteLogger := quicmoq.NewQuicLoggerWithDefaults("publisher-LTE")
	lteLogger.SetLogLevel(quicmoq.LogLevelInfo)
	conn2, err := dialQUICWithLogger(context.Background(), fmt.Sprintf("%s:%d", opts.addr, opts.port2), lteLogger)
	if err != nil {
		return fmt.Errorf("failed to connect via LTE: %v", err)
	}
	log.Printf("Connected via LTE path")

	// Add connections to the publisher with their loggers
	if err := publisher.AddConnectionWithLogger(conn1, "WiFi", wifiLogger); err != nil {
		return fmt.Errorf("failed to add WiFi connection: %v", err)
	}

	if err := publisher.AddConnectionWithLogger(conn2, "LTE", lteLogger); err != nil {
		return fmt.Errorf("failed to add LTE connection: %v", err)
	}

	// TODO: Should be based on a signal from the subscriber
	time.Sleep(2 * time.Second)

	if opts.publish {
		log.Printf("Starting to publish video frames with %s selector...", currentSelector.GetName())
		go publishVideoFrames(publisher)
	}

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			publisher.PrintDetailedStats()
		}
	}()

	log.Printf("Multipath publisher ready. Will rotate through different selectors. Press Ctrl+C to stop...")

	select {}
}

func publishVideoFrames(publisher *MultiPathPublisher) {
	ticker := time.NewTicker(1 * time.Second) // 1 FPS for easier logging
	defer ticker.Stop()

	groupID := uint64(0)
	objectID := uint64(0)

	for ts := range ticker.C {
		frameData := fmt.Sprintf("VIDEO_FRAME_%d_TIMESTAMP_%s", objectID, ts.Format("15:04:05.000"))

		if err := publisher.SendObject(groupID, objectID, []byte(frameData)); err != nil {
			log.Printf("Failed to send video frame: %v", err)
		}

		objectID++

		if objectID%10 == 0 {
			groupID++
			objectID = 0
		}
	}
}

func generateTLSConfigWithCertAndKey(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"moq-00"},
	}, nil
}

func generateTLSConfig() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"moq-00"},
	}, nil
}

func dialQUICWithLogger(ctx context.Context, addr string, logger *quicmoq.QuicLogger) (moqtransport.Connection, error) {
	conn, err := quic.DialAddr(ctx, addr, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"moq-00"},
	}, &quic.Config{
		EnableDatagrams: true,
		Tracer:          logger.CreateConnectionTracer(),
	})
	if err != nil {
		return nil, err
	}
	return quicmoq.NewClient(conn), nil
}
