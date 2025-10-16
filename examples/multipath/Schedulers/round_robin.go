package schedulers

import (
	"sync/atomic"
)

type RoundRobinSelector struct {
	counter atomic.Uint64
}

func NewRoundRobinSelector() *RoundRobinSelector {
	return &RoundRobinSelector{}
}

func (r *RoundRobinSelector) SelectPath(obj MoqtObject, pathStats []PathStats) int {
	availablePaths := make([]int, 0)
	for i, stats := range pathStats {
		if stats.IsConnected {
			availablePaths = append(availablePaths, i)
		}
	}

	if len(availablePaths) == 0 {
		return -1
	}

	index := r.counter.Add(1) % uint64(len(availablePaths))
	return availablePaths[index]
}

func (r *RoundRobinSelector) GetName() string {
	return "RR"
}

func (r *RoundRobinSelector) Reset() {
	r.counter.Store(0)
}
