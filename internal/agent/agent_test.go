package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/enroll"
	"github.com/thumbops/agent/internal/health"
	"github.com/thumbops/agent/internal/identity"
	"github.com/thumbops/agent/internal/kube"
	"github.com/thumbops/agent/internal/kubefake"
	"github.com/thumbops/agent/internal/metrics"
	"github.com/thumbops/agent/internal/mockbackend"
	"github.com/thumbops/agent/internal/policy"
	"github.com/thumbops/agent/internal/protocol"
)

type env struct {
	mb    *mockbackend.Server
	fk    *kubefake.Server
	agent *Agent
}

func newEnv(t *testing.T, pol *policy.Policy, opts ...func(*Config)) *env {
	t.Helper()
	mb := mockbackend.New()
	mb.Poll = protocol.PollConfig{WaitSeconds: 1}
	srv := httptest.NewServer(mb.Handler())
	t.Cleanup(srv.Close)
	fk := kubefake.New()
	t.Cleanup(fk.Close)
	fk.AddNode("worker-1", true, nil)
	fk.AddNode("worker-2", false, nil)
	fk.AddDeployment("payments", "payments-api", 3)

	k := fk.Client()
	exec := actions.New(k)
	exec.PollInterval = 10 * time.Millisecond
	if pol == nil {
		pol = &policy.Policy{
			AllowedActions:   protocol.ActionTypes,
			DeniedNamespaces: []string{"kube-system"},
			MaxReplicas:      10,
		}
	}
	cfg := Config{
		Version:           "0.1.0-test",
		HeartbeatInterval: 50 * time.Millisecond,
		InitialBackoff:    5 * time.Millisecond,
		MaxBackoff:        20 * time.Millisecond,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(&cfg)
	}
	a := New(cfg, backend.New(backend.Options{BaseURL: srv.URL}), k, exec, pol)
	return &env{mb: mb, fk: fk, agent: a}
}

// run starts the agent and returns a function that stops it and reports its error.
func (e *env) run(t *testing.T) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.agent.Run(ctx) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("the agent did not stop")
			return nil
		}
	}
}

func intp(v int) *int { return &v }

func TestEndToEndScale(t *testing.T) {
	e := newEnv(t, nil)
	stop := e.run(t)
	defer stop()

	e.mb.Enqueue(protocol.Action{
		ActionID: "act-1", Type: protocol.ActionScale, RequestedBy: "u_123",
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api", Replicas: intp(6)},
	})
	res, ok := e.mb.WaitResult("act-1", 5*time.Second)
	if !ok {
		t.Fatal("no result received by the backend")
	}
	if res.Status != protocol.StatusSucceeded {
		t.Fatalf("status %s: %s", res.Status, res.Message)
	}
	if got := *e.fk.Deployment("payments", "payments-api").Spec.Replicas; got != 6 {
		t.Fatalf("repliche = %d, attese 6", got)
	}
	if e.mb.State("act-1") != "done" {
		t.Fatalf("state in the backend: %s", e.mb.State("act-1"))
	}
}

func TestHeartbeatContent(t *testing.T) {
	e := newEnv(t, nil)
	e.fk.Deny("create", "", "pods", "eviction") // no drain
	stop := e.run(t)
	e.mb.Enqueue(protocol.Action{ActionID: "act-1", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}})
	if _, ok := e.mb.WaitResult("act-1", 5*time.Second); !ok {
		t.Fatal("no result")
	}
	time.Sleep(150 * time.Millisecond) // at least one heartbeat after the action
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	hbs := e.mb.Heartbeats()
	if len(hbs) < 2 {
		t.Fatalf("expected more heartbeats, received %d", len(hbs))
	}
	last := hbs[len(hbs)-1]
	if last.AgentVersion != "0.1.0-test" || last.KubernetesVersion != "v1.34.3" {
		t.Fatalf("versioni: %+v", last)
	}
	if last.Nodes.Total != 2 || last.Nodes.Ready != 1 {
		t.Fatalf("nodi: %+v", last.Nodes)
	}
	if last.Permissions[protocol.ActionDrain] || !last.Permissions[protocol.ActionCordon] || !last.Permissions[protocol.ActionScale] {
		t.Fatalf("permessi: %+v", last.Permissions)
	}
	if last.LastActionID != "act-1" {
		t.Fatalf("last_action_id = %q", last.LastActionID)
	}
}

func TestPolicyRejection(t *testing.T) {
	e := newEnv(t, nil)
	stop := e.run(t)
	defer stop()

	e.mb.Enqueue(protocol.Action{ActionID: "big", Type: protocol.ActionScale,
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api", Replicas: intp(50)}})
	e.mb.Enqueue(protocol.Action{ActionID: "sys", Type: protocol.ActionRolloutRestart,
		Params: protocol.Params{Namespace: "kube-system", Deployment: "coredns"}})

	for _, id := range []string{"big", "sys"} {
		res, ok := e.mb.WaitResult(id, 5*time.Second)
		if !ok {
			t.Fatalf("%s: no result", id)
		}
		if res.Status != protocol.StatusRejected {
			t.Fatalf("%s: status %s, expected rejected", id, res.Status)
		}
	}
	if len(e.fk.Patches()) != 0 {
		t.Fatalf("no change expected on the cluster, found: %v", e.fk.Patches())
	}
}

func TestControlPlaneNodeProtected(t *testing.T) {
	e := newEnv(t, nil)
	e.fk.AddNode("master-1", true, map[string]string{"node-role.kubernetes.io/control-plane": ""})
	stop := e.run(t)
	defer stop()

	e.mb.Enqueue(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "master-1"}})
	res, ok := e.mb.WaitResult("d1", 5*time.Second)
	if !ok || res.Status != protocol.StatusRejected || !strings.Contains(res.Message, "control plane") {
		t.Fatalf("expected rejection for a control plane node: %+v", res)
	}
	if e.fk.Node("master-1").Spec.Unschedulable {
		t.Fatal("the control plane node should not have been touched")
	}
}

func TestExpiredActionIsNotClaimed(t *testing.T) {
	e := newEnv(t, nil)
	// The agent receives an action that has already expired (e.g. delivered late).
	e.agent.handle(context.Background(), protocol.Action{
		ActionID: "old", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"},
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if len(e.fk.Patches()) != 0 {
		t.Fatal("an expired action must not run")
	}
}

func TestClockSkewIsCorrected(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.cfg.Now = func() time.Time { return time.Now().Add(10 * time.Minute) } // local clock ahead
	if err := e.agent.heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.mb.Enqueue(protocol.Action{ActionID: "a1", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"},
		ExpiresAt: time.Now().Add(2 * time.Minute)})
	e.agent.handle(context.Background(), protocol.Action{ActionID: "a1", Type: protocol.ActionCordon,
		Params: protocol.Params{Node: "worker-1"}, ExpiresAt: time.Now().Add(2 * time.Minute)})
	if _, ok := e.mb.Result("a1"); !ok {
		t.Fatal("with the clock corrected by the heartbeat the action has not expired and must run")
	}
}

func TestClaimConflictIsNotExecuted(t *testing.T) {
	e := newEnv(t, nil)
	e.mb.ClaimOverride["taken"] = http.StatusConflict
	e.mb.Enqueue(protocol.Action{ActionID: "taken", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}})
	e.agent.handle(context.Background(), protocol.Action{ActionID: "taken", Type: protocol.ActionCordon,
		Params: protocol.Params{Node: "worker-1"}, ExpiresAt: time.Now().Add(time.Minute)})
	if len(e.fk.Patches()) != 0 {
		t.Fatal("an unclaimed action must not run")
	}
}

func TestResultIsRetried(t *testing.T) {
	e := newEnv(t, nil)
	e.mb.ResultFailures = 3 // the backend responds 503 three times
	stop := e.run(t)
	defer stop()

	e.mb.Enqueue(protocol.Action{ActionID: "r1", Type: protocol.ActionRolloutRestart,
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api"}})
	res, ok := e.mb.WaitResult("r1", 5*time.Second)
	if !ok || res.Status != protocol.StatusSucceeded {
		t.Fatalf("the result should have arrived after the retries: %+v %v", res, ok)
	}
	if n := len(e.fk.Patches()); n != 1 {
		t.Fatalf("the action must run only once, patches: %d", n)
	}
}

func TestUnauthorizedStopsAgent(t *testing.T) {
	e := newEnv(t, nil)
	e.mb.SetPollStatus(http.StatusUnauthorized)
	done := make(chan error, 1)
	go func() { done <- e.agent.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("on 401 the agent must stop")
	}
}

// An expired certificate is rejected at the TLS handshake, with no 401:
// the agent must stop as it does for a 401, not retry forever.
func TestExpiredCertificateStopsAgent(t *testing.T) {
	mb := mockbackend.New()
	mb.RequireMTLS = true
	mb.CertLifetime = -30 * time.Second // already expired when issued
	srv := httptest.NewUnstartedServer(mb.Handler())
	srv.TLS = mb.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	holder := &identity.Holder{}
	b := backend.New(backend.Options{BaseURL: srv.URL, TLS: holder.ClientTLS(roots)})
	en := &enroll.Enroller{Backend: b, Store: identity.FileStore{Dir: t.TempDir()}, Holder: holder}
	if _, err := en.Start(context.Background(), "bootstrap-test-token", func(context.Context) (protocol.RegisterRequest, error) {
		return protocol.RegisterRequest{}, nil
	}); err != nil {
		t.Fatal(err)
	}

	fk := kubefake.New()
	t.Cleanup(fk.Close)
	k := fk.Client()
	a := New(Config{
		Version:           "0.1.0-test",
		HeartbeatInterval: 50 * time.Millisecond,
		InitialBackoff:    5 * time.Millisecond,
		MaxBackoff:        20 * time.Millisecond,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, b, k, actions.New(k), &policy.Policy{})

	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized, got %v", err)
		}
		if !strings.Contains(err.Error(), "expired certificate") {
			t.Fatalf("the error must name the cause: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("with an expired certificate the agent must stop")
	}
}

func TestUpgradeRequiredKeepsHeartbeat(t *testing.T) {
	e := newEnv(t, nil)
	e.mb.SetPollStatus(http.StatusUpgradeRequired)
	stop := e.run(t)
	time.Sleep(300 * time.Millisecond)
	before := len(e.mb.Heartbeats())
	time.Sleep(200 * time.Millisecond)
	after := len(e.mb.Heartbeats())
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !e.agent.isHeartbeatOnly() {
		t.Fatal("on 426 the agent must switch to heartbeat-only mode")
	}
	if after <= before {
		t.Fatal("in heartbeat-only mode heartbeats must continue")
	}
}

func TestBackendOutageBackoff(t *testing.T) {
	e := newEnv(t, nil)
	e.mb.SetPollStatus(http.StatusServiceUnavailable)
	stop := e.run(t)
	time.Sleep(200 * time.Millisecond)
	e.mb.SetPollStatus(0) // the backend is available again
	e.mb.Enqueue(protocol.Action{ActionID: "after", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}})
	res, ok := e.mb.WaitResult("after", 5*time.Second)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !ok || res.Status != protocol.StatusSucceeded {
		t.Fatalf("after recovery the agent must resume: %+v %v", res, ok)
	}
}

// fakeStatus stands in for the informer-based collector.
type fakeStatus struct {
	mu    sync.Mutex
	ready bool
	calls int
}

func (f *fakeStatus) Collect(at time.Time) (protocol.ClusterStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return protocol.ClusterStatus{CollectedAt: at, Nodes: protocol.NodesSummary{Total: f.calls}}, f.ready
}

func (f *fakeStatus) setReady() {
	f.mu.Lock()
	f.ready = true
	f.mu.Unlock()
}

func waitStatuses(t *testing.T, e *env, n int) []protocol.ClusterStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := e.mb.Statuses(); len(s) >= n {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d status summaries, got %d", n, len(e.mb.Statuses()))
	return nil
}

func TestStatusSentWhenReadyAndOnRequest(t *testing.T) {
	src := &fakeStatus{}
	m := metrics.New("test")
	e := newEnv(t, nil, func(c *Config) {
		c.Metrics = m
		c.Status = src
		c.StatusRetry = 10 * time.Millisecond
		c.StatusInterval = time.Hour // only the first summary and the requested ones
	})
	stop := e.run(t)
	defer stop()

	time.Sleep(100 * time.Millisecond)
	if n := len(e.mb.Statuses()); n != 0 {
		t.Fatalf("nothing must be sent before the caches sync, got %d", n)
	}
	if v := value(t, scrape(t, m), "thumbops_agent_status_ready"); v != 0 {
		t.Fatalf("status_ready before the caches sync: %v", v)
	}
	src.setReady()
	waitStatuses(t, e, 1)
	body := scrape(t, m)
	if v := value(t, body, "thumbops_agent_status_ready"); v != 1 {
		t.Fatalf("status_ready after sync: %v", v)
	}
	if v := value(t, body, "thumbops_agent_status_last_sent_timestamp_seconds"); v == 0 {
		t.Fatal("status_last_sent not set")
	}

	e.mb.RequestStatus() // the user pressed refresh in the app
	got := waitStatuses(t, e, 2)
	if got[1].CollectedAt.Before(got[0].CollectedAt) {
		t.Fatal("the requested summary must be a new one")
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(e.mb.Statuses()); n != 2 {
		t.Fatalf("one request, one summary: got %d", n)
	}
}

func TestStatusUnauthorizedStopsAgent(t *testing.T) {
	src := &fakeStatus{ready: true}
	e := newEnv(t, nil, func(c *Config) {
		c.Status = src
		c.StatusRetry = 10 * time.Millisecond
		c.HeartbeatInterval = time.Hour
	})
	done := make(chan error, 1)
	go func() { done <- e.agent.Run(context.Background()) }()
	waitStatuses(t, e, 1)
	e.mb.SetStatusCode(http.StatusUnauthorized)
	e.mb.RequestStatus()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("expected ErrUnauthorized, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a 401 on the status must stop the agent")
	}
}

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
		`thumbops_agent_actions_total{outcome="succeeded",type="cordon"}`:         1,
		`thumbops_agent_actions_total{outcome="rejected",type="rollout-restart"}`: 1,
		`thumbops_agent_actions_total{outcome="expired",type="scale"}`:            1,
		`thumbops_agent_actions_total{outcome="discarded",type="uncordon"}`:       1,
		`thumbops_agent_action_duration_seconds_count{type="cordon"}`:             1,
		`thumbops_agent_action_in_progress`:                                       0,
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
	deadline := time.Now().Add(5 * time.Second)
	// Read the samples, not substrings: the HELP line of heartbeat_only
	// starts with "1 after a 426" and would match "heartbeat_only 1".
	for body := scrape(t, m); value(t, body, "thumbops_agent_heartbeat_only") != 1 ||
		value(t, body, "thumbops_agent_heartbeat_last_success_timestamp_seconds") == 0; body = scrape(t, m) {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("heartbeat_only never became 1")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	hs.MarkReady()                 // liveness is only judged once the agent is ready
	now = now.Add(2 * time.Minute) // past the threshold
	if err := e.agent.heartbeat(context.Background()); err == nil {
		t.Fatal("expected the heartbeat to fail")
	}
	if err := hs.Live(); err != nil {
		t.Fatalf("a failed heartbeat attempt must keep the agent live: %v", err)
	}
}

func podOn(ns, name, node string) kube.Pod {
	yes := true
	return kube.Pod{
		Metadata: kube.ObjectMeta{Name: name, Namespace: ns, UID: name,
			OwnerReferences: []kube.OwnerReference{{Kind: "ReplicaSet", Name: "rs", Controller: &yes}}},
		Spec:   kube.PodSpec{NodeName: node},
		Status: kube.PodStatus{Phase: "Running"},
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDrainProgressReachesTheBackend(t *testing.T) {
	e := newEnv(t, nil, func(c *Config) { c.ProgressMinGap = time.Millisecond })
	e.fk.AddPod(podOn("payments", "api-1", "worker-1"))
	e.fk.AddPod(podOn("payments", "api-2", "worker-1"))
	e.fk.BlockEviction("payments", "api-2", 3)
	e.mb.Enqueue(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}})
	e.agent.handle(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
		Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 5}, ExpiresAt: time.Now().Add(time.Minute)})
	res, ok := e.mb.Result("d1")
	if !ok || res.Status != protocol.StatusSucceeded {
		t.Fatalf("result: %+v %v", res, ok)
	}
	if _, n := e.mb.Progress("d1"); n < 2 {
		t.Fatalf("expected progress reports, got %d", n)
	}
}

func TestDrainStoppedWhenTheBackendCancels(t *testing.T) {
	e := newEnv(t, nil, func(c *Config) {
		c.ProgressMinGap = time.Millisecond
		c.ProgressMaxGap = 100 * time.Millisecond
	})
	e.fk.AddPod(podOn("payments", "db-0", "worker-1"))
	e.fk.BlockEviction("payments", "db-0", -1)
	e.mb.Enqueue(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 30}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.agent.handle(context.Background(), protocol.Action{ActionID: "d1", Type: protocol.ActionDrain,
			Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 30}, ExpiresAt: time.Now().Add(time.Minute)})
	}()
	waitUntil(t, func() bool { _, n := e.mb.Progress("d1"); return n >= 1 })
	e.mb.Cancel("d1")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain did not stop after the backend cancelled it")
	}
	if _, ok := e.mb.Result("d1"); ok {
		t.Fatal("no result must be sent for a cancelled action")
	}
	if _, ok := e.fk.Node("worker-1").Metadata.Annotations[actions.AnnotationDrainInProgress]; ok {
		t.Fatal("the in-progress annotation must be removed")
	}
}

func TestInterruptedDrainIsResumedAtStartup(t *testing.T) {
	e := newEnv(t, nil)
	e.fk.AddPod(podOn("payments", "api-1", "worker-1"))
	state, _ := json.Marshal(map[string]any{"action_id": "d1", "started_at": time.Now().Add(-time.Second).UTC(), "timeout_seconds": 30})
	e.fk.PatchNodeForTest("worker-1", map[string]any{"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]any{actions.AnnotationDrainInProgress: string(state)}}})
	e.mb.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 30}})

	stop := e.run(t)
	defer stop()
	res, ok := e.mb.WaitResult("d1", 5*time.Second)
	if !ok || res.Status != protocol.StatusSucceeded {
		t.Fatalf("the resumed drain must report its result: %+v %v", res, ok)
	}
	if e.fk.PodExists("payments", "api-1") {
		t.Fatal("the resumed drain must evict the pod")
	}
	if n := e.mb.Claims("d1"); n != 0 {
		t.Fatalf("the resume must not claim the action, got %d claim calls", n)
	}
}

func TestInterruptedDrainOfAnActionTheBackendNoLongerTracks(t *testing.T) {
	e := newEnv(t, nil, func(c *Config) { c.ProgressMinGap = time.Millisecond })
	e.fk.AddPod(podOn("payments", "api-1", "worker-1"))
	state, _ := json.Marshal(map[string]any{"action_id": "d1", "started_at": time.Now().Add(-time.Second).UTC(), "timeout_seconds": 30})
	e.fk.PatchNodeForTest("worker-1", map[string]any{"spec": map[string]any{"unschedulable": true},
		"metadata": map[string]any{"annotations": map[string]any{actions.AnnotationDrainInProgress: string(state)}}})
	e.mb.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain, Params: protocol.Params{Node: "worker-1", TimeoutSeconds: 30}})
	e.mb.Cancel("d1")

	stop := e.run(t)
	defer stop()
	waitUntil(t, func() bool {
		_, ok := e.fk.Node("worker-1").Metadata.Annotations[actions.AnnotationDrainInProgress]
		return !ok
	})
	if n := len(e.fk.Evictions()); n != 0 {
		t.Fatalf("no pod may be evicted for an action the backend no longer tracks, got %d", n)
	}
	if _, ok := e.mb.Result("d1"); ok {
		t.Fatal("no result must be sent for a cancelled action")
	}
	if !e.fk.Node("worker-1").Spec.Unschedulable {
		t.Fatal("the node must stay cordoned")
	}
}
