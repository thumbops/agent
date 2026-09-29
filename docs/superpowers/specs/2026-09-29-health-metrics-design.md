# Health probes and Prometheus metrics for the agent

Date: 2026-09-29. Status: approved design, to be implemented.

## Goal

Give Kubernetes and operators a view of the agent's health without touching
the agent–backend protocol:

- a liveness probe that restarts an agent whose main work is stuck, and
  never restarts it because the backend or the network is down;
- a readiness probe that tells when startup is complete;
- Prometheus metrics for backend connectivity, actions, identity and cluster
  status, for dashboards and alerts.

Out of scope: the Helm chart (next step: Service, ServiceMonitor and
values will be added there), authentication on the metrics endpoint,
NetworkPolicy.

## HTTP server

- New flag `--http-addr`, default `:9090`; empty disables the server.
- Serves `/healthz`, `/readyz` and `/metrics` over plain HTTP, without
  authentication. The metrics carry no resource names (no namespaces, nodes
  or deployments) and nothing sensitive.
- It is separate from the backend connection, which stays outbound only.
- It starts **before** registration, so the liveness probe answers while the
  agent registers; readiness answers `503` until startup is complete.
- It shuts down with the agent's context.

## Liveness: `/healthz`

- `503` if the heartbeat loop has not completed an attempt for more than
  `max(5 × heartbeat-interval, 5 min)`. Before the first attempt, the
  process start time counts as the last attempt.
- An attempt counts whatever its outcome: a heartbeat that fails because the
  backend is unreachable still proves the loop is alive. Every backend and
  API server call already has a timeout, so a hung call cannot keep the loop
  waiting forever without being noticed.
- The heartbeat runs in its own goroutine, so a long action (drain) does not
  delay it.
- Otherwise `200`.

## Readiness: `/readyz`

- `503` until policy and identity are loaded and `Run` has started; `200`
  afterwards.
- It never goes back to `503`: backend connectivity is reported by the
  metrics, and a `401` stops the process anyway. `helm install --wait` and
  `kubectl rollout status` therefore never hang because of a backend
  problem.

## Metrics

Library: `github.com/prometheus/client_golang`, with a private registry
(no global registry). It also provides the standard `go_*` and `process_*`
metrics. All agent metrics use the `thumbops_agent_` prefix.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `thumbops_agent_info` | gauge | `version` | Always 1 |
| `thumbops_agent_backend_requests_total` | counter | `operation`, `code` | Backend calls. `operation`: `register`, `renew`, `heartbeat`, `poll`, `claim`, `result`, `status`. `code`: HTTP status, or `error` for network errors and TLS alerts |
| `thumbops_agent_heartbeat_last_success_timestamp_seconds` | gauge | | Unix time of the last successful heartbeat: alert on "backend unreachable" |
| `thumbops_agent_heartbeat_only` | gauge | | 1 after a `426`: the agent must be upgraded |
| `thumbops_agent_actions_total` | counter | `type`, `outcome` | Actions received. `outcome`: `succeeded`, `failed`, `rejected` (local policy), `expired` (not claimed), `discarded` (claim `409`/`410`). An unknown type is counted as `unknown` |
| `thumbops_agent_action_duration_seconds` | histogram | `type` | Execution time of claimed actions; buckets from 1 s to 30 min |
| `thumbops_agent_action_in_progress` | gauge | | 1 while an action runs |
| `thumbops_agent_certificate_expiry_timestamp_seconds` | gauge | | Unix time when the certificate in use expires |
| `thumbops_agent_certificate_renewals_total` | counter | `result` | Renewals: `success` or `error` |
| `thumbops_agent_status_ready` | gauge | | 1 when the cluster status informers are synced (0 e.g. without the `thumbops-agent-status` RBAC) |
| `thumbops_agent_status_last_sent_timestamp_seconds` | gauge | | Unix time of the last cluster status accepted by the backend |

A claim error other than `409`/`410` is not counted in `actions_total`: the
action will be offered again. It appears in `backend_requests_total`.

## Code layout

- `internal/metrics`: `Metrics` struct holding the collectors, `New(version)`
  registering them on a private registry, and the registry for `/metrics`.
  Methods are nil-safe: a nil `*Metrics` records nothing, so existing tests
  and callers do not change.
- `internal/health`: liveness and readiness state (`HeartbeatAttempted`,
  `MarkReady`, injectable clock) and the HTTP handler serving the three
  paths.
- `backend.Options`, `agent.Config` and `enroll.Enroller` receive an optional
  `*metrics.Metrics`; `agent.Config` also receives the health state.
- `cmd/thumbops-agent`: flag, server start before registration.

## Manifest (`deploy/agent.yaml`)

- Container port `http` 9090.
- `livenessProbe`: `GET /healthz`, `periodSeconds: 30`, `failureThreshold: 3`
  (the real threshold is in the agent; the probe only asks it).
- `readinessProbe`: `GET /readyz`, `periodSeconds: 5`.
- Pod annotations `prometheus.io/scrape`, `prometheus.io/port`,
  `prometheus.io/path`. Service and ServiceMonitor come with the Helm chart.

## Tests

Unit tests:

- `internal/health`, with a fake clock: liveness ok at start, `503` past the
  threshold with no attempt, ok again after an attempt (even a failed one);
  readiness `503` before `MarkReady` and `200` after; the handler serves the
  three paths.
- `internal/metrics`: every metric registered once with the expected names
  and labels; `/metrics` exposes the names in the table above (they are a
  contract for whoever writes alerts).
- `agent` and `backend` tests with a `*metrics.Metrics`: a succeeded action,
  a policy rejection, an expired action and a claim `409` each count under
  the right `outcome`; a backend `503` is counted with `code="503"` and a
  rejected certificate with `code="error"`; the heartbeat timestamp is
  updated after a successful heartbeat; `heartbeat_only` is 1 after a `426`.
- `status`: `status_ready` is 0 before the informers sync and 1 after.

End-to-end (`test/e2e/run.sh`):

- The probes are active, so `rollout status` waits for readiness.
- Through a port-forward to the agent, `/metrics` shows the expected
  `actions_total` counts for `succeeded` and `rejected`, a recent
  `heartbeat_last_success_timestamp_seconds`, at least one successful
  certificate renewal and `status_ready` 1.
- The final agent pod has `restartCount` 0: the liveness probe never fires by
  mistake, drain included.

## Documentation

- README: "Health and metrics" section with endpoints, thresholds and the
  metric table.
- CLAUDE.md: layout, and a new invariant: the liveness probe never depends
  on the backend.
- The protocol spec does not change: this is local observability.
