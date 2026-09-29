# Health Probes and Prometheus Metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The agent serves `/healthz`, `/readyz` and `/metrics` on `--http-addr`, and the manifest and end-to-end tests use them.

**Architecture:** Two new packages: `internal/metrics` holds every Prometheus collector on a private registry, behind nil-safe methods, and `internal/health` holds the liveness and readiness state plus the HTTP handler. `backend`, `enroll` and `agent` receive an optional `*metrics.Metrics` (and the agent a `*health.State`) and record events. `main` starts the HTTP server before registration.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang` (`prometheus`, `prometheus/collectors`, `prometheus/promhttp`, `prometheus/testutil`), kind for the end-to-end tests.

**Spec:** `docs/superpowers/specs/2026-09-29-health-metrics-design.md`

## Global Constraints

- Everything in the repository is in English: code, comments, logs, errors, tests, docs, commit messages.
- Metric names use the prefix `thumbops_agent_`; labels never carry namespace, node or deployment names.
- Private registry only: never use `prometheus.DefaultRegisterer` or the global handler.
- A nil `*metrics.Metrics` and a nil `*health.State` must be valid everywhere (existing tests pass them as nil).
- Liveness threshold: `max(5 × heartbeat-interval, 5 min)`; readiness never goes back to `503`.
- `--http-addr` default `:9090`; empty disables the server.
- Do not break the invariants in `CLAUDE.md` ("Invariants not to break"); `go test -race ./...`, `go vet ./...` and `gofmt -l .` (empty) must pass after every task.
- Never run anything against a real cluster: the end-to-end tests use kind only (`test/e2e/run.sh` refuses any context other than `kind-thumbops-e2e`).
- Commit messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

---

### Task 1: `internal/metrics` package

**Files:**
- Modify: `go.mod`, `go.sum` (new dependency)
- Create: `internal/metrics/metrics.go`
- Test: `internal/metrics/metrics_test.go`

**Interfaces:**
- Produces:
  - `func New(version string) *Metrics`
  - `func (m *Metrics) Handler() http.Handler` (nil `m` → 404 handler)
  - `func (m *Metrics) BackendRequest(operation string, code int)` (`code` 0 → label `error`)
  - `func (m *Metrics) HeartbeatSucceeded(at time.Time)`
  - `func (m *Metrics) SetHeartbeatOnly()`
  - `func (m *Metrics) ActionOutcome(actionType, outcome string)`
  - `func (m *Metrics) ActionRunning(running bool)`
  - `func (m *Metrics) ObserveActionDuration(actionType string, d time.Duration)`
  - `func (m *Metrics) CertificateInUse(notAfter time.Time)`
  - `func (m *Metrics) CertificateRenewal(err error)`
  - `func (m *Metrics) StatusReady(ready bool)`
  - `func (m *Metrics) StatusSent(at time.Time)`
  - constants `OutcomeExpired = "expired"`, `OutcomeDiscarded = "discarded"`; the other outcomes are `protocol.StatusSucceeded`, `protocol.StatusFailed`, `protocol.StatusRejected`
  - backend operation constants: `OpRegister = "register"`, `OpRenew = "renew"`, `OpHeartbeat = "heartbeat"`, `OpPoll = "poll"`, `OpClaim = "claim"`, `OpResult = "result"`, `OpStatus = "status"`

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/prometheus/client_golang@latest`
Expected: `go.mod` lists `github.com/prometheus/client_golang` in the main `require` block after `go mod tidy` (run it at the end of the task).

- [ ] **Step 2: Write the failing test**

`internal/metrics/metrics_test.go`:

```go
package metrics

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordedValues(t *testing.T) {
	m := New("1.2.3")
	m.BackendRequest(OpHeartbeat, 200)
	m.BackendRequest(OpPoll, 503)
	m.BackendRequest(OpPoll, 0)
	m.HeartbeatSucceeded(time.Unix(1700000000, 0))
	m.SetHeartbeatOnly()
	m.ActionOutcome("scale", "succeeded")
	m.ActionOutcome("delete-everything", "rejected")
	m.ActionRunning(true)
	m.ObserveActionDuration("drain", 90*time.Second)
	m.CertificateInUse(time.Unix(1800000000, 0))
	m.CertificateRenewal(nil)
	m.CertificateRenewal(errors.New("boom"))
	m.StatusReady(true)
	m.StatusSent(time.Unix(1700000100, 0))

	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"heartbeat 200", testutil.ToFloat64(m.backendRequests.WithLabelValues("heartbeat", "200")), 1},
		{"poll 503", testutil.ToFloat64(m.backendRequests.WithLabelValues("poll", "503")), 1},
		{"poll error", testutil.ToFloat64(m.backendRequests.WithLabelValues("poll", "error")), 1},
		{"last heartbeat", testutil.ToFloat64(m.heartbeatLastSuccess), 1700000000},
		{"heartbeat only", testutil.ToFloat64(m.heartbeatOnly), 1},
		{"scale succeeded", testutil.ToFloat64(m.actions.WithLabelValues("scale", "succeeded")), 1},
		{"unknown type", testutil.ToFloat64(m.actions.WithLabelValues("unknown", "rejected")), 1},
		{"in progress", testutil.ToFloat64(m.actionInProgress), 1},
		{"cert expiry", testutil.ToFloat64(m.certExpiry), 1800000000},
		{"renewal success", testutil.ToFloat64(m.renewals.WithLabelValues("success")), 1},
		{"renewal error", testutil.ToFloat64(m.renewals.WithLabelValues("error")), 1},
		{"status ready", testutil.ToFloat64(m.statusReady), 1},
		{"status sent", testutil.ToFloat64(m.statusLastSent), 1700000100},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if n := testutil.CollectAndCount(m.actionDuration); n != 1 {
		t.Errorf("duration series: %d", n)
	}
}

// Every action type and outcome starts at 0, so rate() and alerts work
// before the first action.
func TestActionSeriesStartAtZero(t *testing.T) {
	m := New("1.2.3")
	if n := testutil.CollectAndCount(m.actions); n != 5*5 {
		t.Fatalf("pre-initialized action series: %d, want 25", n)
	}
}

// The metric names are a contract for whoever writes dashboards and alerts.
func TestExposedNames(t *testing.T) {
	m := New("1.2.3")
	m.BackendRequest(OpHeartbeat, 200)
	m.ObserveActionDuration("scale", time.Second)
	m.CertificateRenewal(nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, name := range []string{
		`thumbops_agent_info{version="1.2.3"} 1`,
		"thumbops_agent_backend_requests_total",
		"thumbops_agent_heartbeat_last_success_timestamp_seconds",
		"thumbops_agent_heartbeat_only",
		"thumbops_agent_actions_total",
		"thumbops_agent_action_duration_seconds_bucket",
		"thumbops_agent_action_in_progress",
		"thumbops_agent_certificate_expiry_timestamp_seconds",
		"thumbops_agent_certificate_renewals_total",
		"thumbops_agent_status_ready",
		"thumbops_agent_status_last_sent_timestamp_seconds",
		"go_goroutines",
	} {
		if !strings.Contains(string(body), name) {
			t.Errorf("/metrics does not expose %s", name)
		}
	}
}

func TestNilIsSafe(t *testing.T) {
	var m *Metrics
	m.BackendRequest(OpPoll, 200)
	m.HeartbeatSucceeded(time.Now())
	m.SetHeartbeatOnly()
	m.ActionOutcome("scale", "succeeded")
	m.ActionRunning(true)
	m.ObserveActionDuration("scale", time.Second)
	m.CertificateInUse(time.Now())
	m.CertificateRenewal(nil)
	m.StatusReady(true)
	m.StatusSent(time.Now())
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 404 {
		t.Fatalf("nil metrics handler: %d", rec.Code)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/metrics/`
Expected: FAIL, build errors (`undefined: New`, …).

- [ ] **Step 4: Write the implementation**

`internal/metrics/metrics.go`:

```go
// Package metrics holds the agent's Prometheus metrics on a private
// registry. Every method is safe on a nil *Metrics, which records nothing.
package metrics

import (
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/thumbops/agent/internal/protocol"
)

const namespace = "thumbops_agent"

// Outcomes of thumbops_agent_actions_total besides the result statuses
// (protocol.StatusSucceeded, StatusFailed, StatusRejected).
const (
	OutcomeExpired   = "expired"   // received after its deadline, not claimed
	OutcomeDiscarded = "discarded" // claim answered 409 or 410
)

// Backend operations, the operation label of thumbops_agent_backend_requests_total.
const (
	OpRegister  = "register"
	OpRenew     = "renew"
	OpHeartbeat = "heartbeat"
	OpPoll      = "poll"
	OpClaim     = "claim"
	OpResult    = "result"
	OpStatus    = "status"
)

var outcomes = []string{protocol.StatusSucceeded, protocol.StatusFailed, protocol.StatusRejected, OutcomeExpired, OutcomeDiscarded}

type Metrics struct {
	reg                  *prometheus.Registry
	backendRequests      *prometheus.CounterVec
	heartbeatLastSuccess prometheus.Gauge
	heartbeatOnly        prometheus.Gauge
	actions              *prometheus.CounterVec
	actionDuration       *prometheus.HistogramVec
	actionInProgress     prometheus.Gauge
	certExpiry           prometheus.Gauge
	renewals             *prometheus.CounterVec
	statusReady          prometheus.Gauge
	statusLastSent       prometheus.Gauge
}

func New(version string) *Metrics {
	gauge := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help})
	}
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		backendRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "backend_requests_total",
			Help: "Calls to the backend by operation and HTTP status (error = network error or TLS alert).",
		}, []string{"operation", "code"}),
		heartbeatLastSuccess: gauge("heartbeat_last_success_timestamp_seconds", "Unix time of the last successful heartbeat."),
		heartbeatOnly:        gauge("heartbeat_only", "1 after a 426: the agent version is no longer supported and only the heartbeat runs."),
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "actions_total",
			Help: "Actions received, by type and outcome (succeeded, failed, rejected, expired, discarded).",
		}, []string{"type", "outcome"}),
		actionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "action_duration_seconds",
			Help:    "Execution time of the claimed actions.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800},
		}, []string{"type"}),
		actionInProgress: gauge("action_in_progress", "1 while an action runs."),
		certExpiry:       gauge("certificate_expiry_timestamp_seconds", "Unix time when the certificate in use expires."),
		renewals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "certificate_renewals_total",
			Help: "Certificate renewals by result (success, error).",
		}, []string{"result"}),
		statusReady:    gauge("status_ready", "1 when the cluster status informers are synced (0 also when the status is disabled)."),
		statusLastSent: gauge("status_last_sent_timestamp_seconds", "Unix time of the last cluster status accepted by the backend."),
	}
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: "info", Help: "Agent version, always 1.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	info.Set(1)
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		info, m.backendRequests, m.heartbeatLastSuccess, m.heartbeatOnly, m.actions,
		m.actionDuration, m.actionInProgress, m.certExpiry, m.renewals, m.statusReady, m.statusLastSent,
	)
	for _, t := range protocol.ActionTypes {
		for _, o := range outcomes {
			m.actions.WithLabelValues(t, o)
		}
	}
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// BackendRequest counts a backend call; code 0 means no HTTP response.
func (m *Metrics) BackendRequest(operation string, code int) {
	if m == nil {
		return
	}
	label := "error"
	if code != 0 {
		label = strconv.Itoa(code)
	}
	m.backendRequests.WithLabelValues(operation, label).Inc()
}

func (m *Metrics) HeartbeatSucceeded(at time.Time) {
	if m == nil {
		return
	}
	m.heartbeatLastSuccess.Set(float64(at.Unix()))
}

func (m *Metrics) SetHeartbeatOnly() {
	if m == nil {
		return
	}
	m.heartbeatOnly.Set(1)
}

func (m *Metrics) ActionOutcome(actionType, outcome string) {
	if m == nil {
		return
	}
	m.actions.WithLabelValues(typeLabel(actionType), outcome).Inc()
}

func (m *Metrics) ActionRunning(running bool) {
	if m == nil {
		return
	}
	m.actionInProgress.Set(boolValue(running))
}

func (m *Metrics) ObserveActionDuration(actionType string, d time.Duration) {
	if m == nil {
		return
	}
	m.actionDuration.WithLabelValues(typeLabel(actionType)).Observe(d.Seconds())
}

func (m *Metrics) CertificateInUse(notAfter time.Time) {
	if m == nil {
		return
	}
	m.certExpiry.Set(float64(notAfter.Unix()))
}

func (m *Metrics) CertificateRenewal(err error) {
	if m == nil {
		return
	}
	result := "success"
	if err != nil {
		result = "error"
	}
	m.renewals.WithLabelValues(result).Inc()
}

func (m *Metrics) StatusReady(ready bool) {
	if m == nil {
		return
	}
	m.statusReady.Set(boolValue(ready))
}

func (m *Metrics) StatusSent(at time.Time) {
	if m == nil {
		return
	}
	m.statusLastSent.Set(float64(at.Unix()))
}

// typeLabel keeps the type label to the known action types.
func typeLabel(t string) string {
	if slices.Contains(protocol.ActionTypes, t) {
		return t
	}
	return "unknown"
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go mod tidy && go test -race ./internal/metrics/ && go vet ./internal/metrics/ && gofmt -l .`
Expected: PASS, no vet output, `gofmt -l` prints nothing.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/metrics
git commit -m "Add the Prometheus metrics package

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: `internal/health` package

**Files:**
- Create: `internal/health/health.go`
- Test: `internal/health/health_test.go`

**Interfaces:**
- Produces:
  - `func LivenessThreshold(heartbeatInterval time.Duration) time.Duration`
  - `func New(threshold time.Duration, now func() time.Time) *State` (`now` nil → `time.Now`)
  - `func (s *State) HeartbeatAttempted()`, `func (s *State) MarkReady()` (both nil-safe)
  - `func (s *State) Live() error`, `func (s *State) Ready() bool`
  - `func Handler(s *State, metrics http.Handler) http.Handler` serving `/healthz`, `/readyz`, `/metrics`

- [ ] **Step 1: Write the failing test**

`internal/health/health_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/health/`
Expected: FAIL, build errors (`undefined: New`, …).

- [ ] **Step 3: Write the implementation**

`internal/health/health.go`:

```go
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/health/ && go vet ./internal/health/ && gofmt -l .`
Expected: PASS, nothing printed by vet and gofmt.

- [ ] **Step 5: Commit**

```bash
git add internal/health
git commit -m "Add the health package for the liveness and readiness probes

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Count backend requests

**Files:**
- Modify: `internal/backend/client.go` (`Options`, `Client`, `do` and its callers)
- Test: `internal/backend/backend_test.go`

**Interfaces:**
- Consumes: `metrics.Metrics.BackendRequest(operation string, code int)`, `metrics.Op*` constants (Task 1).
- Produces: `backend.Options.Metrics *metrics.Metrics`.

- [ ] **Step 1: Write the failing test**

Append to `internal/backend/backend_test.go` (add the imports `strings` and `github.com/thumbops/agent/internal/metrics`):

```go
func TestBackendRequestsAreCounted(t *testing.T) {
	mb, srv, roots := startTLS(t)
	m := metrics.New("test")
	holder := &identity.Holder{}
	c := backend.New(backend.Options{BaseURL: srv.URL, TLS: holder.ClientTLS(roots), Metrics: m})

	e := &enroll.Enroller{Backend: c, Store: identity.FileStore{Dir: t.TempDir()}, Holder: holder}
	if _, err := e.Start(context.Background(), "bootstrap-test-token", func(context.Context) (protocol.RegisterRequest, error) { return info, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); err != nil {
		t.Fatal(err)
	}
	mb.SetPollStatus(http.StatusServiceUnavailable)
	if _, err := c.PollActions(context.Background(), 1); backend.Code(err) != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %v", err)
	}
	srv.Close()
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); err == nil {
		t.Fatal("expected a network error")
	}

	body := scrape(t, m)
	for _, line := range []string{
		`thumbops_agent_backend_requests_total{code="200",operation="register"} 1`,
		`thumbops_agent_backend_requests_total{code="200",operation="heartbeat"} 1`,
		`thumbops_agent_backend_requests_total{code="503",operation="poll"} 1`,
		`thumbops_agent_backend_requests_total{code="error",operation="heartbeat"} 1`,
	} {
		if !strings.Contains(body, line) {
			t.Errorf("missing %s in:\n%s", line, body)
		}
	}
}

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/backend/ -run TestBackendRequestsAreCounted`
Expected: FAIL, `unknown field Metrics in struct literal of type backend.Options`.

- [ ] **Step 3: Implement**

In `internal/backend/client.go`:

1. Add `Metrics *metrics.Metrics // optional: counts every call` to `Options`, and a `metrics *metrics.Metrics` field to `Client`, set in `New` from `opts.Metrics` (read `New` and follow how the other options are copied).
2. Change the signature of `do` to take the operation first:

```go
func (c *Client) do(ctx context.Context, op, method, path, bearer string, timeout time.Duration, in, out any) (int, error) {
```

and right after `resp, err := c.http.Load().Do(req)`:

```go
	resp, err := c.http.Load().Do(req)
	if err != nil {
		c.metrics.BackendRequest(op, 0)
		return 0, err
	}
	c.metrics.BackendRequest(op, resp.StatusCode)
	defer resp.Body.Close()
```

3. Pass the operation in every caller: `Register` → `metrics.OpRegister`, `RenewCertificate` → `metrics.OpRenew`, `Heartbeat` → `metrics.OpHeartbeat`, `PollActions` → `metrics.OpPoll`, `Claim` → `metrics.OpClaim`, `SendResult` → `metrics.OpResult`, `PutStatus` → `metrics.OpStatus`. Example:

```go
	if _, err := c.do(ctx, metrics.OpHeartbeat, http.MethodPut, "/v1/agent/heartbeat", "", defaultTimeout, req, &out); err != nil {
```

4. Import `github.com/thumbops/agent/internal/metrics`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/backend/ ./internal/agent/ ./internal/enroll/ && go vet ./... && gofmt -l .`
Expected: PASS; existing tests pass with `Metrics` nil.

- [ ] **Step 5: Commit**

```bash
git add internal/backend
git commit -m "Count backend requests by operation and status

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Certificate metrics in the Enroller

**Files:**
- Modify: `internal/enroll/enroll.go`
- Test: `internal/enroll/enroll_test.go`

**Interfaces:**
- Consumes: `metrics.Metrics.CertificateInUse(time.Time)`, `CertificateRenewal(error)` (Task 1).
- Produces: `enroll.Enroller.Metrics *metrics.Metrics`.

- [ ] **Step 1: Write the failing test**

Append to `internal/enroll/enroll_test.go` (add the imports `net/http/httptest` if missing, `strconv`, `strings` and `github.com/thumbops/agent/internal/metrics`):

```go
func TestCertificateMetrics(t *testing.T) {
	e := newEnv(t)
	e.mb.CertLifetime = 3 * time.Hour
	m := metrics.New("test")
	en, c := e.agent()
	en.Metrics = m
	if _, err := en.Start(context.Background(), "bootstrap-test-token", infoFn); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); err != nil {
		t.Fatal(err)
	}
	first := en.Holder.Get().Leaf
	if v := value(t, scrape(m), "thumbops_agent_certificate_expiry_timestamp_seconds"); v != float64(first.NotAfter.Unix()) {
		t.Fatalf("expiry after registration: %v", v)
	}

	e.store.(*memStore).failures = 1
	if _, err := en.Renew(context.Background(), first.NotAfter.Add(-time.Hour)); err == nil {
		t.Fatal("expected the save error")
	}
	if _, err := en.Renew(context.Background(), first.NotAfter.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A certificate that does not need renewal is not an attempt.
	if _, err := en.Renew(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}

	body := scrape(m)
	for series, want := range map[string]float64{
		`thumbops_agent_certificate_renewals_total{result="error"}`:   1,
		`thumbops_agent_certificate_renewals_total{result="success"}`: 1,
		"thumbops_agent_certificate_expiry_timestamp_seconds":         float64(en.Holder.Get().Leaf.NotAfter.Unix()),
	} {
		if got := value(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}

func scrape(m *metrics.Metrics) string {
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// value returns the sample of an exact series line; Prometheus may print
// large values in exponent form, which ParseFloat reads.
func value(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == series {
			v, err := strconv.ParseFloat(f[1], 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("series %s not found", series)
	return 0
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/enroll/ -run TestCertificateMetrics`
Expected: FAIL, `en.Metrics undefined`.

- [ ] **Step 3: Implement**

In `internal/enroll/enroll.go`:

1. Add the field `Metrics *metrics.Metrics // optional` to `Enroller` (after `Logger`).
2. In `activate`, after `e.Holder.Set(cert)`: `e.Metrics.CertificateInUse(cert.Leaf.NotAfter)`.
3. Split `Renew` so every attempt is counted once:

```go
// Renew renews the certificate when less than a third of its validity is
// left, with the same key. It returns true if it renewed.
func (e *Enroller) Renew(ctx context.Context, now time.Time) (bool, error) {
	cur := e.Holder.Get()
	if e.state == nil || cur == nil || cur.Leaf == nil || !identity.NeedsRenewal(cur.Leaf, now) {
		return false, nil
	}
	err := e.renew(ctx)
	e.Metrics.CertificateRenewal(err)
	return err == nil, err
}

func (e *Enroller) renew(ctx context.Context) error {
	csr, err := identity.CSR(e.state.Key)
	if err != nil {
		return err
	}
	resp, err := e.Backend.RenewCertificate(ctx, csr)
	if err != nil {
		return err
	}
	next := *e.state
	next.CertPEM = resp.Certificate
	// Saved first: if saving fails the old certificate stays in use and on
	// disk, and the renewal is tried again at the next heartbeat.
	if err := e.Store.Save(ctx, &next); err != nil {
		return err
	}
	return e.activate(&next)
}
```

4. Import `github.com/thumbops/agent/internal/metrics`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/enroll/ ./internal/backend/ && go vet ./... && gofmt -l .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/enroll
git commit -m "Expose certificate expiry and renewals as metrics

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Agent metrics and health hooks

**Files:**
- Modify: `internal/agent/agent.go` (`Config`, `Run`, `heartbeat`, `setHeartbeatOnly`, `handle`, `statusLoop`)
- Test: `internal/agent/agent_test.go`

**Interfaces:**
- Consumes: Task 1 methods (`HeartbeatSucceeded`, `SetHeartbeatOnly`, `ActionOutcome`, `ActionRunning`, `ObserveActionDuration`, `StatusReady`, `StatusSent`, `OutcomeExpired`, `OutcomeDiscarded`); Task 2 `*health.State` with `HeartbeatAttempted`, `MarkReady`, `Ready`, `Live`.
- Produces: `agent.Config.Metrics *metrics.Metrics`, `agent.Config.Health *health.State`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/agent_test.go` (add the imports `strconv`, `github.com/thumbops/agent/internal/health` and `github.com/thumbops/agent/internal/metrics`):

```go
func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

// value returns the sample of an exact series line such as
// `thumbops_agent_actions_total{outcome="succeeded",type="scale"}`.
func value(t *testing.T, body, series string) float64 {
	t.Helper()
	for _, l := range strings.Split(body, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == series {
			v, err := strconv.ParseFloat(f[1], 64)
			if err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	t.Fatalf("series %s not found", series)
	return 0
}

func TestActionMetrics(t *testing.T) {
	m := metrics.New("test")
	e := newEnv(t, nil, func(c *Config) { c.Metrics = m })
	ctx := context.Background()
	soon := time.Now().Add(time.Minute)

	e.mb.Enqueue(protocol.Action{ActionID: "ok", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}, ExpiresAt: soon})
	e.agent.handle(ctx, protocol.Action{ActionID: "ok", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}, ExpiresAt: soon})

	e.mb.Enqueue(protocol.Action{ActionID: "sys", Type: protocol.ActionRolloutRestart, Params: protocol.Params{Namespace: "kube-system", Deployment: "coredns"}, ExpiresAt: soon})
	e.agent.handle(ctx, protocol.Action{ActionID: "sys", Type: protocol.ActionRolloutRestart, Params: protocol.Params{Namespace: "kube-system", Deployment: "coredns"}, ExpiresAt: soon})

	e.agent.handle(ctx, protocol.Action{ActionID: "old", Type: protocol.ActionScale, ExpiresAt: time.Now().Add(-time.Minute)})

	e.mb.ClaimOverride["taken"] = http.StatusConflict
	e.mb.Enqueue(protocol.Action{ActionID: "taken", Type: protocol.ActionUncordon, Params: protocol.Params{Node: "worker-1"}, ExpiresAt: soon})
	e.agent.handle(ctx, protocol.Action{ActionID: "taken", Type: protocol.ActionUncordon, Params: protocol.Params{Node: "worker-1"}, ExpiresAt: soon})

	body := scrape(t, m)
	for series, want := range map[string]float64{
		`thumbops_agent_actions_total{outcome="succeeded",type="cordon"}`:       1,
		`thumbops_agent_actions_total{outcome="rejected",type="rollout-restart"}`: 1,
		`thumbops_agent_actions_total{outcome="expired",type="scale"}`:          1,
		`thumbops_agent_actions_total{outcome="discarded",type="uncordon"}`:     1,
		`thumbops_agent_action_duration_seconds_count{type="cordon"}`:           1,
		`thumbops_agent_action_in_progress`:                                     0,
	} {
		if got := value(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}

func TestHeartbeatMetricsAndHealth(t *testing.T) {
	m := metrics.New("test")
	hs := health.New(time.Hour, nil)
	e := newEnv(t, nil, func(c *Config) { c.Metrics = m; c.Health = hs })
	e.mb.SetPollStatus(http.StatusUpgradeRequired)
	if hs.Ready() {
		t.Fatal("ready before Run")
	}
	stop := e.run(t)
	time.Sleep(200 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !hs.Ready() {
		t.Fatal("not ready after Run started")
	}
	body := scrape(t, m)
	if v := value(t, body, "thumbops_agent_heartbeat_last_success_timestamp_seconds"); time.Since(time.Unix(int64(v), 0)) > time.Minute {
		t.Fatalf("last heartbeat timestamp not recent: %v", v)
	}
	if v := value(t, body, "thumbops_agent_heartbeat_only"); v != 1 {
		t.Fatalf("heartbeat_only after 426: %v", v)
	}
}

func TestHeartbeatAttemptKeepsLivenessWhenBackendIsDown(t *testing.T) {
	now := time.Unix(1000, 0)
	hs := health.New(time.Minute, func() time.Time { return now })
	e := newEnv(t, nil, func(c *Config) { c.Health = hs })
	e.mb.SetHeartbeatStatus(http.StatusServiceUnavailable)
	now = now.Add(2 * time.Minute) // past the threshold
	if err := e.agent.heartbeat(context.Background()); err == nil {
		t.Fatal("expected the heartbeat to fail")
	}
	if err := hs.Live(); err != nil {
		t.Fatalf("a failed heartbeat attempt must keep the agent live: %v", err)
	}
}
```

The last test needs a way to make the heartbeat fail. Check `internal/mockbackend/server.go` for an existing hook (`SetPollStatus`, `SetStatusCode` exist); if there is no heartbeat equivalent, add one in this task:

```go
// SetHeartbeatStatus makes the heartbeat respond with code (0 = normal).
func (s *Server) SetHeartbeatStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeatCode = code
}
```

with a `heartbeatCode int` field in `Server`, checked at the top of `heartbeat` after decoding:

```go
	s.mu.Lock()
	code := s.heartbeatCode
	s.mu.Unlock()
	if code != 0 {
		http.Error(w, http.StatusText(code), code)
		return
	}
```

(Add `internal/mockbackend/server.go` to the files of this task and to the commit.)

Extend `TestStatusSentWhenReadyAndOnRequest` for `status_ready`: create `m := metrics.New("test")`, set `c.Metrics = m` in its option func, and add after the first `time.Sleep(100 * time.Millisecond)` block:

```go
	if v := value(t, scrape(t, m), "thumbops_agent_status_ready"); v != 0 {
		t.Fatalf("status_ready before the caches sync: %v", v)
	}
```

and after `waitStatuses(t, e, 1)`:

```go
	body := scrape(t, m)
	if v := value(t, body, "thumbops_agent_status_ready"); v != 1 {
		t.Fatalf("status_ready after sync: %v", v)
	}
	if v := value(t, body, "thumbops_agent_status_last_sent_timestamp_seconds"); v == 0 {
		t.Fatal("status_last_sent not set")
	}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/`
Expected: FAIL, `unknown field Metrics in struct literal of type Config`.

- [ ] **Step 3: Implement**

In `internal/agent/agent.go`:

1. `Config`, after `StatusRetry`:

```go
	// Metrics and Health are optional (nil records nothing).
	Metrics *metrics.Metrics
	Health  *health.State
```

2. `Run`, first line after `defer cancel()`: `a.cfg.Health.MarkReady()`, with the comment `// startup is complete: policy and identity are loaded`.

3. `heartbeat`, first line: `defer a.cfg.Health.HeartbeatAttempted() // any outcome proves the loop is alive`. After `resp, err := a.backend.Heartbeat(ctx, req)` and its error check: `a.cfg.Metrics.HeartbeatSucceeded(a.cfg.Now())`.

4. `setHeartbeatOnly`: after unlocking, `a.cfg.Metrics.SetHeartbeatOnly()`.

5. `handle`:
   - in the expired branch, before `return`: `a.cfg.Metrics.ActionOutcome(act.Type, metrics.OutcomeExpired)`;
   - in the claim error switch, for `http.StatusConflict` and `http.StatusGone`: `a.cfg.Metrics.ActionOutcome(act.Type, metrics.OutcomeDiscarded)` (not in `default`);
   - in the policy rejection branch: `a.cfg.Metrics.ActionOutcome(act.Type, protocol.StatusRejected)`;
   - around the execution:

```go
		log.Info("running action", "params", act.Params, "requested_by", act.RequestedBy)
		a.cfg.Metrics.ActionRunning(true)
		start := time.Now()
		res = a.exec.Execute(ctx, act)
		a.cfg.Metrics.ObserveActionDuration(act.Type, time.Since(start))
		a.cfg.Metrics.ActionRunning(false)
		a.cfg.Metrics.ActionOutcome(act.Type, res.Status)
		log.Info("action finished", "status", res.Status, "message", res.Message)
```

6. `statusLoop`: right after `s, ok := a.cfg.Status.Collect(a.serverNow())`: `a.cfg.Metrics.StatusReady(ok)`. Replace the `PutStatus` block with:

```go
		if err := a.backend.PutStatus(ctx, s); err != nil {
			if backend.Unauthorized(err) {
				stop(unauthorized(err))
				return
			}
			if ctx.Err() == nil {
				a.log.Warn("sending the cluster status failed", "err", err)
			}
			continue
		}
		a.cfg.Metrics.StatusSent(a.cfg.Now())
```

7. Imports: `github.com/thumbops/agent/internal/health`, `github.com/thumbops/agent/internal/metrics`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -race ./internal/... && go vet ./... && gofmt -l .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent internal/mockbackend
git commit -m "Record action, heartbeat and status metrics and feed the probes

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: HTTP server in `main`

**Files:**
- Modify: `cmd/thumbops-agent/main.go`

**Interfaces:**
- Consumes: `metrics.New`, `(*Metrics).Handler`, `health.New`, `health.LivenessThreshold`, `health.Handler`, `backend.Options.Metrics`, `enroll.Enroller.Metrics`, `agent.Config.Metrics`, `agent.Config.Health`.

- [ ] **Step 1: Add the flag**

In the `flag` block:

```go
		httpAddr    = flag.String("http-addr", ":9090", "address for /healthz, /readyz and /metrics (empty = disabled)")
```

- [ ] **Step 2: Start the server before anything that can take long**

Right after `ctx, stop := signal.NotifyContext(...)` and `defer stop()`:

```go
	m := metrics.New(version)
	hs := health.New(health.LivenessThreshold(*hbInterval), nil)
	if *httpAddr != "" {
		// Started before registration: liveness answers while the agent
		// registers, readiness answers 503 until Run starts.
		srv := &http.Server{Addr: *httpAddr, Handler: health.Handler(hs, m.Handler()), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fatal("health and metrics server: %v", err)
			}
		}()
		go func() {
			<-ctx.Done()
			srv.Close()
		}()
	}
```

- [ ] **Step 3: Pass metrics and health on**

- `cfg := agent.Config{Version: version, HeartbeatInterval: *hbInterval, Logger: log, Metrics: m, Health: hs}`
- both `backend.New(backend.Options{...})` calls get `Metrics: m`
- `en := &enroll.Enroller{Backend: b, Store: store, Holder: holder, Logger: log, Metrics: m}`
- imports: `net/http`, `github.com/thumbops/agent/internal/health`, `github.com/thumbops/agent/internal/metrics`

- [ ] **Step 4: Verify locally with the mock backend (HTTP, no cluster needed for the probes)**

Run in three terminals (or background jobs):

```bash
go run ./cmd/mock-backend -addr 127.0.0.1:8080
echo '{"allowed_actions":[]}' > "$TMPDIR/policy.json"
go run ./cmd/thumbops-agent --dev-insecure --backend-url http://127.0.0.1:8080 --kube-api http://127.0.0.1:1 --policy-file "$TMPDIR/policy.json" --status=false --http-addr 127.0.0.1:19090
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:19090/healthz
curl -s -o /dev/null -w '%{http_code}\n' 127.0.0.1:19090/readyz
curl -s 127.0.0.1:19090/metrics | grep thumbops_agent_info
```

Expected: `200`, `200`, `thumbops_agent_info{version="0.1.0-dev"} 1`. (The unreachable `--kube-api` only makes heartbeat fields empty; the heartbeat still reaches the mock backend.) Stop both processes afterwards.

- [ ] **Step 5: Run the checks and commit**

Run: `go build ./... && go vet ./... && gofmt -l . && go test -race ./...`
Expected: all pass, nothing printed by gofmt.

```bash
git add cmd/thumbops-agent/main.go
git commit -m "Serve /healthz, /readyz and /metrics on --http-addr

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Manifest, end-to-end tests and docs

**Files:**
- Modify: `deploy/agent.yaml`, `test/e2e/run.sh`, `README.md`, `CLAUDE.md`

**Interfaces:**
- Consumes: the endpoints and metric names from Tasks 1–6.

- [ ] **Step 1: Manifest**

In `deploy/agent.yaml`, Deployment `thumbops-agent`:

- under `spec.template.metadata`, next to `labels`:

```yaml
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
        prometheus.io/path: /metrics
```

- in the container, between `args` and `resources`:

```yaml
          ports:
            - name: http
              containerPort: 9090
          # Liveness only asks the agent: the real threshold (heartbeat loop
          # stuck for max(5 × interval, 5 min)) is computed there.
          livenessProbe:
            httpGet: { path: /healthz, port: http }
            periodSeconds: 30
            failureThreshold: 3
          readinessProbe:
            httpGet: { path: /readyz, port: http }
            periodSeconds: 5
```

Run: `go test ./internal/policy/` (the manifest policy test must still pass).

- [ ] **Step 2: End-to-end checks**

In `test/e2e/run.sh`:

1. After `pf_pid=""` add `agent_pf_pid=""`, and in `cleanup` kill it too: `[[ -n $agent_pf_pid ]] && kill "$agent_pf_pid" 2>/dev/null || true`.
2. Add helpers next to the others:

```bash
METRICS_PORT=${METRICS_PORT:-19090}

# metric SERIES: value of an exact series, e.g. 'thumbops_agent_status_ready'.
metric() {
  curl -fsS "http://localhost:$METRICS_PORT/metrics" | awk -v s="$1" '$1 == s { print $2 }'
}

agent_restarts() {
  kubectl -n thumbops get pods -l app=thumbops-agent -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'
}
```

3. Right after the "certificate renewal" section (before "restart: the identity in the Secret is kept"), add:

```bash
log "health and metrics"
kubectl -n thumbops port-forward deploy/thumbops-agent "$METRICS_PORT:9090" >/dev/null &
agent_pf_pid=$!
wait_for "port-forward to the agent" 30 curl -fsS "http://localhost:$METRICS_PORT/readyz"
curl -fsS "http://localhost:$METRICS_PORT/healthz" >/dev/null
for series in \
  'thumbops_agent_actions_total{outcome="succeeded",type="scale"}' \
  'thumbops_agent_actions_total{outcome="succeeded",type="rollout-restart"}' \
  'thumbops_agent_actions_total{outcome="succeeded",type="cordon"}' \
  'thumbops_agent_actions_total{outcome="succeeded",type="drain"}' \
  'thumbops_agent_actions_total{outcome="succeeded",type="uncordon"}' \
  'thumbops_agent_actions_total{outcome="rejected",type="scale"}' \
  'thumbops_agent_actions_total{outcome="rejected",type="cordon"}' \
  'thumbops_agent_status_ready'; do
  [[ $(metric "$series") == 1 ]] || { echo "$series = $(metric "$series"), expected 1" >&2; exit 1; }
done
(( $(metric 'thumbops_agent_certificate_renewals_total{result="success"}') >= 1 ))
last_hb=$(metric thumbops_agent_heartbeat_last_success_timestamp_seconds)
(( $(date +%s) - ${last_hb%.*} < 60 ))
[[ $(agent_restarts) == 0 ]] # the liveness probe never fired, drain included
kill "$agent_pf_pid"
agent_pf_pid=""
```

Note: Prometheus prints large gauges in exponent form (`1.7e+09`). If `last_hb` comes out like that, convert it with `printf '%.0f' "$last_hb"` before the arithmetic: `last_hb=$(printf '%.0f' "$(metric thumbops_agent_heartbeat_last_success_timestamp_seconds)")` and compare `$last_hb` directly.

4. Before `log "end-to-end tests passed"` add:

```bash
[[ $(agent_restarts) == 0 ]]
```

5. Update the header comment: add "exposes health probes and metrics" to the list of what the test covers.

- [ ] **Step 3: Run the end-to-end tests on kind**

```bash
kind create cluster --name thumbops-e2e --config test/e2e/kind.yaml
test/e2e/run.sh
kind delete cluster --name thumbops-e2e
```

Expected: `--- end-to-end tests passed`. If the current context is not `kind-thumbops-e2e` the script refuses to run: never point it at another cluster.

- [ ] **Step 4: Docs**

`README.md`, new section after "Prototype choices" (before "Layout"):

````markdown
## Health and metrics

`--http-addr` (default `:9090`, empty = disabled) serves, without
authentication:

- `/healthz`: `503` when the heartbeat loop has not completed an attempt for
  `max(5 × --heartbeat-interval, 5 min)`. A failed heartbeat still counts:
  the probe never restarts the agent because the backend or the network is
  down.
- `/readyz`: `200` once policy and identity are loaded and the main loop has
  started; it never goes back to `503`.
- `/metrics`: Prometheus metrics, prefix `thumbops_agent_`, with no resource
  names in the labels, plus the standard `go_*` and `process_*` metrics.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `info` | `version` | Always 1 |
| `backend_requests_total` | `operation`, `code` | Backend calls; `code` is the HTTP status or `error` |
| `heartbeat_last_success_timestamp_seconds` | | Alert here for "backend unreachable" |
| `heartbeat_only` | | 1 after a `426`: upgrade the agent |
| `actions_total` | `type`, `outcome` | `succeeded`, `failed`, `rejected`, `expired`, `discarded` |
| `action_duration_seconds` | `type` | Execution time (histogram) |
| `action_in_progress` | | 1 while an action runs |
| `certificate_expiry_timestamp_seconds` | | Expiry of the certificate in use |
| `certificate_renewals_total` | `result` | `success` or `error` |
| `status_ready` | | 0 until the status informers sync (e.g. missing RBAC) or with `--status=false` |
| `status_last_sent_timestamp_seconds` | | Last cluster status accepted by the backend |
````

Also add to the "Layout" block:

```
internal/metrics     Prometheus metrics (private registry)
internal/health      liveness and readiness probes
```

and remove "Prometheus metrics for the agent and liveness probes." from "Missing for production".

`CLAUDE.md`:

- Layout table: add `| internal/metrics | Prometheus metrics on a private registry; nil-safe |` and `| internal/health | Liveness and readiness state, HTTP handler for /healthz, /readyz, /metrics |` (with backticks around the package names like the other rows).
- Invariants: add

```markdown
- **Liveness never depends on the backend.** `/healthz` fails only when the
  heartbeat loop stops making attempts; a failed heartbeat still counts.
  Readiness never goes back to `503`. Otherwise a backend outage would make
  Kubernetes restart the agent in a loop, interrupting actions.
```

- Next steps: item 1 becomes `Helm chart (replaces deploy/agent.yaml, and the e2e kustomization with it; the identity Secret must survive upgrades and uninstalls, e.g. helm.sh/resource-policy: keep; Service and optional ServiceMonitor for /metrics).` (keep the existing backticks style), and add "health probes and Prometheus metrics" to the "Done" paragraph.

- [ ] **Step 5: Final checks and commit**

Run: `go test -race ./... && go vet ./... && gofmt -l .`
Expected: all pass, gofmt prints nothing.

```bash
git add deploy/agent.yaml test/e2e/run.sh README.md CLAUDE.md
git commit -m "Add probes to the manifest and check health and metrics end to end

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
