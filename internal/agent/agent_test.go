package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/enroll"
	"github.com/thumbops/agent/internal/identity"
	"github.com/thumbops/agent/internal/kubefake"
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
	e := newEnv(t, nil, func(c *Config) {
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
	src.setReady()
	waitStatuses(t, e, 1)

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
