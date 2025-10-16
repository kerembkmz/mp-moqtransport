package main

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	schedulers "multipath-moq/Schedulers"

	"github.com/mengelbart/moqtransport"
	"github.com/mengelbart/moqtransport/quicmoq"
)

type ConnectionInfo struct {
	Connection  moqtransport.Connection
	Session     *moqtransport.Session
	Name        string
	IsConnected bool
	Publisher   moqtransport.Publisher
	Logger      *quicmoq.QuicLogger
	mu          sync.Mutex
}

type MultiPathPublisher struct {
	connections   []*ConnectionInfo
	selector      PathSelector
	namespace     []string
	trackname     string
	mu            sync.RWMutex
	objectCounter atomic.Uint64
	pathStats     map[string]*schedulers.PathStats
	statsMutex    sync.RWMutex
}

func NewMultiPathPublisher(namespace []string, trackname string, selector PathSelector) *MultiPathPublisher {
	if selector == nil {
		selector = schedulers.NewRoundRobinSelector() // Default to round-robin
	}

	return &MultiPathPublisher{
		connections: make([]*ConnectionInfo, 0),
		selector:    selector,
		namespace:   namespace,
		trackname:   trackname,
		pathStats:   make(map[string]*schedulers.PathStats),
	}
}

func (mp *MultiPathPublisher) AddConnectionWithLogger(conn moqtransport.Connection, name string, logger *quicmoq.QuicLogger) error {
	mp.mu.Lock()
	defer mp.mu.Unlock()

	connInfo := &ConnectionInfo{
		Connection:  conn,
		Name:        name,
		IsConnected: false,
		Logger:      logger,
	}

	mp.statsMutex.Lock()
	mp.pathStats[name] = &schedulers.PathStats{
		Name:        name,
		IsConnected: false,
		Latency:     0,
		Bandwidth:   0,
		PacketLoss:  0,
		ObjectsSent: 0,
		BytesSent:   0,
		LastUsed:    time.Now(),
		ErrorCount:  0,
	}
	mp.statsMutex.Unlock()

	session := &moqtransport.Session{
		Handler:                mp.createHandler(name),
		SubscribeHandler:       mp.createSubscribeHandler(name, connInfo),
		SubscribeUpdateHandler: mp.createSubscribeUpdateHandler(name),
		InitialMaxRequestID:    100,
	}

	connInfo.Session = session
	mp.connections = append(mp.connections, connInfo)

	// Start the session and announce in a single goroutine to ensure proper initialization
	go func() {
		// Run the session in a separate goroutine
		sessionRunning := make(chan error, 1)
		go func() {
			sessionRunning <- session.Run(conn)
		}()

		//TODO: Announce should not depend on a fixed sleep
		time.Sleep(100 * time.Millisecond)
		if err := session.Announce(context.Background(), mp.namespace); err != nil {
			log.Printf("[%s] Failed to announce namespace '%v': %v", name, mp.namespace, err)
			return
		}

		log.Printf("[%s] Connection added and announced successfully", name)

		if err := <-sessionRunning; err != nil {
			log.Printf("[%s] Session error: %v", name, err)
		}
	}()
	return nil
}

func (mp *MultiPathPublisher) SendObject(groupID, objectID uint64, payload []byte) error {
	mp.mu.RLock()
	availableConnections := make([]*ConnectionInfo, 0)
	for _, conn := range mp.connections {
		conn.mu.Lock()
		if conn.IsConnected && conn.Publisher != nil {
			availableConnections = append(availableConnections, conn)
		}
		conn.mu.Unlock()
	}
	mp.mu.RUnlock()

	if len(availableConnections) == 0 {
		log.Printf("No available connections for sending object")
		return nil
	}

	// TODO: Schedulers should use the actual object directly instead of copying
	obj := schedulers.MoqtObject{
		GroupID:   groupID,
		ObjectID:  objectID,
		Payload:   payload,
		Timestamp: time.Now(),
		Priority:  1, // TODO: Priority handling
	}

	pathStats := mp.getCurrentPathStats()
	selectedIndex := mp.selector.SelectPath(obj, pathStats)

	if selectedIndex < 0 || selectedIndex >= len(availableConnections) {
		log.Printf("Scheduler returned invalid path index: %d (available: %d)", selectedIndex, len(availableConnections))
		return nil
	}

	selectedConn := availableConnections[selectedIndex]
	objectNum := mp.objectCounter.Add(1)

	log.Printf("[%s] Sending object #%d (Group: %d, Object: %d) via %s connection",
		mp.selector.GetName(), objectNum, groupID, objectID, selectedConn.Name)

	startTime := time.Now()
	err := mp.sendObjectToConnection(selectedConn, groupID, objectID, payload, objectNum)
	duration := time.Since(startTime)
	mp.updatePathStats(selectedConn.Name, len(payload), duration, err == nil)

	return err
}

func (mp *MultiPathPublisher) sendObjectToConnection(conn *ConnectionInfo, groupID, objectID uint64, payload []byte, objectNum uint64) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	if conn.Publisher != nil {
		// Open subgroup and send object
		sg, err := conn.Publisher.OpenSubgroup(groupID, 0, 0)
		if err != nil {
			log.Printf("[%s] Failed to open subgroup: %v", conn.Name, err)
			return err
		}
		defer sg.Close()

		if _, err := sg.WriteObject(objectID, payload); err != nil {
			log.Printf("[%s] Failed to write object: %v", conn.Name, err)
			return err
		}

		// Log MoQ object transfer
		if conn.Logger != nil {
			conn.Logger.RecordMOQObject(groupID, 0, objectID, len(payload), "sent", mp.trackname, mp.namespace)
		}

		log.Printf("[%s] Successfully sent object #%d (Group: %d, Object: %d, Size: %d bytes)",
			conn.Name, objectNum, groupID, objectID, len(payload))
	}

	return nil
}

// GetConnectionStats returns statistics about the connections
func (mp *MultiPathPublisher) GetConnectionStats() map[string]bool {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	stats := make(map[string]bool)
	for _, conn := range mp.connections {
		conn.mu.Lock()
		stats[conn.Name] = conn.IsConnected
		conn.mu.Unlock()
	}
	return stats
}

func (mp *MultiPathPublisher) getCurrentPathStats() []schedulers.PathStats {
	mp.statsMutex.RLock()
	defer mp.statsMutex.RUnlock()

	stats := make([]schedulers.PathStats, 0, len(mp.pathStats))
	for _, stat := range mp.pathStats {
		// Create a copy to avoid race condition
		statCopy := *stat
		stats = append(stats, statCopy)
	}
	return stats
}

// updatePathStats updates statistics for a specific path
func (mp *MultiPathPublisher) updatePathStats(pathName string, bytesSent int, latency time.Duration, success bool) {
	mp.statsMutex.Lock()
	defer mp.statsMutex.Unlock()

	if stats, exists := mp.pathStats[pathName]; exists {
		stats.ObjectsSent++
		stats.BytesSent += uint64(bytesSent)
		stats.LastUsed = time.Now()

		stats.Latency = mp.getConnectionRTT(pathName)

		//TODO: Rolling window based bandwidth should be implemented
	}
}

// getConnectionRTT attempts to get the actual network RTT from the QUIC connection
func (mp *MultiPathPublisher) getConnectionRTT(pathName string) time.Duration {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	// Find connection by path name
	for _, conn := range mp.connections {
		if conn.Name == pathName && conn.Connection != nil {
			// Try to get RTT from the underlying connection
			// This will depend on the specific moqtransport.Connection interface
			// Let's try a few common ways to access QUIC connection stats

			// Method 1: Check if Connection has a Stats() method
			if statsGetter, ok := conn.Connection.(interface{ Stats() interface{} }); ok {
				stats := statsGetter.Stats()
				// Try to extract RTT from stats
				if rttStats, hasRTT := stats.(interface{ RTT() time.Duration }); hasRTT {
					return rttStats.RTT()
				}
			}

			// Method 2: Check if Connection has RTT() method directly
			if rttGetter, ok := conn.Connection.(interface{ RTT() time.Duration }); ok {
				return rttGetter.RTT()
			}

			// Method 3: Check if we can access underlying QUIC connection
			if quicGetter, ok := conn.Connection.(interface{ QUICConnection() interface{} }); ok {
				quicConn := quicGetter.QUICConnection()
				if rttGetter, hasRTT := quicConn.(interface{ RTT() time.Duration }); hasRTT {
					return rttGetter.RTT()
				}
			}
		}
	}
	return 0 // No RTT data available
}

// GetPathStats returns current path statistics (for monitoring/debugging)
func (mp *MultiPathPublisher) GetPathStats() map[string]schedulers.PathStats {
	mp.statsMutex.RLock()
	defer mp.statsMutex.RUnlock()

	result := make(map[string]schedulers.PathStats)
	for name, stats := range mp.pathStats {
		result[name] = *stats // Copy
	}
	return result
}

// GetConnectionLoggers returns the QUIC loggers for all connections
func (mp *MultiPathPublisher) GetConnectionLoggers() map[string]*quicmoq.QuicLogger {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	loggers := make(map[string]*quicmoq.QuicLogger)
	for _, conn := range mp.connections {
		conn.mu.Lock()
		if conn.Logger != nil {
			loggers[conn.Name] = conn.Logger
		}
		conn.mu.Unlock()
	}
	return loggers
}

// PrintDetailedStats prints comprehensive statistics including QUIC connection details
func (mp *MultiPathPublisher) PrintDetailedStats() {
	// Print basic multipath stats
	connectionStats := mp.GetConnectionStats()
	pathStats := mp.GetPathStats()

	log.Printf("=== MULTIPATH PUBLISHER DETAILED STATISTICS ===")
	log.Printf("Selector: %s", mp.selector.GetName())

	for connName, isConnected := range connectionStats {
		if pathStat, exists := pathStats[connName]; exists {
			log.Printf("[%s] Connected: %v, Objects: %d, Bytes: %d, Errors: %d, RTT: %v",
				connName, isConnected, pathStat.ObjectsSent, pathStat.BytesSent,
				pathStat.ErrorCount, pathStat.Latency)
		}
	}

	// Print detailed QUIC connection statistics
	loggers := mp.GetConnectionLoggers()
	for connName, logger := range loggers {
		log.Printf("\\n=== QUIC Connection Details for %s ===", connName)
		logger.LogDetailedStats()
	}
	log.Printf("================================================")
}

// Close closes all connections
func (mp *MultiPathPublisher) Close() {
	mp.mu.Lock()
	defer mp.mu.Unlock()

	for _, conn := range mp.connections {
		if conn.Session != nil {
			conn.Session.Close()
		}
		// Log final summary for each connection
		if conn.Logger != nil {
			conn.Logger.LogSummary()
		}
	}
}

// SetLogLevel sets the log level for all connection loggers
func (mp *MultiPathPublisher) SetLogLevel(level quicmoq.LogLevel) {
	mp.mu.RLock()
	defer mp.mu.RUnlock()

	for _, conn := range mp.connections {
		conn.mu.Lock()
		if conn.Logger != nil {
			conn.Logger.SetLogLevel(level)
		}
		conn.mu.Unlock()
	}
}

// Helper methods for creating handlers

func (mp *MultiPathPublisher) createHandler(connectionName string) moqtransport.Handler {
	return moqtransport.HandlerFunc(func(rw moqtransport.ResponseWriter, r *moqtransport.Message) {
		log.Printf("[%s] Handler called with message type: %T", connectionName, r)
	})
}

func (mp *MultiPathPublisher) createSubscribeHandler(connectionName string, connInfo *ConnectionInfo) moqtransport.SubscribeHandler {
	return moqtransport.SubscribeHandlerFunc(func(w *moqtransport.SubscribeResponseWriter, m *moqtransport.SubscribeMessage) {
		log.Printf("[%s] Subscribe request received for namespace: %v, track: %s", connectionName, m.Namespace, m.Track)

		// Check if this is the right namespace and track
		if !tupleEqual(m.Namespace, mp.namespace) || m.Track != mp.trackname {
			log.Printf("[%s] Rejecting subscription - namespace/track mismatch: %v/%s (expected %v/%s)",
				connectionName, m.Namespace, m.Track, mp.namespace, mp.trackname)
			w.Reject(moqtransport.ErrorCodeSubscribeTrackDoesNotExist, "unknown track")
			return
		}

		// Accept the subscription
		err := w.Accept(moqtransport.WithLargestLocation(&moqtransport.Location{
			Group:  0,
			Object: 0,
		}))
		if err != nil {
			log.Printf("[%s] Failed to accept subscription: %v", connectionName, err)
			return
		}

		log.Printf("[%s] Accepted subscription for namespace %v track %s", connectionName, m.Namespace, m.Track)

		// Store the publisher and mark as connected
		connInfo.mu.Lock()
		connInfo.Publisher = w
		connInfo.IsConnected = true
		connInfo.mu.Unlock()

		// Update path statistics to mark as connected
		mp.statsMutex.Lock()
		if stats, exists := mp.pathStats[connectionName]; exists {
			stats.IsConnected = true
		}
		mp.statsMutex.Unlock()

		log.Printf("[%s] Connection marked as ready for publishing", connectionName)
	})
}

func (mp *MultiPathPublisher) createSubscribeUpdateHandler(connectionName string) moqtransport.SubscribeUpdateHandler {
	return moqtransport.SubscribeUpdateHandlerFunc(func(m *moqtransport.SubscribeUpdateMessage) {
		log.Printf("[%s] Subscribe update received for requestID %d", connectionName, m.RequestID)
	})
}

// tupleEqual checks if two string slices are equal
func tupleEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, t := range a {
		if t != b[i] {
			return false
		}
	}
	return true
}
