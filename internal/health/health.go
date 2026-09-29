// Package health answers the Kubernetes probes. Liveness depends only on
// the heartbeat loop making progress, never on the backend being reachable:
// restarting the agent does not fix the network. Liveness never fails before
// the agent is ready: startup, including registration and its save retries,
// is not judged, because a restart there could lose a single-use bootstrap
// token.
package health

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// LivenessThreshold is max(5 × heartbeat interval, 5 min). One heartbeat
// attempt runs several sequential API server calls (permission checks,
// version, nodes) and backend calls, each with a 30 s timeout, so the worst
// case is about 4-5 minutes: the 5-minute floor must stay above that.
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

// New returns a State that is not ready. The liveness clock only starts at
// MarkReady.
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

// MarkReady records that startup is complete; readiness never goes back. It
// also resets the liveness clock, so the threshold counts from the end of
// startup.
func (s *State) MarkReady() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ready = true
	s.lastAttempt = s.now()
	s.mu.Unlock()
}

// Live returns an error when the heartbeat loop is stuck. It returns nil
// while the state is not ready (startup is never judged by liveness) and on
// a nil receiver.
func (s *State) Live() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return nil
	}
	if since := s.now().Sub(s.lastAttempt); since > s.threshold {
		return fmt.Errorf("no heartbeat attempt for %s (threshold %s)", since.Round(time.Second), s.threshold)
	}
	return nil
}

// Ready reports whether startup is complete. A nil State is always ready.
func (s *State) Ready() bool {
	if s == nil {
		return true
	}
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
