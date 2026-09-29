// Package identity manages the private key and the certificate the agent
// uses to authenticate to the backend (mTLS). The key is generated here and
// never leaves the cluster: the backend only receives a CSR.
package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Keys of the saved state: file names in a FileStore, data keys in a SecretStore.
const (
	keyKey       = "key.pem"
	certKey      = "cert.pem"
	caKey        = "ca.pem"
	clusterIDKey = "cluster_id"
	tokenHashKey = "bootstrap_token_sha256"
)

// State is everything the agent keeps about its identity. Key and
// certificate are always saved together, so they never get out of step.
type State struct {
	Key       ed25519.PrivateKey
	CertPEM   string
	CAPEM     string
	ClusterID string
	// BootstrapTokenHash is the HashToken of the bootstrap token used for the
	// registration: a different token means "register again".
	BootstrapTokenHash string
}

// Store saves the State: a Secret in the cluster, a directory in development.
type Store interface {
	// Load returns the saved state, or nil (and no error) before the first registration.
	Load(ctx context.Context) (*State, error)
	// Save replaces the saved state.
	Save(ctx context.Context, st *State) error
}

// NewKey generates a new Ed25519 key.
func NewKey() (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	return key, err
}

// HashToken returns the hex SHA-256 of a bootstrap token, ignoring the
// surrounding whitespace: the token itself is never saved.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// CSR creates a signing request. The backend ignores the subject and uses
// the cluster_id as the certificate CN.
func CSR(key ed25519.PrivateKey) (string, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "thumbops-agent"},
	}, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// Certificate returns the certificate and key ready for tls.
func (st *State) Certificate() (*tls.Certificate, error) {
	block, _ := pem.Decode([]byte(st.CertPEM))
	if block == nil {
		return nil, errors.New("cannot read the certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid certificate: %w", err)
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !pub.Equal(st.Key.Public()) {
		return nil, errors.New("certificate and key do not match")
	}
	return &tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: st.Key, Leaf: leaf}, nil
}

// encode turns the state into the saved entries.
func (st *State) encode() (map[string][]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(st.Key)
	if err != nil {
		return nil, err
	}
	data := map[string][]byte{
		keyKey:       pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}),
		certKey:      []byte(st.CertPEM),
		clusterIDKey: []byte(st.ClusterID),
		tokenHashKey: []byte(st.BootstrapTokenHash),
	}
	if st.CAPEM != "" {
		data[caKey] = []byte(st.CAPEM)
	}
	return data, nil
}

// decode reads the saved entries; without a certificate there is no state.
func decode(data map[string][]byte) (*State, error) {
	if len(data[certKey]) == 0 {
		return nil, nil
	}
	block, _ := pem.Decode(data[keyKey])
	if block == nil {
		return nil, errors.New("cannot read the private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("the private key is not Ed25519")
	}
	st := &State{
		Key:                key,
		CertPEM:            string(data[certKey]),
		CAPEM:              string(data[caKey]),
		ClusterID:          strings.TrimSpace(string(data[clusterIDKey])),
		BootstrapTokenHash: strings.TrimSpace(string(data[tokenHashKey])),
	}
	if _, err := st.Certificate(); err != nil {
		return nil, err
	}
	return st, nil
}

// NeedsRenewal is true when less than a third of the validity is left.
func NeedsRenewal(leaf *x509.Certificate, now time.Time) bool {
	total := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotAfter.Sub(now) < total/3
}

// Holder keeps the current certificate and lets it be replaced after a
// renewal without recreating the TLS configuration.
type Holder struct {
	mu   sync.RWMutex
	cert *tls.Certificate
}

func (h *Holder) Set(c *tls.Certificate) {
	h.mu.Lock()
	h.cert = c
	h.mu.Unlock()
}

func (h *Holder) Get() *tls.Certificate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cert
}

// ClientTLS returns a TLS configuration that always presents the current
// certificate. nil roots = system CAs.
func (h *Holder) ClientTLS(roots *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			if c := h.Get(); c != nil {
				return c, nil
			}
			return &tls.Certificate{}, nil
		},
	}
}
