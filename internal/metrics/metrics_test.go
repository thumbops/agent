package metrics

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRecordedValues(t *testing.T) {
	m := New("1.2.3")
	m.BackendRequest(OpHeartbeat, 200)
	m.BackendRequest(OpPoll, 503)
	m.BackendRequest(OpPoll, 0)
	m.HeartbeatSucceeded(time.Unix(1700000000, 0))
	m.SetHeartbeatOnly()
	m.ActionOutcome("scale", "succeeded")
	m.ActionOutcome("delete-everything", "rejected")
	m.ActionRunning(true)
	m.ObserveActionDuration("drain", 90*time.Second)
	m.CertificateInUse(time.Unix(1800000000, 0))
	m.CertificateRenewal(nil)
	m.CertificateRenewal(errors.New("boom"))
	m.StatusReady(true)
	m.StatusSent(time.Unix(1700000100, 0))

	checks := []struct {
		name string
		got  float64
		want float64
	}{
		{"heartbeat 200", testutil.ToFloat64(m.backendRequests.WithLabelValues("heartbeat", "200")), 1},
		{"poll 503", testutil.ToFloat64(m.backendRequests.WithLabelValues("poll", "503")), 1},
		{"poll error", testutil.ToFloat64(m.backendRequests.WithLabelValues("poll", "error")), 1},
		{"last heartbeat", testutil.ToFloat64(m.heartbeatLastSuccess), 1700000000},
		{"heartbeat only", testutil.ToFloat64(m.heartbeatOnly), 1},
		{"scale succeeded", testutil.ToFloat64(m.actions.WithLabelValues("scale", "succeeded")), 1},
		{"unknown type", testutil.ToFloat64(m.actions.WithLabelValues("unknown", "rejected")), 1},
		{"in progress", testutil.ToFloat64(m.actionInProgress), 1},
		{"cert expiry", testutil.ToFloat64(m.certExpiry), 1800000000},
		{"renewal success", testutil.ToFloat64(m.renewals.WithLabelValues("success")), 1},
		{"renewal error", testutil.ToFloat64(m.renewals.WithLabelValues("error")), 1},
		{"status ready", testutil.ToFloat64(m.statusReady), 1},
		{"status sent", testutil.ToFloat64(m.statusLastSent), 1700000100},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
	if n := testutil.CollectAndCount(m.actionDuration); n != 1 {
		t.Errorf("duration series: %d", n)
	}
}

// Every action type and outcome starts at 0, so rate() and alerts work
// before the first action.
func TestActionSeriesStartAtZero(t *testing.T) {
	m := New("1.2.3")
	if n := testutil.CollectAndCount(m.actions); n != 5*5 {
		t.Fatalf("pre-initialized action series: %d, want 25", n)
	}
}

// The metric names are a contract for whoever writes dashboards and alerts.
func TestExposedNames(t *testing.T) {
	m := New("1.2.3")
	m.BackendRequest(OpHeartbeat, 200)
	m.ObserveActionDuration("scale", time.Second)
	m.CertificateRenewal(nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, name := range []string{
		`thumbops_agent_info{version="1.2.3"} 1`,
		"thumbops_agent_backend_requests_total",
		"thumbops_agent_heartbeat_last_success_timestamp_seconds",
		"thumbops_agent_heartbeat_only",
		"thumbops_agent_actions_total",
		"thumbops_agent_action_duration_seconds_bucket",
		"thumbops_agent_action_in_progress",
		"thumbops_agent_certificate_expiry_timestamp_seconds",
		"thumbops_agent_certificate_renewals_total",
		"thumbops_agent_status_ready",
		"thumbops_agent_status_last_sent_timestamp_seconds",
		"go_goroutines",
	} {
		if !strings.Contains(string(body), name) {
			t.Errorf("/metrics does not expose %s", name)
		}
	}
}

func TestNilIsSafe(t *testing.T) {
	var m *Metrics
	m.BackendRequest(OpPoll, 200)
	m.HeartbeatSucceeded(time.Now())
	m.SetHeartbeatOnly()
	m.ActionOutcome("scale", "succeeded")
	m.ActionRunning(true)
	m.ObserveActionDuration("scale", time.Second)
	m.CertificateInUse(time.Now())
	m.CertificateRenewal(nil)
	m.StatusReady(true)
	m.StatusSent(time.Now())
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 404 {
		t.Fatalf("nil metrics handler: %d", rec.Code)
	}
}
