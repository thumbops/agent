package mockbackend

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thumbops/agent/internal/protocol"
)

func post(t *testing.T, h http.Handler, path string, body any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	return rec.Code
}

func TestProgressRenewsTheLease(t *testing.T) {
	s := New()
	s.ClaimLease = 100 * time.Millisecond
	h := s.Handler()
	s.Enqueue(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	if code := post(t, h, "/v1/agent/actions/d1/claim", nil); code != http.StatusOK {
		t.Fatalf("claim: %d", code)
	}
	p := protocol.Progress{UpdatedAt: time.Now(), Message: "draining"}
	for range 3 { // three renewals, each before the lease ends
		time.Sleep(60 * time.Millisecond)
		if code := post(t, h, "/v1/agent/actions/d1/progress", p); code != http.StatusOK {
			t.Fatalf("progress within the lease: %d", code)
		}
	}
	if last, n := s.Progress("d1"); n != 3 || last.Message != "draining" {
		t.Fatalf("progress stored: %d %+v", n, last)
	}
	time.Sleep(150 * time.Millisecond) // lease over
	if code := post(t, h, "/v1/agent/actions/d1/progress", p); code != http.StatusGone {
		t.Fatalf("progress after the lease: %d, want 410", code)
	}
	if code := post(t, h, "/v1/agent/actions/d1/result", protocol.Result{Status: protocol.StatusSucceeded}); code != http.StatusGone {
		t.Fatalf("result after the lease: %d, want 410", code)
	}
}

func TestProgressOnCancelledAndUnknownActions(t *testing.T) {
	s := New()
	h := s.Handler()
	s.EnqueueClaimed(protocol.Action{ActionID: "d1", Type: protocol.ActionDrain})
	p := protocol.Progress{UpdatedAt: time.Now(), Message: "draining"}
	if code := post(t, h, "/v1/agent/actions/d1/progress", p); code != http.StatusOK {
		t.Fatalf("progress on an action claimed before a restart: %d", code)
	}
	if code := post(t, h, "/debug/actions/d1/cancel", nil); code != http.StatusOK {
		t.Fatalf("cancel: %d", code)
	}
	if code := post(t, h, "/v1/agent/actions/d1/progress", p); code != http.StatusGone {
		t.Fatalf("progress on a cancelled action: %d, want 410", code)
	}
	if code := post(t, h, "/v1/agent/actions/nope/progress", p); code != http.StatusNotFound {
		t.Fatalf("progress on an unknown action: %d, want 404", code)
	}
}
