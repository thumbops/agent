// Package enroll gestisce registrazione e rinnovo del certificato dell'agente.
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

// Register completa la prima registrazione con il token di bootstrap:
// genera la chiave, invia la CSR, salva il certificato e lo mette in uso.
// info contiene i dati del cluster; il campo CSR viene compilato qui.
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

// Activate carica il certificato salvato e lo mette in uso sulle nuove connessioni.
func Activate(b *backend.Client, store identity.Store, holder *identity.Holder) error {
	cert, err := store.Load()
	if err != nil {
		return fmt.Errorf("certificato dell'agente: %w", err)
	}
	holder.Set(cert)
	b.ResetConnections()
	return nil
}

// Renew rinnova il certificato quando resta meno di un terzo della validità.
// Restituisce true se ha rinnovato.
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
