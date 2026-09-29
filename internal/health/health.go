// Package health answers the Kubernetes probes. Liveness depends only on
// the heartbeat loop making progress, never on the backend being reachable:
// restarting the agent does not fix the network.
package health

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// LivenessThreshold is max(5 × heartbeat interval, 5 min).
func LivenessThreshold(heartbeatInterval time.Duration) time.Duration {
	return max(5*heartbeatInterval, 5*time.Minute)
}

// State tracks liveness and readiness. Its methods that record events are
// safe on a nil *State.
type State struct {
	threshold time.Duration
	now       func() time.Time

	mu          sync.Mutex
	lastAttempt time.Time
	ready       bool
}

// New starts the liveness clock now: the process start counts as the last
// heartbeat attempt until the first one completes.
func New(threshold time.Duration, now func() time.Time) *State {
	if now == nil {
		now = time.Now
	}
	return &State{threshold: threshold, now: now, lastAttempt: now()}
}

// HeartbeatAttempted records a completed heartbeat attempt, whatever its outcome.
func (s *State) HeartbeatAttempted() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastAttempt = s.now()
	s.mu.Unlock()
}

// MarkReady records that startup is complete; readiness never goes back.
func (s *State) MarkReady() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
}

// Live returns an error when the heartbeat loop is stuck.
func (s *State) Live() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if since := s.now().Sub(s.lastAttempt); since > s.threshold {
		return fmt.Errorf("no heartbeat attempt for %s (threshold %s)", since.Round(time.Second), s.threshold)
	}
	return nil
}

func (s *State) Ready() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// Handler serves /healthz, /readyz and /metrics.
func Handler(s *State, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if err := s.Live(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.Ready() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /metrics", metrics)
	return mux
}
