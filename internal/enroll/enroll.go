// Package enroll handles registration and renewal of the agent certificate.
package enroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/identity"
	"github.com/thumbops/agent/internal/protocol"
)

// ErrNoToken means the agent is not registered and has no bootstrap token.
var ErrNoToken = errors.New("the agent is not registered and there is no bootstrap token")

// Enroller owns the agent identity: it registers, loads and renews it, and
// puts every new certificate in use.
type Enroller struct {
	Backend *backend.Client
	Store   identity.Store
	Holder  *identity.Holder
	Logger  *slog.Logger
	// SaveBackoff is the first wait between attempts to save the identity
	// after a registration (default 1s, doubled up to 30s).
	SaveBackoff time.Duration

	state *identity.State
}

// Start loads the saved identity or registers. It registers when there is no
// identity yet, or when token differs from the one used for the saved
// identity: a new bootstrap token means "register again". The saved identity
// is replaced only after the new registration succeeds. info is called only
// to register. It returns true if it registered.
func (e *Enroller) Start(ctx context.Context, token string, info func(context.Context) (protocol.RegisterRequest, error)) (bool, error) {
	st, err := e.Store.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("loading the agent identity: %w", err)
	}
	token = strings.TrimSpace(token)
	switch {
	case st == nil && token == "":
		return false, ErrNoToken
	case st != nil && (token == "" || identity.HashToken(token) == st.BootstrapTokenHash):
		return false, e.activate(st)
	}
	req, err := info(ctx)
	if err != nil {
		return false, err
	}
	if err := e.register(ctx, token, req); err != nil {
		return false, err
	}
	return true, nil
}

// register sends a new key's CSR with the bootstrap token and saves the result.
func (e *Enroller) register(ctx context.Context, token string, req protocol.RegisterRequest) error {
	key, err := identity.NewKey()
	if err != nil {
		return err
	}
	if req.CSR, err = identity.CSR(key); err != nil {
		return err
	}
	resp, err := e.Backend.Register(ctx, token, req)
	if err != nil {
		return err
	}
	st := &identity.State{
		Key:                key,
		CertPEM:            resp.Certificate,
		CAPEM:              resp.CAChain,
		ClusterID:          resp.ClusterID,
		BootstrapTokenHash: identity.HashToken(token),
	}
	// The token is single-use and already consumed: losing this identity
	// means asking for a new token, so keep trying.
	if err := e.saveWithRetry(ctx, st); err != nil {
		return err
	}
	return e.activate(st)
}

func (e *Enroller) saveWithRetry(ctx context.Context, st *identity.State) error {
	wait := e.SaveBackoff
	if wait == 0 {
		wait = time.Second
	}
	for {
		err := e.Store.Save(ctx, st)
		if err == nil {
			return nil
		}
		e.logger().Error("saving the new identity failed, retrying: the bootstrap token is already used", "err", err, "retry_in", wait)
		select {
		case <-ctx.Done():
			return fmt.Errorf("saving the new identity: %w", err)
		case <-time.After(wait):
		}
		wait = min(wait*2, 30*time.Second)
	}
}

// activate puts the state's certificate in use on new connections: the
// client certificate is presented only at the handshake.
func (e *Enroller) activate(st *identity.State) error {
	cert, err := st.Certificate()
	if err != nil {
		return fmt.Errorf("agent certificate: %w", err)
	}
	e.state = st
	e.Holder.Set(cert)
	e.Backend.ResetConnections()
	return nil
}

// ClusterID returns the cluster_id of the identity in use.
func (e *Enroller) ClusterID() string {
	if e.state == nil {
		return ""
	}
	return e.state.ClusterID
}

// Renew renews the certificate when less than a third of its validity is
// left, with the same key. It returns true if it renewed.
func (e *Enroller) Renew(ctx context.Context, now time.Time) (bool, error) {
	cur := e.Holder.Get()
	if e.state == nil || cur == nil || cur.Leaf == nil || !identity.NeedsRenewal(cur.Leaf, now) {
		return false, nil
	}
	csr, err := identity.CSR(e.state.Key)
	if err != nil {
		return false, err
	}
	resp, err := e.Backend.RenewCertificate(ctx, csr)
	if err != nil {
		return false, err
	}
	next := *e.state
	next.CertPEM = resp.Certificate
	// Saved first: if saving fails the old certificate stays in use and on
	// disk, and the renewal is tried again at the next heartbeat.
	if err := e.Store.Save(ctx, &next); err != nil {
		return false, err
	}
	return true, e.activate(&next)
}

func (e *Enroller) logger() *slog.Logger {
	if e.Logger == nil {
		return slog.Default()
	}
	return e.Logger
}
