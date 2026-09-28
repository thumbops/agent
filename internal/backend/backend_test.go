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

// startTLS avvia il backend finto in HTTPS con mTLS obbligatorio su /v1/agent/*.
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

	// Prima della registrazione il backend rifiuta le chiamate dell'agente.
	// Questa chiamata apre anche una connessione TLS senza certificato, che
	// il client non deve più riusare dopo la registrazione.
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); backend.Code(err) != http.StatusUnauthorized {
		t.Fatalf("senza certificato atteso 401, ottenuto %v", err)
	}

	if err := enroll.Register(context.Background(), c, store, holder, "bootstrap-test-token\n", info); err != nil {
		t.Fatal(err)
	}
	if store.ClusterID() == "" {
		t.Fatal("cluster_id non salvato")
	}
	if cn := holder.Get().Leaf.Subject.CommonName; cn != store.ClusterID() {
		t.Fatalf("CN del certificato %q diverso dal cluster_id %q", cn, store.ClusterID())
	}
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{AgentVersion: "0.1.0"}); err != nil {
		t.Fatalf("heartbeat con mTLS dopo la registrazione: %v", err)
	}

	info, err := os.Stat(filepath.Join(store.Dir, "key.pem"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permessi della chiave privata: %v %v", info.Mode(), err)
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
		t.Fatalf("secondo uso del token: atteso 401, ottenuto %v", err)
	}
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
		t.Fatal("la chiave esistente non è stata riletta")
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
		t.Fatalf("un certificato appena emesso non va rinnovato: %v %v", renewed, err)
	}

	later := first.NotAfter.Add(-time.Hour) // meno di un terzo della validità
	renewed, err = enroll.Renew(context.Background(), c, store, holder, later)
	if err != nil || !renewed {
		t.Fatalf("rinnovo atteso: %v %v", renewed, err)
	}
	if holder.Get().Leaf.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatal("il certificato in uso non è cambiato")
	}
	if _, err := c.Heartbeat(context.Background(), protocol.HeartbeatRequest{}); err != nil {
		t.Fatalf("heartbeat con il certificato rinnovato: %v", err)
	}
	if mb.Renewals() != 1 {
		t.Fatalf("rinnovi registrati dal backend: %d", mb.Renewals())
	}
	// Il certificato rinnovato è anche quello salvato su disco.
	onDisk, err := store.Load()
	if err != nil || onDisk.Leaf.SerialNumber.Cmp(holder.Get().Leaf.SerialNumber) != 0 {
		t.Fatalf("certificato su disco non aggiornato: %v", err)
	}
}
