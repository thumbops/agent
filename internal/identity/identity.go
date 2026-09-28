// Package identity manages the private key and the certificate the agent
// uses to authenticate to the backend (mTLS). The key is generated here and
// never leaves the cluster: the backend only receives a CSR.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	keyFile       = "key.pem"
	certFile      = "cert.pem"
	caFile        = "ca.pem"
	clusterIDFile = "cluster_id"
)

// Store keeps the identity and certificate in a directory.
// In the prototype it is a volume; in production it will be a Secret (see README).
type Store struct {
	Dir string
}

func (s Store) path(name string) string { return filepath.Join(s.Dir, name) }

// LoadOrCreateKey reads the Ed25519 key or generates a new one.
func (s Store) LoadOrCreateKey() (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(s.path(keyFile))
	if err == nil {
		block, _ := pem.Decode(data)
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
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(s.path(keyFile), out, 0o600); err != nil {
		return nil, err
	}
	return key, nil
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

// Registered reports whether a certificate already exists.
func (s Store) Registered() bool {
	_, err := os.Stat(s.path(certFile))
	return err == nil
}

// Save stores the certificate; caPEM and clusterID are written only if not empty.
func (s Store) Save(certPEM, caPEM, clusterID string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := writeAtomic(s.path(certFile), []byte(certPEM)); err != nil {
		return err
	}
	if caPEM != "" {
		if err := writeAtomic(s.path(caFile), []byte(caPEM)); err != nil {
			return err
		}
	}
	if clusterID != "" {
		if err := writeAtomic(s.path(clusterIDFile), []byte(clusterID)); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s Store) ClusterID() string {
	b, _ := os.ReadFile(s.path(clusterIDFile))
	return strings.TrimSpace(string(b))
}

// Load returns the certificate and key ready for tls.
func (s Store) Load() (*tls.Certificate, error) {
	certPEM, err := os.ReadFile(s.path(certFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(s.path(keyFile))
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate and key do not match: %w", err)
	}
	if cert.Leaf == nil {
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, err
		}
		cert.Leaf = leaf
	}
	return &cert, nil
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
