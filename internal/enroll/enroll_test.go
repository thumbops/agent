package enroll_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/enroll"
	"github.com/thumbops/agent/internal/identity"
	"github.com/thumbops/agent/internal/mockbackend"
	"github.com/thumbops/agent/internal/protocol"
)

var info = protocol.RegisterRequest{AgentVersion: "0.1.0", KubernetesVersion: "v1.34.3", ClusterUID: "uid"}

func infoFn(context.Context) (protocol.RegisterRequest, error) { return info, nil }

type env struct {
	mb    *mockbackend.Server
	srv   *httptest.Server
	roots *x509.CertPool
	store identity.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mb := mockbackend.New()
	mb.RequireMTLS = true
	srv := httptest.NewUnstartedServer(mb.Handler())
	srv.TLS = mb.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return &env{mb: mb, srv: srv, roots: roots, store: &memStore{}}
}

// agent simulates an agent (re)start: new client, same saved state.
func (e *env) agent() (*enroll.Enroller, *backend.Client) {
	holder := &identity.Holder{}
	c := backend.New(backend.Options{BaseURL: e.srv.URL, TLS: holder.ClientTLS(e.roots)})
	return &enroll.Enroller{Backend: c, Store: e.store, Holder: holder, SaveBackoff: time.Millisecond}, c
}

func (e *env) start(t *testing.T, token string) (*enroll.Enroller, bool, error) {
	t.Helper()
	en, c := e.agent()
	registered, err := en.Start(context.Background(), token, infoFn)
	if err == nil {
		if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); err != nil {
			t.Fatalf("heartbeat with the identity in use: %v", err)
		}
	}
	return en, registered, err
}

func (e *env) saved(t *testing.T) *identity.State {
	t.Helper()
	st, err := e.store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRestartKeepsTheIdentity(t *testing.T) {
	e := newEnv(t)
	if _, registered, err := e.start(t, "bootstrap-test-token"); err != nil || !registered {
		t.Fatalf("first start: registered=%v err=%v", registered, err)
	}
	first := e.saved(t)

	// Restart with the same token still mounted, and without it.
	for _, token := range []string{"bootstrap-test-token\n", ""} {
		if _, registered, err := e.start(t, token); err != nil || registered {
			t.Fatalf("restart with token %q: registered=%v err=%v", token, registered, err)
		}
	}
	if !e.saved(t).Key.Equal(first.Key) || len(e.mb.Registrations()) != 1 {
		t.Fatalf("the restart changed the identity: %d registrations", len(e.mb.Registrations()))
	}
}

func TestNewTokenRegistersAgain(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.start(t, "bootstrap-test-token"); err != nil {
		t.Fatal(err)
	}
	first := e.saved(t)

	e.mb.AddBootstrapToken("second-token")
	if _, registered, err := e.start(t, "second-token"); err != nil || !registered {
		t.Fatalf("new token: registered=%v err=%v", registered, err)
	}
	second := e.saved(t)
	if second.Key.Equal(first.Key) {
		t.Fatal("a new registration must use a new key")
	}
	if second.BootstrapTokenHash != identity.HashToken("second-token") {
		t.Fatal("hash of the new token not saved")
	}
	if len(e.mb.Registrations()) != 2 {
		t.Fatalf("registrations: %d", len(e.mb.Registrations()))
	}

	// The next restart with the same new token does not register a third time.
	if _, registered, err := e.start(t, "second-token"); err != nil || registered {
		t.Fatalf("restart after the new registration: registered=%v err=%v", registered, err)
	}
}

func TestFailedRegistrationKeepsTheIdentity(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.start(t, "bootstrap-test-token"); err != nil {
		t.Fatal(err)
	}
	first := e.saved(t)

	_, _, err := e.start(t, "unknown-token")
	if backend.Code(err) != http.StatusUnauthorized {
		t.Fatalf("unknown token: expected 401, got %v", err)
	}
	if st := e.saved(t); !st.Key.Equal(first.Key) || st.CertPEM != first.CertPEM {
		t.Fatal("a failed registration changed the saved identity")
	}
}

func TestNoIdentityAndNoToken(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.start(t, " \n"); !errors.Is(err, enroll.ErrNoToken) {
		t.Fatalf("expected ErrNoToken, got %v", err)
	}
}

func TestSaveIsRetriedAfterRegistration(t *testing.T) {
	e := newEnv(t)
	ms := e.store.(*memStore)
	ms.failures = 3
	if _, registered, err := e.start(t, "bootstrap-test-token"); err != nil || !registered {
		t.Fatalf("registered=%v err=%v", registered, err)
	}
	if ms.saves != 4 || e.saved(t) == nil {
		t.Fatalf("saves: %d", ms.saves)
	}
}

func TestRenewalFailingToSaveKeepsTheOldCertificate(t *testing.T) {
	e := newEnv(t)
	e.mb.CertLifetime = 3 * time.Hour
	en, _, err := e.start(t, "bootstrap-test-token")
	if err != nil {
		t.Fatal(err)
	}
	first := en.Holder.Get().Leaf
	e.store.(*memStore).failures = 1
	if _, err := en.Renew(context.Background(), first.NotAfter.Add(-time.Hour)); err == nil {
		t.Fatal("expected the save error")
	}
	if en.Holder.Get().Leaf.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("certificate in use changed although it was not saved")
	}
	if renewed, err := en.Renew(context.Background(), first.NotAfter.Add(-time.Hour)); err != nil || !renewed {
		t.Fatalf("second attempt: renewed=%v err=%v", renewed, err)
	}
	cert, err := e.saved(t).Certificate()
	if err != nil || cert.Leaf.SerialNumber.Cmp(en.Holder.Get().Leaf.SerialNumber) != 0 {
		t.Fatalf("saved certificate differs from the one in use: %v", err)
	}
	if !cert.PrivateKey.(ed25519.PrivateKey).Equal(en.Holder.Get().PrivateKey) {
		t.Fatal("the renewal changed the key")
	}
}

// memStore is an in-memory Store whose saves can fail.
type memStore struct {
	mu       sync.Mutex
	st       *identity.State
	failures int
	saves    int
}

func (m *memStore) Load(context.Context) (*identity.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st == nil {
		return nil, nil
	}
	cp := *m.st
	return &cp, nil
}

func (m *memStore) Save(_ context.Context, st *identity.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves++
	if m.failures > 0 {
		m.failures--
		return errors.New("API server unavailable")
	}
	cp := *st
	m.st = &cp
	return nil
}
