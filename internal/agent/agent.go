// Package agent collega backend, policy locale ed esecutore: invia gli
// heartbeat, riceve le azioni in long polling, le prende in carico, le
// esegue e ne comunica l'esito.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/kube"
	"github.com/thumbops/agent/internal/policy"
	"github.com/thumbops/agent/internal/protocol"
)

type Config struct {
	Version           string
	HeartbeatInterval time.Duration // default 60s
	InitialBackoff    time.Duration // default 1s
	MaxBackoff        time.Duration // default 60s
	Logger            *slog.Logger
	Now               func() time.Time
	// Renew viene chiamata dopo ogni heartbeat riuscito (rinnovo del certificato).
	Renew func(ctx context.Context) error
}

type Agent struct {
	cfg     Config
	backend *backend.Client
	kube    *kube.Client
	exec    *actions.Executor
	policy  *policy.Policy
	log     *slog.Logger

	mu            sync.Mutex
	clockOffset   time.Duration
	poll          protocol.PollConfig
	lastActionID  string
	heartbeatOnly bool
}

// ErrUnauthorized indica che il backend non accetta più il certificato
// (cluster revocato o certificato scaduto): serve una nuova registrazione.
var ErrUnauthorized = errors.New("il backend rifiuta il certificato dell'agente: cluster revocato o certificato non valido, serve una nuova registrazione")

// unauthorized conserva la causa (401 o alert TLS) accanto a ErrUnauthorized.
func unauthorized(cause error) error {
	return fmt.Errorf("%w (%v)", ErrUnauthorized, cause)
}

func New(cfg Config, b *backend.Client, k *kube.Client, exec *actions.Executor, pol *policy.Policy) *Agent {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 60 * time.Second
	}
	if cfg.InitialBackoff == 0 {
		cfg.InitialBackoff = time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 60 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Agent{
		cfg: cfg, backend: b, kube: k, exec: exec, policy: pol, log: cfg.Logger,
		poll: protocol.PollConfig{WaitSeconds: 20},
	}
}

// Run blocca fino alla cancellazione del contesto. Restituisce un errore
// solo se l'agente non può più funzionare (ErrUnauthorized).
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ferma anche la goroutine degli heartbeat

	if err := a.heartbeat(ctx); err != nil {
		if backend.Unauthorized(err) {
			return unauthorized(err)
		}
		a.log.Warn("primo heartbeat non riuscito", "err", err)
	}

	fatal := make(chan error, 1)
	go func() {
		t := time.NewTicker(a.cfg.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := a.heartbeat(ctx); err != nil {
					if backend.Unauthorized(err) {
						fatal <- unauthorized(err)
						return
					}
					if ctx.Err() == nil {
						a.log.Warn("heartbeat non riuscito", "err", err)
					}
				}
			}
		}
	}()

	backoff := a.cfg.InitialBackoff
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-fatal:
			return err
		default:
		}

		if a.isHeartbeatOnly() {
			sleep(ctx, a.cfg.HeartbeatInterval)
			continue
		}

		pc := a.pollConfig()
		resp, err := a.backend.PollActions(ctx, pc.WaitSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if backend.Unauthorized(err) {
				return unauthorized(err)
			}
			switch backend.Code(err) {
			case http.StatusUpgradeRequired:
				a.log.Error("versione dell'agente non più supportata: resta attivo solo l'heartbeat, aggiornare l'agente")
				a.setHeartbeatOnly()
				continue
			}
			a.log.Warn("polling delle azioni non riuscito", "err", err, "retry_in", backoff)
			sleep(ctx, jitter(backoff))
			backoff = min(backoff*2, a.cfg.MaxBackoff)
			continue
		}
		backoff = a.cfg.InitialBackoff

		if resp.StatusRequested {
			a.log.Info("richiesto un aggiornamento dello stato del cluster: non ancora implementato nel prototipo")
		}
		for _, act := range resp.Actions {
			a.handle(ctx, act)
		}
		if len(resp.Actions) == 0 && pc.IntervalSeconds > 0 {
			sleep(ctx, time.Duration(pc.IntervalSeconds)*time.Second)
		}
	}
}

func (a *Agent) handle(ctx context.Context, act protocol.Action) {
	log := a.log.With("action_id", act.ActionID, "type", act.Type)

	if !act.ExpiresAt.IsZero() && a.serverNow().After(act.ExpiresAt) {
		log.Warn("azione scaduta, scartata senza eseguirla", "expires_at", act.ExpiresAt)
		return
	}
	if err := a.backend.Claim(ctx, act.ActionID); err != nil {
		switch backend.Code(err) {
		case http.StatusConflict:
			log.Info("azione già presa in carico, scartata")
		case http.StatusGone:
			log.Info("azione scaduta o annullata, scartata")
		default:
			log.Error("presa in carico non riuscita: l'azione verrà riproposta", "err", err)
		}
		return
	}

	var res protocol.Result
	if err := a.checkPolicy(ctx, act); err != nil {
		now := a.cfg.Now().UTC()
		res = protocol.Result{Status: protocol.StatusRejected, StartedAt: now, FinishedAt: now, Message: err.Error()}
		log.Warn("azione rifiutata dalla policy locale", "reason", err)
	} else {
		log.Info("esecuzione azione", "params", act.Params, "requested_by", act.RequestedBy)
		res = a.exec.Execute(ctx, act)
		log.Info("azione conclusa", "status", res.Status, "message", res.Message)
	}

	a.sendResult(ctx, act.ActionID, res, log)
	a.mu.Lock()
	a.lastActionID = act.ActionID
	a.mu.Unlock()
}

func (a *Agent) checkPolicy(ctx context.Context, act protocol.Action) error {
	if err := a.policy.Check(act); err != nil {
		return err
	}
	if protocol.IsNodeAction(act.Type) {
		n, err := a.kube.GetNode(ctx, act.Params.Node)
		if err != nil {
			return nil // il nodo inesistente o illeggibile lo segnala l'esecutore
		}
		return a.policy.CheckNode(n.Metadata.Name, n.Metadata.Labels)
	}
	return nil
}

// sendResult ritenta finché il backend conferma: l'esito non deve andare perso.
func (a *Agent) sendResult(ctx context.Context, id string, res protocol.Result, log *slog.Logger) {
	backoff := a.cfg.InitialBackoff
	for {
		err := a.backend.SendResult(ctx, id, res)
		if err == nil {
			return
		}
		switch backend.Code(err) {
		case http.StatusBadRequest, http.StatusConflict, http.StatusGone:
			log.Error("il backend ha rifiutato l'esito", "err", err)
			return
		}
		if ctx.Err() != nil {
			log.Error("esito non inviato: agente in arresto", "status", res.Status)
			return
		}
		log.Warn("invio dell'esito non riuscito, nuovo tentativo", "err", err, "retry_in", backoff)
		sleep(ctx, jitter(backoff))
		backoff = min(backoff*2, a.cfg.MaxBackoff)
	}
}

// Permessi verificati per ogni tipo di azione, riportati nell'heartbeat.
var actionPermissions = map[string][]kube.ResourceAttributes{
	protocol.ActionRolloutRestart: {{Verb: "get", Group: "apps", Resource: "deployments"}, {Verb: "patch", Group: "apps", Resource: "deployments"}},
	protocol.ActionScale:          {{Verb: "get", Group: "apps", Resource: "deployments"}, {Verb: "patch", Group: "apps", Resource: "deployments"}},
	protocol.ActionCordon:         {{Verb: "get", Resource: "nodes"}, {Verb: "patch", Resource: "nodes"}},
	protocol.ActionUncordon:       {{Verb: "get", Resource: "nodes"}, {Verb: "patch", Resource: "nodes"}},
	protocol.ActionDrain: {
		{Verb: "get", Resource: "nodes"}, {Verb: "patch", Resource: "nodes"},
		{Verb: "list", Resource: "pods"}, {Verb: "create", Resource: "pods", Subresource: "eviction"},
	},
}

func (a *Agent) permissions(ctx context.Context) map[string]bool {
	perms := make(map[string]bool, len(protocol.ActionTypes))
	for _, t := range protocol.ActionTypes {
		ok := true
		for _, attrs := range actionPermissions[t] {
			allowed, err := a.kube.CanI(ctx, attrs)
			if err != nil || !allowed {
				ok = false
				break
			}
		}
		perms[t] = ok
	}
	return perms
}

func (a *Agent) heartbeat(ctx context.Context) error {
	req := protocol.HeartbeatRequest{AgentVersion: a.cfg.Version, Permissions: a.permissions(ctx)}
	if v, err := a.kube.ServerVersion(ctx); err == nil {
		req.KubernetesVersion = v
	} else {
		a.log.Warn("versione di Kubernetes non leggibile", "err", err)
	}
	if nodes, err := a.kube.ListNodes(ctx); err == nil {
		req.Nodes.Total = len(nodes)
		for i := range nodes {
			if nodes[i].Ready() {
				req.Nodes.Ready++
			}
		}
	} else {
		a.log.Warn("elenco dei nodi non leggibile", "err", err)
	}
	a.mu.Lock()
	req.LastActionID = a.lastActionID
	a.mu.Unlock()

	sent := a.cfg.Now()
	resp, err := a.backend.Heartbeat(ctx, req)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if !resp.ServerTime.IsZero() {
		a.clockOffset = resp.ServerTime.Sub(sent)
	}
	if resp.Poll.WaitSeconds > 0 || resp.Poll.IntervalSeconds > 0 {
		a.poll = resp.Poll
	}
	a.mu.Unlock()

	if a.cfg.Renew != nil {
		if err := a.cfg.Renew(ctx); err != nil {
			a.log.Error("rinnovo del certificato non riuscito", "err", err)
		}
	}
	return nil
}

func (a *Agent) serverNow() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Now().Add(a.clockOffset)
}

func (a *Agent) pollConfig() protocol.PollConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.poll
}

func (a *Agent) isHeartbeatOnly() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.heartbeatOnly
}

func (a *Agent) setHeartbeatOnly() {
	a.mu.Lock()
	a.heartbeatOnly = true
	a.mu.Unlock()
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + rand.N(d/2+1)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// String rende leggibile lo stato nei log di debug.
func (a *Agent) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return fmt.Sprintf("agent(last=%s offset=%s poll=%+v)", a.lastActionID, a.clockOffset, a.poll)
}
