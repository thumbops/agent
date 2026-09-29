package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestLivenessThreshold(t *testing.T) {
	if got := LivenessThreshold(10 * time.Second); got != 5*time.Minute {
		t.Fatalf("short interval: %s", got)
	}
	if got := LivenessThreshold(2 * time.Minute); got != 10*time.Minute {
		t.Fatalf("long interval: %s", got)
	}
}

func TestLiveness(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := New(5*time.Minute, c.now)
	if err := s.Live(); err != nil {
		t.Fatalf("at start: %v", err)
	}
	c.t = c.t.Add(5*time.Minute + time.Second)
	if err := s.Live(); err == nil {
		t.Fatal("no heartbeat attempt past the threshold must fail")
	}
	s.HeartbeatAttempted() // a failed attempt counts too
	if err := s.Live(); err != nil {
		t.Fatalf("after an attempt: %v", err)
	}
}

func TestReadiness(t *testing.T) {
	s := New(time.Minute, nil)
	if s.Ready() {
		t.Fatal("ready before MarkReady")
	}
	s.MarkReady()
	if !s.Ready() {
		t.Fatal("not ready after MarkReady")
	}
}

func TestHandler(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	s := New(time.Minute, c.now)
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("metrics body")) })
	h := Handler(s, metrics)
	get := func(path string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec.Code, rec.Body.String()
	}

	if code, _ := get("/healthz"); code != 200 {
		t.Fatalf("/healthz at start: %d", code)
	}
	if code, _ := get("/readyz"); code != 503 {
		t.Fatalf("/readyz before MarkReady: %d", code)
	}
	s.MarkReady()
	if code, _ := get("/readyz"); code != 200 {
		t.Fatalf("/readyz after MarkReady: %d", code)
	}
	c.t = c.t.Add(2 * time.Minute)
	if code, body := get("/healthz"); code != 503 || !strings.Contains(body, "no heartbeat attempt") {
		t.Fatalf("/healthz past the threshold: %d %q", code, body)
	}
	if code, body := get("/metrics"); code != 200 || body != "metrics body" {
		t.Fatalf("/metrics: %d %q", code, body)
	}
	if code, _ := get("/other"); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
}

func TestNilIsSafe(t *testing.T) {
	var s *State
	s.HeartbeatAttempted()
	s.MarkReady()
}
