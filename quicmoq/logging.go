package quicmoq

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go/logging"
)

// LogLevel defines logging verbosity
type LogLevel int

const (
	LogLevelSilent LogLevel = iota
	LogLevelError
	LogLevelWarn
	LogLevelInfo
	LogLevelDebug
	LogLevelTrace
)

// AckEvent represents a packet acknowledgment event
type AckEvent struct {
	Timestamp       time.Time
	PacketNumber    logging.PacketNumber
	EncryptionLevel logging.EncryptionLevel
	DelayTime       time.Duration
	AckRanges       []logging.AckRange
}

// LossEvent represents a packet loss event
type LossEvent struct {
	Timestamp       time.Time
	PacketNumber    logging.PacketNumber
	EncryptionLevel logging.EncryptionLevel
	Reason          logging.PacketLossReason
}

// CongestionEvent represents congestion control state changes
type CongestionEvent struct {
	Timestamp time.Time
	State     logging.CongestionState
}

// ConnectionEvent represents connection lifecycle events
type ConnectionEvent struct {
	Timestamp time.Time
	EventType string
	Local     net.Addr
	Remote    net.Addr
	Error     error
}

// MOQObjectEvent represents MoQ object events
type MOQObjectEvent struct {
	Timestamp     time.Time
	GroupID       uint64
	SubGroupID    uint64
	ObjectID      uint64
	Size          int
	Direction     string // "sent" or "received"
	TransportType string // "datagram" or "stream"
	TrackName     string
	Namespace     []string
}

// QuicLogger is the main logging structure for MoQ transport
type QuicLogger struct {
	sessionID   string
	perspective logging.Perspective
	startTime   time.Time
	logLevel    LogLevel

	// Event storage
	ackEvents        []AckEvent
	lossEvents       []LossEvent
	congestionEvents []CongestionEvent
	connectionEvents []ConnectionEvent
	moqObjectEvents  []MOQObjectEvent

	// Metrics
	totalPacketsSent      uint64
	totalPacketsReceived  uint64
	totalPacketsAcked     uint64
	totalPacketsLost      uint64
	totalBytesTransferred uint64

	// Latest RTT (latency)
	latestRTT time.Duration

	// Thread safety
	mutex sync.RWMutex
}

// LoggerConfig holds configuration for the logger
type LoggerConfig struct {
	LogLevel          LogLevel
	MaxEventsPerType  int
	EnableTerminalLog bool
	EnableMetrics     bool
}

// NewQuicLogger creates a new QUIC logger for MoQ transport
func NewQuicLogger(sessionID string, perspective logging.Perspective, config *LoggerConfig) *QuicLogger {
	if config == nil {
		config = &LoggerConfig{
			LogLevel:          LogLevelInfo,
			MaxEventsPerType:  1000,
			EnableTerminalLog: true,
			EnableMetrics:     true,
		}
	}

	return &QuicLogger{
		sessionID:        sessionID,
		perspective:      perspective,
		startTime:        time.Now(),
		logLevel:         config.LogLevel,
		ackEvents:        make([]AckEvent, 0, config.MaxEventsPerType),
		lossEvents:       make([]LossEvent, 0, config.MaxEventsPerType),
		congestionEvents: make([]CongestionEvent, 0, config.MaxEventsPerType),
		connectionEvents: make([]ConnectionEvent, 0, config.MaxEventsPerType),
		moqObjectEvents:  make([]MOQObjectEvent, 0, config.MaxEventsPerType),
	}
}

func NewQuicLoggerWithDefaults(sessionID string) *QuicLogger {
	return NewQuicLogger(sessionID, logging.PerspectiveClient, nil)
}

// CreateConnectionTracer returns a QUIC connection tracer that feeds into this logger
func (ql *QuicLogger) CreateConnectionTracer() func(context.Context, logging.Perspective, logging.ConnectionID) *logging.ConnectionTracer {
	return func(ctx context.Context, p logging.Perspective, connID logging.ConnectionID) *logging.ConnectionTracer {
		ql.perspective = p
		return &logging.ConnectionTracer{
			StartedConnection: func(local, remote net.Addr, srcConnID, destConnID logging.ConnectionID) {
				ql.recordConnectionEvent("started", local, remote, nil)
				ql.logToTerminal(LogLevelInfo, "Connection started: %v -> %v", local, remote)
			},
			ClosedConnection: func(err error) {
				ql.recordConnectionEvent("closed", nil, nil, err)
				ql.logToTerminal(LogLevelInfo, "Connection closed: %v", err)
			},
			SentLongHeaderPacket: func(hdr *logging.ExtendedHeader, size logging.ByteCount, ecn logging.ECN, ack *logging.AckFrame, frames []logging.Frame) {
				ql.recordPacketSent(size)
				if ack != nil {
					ql.recordAckSent(ack)
				}
			},
			SentShortHeaderPacket: func(hdr *logging.ShortHeader, size logging.ByteCount, ecn logging.ECN, ack *logging.AckFrame, frames []logging.Frame) {
				ql.recordPacketSent(size)
				if ack != nil {
					ql.recordAckSent(ack)
				}
			},
			ReceivedLongHeaderPacket: func(hdr *logging.ExtendedHeader, size logging.ByteCount, ecn logging.ECN, frames []logging.Frame) {
				ql.recordPacketReceived(size)
				for _, frame := range frames {
					if ackFrame, ok := frame.(*logging.AckFrame); ok {
						ql.recordAckReceived(ackFrame)
					}
				}
			},
			ReceivedShortHeaderPacket: func(hdr *logging.ShortHeader, size logging.ByteCount, ecn logging.ECN, frames []logging.Frame) {
				ql.recordPacketReceived(size)
				for _, frame := range frames {
					if ackFrame, ok := frame.(*logging.AckFrame); ok {
						ql.recordAckReceived(ackFrame)
					}
				}
			},
			AcknowledgedPacket: func(level logging.EncryptionLevel, number logging.PacketNumber) {
				ql.recordPacketAcked(level, number)
				ql.logToTerminal(LogLevelDebug, "Packet acknowledged: level=%v, number=%d", level, number)
			},
			LostPacket: func(level logging.EncryptionLevel, number logging.PacketNumber, reason logging.PacketLossReason) {
				ql.recordPacketLost(level, number, reason)
				ql.logToTerminal(LogLevelWarn, "Packet lost: level=%v, number=%d, reason=%v", level, number, reason)
			},
			UpdatedCongestionState: func(state logging.CongestionState) {
				ql.recordCongestionStateChange(state)
				ql.logToTerminal(LogLevelDebug, "Congestion state updated: %v", state)
			},
			UpdatedMetrics: func(rttStats *logging.RTTStats, cwnd, bytesInFlight logging.ByteCount, packetsInFlight int) {
				ql.mutex.Lock()
				ql.latestRTT = rttStats.SmoothedRTT()
				ql.mutex.Unlock()
				ql.logToTerminal(LogLevelDebug, "Updated RTT: %v", rttStats.SmoothedRTT())
			},
			Close: func() {
				ql.logToTerminal(LogLevelInfo, "Connection tracer closed")
			},
		}
	}
}

// Record functions for different event types
func (ql *QuicLogger) recordConnectionEvent(eventType string, local, remote net.Addr, err error) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()

	event := ConnectionEvent{
		Timestamp: time.Now(),
		EventType: eventType,
		Local:     local,
		Remote:    remote,
		Error:     err,
	}
	ql.connectionEvents = append(ql.connectionEvents, event)
}

func (ql *QuicLogger) recordPacketSent(size logging.ByteCount) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()
	ql.totalPacketsSent++
	ql.totalBytesTransferred += uint64(size)
}

func (ql *QuicLogger) recordPacketReceived(size logging.ByteCount) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()
	ql.totalPacketsReceived++
	ql.totalBytesTransferred += uint64(size)
}

func (ql *QuicLogger) recordAckSent(ack *logging.AckFrame) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()

	event := AckEvent{
		Timestamp: time.Now(),
		DelayTime: ack.DelayTime,
		AckRanges: ack.AckRanges,
	}
	ql.ackEvents = append(ql.ackEvents, event)
	ql.logToTerminal(LogLevelTrace, "ACK sent: DelayTime=%v, Ranges=%v", ack.DelayTime, ack.AckRanges)
}

func (ql *QuicLogger) recordAckReceived(ack *logging.AckFrame) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()

	event := AckEvent{
		Timestamp: time.Now(),
		DelayTime: ack.DelayTime,
		AckRanges: ack.AckRanges,
	}
	ql.ackEvents = append(ql.ackEvents, event)
	ql.logToTerminal(LogLevelTrace, "ACK received: DelayTime=%v, Ranges=%v", ack.DelayTime, ack.AckRanges)
}

func (ql *QuicLogger) recordPacketAcked(level logging.EncryptionLevel, number logging.PacketNumber) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()

	event := AckEvent{
		Timestamp:       time.Now(),
		PacketNumber:    number,
		EncryptionLevel: level,
	}
	ql.ackEvents = append(ql.ackEvents, event)
	ql.totalPacketsAcked++
}

func (ql *QuicLogger) recordPacketLost(level logging.EncryptionLevel, number logging.PacketNumber, reason logging.PacketLossReason) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()

	event := LossEvent{
		Timestamp:       time.Now(),
		PacketNumber:    number,
		EncryptionLevel: level,
		Reason:          reason,
	}
	ql.lossEvents = append(ql.lossEvents, event)
	ql.totalPacketsLost++
}

func (ql *QuicLogger) recordCongestionStateChange(state logging.CongestionState) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()

	event := CongestionEvent{
		Timestamp: time.Now(),
		State:     state,
	}
	ql.congestionEvents = append(ql.congestionEvents, event)
}

// RecordMOQObject records MoQ-specific object events
func (ql *QuicLogger) RecordMOQObject(groupID, subGroupID, objectID uint64, size int, direction, trackName string, namespace []string) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()

	event := MOQObjectEvent{
		Timestamp:  time.Now(),
		GroupID:    groupID,
		SubGroupID: subGroupID,
		ObjectID:   objectID,
		Size:       size,
		Direction:  direction,
		TrackName:  trackName,
		Namespace:  namespace,
	}
	ql.moqObjectEvents = append(ql.moqObjectEvents, event)
	// ql.logToTerminal(LogLevelInfo, "MoQ Object %s: Track=%s, Group=%d, Object=%d, Size=%d",
	// 	direction, trackName, groupID, objectID, size)
}

// Getter functions for retrieving logged events
func (ql *QuicLogger) GetAckEvents() []AckEvent {
	ql.mutex.RLock()
	defer ql.mutex.RUnlock()
	result := make([]AckEvent, len(ql.ackEvents))
	copy(result, ql.ackEvents)
	return result
}

func (ql *QuicLogger) GetLossEvents() []LossEvent {
	ql.mutex.RLock()
	defer ql.mutex.RUnlock()
	result := make([]LossEvent, len(ql.lossEvents))
	copy(result, ql.lossEvents)
	return result
}

func (ql *QuicLogger) GetCongestionEvents() []CongestionEvent {
	ql.mutex.RLock()
	defer ql.mutex.RUnlock()
	result := make([]CongestionEvent, len(ql.congestionEvents))
	copy(result, ql.congestionEvents)
	return result
}

func (ql *QuicLogger) GetConnectionEvents() []ConnectionEvent {
	ql.mutex.RLock()
	defer ql.mutex.RUnlock()
	result := make([]ConnectionEvent, len(ql.connectionEvents))
	copy(result, ql.connectionEvents)
	return result
}

func (ql *QuicLogger) GetMOQObjectEvents() []MOQObjectEvent {
	ql.mutex.RLock()
	defer ql.mutex.RUnlock()
	result := make([]MOQObjectEvent, len(ql.moqObjectEvents))
	copy(result, ql.moqObjectEvents)
	return result
}

// GetMetrics returns basic connection metrics
func (ql *QuicLogger) GetMetrics() map[string]interface{} {
	ql.mutex.RLock()
	defer ql.mutex.RUnlock()

	duration := time.Since(ql.startTime)
	lossRate := float64(0)
	if ql.totalPacketsSent > 0 {
		lossRate = float64(ql.totalPacketsLost) / float64(ql.totalPacketsSent) * 100
	}

	return map[string]interface{}{
		"session_id":              ql.sessionID,
		"perspective":             ql.perspective.String(),
		"duration_seconds":        duration.Seconds(),
		"total_packets_sent":      ql.totalPacketsSent,
		"total_packets_received":  ql.totalPacketsReceived,
		"total_packets_acked":     ql.totalPacketsAcked,
		"total_packets_lost":      ql.totalPacketsLost,
		"loss_rate_percent":       lossRate,
		"total_bytes":             ql.totalBytesTransferred,
		"ack_events_count":        len(ql.ackEvents),
		"loss_events_count":       len(ql.lossEvents),
		"congestion_events_count": len(ql.congestionEvents),
		"moq_objects_count":       len(ql.moqObjectEvents),
	}
}

// SetLogLevel changes the current log level
func (ql *QuicLogger) SetLogLevel(level LogLevel) {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()
	ql.logLevel = level
}

// LogSummary prints a summary of the connection to the terminal
func (ql *QuicLogger) LogSummary() {
	metrics := ql.GetMetrics()

	ql.mutex.RLock()
	rtt := ql.latestRTT
	ql.mutex.RUnlock()

	log.Printf("=== QUIC Connection Summary for %s ===", ql.sessionID)
	log.Printf("Duration: %.2f seconds", metrics["duration_seconds"])
	log.Printf("Packets: Sent=%d, Received=%d, Acked=%d, Lost=%d",
		metrics["total_packets_sent"], metrics["total_packets_received"],
		metrics["total_packets_acked"], metrics["total_packets_lost"])
	log.Printf("Loss Rate: %.2f%%", metrics["loss_rate_percent"])
	log.Printf("Total Bytes: %d", metrics["total_bytes"])
	log.Printf("RTT: %v", rtt)
	log.Printf("Events: ACKs=%d, Losses=%d, Congestion=%d, MoQ Objects=%d",
		metrics["ack_events_count"], metrics["loss_events_count"],
		metrics["congestion_events_count"], metrics["moq_objects_count"])
	log.Printf("=======================================")
}

// LogDetailedStats prints detailed statistics including recent events
func (ql *QuicLogger) LogDetailedStats() {
	ql.LogSummary()

	// Show recent loss events
	lossEvents := ql.GetLossEvents()
	if len(lossEvents) > 0 {
		log.Printf("\\n=== Recent Packet Loss Events ===")
		recentLosses := lossEvents
		if len(recentLosses) > 5 {
			recentLosses = recentLosses[len(recentLosses)-5:]
		}
		for _, event := range recentLosses {
			log.Printf("Loss: Packet=%d, Level=%v, Reason=%v, Time=%v",
				event.PacketNumber, event.EncryptionLevel, event.Reason, event.Timestamp.Format("15:04:05.000"))
		}
	}

	// Show MoQ object transfer stats
	moqEvents := ql.GetMOQObjectEvents()
	if len(moqEvents) > 0 {
		log.Printf("\\n=== MoQ Object Transfer Stats ===")
		sentObjects := 0
		receivedObjects := 0
		totalSentBytes := 0
		totalReceivedBytes := 0

		for _, event := range moqEvents {
			if event.Direction == "sent" {
				sentObjects++
				totalSentBytes += event.Size
			} else {
				receivedObjects++
				totalReceivedBytes += event.Size
			}
		}

		log.Printf("Objects: Sent=%d, Received=%d", sentObjects, receivedObjects)
		log.Printf("MoQ Bytes: Sent=%d, Received=%d", totalSentBytes, totalReceivedBytes)

		// Show recent transfers
		recentMoQ := moqEvents
		if len(recentMoQ) > 3 {
			recentMoQ = recentMoQ[len(recentMoQ)-3:]
		}
		for _, event := range recentMoQ {
			log.Printf("MoQ %s: Track=%s, Group=%d, Object=%d, Size=%d bytes",
				event.Direction, event.TrackName, event.GroupID, event.ObjectID, event.Size)
		}
	}

	// Show congestion events
	congestionEvents := ql.GetCongestionEvents()
	if len(congestionEvents) > 0 {
		log.Printf("\\n=== Congestion Control Events ===")
		recentCongestion := congestionEvents
		if len(recentCongestion) > 3 {
			recentCongestion = recentCongestion[len(recentCongestion)-3:]
		}
		for _, event := range recentCongestion {
			log.Printf("Congestion: State=%v, Time=%v",
				event.State, event.Timestamp.Format("15:04:05.000"))
		}
	}
} // logToTerminal handles terminal output based on log level
func (ql *QuicLogger) logToTerminal(level LogLevel, format string, args ...interface{}) {
	if level <= ql.logLevel {
		prefix := fmt.Sprintf("[%s]", ql.sessionID)
		log.Printf(prefix+" "+format, args...)
	}
}

// Reset clears all stored events and metrics
func (ql *QuicLogger) Reset() {
	ql.mutex.Lock()
	defer ql.mutex.Unlock()

	ql.ackEvents = ql.ackEvents[:0]
	ql.lossEvents = ql.lossEvents[:0]
	ql.congestionEvents = ql.congestionEvents[:0]
	ql.connectionEvents = ql.connectionEvents[:0]
	ql.moqObjectEvents = ql.moqObjectEvents[:0]

	ql.totalPacketsSent = 0
	ql.totalPacketsReceived = 0
	ql.totalPacketsAcked = 0
	ql.totalPacketsLost = 0
	ql.totalBytesTransferred = 0
	ql.startTime = time.Now()
}
