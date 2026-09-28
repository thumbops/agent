package backend_test

import (
	"context"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/enroll"
	"github.com/thumbops/agent/internal/identity"
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

func TestRegistrationAndMTLS(t *testing.T) {
	_, srv, roots := startTLS(t)
	store := identity.Store{Dir: t.TempDir()}
	c, holder := newClient(srv, roots)

	// Before registration the backend rejects the agent's calls.
	// This call also opens a TLS connection without a certificate, which
	// the client must not reuse after registration.
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); backend.Code(err) != http.StatusUnauthorized {
		t.Fatalf("without a certificate expected 401, got %v", err)
	}

	if err := enroll.Register(context.Background(), c, store, holder, "bootstrap-test-token\n", info); err != nil {
		t.Fatal(err)
	}
	if store.ClusterID() == "" {
		t.Fatal("cluster_id not saved")
	}
	if cn := holder.Get().Leaf.Subject.CommonName; cn != store.ClusterID() {
		t.Fatalf("certificate CN %q differs from cluster_id %q", cn, store.ClusterID())
	}
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{AgentVersion: "0.1.0"}); err != nil {
		t.Fatalf("heartbeat with mTLS after registration: %v", err)
	}

	info, err := os.Stat(filepath.Join(store.Dir, "key.pem"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private key permissions: %v %v", info.Mode(), err)
	}
}

func TestBootstrapTokenIsSingleUse(t *testing.T) {
	_, srv, roots := startTLS(t)
	c1, h1 := newClient(srv, roots)
	if err := enroll.Register(context.Background(), c1, identity.Store{Dir: t.TempDir()}, h1, "bootstrap-test-token", info); err != nil {
		t.Fatal(err)
	}
	c2, h2 := newClient(srv, roots)
	err := enroll.Register(context.Background(), c2, identity.Store{Dir: t.TempDir()}, h2, "bootstrap-test-token", info)
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
		if err := enroll.Register(context.Background(), c, identity.Store{Dir: t.TempDir()}, holder, "bootstrap-test-token", info); err != nil {
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
		if err := enroll.Register(context.Background(), c1, identity.Store{Dir: t.TempDir()}, holder, "bootstrap-test-token", info); err != nil {
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

func TestKeyIsReused(t *testing.T) {
	store := identity.Store{Dir: t.TempDir()}
	k1, err := store.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := store.LoadOrCreateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !k1.Equal(k2) {
		t.Fatal("the existing key was not read back")
	}
}

func TestCertificateRenewal(t *testing.T) {
	mb, srv, roots := startTLS(t)
	mb.CertLifetime = 3 * time.Hour
	store := identity.Store{Dir: t.TempDir()}
	c, holder := newClient(srv, roots)
	if err := enroll.Register(context.Background(), c, store, holder, "bootstrap-test-token", info); err != nil {
		t.Fatal(err)
	}
	first := holder.Get().Leaf

	renewed, err := enroll.Renew(context.Background(), c, store, holder, time.Now())
	if err != nil || renewed {
		t.Fatalf("a freshly issued certificate must not be renewed: %v %v", renewed, err)
	}

	later := first.NotAfter.Add(-time.Hour) // less than a third of the validity
	renewed, err = enroll.Renew(context.Background(), c, store, holder, later)
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
	// The renewed certificate is also the one saved on disk.
	onDisk, err := store.Load()
	if err != nil || onDisk.Leaf.SerialNumber.Cmp(holder.Get().Leaf.SerialNumber) != 0 {
		t.Fatalf("certificate on disk not updated: %v", err)
	}
}
