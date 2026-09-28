// Package identity gestisce la chiave privata e il certificato con cui
// l'agente si autentica al backend (mTLS). La chiave viene generata qui e non
// lascia mai il cluster: al backend arriva solo una CSR.
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

// Store conserva identità e certificato in una cartella.
// Nel prototipo è un volume; in produzione sarà un Secret (vedi README).
type Store struct {
	Dir string
}

func (s Store) path(name string) string { return filepath.Join(s.Dir, name) }

// LoadOrCreateKey legge la chiave Ed25519 o ne genera una nuova.
func (s Store) LoadOrCreateKey() (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(s.path(keyFile))
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, errors.New("chiave privata non leggibile")
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("chiave privata non valida: %w", err)
		}
		key, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("la chiave privata non è Ed25519")
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

// CSR crea una richiesta di firma. Il backend ignora il soggetto e usa
// il cluster_id come CN del certificato.
func CSR(key ed25519.PrivateKey) (string, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "thumbops-agent"},
	}, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// Registered indica se esiste già un certificato.
func (s Store) Registered() bool {
	_, err := os.Stat(s.path(certFile))
	return err == nil
}

// Save salva il certificato; caPEM e clusterID vengono scritti solo se non vuoti.
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

// Load restituisce certificato e chiave pronti per tls.
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
		return nil, fmt.Errorf("certificato e chiave non corrispondono: %w", err)
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

// NeedsRenewal è vero quando resta meno di un terzo della validità.
func NeedsRenewal(leaf *x509.Certificate, now time.Time) bool {
	total := leaf.NotAfter.Sub(leaf.NotBefore)
	return leaf.NotAfter.Sub(now) < total/3
}

// Holder tiene il certificato corrente e permette di sostituirlo
// dopo un rinnovo senza ricreare le connessioni.
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

// ClientTLS restituisce una configurazione TLS che presenta sempre
// il certificato corrente. roots nil = CA di sistema.
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
