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

func newEnv(t *testing.T, pol *policy.Policy) *env {
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
	a := New(Config{
		Version:           "0.1.0-test",
		HeartbeatInterval: 50 * time.Millisecond,
		InitialBackoff:    5 * time.Millisecond,
		MaxBackoff:        20 * time.Millisecond,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, backend.New(backend.Options{BaseURL: srv.URL}), k, exec, pol)
	return &env{mb: mb, fk: fk, agent: a}
}

// run avvia l'agente e restituisce una funzione che lo ferma e ne riporta l'errore.
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
			t.Fatal("l'agente non si è fermato")
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
		t.Fatal("nessun esito ricevuto dal backend")
	}
	if res.Status != protocol.StatusSucceeded {
		t.Fatalf("esito %s: %s", res.Status, res.Message)
	}
	if got := *e.fk.Deployment("payments", "payments-api").Spec.Replicas; got != 6 {
		t.Fatalf("repliche = %d, attese 6", got)
	}
	if e.mb.State("act-1") != "done" {
		t.Fatalf("stato nel backend: %s", e.mb.State("act-1"))
	}
}

func TestHeartbeatContent(t *testing.T) {
	e := newEnv(t, nil)
	e.fk.Deny("create", "", "pods", "eviction") // niente drain
	stop := e.run(t)
	e.mb.Enqueue(protocol.Action{ActionID: "act-1", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}})
	if _, ok := e.mb.WaitResult("act-1", 5*time.Second); !ok {
		t.Fatal("nessun esito")
	}
	time.Sleep(150 * time.Millisecond) // almeno un heartbeat dopo l'azione
	if err := stop(); err != nil {
		t.Fatal(err)
	}

	hbs := e.mb.Heartbeats()
	if len(hbs) < 2 {
		t.Fatalf("attesi più heartbeat, ricevuti %d", len(hbs))
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
			t.Fatalf("%s: nessun esito", id)
		}
		if res.Status != protocol.StatusRejected {
			t.Fatalf("%s: esito %s, atteso rejected", id, res.Status)
		}
	}
	if len(e.fk.Patches()) != 0 {
		t.Fatalf("nessuna modifica attesa sul cluster, trovate: %v", e.fk.Patches())
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
		t.Fatalf("atteso rifiuto per nodo del control plane: %+v", res)
	}
	if e.fk.Node("master-1").Spec.Unschedulable {
		t.Fatal("il nodo del control plane non doveva essere toccato")
	}
}

func TestExpiredActionIsNotClaimed(t *testing.T) {
	e := newEnv(t, nil)
	// L'agente riceve un'azione già scaduta (es. consegnata in ritardo).
	e.agent.handle(context.Background(), protocol.Action{
		ActionID: "old", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"},
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if len(e.fk.Patches()) != 0 {
		t.Fatal("un'azione scaduta non va eseguita")
	}
}

func TestClockSkewIsCorrected(t *testing.T) {
	e := newEnv(t, nil)
	e.agent.cfg.Now = func() time.Time { return time.Now().Add(10 * time.Minute) } // orologio locale avanti
	if err := e.agent.heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.mb.Enqueue(protocol.Action{ActionID: "a1", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"},
		ExpiresAt: time.Now().Add(2 * time.Minute)})
	e.agent.handle(context.Background(), protocol.Action{ActionID: "a1", Type: protocol.ActionCordon,
		Params: protocol.Params{Node: "worker-1"}, ExpiresAt: time.Now().Add(2 * time.Minute)})
	if _, ok := e.mb.Result("a1"); !ok {
		t.Fatal("con l'orologio corretto dall'heartbeat l'azione non è scaduta e va eseguita")
	}
}

func TestClaimConflictIsNotExecuted(t *testing.T) {
	e := newEnv(t, nil)
	e.mb.ClaimOverride["taken"] = http.StatusConflict
	e.mb.Enqueue(protocol.Action{ActionID: "taken", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}})
	e.agent.handle(context.Background(), protocol.Action{ActionID: "taken", Type: protocol.ActionCordon,
		Params: protocol.Params{Node: "worker-1"}, ExpiresAt: time.Now().Add(time.Minute)})
	if len(e.fk.Patches()) != 0 {
		t.Fatal("un'azione non presa in carico non va eseguita")
	}
}

func TestResultIsRetried(t *testing.T) {
	e := newEnv(t, nil)
	e.mb.ResultFailures = 3 // il backend risponde 503 tre volte
	stop := e.run(t)
	defer stop()

	e.mb.Enqueue(protocol.Action{ActionID: "r1", Type: protocol.ActionRolloutRestart,
		Params: protocol.Params{Namespace: "payments", Deployment: "payments-api"}})
	res, ok := e.mb.WaitResult("r1", 5*time.Second)
	if !ok || res.Status != protocol.StatusSucceeded {
		t.Fatalf("l'esito doveva arrivare dopo i tentativi: %+v %v", res, ok)
	}
	if n := len(e.fk.Patches()); n != 1 {
		t.Fatalf("l'azione va eseguita una sola volta, patch: %d", n)
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
			t.Fatalf("atteso ErrUnauthorized, ottenuto %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("con 401 l'agente deve fermarsi")
	}
}

// Un certificato scaduto viene rifiutato all'handshake TLS, senza nessun 401:
// l'agente deve fermarsi come per un 401, non ritentare all'infinito.
func TestExpiredCertificateStopsAgent(t *testing.T) {
	mb := mockbackend.New()
	mb.RequireMTLS = true
	mb.CertLifetime = -30 * time.Second // già scaduto all'emissione
	srv := httptest.NewUnstartedServer(mb.Handler())
	srv.TLS = mb.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	holder := &identity.Holder{}
	b := backend.New(backend.Options{BaseURL: srv.URL, TLS: holder.ClientTLS(roots)})
	if err := enroll.Register(context.Background(), b, identity.Store{Dir: t.TempDir()}, holder, "bootstrap-test-token", protocol.RegisterRequest{}); err != nil {
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
			t.Fatalf("atteso ErrUnauthorized, ottenuto %v", err)
		}
		if !strings.Contains(err.Error(), "expired certificate") {
			t.Fatalf("l'errore deve indicare la causa: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("con il certificato scaduto l'agente deve fermarsi")
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
		t.Fatal("con 426 l'agente deve passare alla sola modalità heartbeat")
	}
	if after <= before {
		t.Fatal("in modalità heartbeat gli heartbeat devono continuare")
	}
}

func TestBackendOutageBackoff(t *testing.T) {
	e := newEnv(t, nil)
	e.mb.SetPollStatus(http.StatusServiceUnavailable)
	stop := e.run(t)
	time.Sleep(200 * time.Millisecond)
	e.mb.SetPollStatus(0) // il backend torna disponibile
	e.mb.Enqueue(protocol.Action{ActionID: "after", Type: protocol.ActionCordon, Params: protocol.Params{Node: "worker-1"}})
	res, ok := e.mb.WaitResult("after", 5*time.Second)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !ok || res.Status != protocol.StatusSucceeded {
		t.Fatalf("dopo il ripristino l'agente deve riprendere: %+v %v", res, ok)
	}
}
