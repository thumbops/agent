package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/protocol"
)

// progressReporter sends the progress of one action: the first report at
// once, a change at most every minGap, an unchanged state at least every
// maxGap. It never retries, except the first progress of a resumed drain.
// 409/410 cancel the action with actions.ErrActionGone; 404 (no progress
// support) stops the reports; 401 or a rejected certificate cancel the
// action with errUnauthorized. For a resumed drain (requireFirstAck) the
// first progress must get a 200: any other outcome, after the retries,
// cancels the action with actions.ErrActionGone.
type progressReporter struct {
	ctx      context.Context
	backend  *backend.Client
	actionID string
	cancel   context.CancelCauseFunc
	now      func() time.Time
	log      *slog.Logger
	minGap   time.Duration
	maxGap   time.Duration

	// requireFirstAck is set for a resumed drain: its first send is the
	// backend's authorization to go on (see sendFirst).
	requireFirstAck bool
	firstAckRetry   time.Duration

	last     time.Time
	lastMsg  string
	disabled bool
}

// firstAckAttempts is how many times the first progress of a resumed drain
// is tried, firstAckRetry apart.
const firstAckAttempts = 3

// errUnauthorized is the cancellation cause when the backend rejects the
// agent certificate (401 or TLS alert) during an action. It is not
// actions.ErrActionGone: a drain keeps its annotation and is resumed after a
// new registration.
var errUnauthorized = errors.New("the backend rejects the agent certificate")

func newProgressReporter(ctx context.Context, b *backend.Client, actionID string, cancel context.CancelCauseFunc,
	now func() time.Time, log *slog.Logger, minGap, maxGap time.Duration) *progressReporter {
	return &progressReporter{ctx: ctx, backend: b, actionID: actionID, cancel: cancel, now: now, log: log, minGap: minGap, maxGap: maxGap,
		firstAckRetry: time.Second}
}

// report is an actions.ProgressFunc; it runs on the action's goroutine.
func (r *progressReporter) report(p protocol.Progress) {
	if r.disabled {
		return
	}
	now := r.now()
	switch {
	case r.last.IsZero():
	case p.Message != r.lastMsg && now.Sub(r.last) >= r.minGap:
	case now.Sub(r.last) >= r.maxGap:
	default:
		return
	}
	r.last, r.lastMsg = now, p.Message
	first := r.requireFirstAck
	r.requireFirstAck = false
	err := r.send(p, first)
	if err == nil || r.ctx.Err() != nil {
		return // sent, or the agent is shutting down
	}
	code := backend.Code(err)
	switch {
	case backend.Unauthorized(err):
		r.disabled = true
		r.log.Error("the backend rejects the agent certificate: stopping the action", "err", err)
		r.cancel(errUnauthorized)
	case code == http.StatusConflict || code == http.StatusGone:
		r.disabled = true
		r.log.Warn("the backend no longer tracks the action: stopping it", "code", code)
		r.cancel(actions.ErrActionGone)
	case first:
		r.disabled = true
		r.log.Warn("cannot confirm the interrupted drain with the backend: abandoned; the node is left as it is", "err", err)
		r.cancel(actions.ErrActionGone)
	case code == http.StatusNotFound:
		r.disabled = true
		r.log.Info("the backend does not accept progress for this action: no more progress")
	default:
		r.log.Warn("sending the progress failed", "err", err)
	}
}

// send sends one progress. The first progress of a resumed drain (first) is
// retried a few times on a network error or a 5xx, because its failure
// abandons the drain; every other progress is sent once.
func (r *progressReporter) send(p protocol.Progress, first bool) error {
	attempts := 1
	if first {
		attempts = firstAckAttempts
	}
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			sleep(r.ctx, r.firstAckRetry)
			if r.ctx.Err() != nil {
				return err
			}
		}
		err = r.backend.SendProgress(r.ctx, r.actionID, p)
		code := backend.Code(err)
		if err == nil || backend.Unauthorized(err) || (code != 0 && code < 500) {
			return err
		}
	}
	return err
}
