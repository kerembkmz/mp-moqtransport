package schedulers

import (
	"time"
)

type MoqtObject struct {
	GroupID   uint64
	ObjectID  uint64
	Payload   []byte
	Timestamp time.Time
	Priority  int // TODO: Priority should match the priority levels defined in the MOQT spec
}

type PathStats struct {
	Name        string
	IsConnected bool
	Latency     time.Duration
	Bandwidth   float64 // bytes per second
	PacketLoss  float64 // percentage 0-100
	ObjectsSent uint64
	BytesSent   uint64
	LastUsed    time.Time
	ErrorCount  uint64
	// TODO: Path Health could be added here as a computed metric
}

// PathSelector interface defines different path selection strategies
// This is the abstract base interface that all schedulers must implement
type PathSelector interface {
	// SelectPath chooses which path to use for sending an object
	// Returns the index of the selected path, or -1 if no path is available
	SelectPath(obj MoqtObject, pathStats []PathStats) int

	// GetName returns the name of the scheduler for logging
	GetName() string

	// Reset clears any internal state
	Reset()
}
