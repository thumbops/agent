// Package metrics holds the agent's Prometheus metrics on a private
// registry. Every method is safe on a nil *Metrics, which records nothing.
package metrics

import (
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/thumbops/agent/internal/protocol"
)

const namespace = "thumbops_agent"

// Outcomes of thumbops_agent_actions_total besides the result statuses
// (protocol.StatusSucceeded, StatusFailed, StatusRejected).
const (
	OutcomeExpired   = "expired"   // received after its deadline, not claimed
	OutcomeDiscarded = "discarded" // claim answered 409 or 410
)

// Backend operations, the operation label of thumbops_agent_backend_requests_total.
const (
	OpRegister  = "register"
	OpRenew     = "renew"
	OpHeartbeat = "heartbeat"
	OpPoll      = "poll"
	OpClaim     = "claim"
	OpResult    = "result"
	OpStatus    = "status"
	OpProgress  = "progress"
)

var outcomes = []string{protocol.StatusSucceeded, protocol.StatusFailed, protocol.StatusRejected, OutcomeExpired, OutcomeDiscarded}

type Metrics struct {
	reg                  *prometheus.Registry
	backendRequests      *prometheus.CounterVec
	heartbeatLastSuccess prometheus.Gauge
	heartbeatOnly        prometheus.Gauge
	actions              *prometheus.CounterVec
	actionDuration       *prometheus.HistogramVec
	actionInProgress     prometheus.Gauge
	certExpiry           prometheus.Gauge
	renewals             *prometheus.CounterVec
	statusReady          prometheus.Gauge
	statusLastSent       prometheus.Gauge
}

func New(version string) *Metrics {
	gauge := func(name, help string) prometheus.Gauge {
		return prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help})
	}
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		backendRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "backend_requests_total",
			Help: "Calls to the backend by operation and HTTP status (error = network error or TLS alert).",
		}, []string{"operation", "code"}),
		heartbeatLastSuccess: gauge("heartbeat_last_success_timestamp_seconds", "Unix time of the last successful heartbeat."),
		heartbeatOnly:        gauge("heartbeat_only", "1 after a 426: the agent version is no longer supported and only the heartbeat runs."),
		actions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "actions_total",
			Help: "Actions received, by type and outcome (succeeded, failed, rejected, expired, discarded).",
		}, []string{"type", "outcome"}),
		actionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "action_duration_seconds",
			Help:    "Execution time of the claimed actions.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800},
		}, []string{"type"}),
		actionInProgress: gauge("action_in_progress", "1 while an action runs."),
		certExpiry:       gauge("certificate_expiry_timestamp_seconds", "Unix time when the certificate in use expires."),
		renewals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "certificate_renewals_total",
			Help: "Certificate renewals by result (success, error).",
		}, []string{"result"}),
		statusReady:    gauge("status_ready", "1 when the cluster status informers are synced (0 also when the status is disabled)."),
		statusLastSent: gauge("status_last_sent_timestamp_seconds", "Unix time of the last cluster status accepted by the backend."),
	}
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: "info", Help: "Agent version, always 1.",
		ConstLabels: prometheus.Labels{"version": version},
	})
	info.Set(1)
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		info, m.backendRequests, m.heartbeatLastSuccess, m.heartbeatOnly, m.actions,
		m.actionDuration, m.actionInProgress, m.certExpiry, m.renewals, m.statusReady, m.statusLastSent,
	)
	for _, t := range protocol.ActionTypes {
		for _, o := range outcomes {
			m.actions.WithLabelValues(t, o)
		}
	}
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// BackendRequest counts a backend call; code 0 means no HTTP response.
func (m *Metrics) BackendRequest(operation string, code int) {
	if m == nil {
		return
	}
	label := "error"
	if code != 0 {
		label = strconv.Itoa(code)
	}
	m.backendRequests.WithLabelValues(operation, label).Inc()
}

func (m *Metrics) HeartbeatSucceeded(at time.Time) {
	if m == nil {
		return
	}
	m.heartbeatLastSuccess.Set(float64(at.Unix()))
}

func (m *Metrics) SetHeartbeatOnly() {
	if m == nil {
		return
	}
	m.heartbeatOnly.Set(1)
}

func (m *Metrics) ActionOutcome(actionType, outcome string) {
	if m == nil {
		return
	}
	m.actions.WithLabelValues(typeLabel(actionType), outcome).Inc()
}

func (m *Metrics) ActionRunning(running bool) {
	if m == nil {
		return
	}
	m.actionInProgress.Set(boolValue(running))
}

func (m *Metrics) ObserveActionDuration(actionType string, d time.Duration) {
	if m == nil {
		return
	}
	m.actionDuration.WithLabelValues(typeLabel(actionType)).Observe(d.Seconds())
}

func (m *Metrics) CertificateInUse(notAfter time.Time) {
	if m == nil {
		return
	}
	m.certExpiry.Set(float64(notAfter.Unix()))
}

func (m *Metrics) CertificateRenewal(err error) {
	if m == nil {
		return
	}
	result := "success"
	if err != nil {
		result = "error"
	}
	m.renewals.WithLabelValues(result).Inc()
}

func (m *Metrics) StatusReady(ready bool) {
	if m == nil {
		return
	}
	m.statusReady.Set(boolValue(ready))
}

func (m *Metrics) StatusSent(at time.Time) {
	if m == nil {
		return
	}
	m.statusLastSent.Set(float64(at.Unix()))
}

// typeLabel keeps the type label to the known action types.
func typeLabel(t string) string {
	if slices.Contains(protocol.ActionTypes, t) {
		return t
	}
	return "unknown"
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
