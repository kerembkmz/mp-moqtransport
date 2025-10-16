package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"

	"github.com/mengelbart/moqtransport"
	"github.com/mengelbart/moqtransport/quicmoq"
)

type SubscriberConnectionInfo struct {
	Connection   moqtransport.Connection
	Session      *moqtransport.Session
	Name         string
	IsSubscribed bool
	ObjectCount  atomic.Uint64
	Logger       *quicmoq.QuicLogger
	mu           sync.Mutex
}

type MultiPathSubscriber struct {
	connections  []*SubscriberConnectionInfo
	namespace    []string
	trackname    string
	mu           sync.RWMutex
	totalObjects atomic.Uint64
}

func NewMultiPathSubscriber(namespace []string, trackname string) *MultiPathSubscriber {
	return &MultiPathSubscriber{
		connections: make([]*SubscriberConnectionInfo, 0),
		namespace:   namespace,
		trackname:   trackname,
	}
}

// TODO: Do we need connection logger in subscriber
func (ms *MultiPathSubscriber) AddConnection(conn moqtransport.Connection, name string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	logger := quicmoq.NewQuicLoggerWithDefaults(fmt.Sprintf("subscriber-%s", name))
	logger.SetLogLevel(quicmoq.LogLevelInfo)

	connInfo := &SubscriberConnectionInfo{
		Connection:   conn,
		Name:         name,
		IsSubscribed: false,
		Logger:       logger,
	}

	session := &moqtransport.Session{
		Handler:                ms.createHandler(name),
		SubscribeHandler:       ms.createSubscribeHandler(name),
		SubscribeUpdateHandler: ms.createSubscribeUpdateHandler(name),
		InitialMaxRequestID:    100,
	}

	connInfo.Session = session
	ms.connections = append(ms.connections, connInfo)

	go func() {
		// Run the session - this blocks until handshake is complete
		if err := session.Run(conn); err != nil {
			log.Printf("[%s] Subscriber session error: %v", name, err)
			return
		}

		// Handshake is now complete, we can safely subscribe
		if err := ms.subscribeAndRead(session, name, connInfo); err != nil {
			log.Printf("[%s] Subscribe error: %v", name, err)
		}
	}()

	log.Printf("[%s] Subscriber connection added successfully", name)
	return nil
}

// subscribeAndRead subscribes to the track and reads objects
func (ms *MultiPathSubscriber) subscribeAndRead(session *moqtransport.Session, connectionName string, connInfo *SubscriberConnectionInfo) error {
	log.Printf("[%s] Attempting to subscribe to namespace %v, track %s", connectionName, ms.namespace, ms.trackname)

	rs, err := session.Subscribe(context.Background(), ms.namespace, ms.trackname)
	if err != nil {
		log.Printf("[%s] Failed to subscribe: %v", connectionName, err)
		return err
	}

	connInfo.mu.Lock()
	connInfo.IsSubscribed = true
	connInfo.mu.Unlock()

	log.Printf("[%s] Successfully subscribed to track", connectionName)

	// Read objects in a loop
	for {
		o, err := rs.ReadObject(context.Background())
		if err != nil {
			if err == io.EOF {
				log.Printf("[%s] Received last object (EOF)", connectionName)
				return nil
			}
			log.Printf("[%s] Error reading object: %v", connectionName, err)
			return err
		}

		// Increment counters
		connectionObjectCount := connInfo.ObjectCount.Add(1)
		totalObjectCount := ms.totalObjects.Add(1)

		// Log MoQ object reception
		if connInfo.Logger != nil {
			connInfo.Logger.RecordMOQObject(o.GroupID, o.SubGroupID, o.ObjectID, len(o.Payload), "received", ms.trackname, ms.namespace)
		}

		log.Printf("RECEIVED object #%d via [%s: #%d] - Group: %d, Subgroup: %d, Object: %d, Size: %d bytes, Content: %s",
			totalObjectCount, connectionName, connectionObjectCount,
			o.GroupID, o.SubGroupID, o.ObjectID, len(o.Payload), string(o.Payload))
	}
}

func (ms *MultiPathSubscriber) GetConnectionStats() map[string]map[string]interface{} {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	stats := make(map[string]map[string]interface{})
	for _, conn := range ms.connections {
		conn.mu.Lock()
		stats[conn.Name] = map[string]interface{}{
			"isSubscribed": conn.IsSubscribed,
			"objectCount":  conn.ObjectCount.Load(),
		}
		conn.mu.Unlock()
	}
	return stats
}

func (ms *MultiPathSubscriber) GetTotalObjectsReceived() uint64 {
	return ms.totalObjects.Load()
}

func (ms *MultiPathSubscriber) PrintStats() {
	log.Printf("=== MULTIPATH SUBSCRIBER STATISTICS ===")
	log.Printf("Total objects received: %d", ms.GetTotalObjectsReceived())

	stats := ms.GetConnectionStats()
	for connName, connStats := range stats {
		log.Printf("[%s] Subscribed: %v, Objects received: %d",
			connName, connStats["isSubscribed"], connStats["objectCount"])
	}
	log.Printf("======================================")
}

// PrintDetailedStats prints comprehensive statistics including QUIC connection details
func (ms *MultiPathSubscriber) PrintDetailedStats() {
	// Print basic multipath stats
	log.Printf("=== MULTIPATH SUBSCRIBER DETAILED STATISTICS ===")
	log.Printf("Total objects received: %d", ms.GetTotalObjectsReceived())

	stats := ms.GetConnectionStats()
	for connName, connStats := range stats {
		log.Printf("[%s] Subscribed: %v, Objects received: %d",
			connName, connStats["isSubscribed"], connStats["objectCount"])
	}
	log.Printf("===================================================")
}

// Close closes all connections
func (ms *MultiPathSubscriber) Close() {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	for _, conn := range ms.connections {
		if conn.Session != nil {
			conn.Session.Close()
		}
		// Log final summary for each connection
		if conn.Logger != nil {
			conn.Logger.LogSummary()
		}
	}
}

// Helper methods for creating handlers

func (ms *MultiPathSubscriber) createHandler(connectionName string) moqtransport.Handler {
	return moqtransport.HandlerFunc(func(rw moqtransport.ResponseWriter, r *moqtransport.Message) {
		log.Printf("[%s] Subscriber handler called with message type: %T", connectionName, r)
	})
}

func (ms *MultiPathSubscriber) createSubscribeHandler(connectionName string) moqtransport.SubscribeHandler {
	return moqtransport.SubscribeHandlerFunc(func(w *moqtransport.SubscribeResponseWriter, m *moqtransport.SubscribeMessage) {
		log.Printf("[%s] Unexpected subscribe request received (subscriber should not handle subscribe requests)", connectionName)
		w.Reject(moqtransport.ErrorCodeSubscribeInternal, "subscriber does not handle subscribe requests")
	})
}

func (ms *MultiPathSubscriber) createSubscribeUpdateHandler(connectionName string) moqtransport.SubscribeUpdateHandler {
	return moqtransport.SubscribeUpdateHandlerFunc(func(m *moqtransport.SubscribeUpdateMessage) {
		log.Printf("[%s] Subscribe update received for requestID %d", connectionName, m.RequestID)
	})
}
