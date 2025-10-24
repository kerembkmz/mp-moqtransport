package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"text/tabwriter"
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
		selector = schedulers.NewRoundRobinSelector()
	}
	return &MultiPathPublisher{
		connections: make([]*ConnectionInfo, 0),
		selector:    selector,
		namespace:   namespace,
		trackname:   trackname,
		pathStats:   make(map[string]*schedulers.PathStats),
	}
}

// ---- wiring ----
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
		Name:             name,
		IsConnected:      false,
		Latency:          0,
		Bandwidth:        0,
		PacketLoss:       0,
		ObjectsSent:      0,
		BytesSent:        0,
		LastUsed:         time.Now(),
		ErrorCount:       0,
		SmoothedRTT:      0,
		TransportLossPct: 0,
		CwndBytes:        0,
		BytesInFlight:    0,
		PacketsInFlight:  0,
		SendRateBps:      0,
		RecvRateBps:      0,
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

	go func() {
		sessionRunning := make(chan error, 1)
		go func() { sessionRunning <- session.Run(conn) }()
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

// ---- send path ----
func (mp *MultiPathPublisher) SendObject(groupID, objectID uint64, payload []byte) error {
	mp.mu.RLock()
	available := make([]*ConnectionInfo, 0)
	for _, conn := range mp.connections {
		conn.mu.Lock()
		if conn.IsConnected && conn.Publisher != nil {
			available = append(available, conn)
		}
		conn.mu.Unlock()
	}
	mp.mu.RUnlock()
	if len(available) == 0 {
		log.Printf("No available connections for sending object")
		return nil
	}

	obj := schedulers.MoqtObject{
		GroupID:   groupID,
		ObjectID:  objectID,
		Payload:   payload,
		Timestamp: time.Now(),
		Priority:  1,
	}

	pathStats := mp.getCurrentPathStats()
	idx := mp.selector.SelectPath(obj, pathStats)
	if idx < 0 || idx >= len(available) {
		log.Printf("Scheduler returned invalid path index: %d (available: %d)", idx, len(available))
		return nil
	}

	selected := available[idx]
	objectNum := mp.objectCounter.Add(1)
	_ = objectNum // reserved for future logging

	start := time.Now()
	err := mp.sendObjectToConnection(selected, groupID, objectID, payload, objectNum)
	_ = time.Since(start) // latency is derived from RTT in telemetry sampler

	mp.updatePathStats(selected.Name, len(payload), err == nil)
	return err
}

func (mp *MultiPathPublisher) sendObjectToConnection(conn *ConnectionInfo, groupID, objectID uint64, payload []byte, objectNum uint64) error {
	conn.mu.Lock()
	defer conn.mu.Unlock()

	// To Test Datagram sending
	// obj := moqtransport.Object{
	// 	GroupID:              groupID,
	// 	ObjectID:             0,       // Single object per group for simplicity
	// 	SubGroupID:           groupID, // Same as GroupID according to moqt-draft
	// 	Payload:              payload,
	// 	ForwardingPreference: moqtransport.ObjectForwardingPreferenceDatagram,
	// }

	if conn.Publisher == nil {
		return nil
	}

	//err := conn.Publisher.SendDatagram(obj)
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

	if conn.Logger != nil {
		conn.Logger.RecordMOQObject(groupID, 0, objectID, len(payload), "sent", mp.trackname, mp.namespace)
	}
	return nil
}

// ---- stats book-keeping (object counters only) ----
func (mp *MultiPathPublisher) updatePathStats(pathName string, bytesSent int, success bool) {
	mp.statsMutex.Lock()
	defer mp.statsMutex.Unlock()
	if stats, ok := mp.pathStats[pathName]; ok {
		stats.ObjectsSent++
		stats.BytesSent += uint64(bytesSent)
		stats.LastUsed = time.Now()
		if !success {
			stats.ErrorCount++
		}
	}
}

func (mp *MultiPathPublisher) getCurrentPathStats() []schedulers.PathStats {
	mp.statsMutex.RLock()
	defer mp.statsMutex.RUnlock()
	stats := make([]schedulers.PathStats, 0, len(mp.pathStats))
	for _, stat := range mp.pathStats {
		cp := *stat
		stats = append(stats, cp)
	}
	return stats
}

func (mp *MultiPathPublisher) GetPathStats() map[string]schedulers.PathStats {
	mp.statsMutex.RLock()
	defer mp.statsMutex.RUnlock()
	out := make(map[string]schedulers.PathStats)
	for k, v := range mp.pathStats {
		out[k] = *v
	}
	return out
}

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

// ---- periodic transport telemetry sampler ----
func (mp *MultiPathPublisher) StartTelemetrySampler(interval time.Duration, stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				mp.statsMutex.Lock()
				for _, c := range mp.connections {
					c.mu.Lock()
					lg := c.Logger
					name := c.Name
					connected := c.IsConnected
					c.mu.Unlock()

					ps, ok := mp.pathStats[name]
					if !ok {
						continue
					}
					ps.IsConnected = connected

					if lg == nil {
						continue
					}
					rm := lg.GetRollingMetrics()

					// transport-layer feedback
					ps.SmoothedRTT = rm.SmoothedRTT
					ps.TransportLossPct = rm.LossPercent
					ps.CwndBytes = rm.CwndBytes
					ps.BytesInFlight = rm.BytesInFlight
					ps.PacketsInFlight = rm.PacketsInFlight
					ps.SendRateBps = rm.SendRateBps
					ps.RecvRateBps = rm.RecvRateBps

					// legacy fields for backward compatibility
					ps.Latency = rm.SmoothedRTT
					ps.PacketLoss = rm.LossPercent
					ps.Bandwidth = rm.SendRateBps / 8.0

					ps.LastUsed = time.Now()
				}
				mp.statsMutex.Unlock()
			case <-stop:
				return
			}
		}
	}()
}

// ---- pretty print ----
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
func humanBps(bps float64) string {
	const k = 1000.0
	switch {
	case bps < k:
		return fmt.Sprintf("%.0f bps", bps)
	case bps < k*k:
		return fmt.Sprintf("%.1f kbps", bps/k)
	case bps < k*k*k:
		return fmt.Sprintf("%.1f Mbps", bps/(k*k))
	default:
		return fmt.Sprintf("%.1f Gbps", bps/(k*k*k))
	}
}

func (mp *MultiPathPublisher) PrintDetailedStats() {
	connectionStats := mp.GetConnectionStats()
	pathStats := mp.GetPathStats()

	log.Printf("=== MULTIPATH PUBLISHER STATS ===")

	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tCONN\tRTT\tLOSS\tCWND\tBIF\tPIF\tTX RATE\tRX RATE\tSENT BYTES\tOBJ\tERR\tNOTE")

	for name, connected := range connectionStats {
		ps := pathStats[name]
		note := ""
		fmt.Fprintf(
			tw,
			"%s\t%v\t%v\t%.1f%%\t%s\t%s\t%d\t%s\t%s\t%s\t%d\t%d\t%s\n",
			name,
			connected,
			ps.SmoothedRTT,
			ps.TransportLossPct,
			humanBytes(ps.CwndBytes),
			humanBytes(ps.BytesInFlight),
			ps.PacketsInFlight,
			humanBps(ps.SendRateBps),
			humanBps(ps.RecvRateBps),
			humanBytes(int64(ps.BytesSent)),
			ps.ObjectsSent,
			ps.ErrorCount,
			note,
		)
	}
	_ = tw.Flush()
	log.Print("\n" + buf.String())
	log.Printf("=================================")
}

// ---- lifecycle ----
func (mp *MultiPathPublisher) Close() {
	mp.mu.Lock()
	defer mp.mu.Unlock()
	for _, conn := range mp.connections {
		if conn.Session != nil {
			conn.Session.Close()
		}
		if conn.Logger != nil {
			conn.Logger.LogSummary()
		}
	}
}

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

// ---- handlers ----
func (mp *MultiPathPublisher) createHandler(connectionName string) moqtransport.Handler {
	return moqtransport.HandlerFunc(func(rw moqtransport.ResponseWriter, r *moqtransport.Message) {
		log.Printf("[%s] Handler called with message type: %T", connectionName, r)
	})
}

func (mp *MultiPathPublisher) createSubscribeHandler(connectionName string, connInfo *ConnectionInfo) moqtransport.SubscribeHandler {
	return moqtransport.SubscribeHandlerFunc(func(w *moqtransport.SubscribeResponseWriter, m *moqtransport.SubscribeMessage) {
		log.Printf("[%s] Subscribe request received for namespace: %v, track: %s", connectionName, m.Namespace, m.Track)

		if !tupleEqual(m.Namespace, mp.namespace) || m.Track != mp.trackname {
			log.Printf("[%s] Rejecting subscription - namespace/track mismatch: %v/%s (expected %v/%s)",
				connectionName, m.Namespace, m.Track, mp.namespace, mp.trackname)
			w.Reject(moqtransport.ErrorCodeSubscribeTrackDoesNotExist, "unknown track")
			return
		}

		if err := w.Accept(moqtransport.WithLargestLocation(&moqtransport.Location{Group: 0, Object: 0})); err != nil {
			log.Printf("[%s] Failed to accept subscription: %v", connectionName, err)
			return
		}

		log.Printf("[%s] Accepted subscription for namespace %v track %s", connectionName, m.Namespace, m.Track)

		connInfo.mu.Lock()
		connInfo.Publisher = w
		connInfo.IsConnected = true
		connInfo.mu.Unlock()

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
