// Package enroll handles registration and renewal of the agent certificate.
package enroll

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/identity"
	"github.com/thumbops/agent/internal/protocol"
)

// Register completes the first registration with the bootstrap token:
// it generates the key, sends the CSR, saves the certificate and puts it in use.
// info holds the cluster data; the CSR field is filled in here.
func Register(ctx context.Context, b *backend.Client, store identity.Store, holder *identity.Holder, token string, info protocol.RegisterRequest) error {
	key, err := store.LoadOrCreateKey()
	if err != nil {
		return err
	}
	if info.CSR, err = identity.CSR(key); err != nil {
		return err
	}
	resp, err := b.Register(ctx, strings.TrimSpace(token), info)
	if err != nil {
		return err
	}
	if err := store.Save(resp.Certificate, resp.CAChain, resp.ClusterID); err != nil {
		return err
	}
	return Activate(b, store, holder)
}

// Activate loads the saved certificate and puts it in use on new connections.
func Activate(b *backend.Client, store identity.Store, holder *identity.Holder) error {
	cert, err := store.Load()
	if err != nil {
		return fmt.Errorf("agent certificate: %w", err)
	}
	holder.Set(cert)
	b.ResetConnections()
	return nil
}

// Renew renews the certificate when less than a third of its validity is left.
// It returns true if it renewed.
func Renew(ctx context.Context, b *backend.Client, store identity.Store, holder *identity.Holder, now time.Time) (bool, error) {
	cur := holder.Get()
	if cur == nil || cur.Leaf == nil || !identity.NeedsRenewal(cur.Leaf, now) {
		return false, nil
	}
	key, err := store.LoadOrCreateKey()
	if err != nil {
		return false, err
	}
	csr, err := identity.CSR(key)
	if err != nil {
		return false, err
	}
	resp, err := b.RenewCertificate(ctx, csr)
	if err != nil {
		return false, err
	}
	if err := store.Save(resp.Certificate, "", ""); err != nil {
		return false, err
	}
	return true, Activate(b, store, holder)
}
