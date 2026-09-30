package agent

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/protocol"
)

// progressReporter sends the progress of one action: the first report at
// once, a change at most every minGap, an unchanged state at least every
// maxGap. It never retries. 409/410 cancel the action with
// actions.ErrActionGone; 404 (no progress support) stops the reports.
type progressReporter struct {
	ctx      context.Context
	backend  *backend.Client
	actionID string
	cancel   context.CancelCauseFunc
	now      func() time.Time
	log      *slog.Logger
	minGap   time.Duration
	maxGap   time.Duration

	last     time.Time
	lastMsg  string
	disabled bool
}

func newProgressReporter(ctx context.Context, b *backend.Client, actionID string, cancel context.CancelCauseFunc,
	now func() time.Time, log *slog.Logger, minGap, maxGap time.Duration) *progressReporter {
	return &progressReporter{ctx: ctx, backend: b, actionID: actionID, cancel: cancel, now: now, log: log, minGap: minGap, maxGap: maxGap}
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
	err := r.backend.SendProgress(r.ctx, r.actionID, p)
	switch backend.Code(err) {
	case 0:
		if err != nil && r.ctx.Err() == nil {
			r.log.Warn("sending the progress failed", "err", err)
		}
	case http.StatusConflict, http.StatusGone:
		r.disabled = true
		r.log.Warn("the backend no longer tracks the action: stopping it", "code", backend.Code(err))
		r.cancel(actions.ErrActionGone)
	case http.StatusNotFound:
		r.disabled = true
		r.log.Info("the backend does not accept progress for this action: no more progress")
	default:
		r.log.Warn("sending the progress failed", "err", err)
	}
}
