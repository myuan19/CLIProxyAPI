package unifiedrouting

import (
	"sync"
	"time"
)

// RouteActivityWindow is how long a route is considered "processing" after a request (20s).
const RouteActivityWindow = 20 * time.Second

// RouteActivityTracker records the last request time per route (alias entry).
// Used to decide timed vs untimed cooling: isProcessing = last request within RouteActivityWindow.
type RouteActivityTracker struct {
	mu   sync.RWMutex
	last map[string]time.Time
}

// NewRouteActivityTracker creates a new route activity tracker.
func NewRouteActivityTracker() *RouteActivityTracker {
	return &RouteActivityTracker{
		last: make(map[string]time.Time),
	}
}

// Mark records that the given route had a request now.
func (r *RouteActivityTracker) Mark(routeID string) {
	if routeID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last[routeID] = time.Now()
}

// IsProcessing returns true if the route had a request within the last RouteActivityWindow (20s).
func (r *RouteActivityTracker) IsProcessing(routeID string) bool {
	r.mu.RLock()
	t, ok := r.last[routeID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return time.Since(t) < RouteActivityWindow
}
