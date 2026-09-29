package identity

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// testState returns a state with a self-signed certificate for its key.
func testState(t *testing.T, token string) *State {
	t.Helper()
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "cluster-1"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return &State{
		Key:                key,
		CertPEM:            string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		CAPEM:              "ca",
		ClusterID:          "cluster-1",
		BootstrapTokenHash: HashToken(token),
	}
}

func equal(a, b *State) bool {
	return a.Key.Equal(b.Key) && a.CertPEM == b.CertPEM && a.CAPEM == b.CAPEM &&
		a.ClusterID == b.ClusterID && a.BootstrapTokenHash == b.BootstrapTokenHash
}

func emptySecret() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "thumbops", Name: "thumbops-agent-identity"}}
}

func TestSecretStore(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset(emptySecret())
	s := SecretStore{Client: cs, Namespace: "thumbops", Name: "thumbops-agent-identity"}

	if st, err := s.Load(ctx); err != nil || st != nil {
		t.Fatalf("empty Secret: expected no state, got %v %v", st, err)
	}
	want := testState(t, "token-1")
	if err := s.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil || !equal(got, want) {
		t.Fatalf("state read back differs: %v", err)
	}

	// A new save replaces everything, key included.
	next := testState(t, "token-2")
	if err := s.Save(ctx, next); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Load(ctx); !equal(got, next) {
		t.Fatal("second save not read back")
	}
	sec, _ := cs.CoreV1().Secrets("thumbops").Get(ctx, "thumbops-agent-identity", metav1.GetOptions{})
	if strings.Contains(string(sec.Data[tokenHashKey]), "token-2") {
		t.Fatal("the bootstrap token must not be saved in clear")
	}
}

func TestSecretStoreWithoutSecret(t *testing.T) {
	s := SecretStore{Client: fake.NewClientset(), Namespace: "thumbops", Name: "thumbops-agent-identity"}
	for name, err := range map[string]error{
		"load": func() error { _, err := s.Load(context.Background()); return err }(),
		"save": s.Save(context.Background(), testState(t, "t")),
	} {
		if err == nil || !strings.Contains(err.Error(), "created empty by the manifest") {
			t.Fatalf("%s without the Secret: %v", name, err)
		}
	}
}

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	s := FileStore{Dir: filepath.Join(t.TempDir(), "state")}
	if st, err := s.Load(ctx); err != nil || st != nil {
		t.Fatalf("empty directory: expected no state, got %v %v", st, err)
	}
	want := testState(t, "token-1")
	if err := s.Save(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Load(ctx); err != nil || !equal(got, want) {
		t.Fatalf("state read back differs: %v", err)
	}
	fi, err := os.Stat(filepath.Join(s.Dir, keyKey))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key permissions: %v", err)
	}
}

func TestMismatchedKeyIsAnError(t *testing.T) {
	st := testState(t, "t")
	other := testState(t, "t")
	st.Key = other.Key
	data, err := st.encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode(data); err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("expected a key mismatch error, got %v", err)
	}
}
