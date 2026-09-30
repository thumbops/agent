// Package agent ties together backend, local policy and executor: it sends
// heartbeats, receives actions through long polling, claims them, runs them
// and reports the result.
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
	"github.com/thumbops/agent/internal/health"
	"github.com/thumbops/agent/internal/kube"
	"github.com/thumbops/agent/internal/metrics"
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
	// Renew is called after every successful heartbeat (certificate renewal).
	Renew func(ctx context.Context) error
	// Status, if set, is sent with PUT /v1/agent/status every StatusInterval
	// and right away when a poll response has status_requested.
	Status         StatusSource
	StatusInterval time.Duration // default 60s
	StatusRetry    time.Duration // until the first summary is ready; default 5s
	// Metrics and Health are optional (nil records nothing).
	Metrics *metrics.Metrics
	Health  *health.State
	// Progress throttling (defaults 5s and 30s).
	ProgressMinGap time.Duration
	ProgressMaxGap time.Duration
}

// StatusSource builds the cluster status summary; ok is false while it is
// not ready yet (informer caches not synced, e.g. missing RBAC).
type StatusSource interface {
	Collect(at time.Time) (s protocol.ClusterStatus, ok bool)
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

	statusNow chan struct{} // status_requested from the backend
}

// ErrUnauthorized means the backend no longer accepts the certificate
// (cluster revoked or certificate expired): a new registration is needed.
var ErrUnauthorized = errors.New("the backend rejects the agent certificate: cluster revoked or certificate invalid, a new registration is needed (put a new bootstrap token in the bootstrap Secret and restart the agent)")

// unauthorized keeps the cause (401 or TLS alert) next to ErrUnauthorized.
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
	if cfg.StatusInterval == 0 {
		cfg.StatusInterval = 60 * time.Second
	}
	if cfg.StatusRetry == 0 {
		cfg.StatusRetry = 5 * time.Second
	}
	if cfg.ProgressMinGap == 0 {
		cfg.ProgressMinGap = 5 * time.Second
	}
	if cfg.ProgressMaxGap == 0 {
		cfg.ProgressMaxGap = 30 * time.Second
	}
	return &Agent{
		cfg: cfg, backend: b, kube: k, exec: exec, policy: pol, log: cfg.Logger,
		poll:      protocol.PollConfig{WaitSeconds: 20},
		statusNow: make(chan struct{}, 1),
	}
}

// Run blocks until the context is canceled. It returns an error only if
// the agent can no longer work (ErrUnauthorized).
func (a *Agent) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()           // also stops the heartbeat goroutine
	a.cfg.Health.MarkReady() // startup is complete: policy and identity are loaded

	if err := a.heartbeat(ctx); err != nil {
		if backend.Unauthorized(err) {
			return unauthorized(err)
		}
		a.log.Warn("first heartbeat failed", "err", err)
	}

	fatal := make(chan error, 1)
	stop := func(err error) {
		select {
		case fatal <- err:
		default: // another goroutine already reported a fatal error
		}
	}
	if a.cfg.Status != nil {
		go a.statusLoop(ctx, stop)
	}
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
						stop(unauthorized(err))
						return
					}
					if ctx.Err() == nil {
						a.log.Warn("heartbeat failed", "err", err)
					}
				}
			}
		}
	}()

	a.resumeDrains(ctx)

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
				a.log.Error("agent version no longer supported: only the heartbeat stays active, upgrade the agent")
				a.setHeartbeatOnly()
				continue
			}
			a.log.Warn("polling for actions failed", "err", err, "retry_in", backoff)
			sleep(ctx, jitter(backoff))
			backoff = min(backoff*2, a.cfg.MaxBackoff)
			continue
		}
		backoff = a.cfg.InitialBackoff

		if resp.StatusRequested {
			select {
			case a.statusNow <- struct{}{}:
			default: // a refresh is already pending
			}
		}
		for _, act := range resp.Actions {
			a.handle(ctx, act)
		}
		if len(resp.Actions) == 0 && pc.IntervalSeconds > 0 {
			sleep(ctx, time.Duration(pc.IntervalSeconds)*time.Second)
		}
	}
}

// statusLoop sends the cluster status every StatusInterval and when the
// backend asks for it. A failed send is not retried: the next one carries
// fresher data anyway.
func (a *Agent) statusLoop(ctx context.Context, stop func(error)) {
	start := a.cfg.Now()
	warned := false
	wait := a.cfg.StatusRetry // the first summary as soon as it is ready
	for {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-a.statusNow:
			t.Stop()
		}
		s, ok := a.cfg.Status.Collect(a.serverNow())
		a.cfg.Metrics.StatusReady(ok)
		if !ok {
			if !warned && a.cfg.Now().Sub(start) > time.Minute {
				a.log.Warn("cluster status not available yet: check the thumbops-agent-status ClusterRole (get, list, watch on nodes, pods, deployments)")
				warned = true
			}
			wait = a.cfg.StatusRetry
			continue
		}
		wait = a.cfg.StatusInterval
		if err := a.backend.PutStatus(ctx, s); err != nil {
			if backend.Unauthorized(err) {
				stop(unauthorized(err))
				return
			}
			if ctx.Err() == nil {
				a.log.Warn("sending the cluster status failed", "err", err)
			}
			continue
		}
		a.cfg.Metrics.StatusSent(a.cfg.Now())
	}
}

func (a *Agent) handle(ctx context.Context, act protocol.Action) {
	log := a.log.With("action_id", act.ActionID, "type", act.Type)

	if !act.ExpiresAt.IsZero() && a.serverNow().After(act.ExpiresAt) {
		log.Warn("action expired, discarded without running it", "expires_at", act.ExpiresAt)
		a.cfg.Metrics.ActionOutcome(act.Type, metrics.OutcomeExpired)
		return
	}
	if err := a.backend.Claim(ctx, act.ActionID); err != nil {
		switch backend.Code(err) {
		case http.StatusConflict:
			log.Info("action already claimed, discarded")
			a.cfg.Metrics.ActionOutcome(act.Type, metrics.OutcomeDiscarded)
		case http.StatusGone:
			log.Info("action expired or canceled, discarded")
			a.cfg.Metrics.ActionOutcome(act.Type, metrics.OutcomeDiscarded)
		default:
			log.Error("claim failed: the action will be offered again", "err", err)
		}
		return
	}

	var res protocol.Result
	if err := a.checkPolicy(ctx, act); err != nil {
		now := a.cfg.Now().UTC()
		res = protocol.Result{Status: protocol.StatusRejected, StartedAt: now, FinishedAt: now, Message: err.Error()}
		log.Warn("action rejected by the local policy", "reason", err)
		a.cfg.Metrics.ActionOutcome(act.Type, protocol.StatusRejected)
	} else {
		var gone bool
		if res, gone = a.execute(ctx, act, log); gone {
			return
		}
	}

	a.sendResult(ctx, act.ActionID, res, log)
	a.mu.Lock()
	a.lastActionID = act.ActionID
	a.mu.Unlock()
}

// execute runs a claimed action with a progress reporter. gone is true when
// the backend stopped tracking the action (409/410 on progress): no result
// must be sent.
func (a *Agent) execute(ctx context.Context, act protocol.Action, log *slog.Logger) (res protocol.Result, gone bool) {
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	rep := newProgressReporter(actx, a.backend, act.ActionID, cancel, a.serverNow, log, a.cfg.ProgressMinGap, a.cfg.ProgressMaxGap)
	log.Info("running action", "params", act.Params, "requested_by", act.RequestedBy)
	a.cfg.Metrics.ActionRunning(true)
	start := time.Now()
	res = a.exec.Execute(actx, act, rep.report)
	a.cfg.Metrics.ObserveActionDuration(act.Type, time.Since(start))
	a.cfg.Metrics.ActionRunning(false)
	if errors.Is(context.Cause(actx), actions.ErrActionGone) {
		a.cfg.Metrics.ActionOutcome(act.Type, metrics.OutcomeDiscarded)
		log.Warn("action stopped: the backend no longer tracks it, no result sent")
		return res, true
	}
	a.cfg.Metrics.ActionOutcome(act.Type, res.Status)
	log.Info("action finished", "status", res.Status, "message", res.Message)
	return res, false
}

// resumeDrains finishes the drains interrupted by a restart (nodes with the
// drain-in-progress annotation): no new claim, no policy check (both
// happened before the restart).
func (a *Agent) resumeDrains(ctx context.Context) {
	acts, dropped, err := a.exec.InProgressDrains(ctx)
	if err != nil {
		a.log.Warn("cannot look for interrupted drains", "err", err)
		return
	}
	for _, node := range dropped {
		a.log.Warn("unreadable drain-in-progress annotation removed; the node stays cordoned", "node", node)
	}
	for _, act := range acts {
		log := a.log.With("action_id", act.ActionID, "type", act.Type)
		log.Info("resuming an interrupted drain", "node", act.Params.Node)
		res, gone := a.execute(ctx, act, log)
		if gone {
			continue
		}
		a.sendResult(ctx, act.ActionID, res, log)
		a.mu.Lock()
		a.lastActionID = act.ActionID
		a.mu.Unlock()
	}
}

func (a *Agent) checkPolicy(ctx context.Context, act protocol.Action) error {
	if err := a.policy.Check(act); err != nil {
		return err
	}
	if protocol.IsNodeAction(act.Type) {
		n, err := a.kube.GetNode(ctx, act.Params.Node)
		if err != nil {
			return nil // a missing or unreadable node is reported by the executor
		}
		return a.policy.CheckNode(n.Metadata.Name, n.Metadata.Labels)
	}
	return nil
}

// sendResult retries until the backend confirms: the result must never be lost.
func (a *Agent) sendResult(ctx context.Context, id string, res protocol.Result, log *slog.Logger) {
	backoff := a.cfg.InitialBackoff
	for {
		err := a.backend.SendResult(ctx, id, res)
		if err == nil {
			return
		}
		switch backend.Code(err) {
		case http.StatusBadRequest, http.StatusConflict, http.StatusGone:
			log.Error("the backend rejected the result", "err", err)
			return
		}
		if ctx.Err() != nil {
			log.Error("result not sent: agent shutting down", "status", res.Status)
			return
		}
		log.Warn("sending the result failed, retrying", "err", err, "retry_in", backoff)
		sleep(ctx, jitter(backoff))
		backoff = min(backoff*2, a.cfg.MaxBackoff)
	}
}

// Permissions checked for each action type, reported in the heartbeat.
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
	defer a.cfg.Health.HeartbeatAttempted() // any outcome proves the loop is alive
	req := protocol.HeartbeatRequest{AgentVersion: a.cfg.Version, Permissions: a.permissions(ctx)}
	if v, err := a.kube.ServerVersion(ctx); err == nil {
		req.KubernetesVersion = v
	} else {
		a.log.Warn("cannot read the Kubernetes version", "err", err)
	}
	if nodes, err := a.kube.ListNodes(ctx); err == nil {
		req.Nodes.Total = len(nodes)
		for i := range nodes {
			if nodes[i].Ready() {
				req.Nodes.Ready++
			}
		}
	} else {
		a.log.Warn("cannot list the nodes", "err", err)
	}
	a.mu.Lock()
	req.LastActionID = a.lastActionID
	a.mu.Unlock()

	sent := a.cfg.Now()
	resp, err := a.backend.Heartbeat(ctx, req)
	if err != nil {
		return err
	}
	a.cfg.Metrics.HeartbeatSucceeded(a.cfg.Now())
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
			a.log.Error("certificate renewal failed", "err", err)
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
	a.cfg.Metrics.SetHeartbeatOnly()
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

// String makes the state readable in debug logs.
func (a *Agent) String() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return fmt.Sprintf("agent(last=%s offset=%s poll=%+v)", a.lastActionID, a.clockOffset, a.poll)
}
