package backend_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/enroll"
	"github.com/thumbops/agent/internal/identity"
	"github.com/thumbops/agent/internal/metrics"
	"github.com/thumbops/agent/internal/mockbackend"
	"github.com/thumbops/agent/internal/protocol"
)

// startTLS starts the mock backend over HTTPS with mTLS required on /v1/agent/*.
func startTLS(t *testing.T) (*mockbackend.Server, *httptest.Server, *x509.CertPool) {
	t.Helper()
	mb := mockbackend.New()
	mb.RequireMTLS = true
	srv := httptest.NewUnstartedServer(mb.Handler())
	srv.TLS = mb.TLSConfig()
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return mb, srv, roots
}

func newClient(srv *httptest.Server, roots *x509.CertPool) (*backend.Client, *identity.Holder) {
	holder := &identity.Holder{}
	return backend.New(backend.Options{BaseURL: srv.URL, TLS: holder.ClientTLS(roots), UserAgent: "thumbops-agent/test"}), holder
}

var info = protocol.RegisterRequest{AgentVersion: "0.1.0", KubernetesVersion: "v1.34.3", ClusterUID: "uid"}

// register completes the first registration of a new agent with its state in dir.
func register(t *testing.T, c *backend.Client, holder *identity.Holder, dir, token string) (*enroll.Enroller, error) {
	t.Helper()
	e := &enroll.Enroller{Backend: c, Store: identity.FileStore{Dir: dir}, Holder: holder}
	_, err := e.Start(context.Background(), token, func(context.Context) (protocol.RegisterRequest, error) { return info, nil })
	return e, err
}

func TestRegistrationAndMTLS(t *testing.T) {
	_, srv, roots := startTLS(t)
	dir := t.TempDir()
	c, holder := newClient(srv, roots)

	// Before registration the backend rejects the agent's calls.
	// This call also opens a TLS connection without a certificate, which
	// the client must not reuse after registration.
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); backend.Code(err) != http.StatusUnauthorized {
		t.Fatalf("without a certificate expected 401, got %v", err)
	}

	e, err := register(t, c, holder, dir, "bootstrap-test-token\n")
	if err != nil {
		t.Fatal(err)
	}
	if e.ClusterID() == "" {
		t.Fatal("cluster_id not saved")
	}
	if cn := holder.Get().Leaf.Subject.CommonName; cn != e.ClusterID() {
		t.Fatalf("certificate CN %q differs from cluster_id %q", cn, e.ClusterID())
	}
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{AgentVersion: "0.1.0"}); err != nil {
		t.Fatalf("heartbeat with mTLS after registration: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "key.pem"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private key permissions: %v %v", info.Mode(), err)
	}
}

func TestBootstrapTokenIsSingleUse(t *testing.T) {
	_, srv, roots := startTLS(t)
	c1, h1 := newClient(srv, roots)
	if _, err := register(t, c1, h1, t.TempDir(), "bootstrap-test-token"); err != nil {
		t.Fatal(err)
	}
	c2, h2 := newClient(srv, roots)
	_, err := register(t, c2, h2, t.TempDir(), "bootstrap-test-token")
	if backend.Code(err) != http.StatusUnauthorized {
		t.Fatalf("second use of the token: expected 401, got %v", err)
	}
}

// A certificate that is expired or signed by a CA the backend does not know
// is rejected during the TLS handshake, before any HTTP response: for the
// agent it must count as a 401.
func TestRejectedCertificateIsUnauthorized(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		mb, srv, roots := startTLS(t)
		mb.CertLifetime = -30 * time.Second // already expired when issued
		c, holder := newClient(srv, roots)
		if _, err := register(t, c, holder, t.TempDir(), "bootstrap-test-token"); err != nil {
			t.Fatal(err)
		}
		_, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{})
		if !backend.Unauthorized(err) {
			t.Fatalf("expired certificate: expected a rejection equivalent to 401, got %v", err)
		}
	})
	t.Run("unknown CA", func(t *testing.T) {
		_, srv1, roots1 := startTLS(t)
		c1, holder := newClient(srv1, roots1)
		if _, err := register(t, c1, holder, t.TempDir(), "bootstrap-test-token"); err != nil {
			t.Fatal(err)
		}
		// Same client certificate presented to a backend with another CA.
		_, srv2, roots2 := startTLS(t)
		c2 := backend.New(backend.Options{BaseURL: srv2.URL, TLS: holder.ClientTLS(roots2)})
		_, err := c2.Heartbeat(context.Background(), protocol.HeartbeatRequest{})
		if !backend.Unauthorized(err) {
			t.Fatalf("unknown CA: expected a rejection equivalent to 401, got %v", err)
		}
	})
	t.Run("network and server errors do not count", func(t *testing.T) {
		_, srv, roots := startTLS(t)
		c, _ := newClient(srv, roots)
		srv.Close()
		_, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{})
		if err == nil || backend.Unauthorized(err) {
			t.Fatalf("backend unreachable: expected a temporary error, got %v", err)
		}
		if backend.Unauthorized(&backend.StatusError{Code: http.StatusServiceUnavailable}) {
			t.Fatal("503 is not a certificate rejection")
		}
	})
}

func TestCertificateRenewal(t *testing.T) {
	mb, srv, roots := startTLS(t)
	mb.CertLifetime = 3 * time.Hour
	dir := t.TempDir()
	c, holder := newClient(srv, roots)
	e, err := register(t, c, holder, dir, "bootstrap-test-token")
	if err != nil {
		t.Fatal(err)
	}
	first := holder.Get().Leaf

	renewed, err := e.Renew(context.Background(), time.Now())
	if err != nil || renewed {
		t.Fatalf("a freshly issued certificate must not be renewed: %v %v", renewed, err)
	}

	later := first.NotAfter.Add(-time.Hour) // less than a third of the validity
	renewed, err = e.Renew(context.Background(), later)
	if err != nil || !renewed {
		t.Fatalf("renewal expected: %v %v", renewed, err)
	}
	if holder.Get().Leaf.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatal("the certificate in use did not change")
	}
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); err != nil {
		t.Fatalf("heartbeat with the renewed certificate: %v", err)
	}
	if mb.Renewals() != 1 {
		t.Fatalf("renewals recorded by the backend: %d", mb.Renewals())
	}
	// The renewed certificate is also the one saved, with the same key.
	saved, err := identity.FileStore{Dir: dir}.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := saved.Certificate()
	if err != nil || cert.Leaf.SerialNumber.Cmp(holder.Get().Leaf.SerialNumber) != 0 {
		t.Fatalf("saved certificate not updated: %v", err)
	}
	if !cert.PrivateKey.(ed25519.PrivateKey).Equal(holder.Get().PrivateKey) {
		t.Fatal("the renewal changed the key")
	}
}

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
