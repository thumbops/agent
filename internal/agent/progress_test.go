package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/actions"
	"github.com/thumbops/agent/internal/backend"
	"github.com/thumbops/agent/internal/mockbackend"
	"github.com/thumbops/agent/internal/protocol"
)

func newReporterEnv(t *testing.T) (*mockbackend.Server, *backend.Client) {
	t.Helper()
	mb := mockbackend.New()
	srv := httptest.NewServer(mb.Handler())
	t.Cleanup(srv.Close)
	return mb, backend.New(backend.Options{BaseURL: srv.URL})
}

func TestReporterThrottles(t *testing.T) {
	mb, b := newReporterEnv(t)
	mb.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	now := time.Unix(1000, 0)
	_, cancel := context.WithCancelCause(context.Background())
	r := newProgressReporter(context.Background(), b, "d1", cancel, func() time.Time { return now },
		slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, 30*time.Second)

	p := func(msg string) protocol.Progress { return protocol.Progress{Message: msg} }
	r.report(p("a")) // first: sent at once
	now = now.Add(time.Second)
	r.report(p("b")) // changed but within 5 s: skipped
	now = now.Add(5 * time.Second)
	r.report(p("c")) // changed, 6 s later: sent
	now = now.Add(10 * time.Second)
	r.report(p("c")) // unchanged, 10 s later: skipped
	now = now.Add(25 * time.Second)
	r.report(p("c")) // unchanged, 35 s later: sent
	if last, n := mb.Progress("d1"); n != 3 || last.Message != "c" {
		t.Fatalf("sent %d, last %q; want 3 and c", n, last.Message)
	}
}

func TestReporterStopsTheActionOnGone(t *testing.T) {
	mb, b := newReporterEnv(t)
	mb.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	mb.Cancel("d1")
	ctx, cancel := context.WithCancelCause(context.Background())
	r := newProgressReporter(ctx, b, "d1", cancel, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, 30*time.Second)
	r.report(protocol.Progress{Message: "a"})
	if !errors.Is(context.Cause(ctx), actions.ErrActionGone) {
		t.Fatalf("a 410 must cancel the action with ErrActionGone, got %v", context.Cause(ctx))
	}
}

func TestReporterStopsReportingOn404(t *testing.T) {
	_, b := newReporterEnv(t) // action unknown to the backend: 404
	ctx, cancel := context.WithCancelCause(context.Background())
	now := time.Unix(1000, 0)
	r := newProgressReporter(ctx, b, "d1", cancel, func() time.Time { return now }, slog.New(slog.NewTextHandler(io.Discard, nil)), 5*time.Second, 30*time.Second)
	r.report(protocol.Progress{Message: "a"})
	if ctx.Err() != nil {
		t.Fatal("a 404 must not stop the action")
	}
	if !r.disabled {
		t.Fatal("a 404 must stop further progress for the action")
	}
}
