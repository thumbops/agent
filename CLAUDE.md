# ThumbOps agent

Go agent that runs in every cluster: it receives approved actions from the
backend and runs them within the cluster's local policy. Public repository
`thumbops/agent` (Apache 2.0).

The agent–backend protocol is in `../spec/protocol/protocol.md`
(repository `thumbops/spec`), the single source of truth: if you change it,
change it there. The general project context is in `../platform/CLAUDE.md`, if
present on disk (private repository). Do not copy internal material (business
model, plans, brand) into this repository.

Never run the agent or kubectl experiments against real clusters without an
explicit request from the user: use kind or the fake API.

Never touch the user's `~/.kube/config` or current kubectl context: they use
kubectl for other work at the same time. Every kind, kubectl or helm command
for tests runs with `KUBECONFIG` exported to a dedicated file (in this
repository `.kind-kubeconfig`, git-ignored; `test/e2e/run.sh` refuses to run
without it outside CI). Never run `kubectl config use-context`.

Everything written into the repository is in English: code comments, docs,
log and error messages, action result messages, tests, commit messages and PR
descriptions.

## Commands

```
go test -race ./...     # all tests (fake API server and mock backend)
go vet ./...
gofmt -l .              # must print nothing
test/chart/check.sh     # chart lint, render, kubeconform, manifest up to date
hack/gen-manifest.sh    # regenerate deploy/agent.yaml after a chart change
test/e2e/run.sh         # end-to-end on kind (cluster from test/e2e/kind.yaml, KUBECONFIG=.kind-kubeconfig)
go run ./cmd/mock-backend -addr 127.0.0.1:8080
go run ./cmd/thumbops-agent --dev-insecure --backend-url http://127.0.0.1:8080 --kube-api http://127.0.0.1:8001 --policy-file /tmp/policy.json
```

To try it on kind see `README.md` (including registration with mTLS against
the mock backend in HTTPS mode). Never on a production cluster.

## Layout

| Package | Role |
| --- | --- |
| `cmd/thumbops-agent` | Flags, startup, initial registration |
| `cmd/mock-backend` | Mock backend for development (HTTP, or HTTPS with mTLS) |
| `internal/protocol` | Protocol v1 message types |
| `internal/kube` | Minimal Kubernetes REST client (standard library only), used by the actions |
| `internal/actions` | Runs rollout-restart, scale, cordon, uncordon, drain |
| `internal/policy` | Cluster local policy (JSON from a ConfigMap) |
| `internal/backend` | Backend HTTP client |
| `internal/identity` | Ed25519 key, CSR, certificate; `SecretStore` (in the cluster) and `FileStore` (`--state-dir`, development) |
| `internal/enroll` | `Enroller`: registration, new registration with a new token, renewal |
| `internal/agent` | Main loop: heartbeat, poll, claim, execution, result, status sending, drain progress and resume |
| `internal/status` | Cluster status for the dashboard: client-go informers and the summary |
| `internal/metrics` | Prometheus metrics on a private registry; nil-safe |
| `internal/health` | Liveness and readiness state, HTTP handler for /healthz, /readyz, /metrics |
| `internal/kubefake` | Fake API server for tests |
| `internal/mockbackend` | Mock backend for tests and development |
| `charts/thumbops-agent` | Helm chart: the source of the installation |
| `hack` | `gen-manifest.sh`: deploy/agent.yaml from the chart |
| `test/chart` | Chart checks |
| `test/e2e` | End-to-end tests on kind: the agent installed with the Helm chart against the mock backend with mTLS |

## client-go and the REST client

The cluster status uses client-go informers (`internal/status`), tested with
the fake clientset. The actions still use the small REST client in
`internal/kube`, written when the prototype had no access to
proxy.golang.org; migrating them to client-go is fine if it simplifies the
code, replacing or complementing the `kubefake` tests with the fake clientset
or envtest. client-go logs through klog, which `main` routes to the same JSON
`slog` logger.

## Invariants not to break

Each one is covered by tests; if a change makes them fail, stop and understand why.

- **Claim before running.** An action runs only after a successful claim
  (`200`); `409`/`410` = discard. Expired actions are not even claimed.
- **Deadlines use the backend clock.** The agent corrects clock skew with the
  heartbeat's `server_time`.
- **Idempotency in the same patch.** The `last-action-id` and
  `last-action-result` annotations must be written in the same merge patch
  that applies the change. If the ID is already there, resend the saved result.
- **Policy default deny.** Without a policy file everything is rejected;
  unknown fields in the policy are a startup error. Control plane nodes are
  protected unless `allow_control_plane_nodes`.
- **Safe drain.** Stop *before* cordoning if there are pods without a
  controller or with `emptyDir` (without consent); use the Eviction API
  (honors PDBs); skip DaemonSet, static and terminated pods; on timeout the
  node stays cordoned and the result lists the remaining pods; never evict the
  agent's own pod.
- **Drain resume.** The `drain-in-progress` annotation is written with the
  cordon and removed with the result; it is kept on shutdown and on `401`, so
  the drain is resumed; it is removed on `409`/`410` to progress (which stop
  the drain without a result), on a resume the policy rejects and on an
  explicit `uncordon`. A resume makes no claim; it changes nothing until its
  first progress gets a `200` (`404`, `409`, `410` or repeated failures
  abandon it and leave the node as it is); it is checked against the current
  local policy (rejected: `rejected` result, annotation removed); then it
  re-asserts the cordon and uses the time left from `started_at`.
- **Results are never lost.** Sending the result is retried with backoff
  until the backend confirms (except `400`/`409`/`410`); a 401 or a rejected
  certificate is the other exception: the agent stops and a new registration
  is needed.
- **Certificate change = new connections.** After registration and renewal
  `backend.Client.ResetConnections()` must be called (`Enroller.activate`
  does it): the client certificate is presented only at the handshake, and long
  polling keeps the connection always active. This bug has been hit once.
- **`401` stops the agent** (`ErrUnauthorized`); `426` leaves it in
  heartbeat-only mode. An expired or rejected certificate produces no `401`
  but a TLS alert at the handshake: `backend.Unauthorized` treats both cases
  the same way, otherwise the agent would retry forever. This was also found
  on kind.
- **The identity is replaced only after success.** A new registration
  (bootstrap token whose hash differs from the saved one) keeps the old key
  and certificate until the new ones are saved; a renewal whose save fails
  keeps the old certificate in use. Key and certificate are always saved
  together. The token is never saved, only its SHA-256.
- **The agent can only get and update its identity Secret.** The chart
  creates it empty (and so does the generated deploy/agent.yaml); no `create`
  and no access to other Secrets (checked by the end-to-end tests).
- **`deploy/agent.yaml` is generated.** Change the chart and run
  `hack/gen-manifest.sh`; `test/chart/check.sh` fails if the committed
  manifest differs. The identity Secret stays empty in the chart with
  `helm.sh/resource-policy: keep`, and the agent never gets `create` on
  Secrets.
- **The status never blocks the actions.** Without the
  `thumbops-agent-status` RBAC the informers never sync, no status is sent,
  a warning is logged after a minute, and actions keep working; the status
  resumes by itself once the RBAC is back.
- **Excluded namespaces are never listed.** Pods and deployments in the
  policy's `status.exclude_namespaces` never appear in the status (their
  requests still count in the aggregates, which carry no names).
- **Liveness never depends on the backend.** `/healthz` fails only when the
  heartbeat loop stops making attempts; a failed heartbeat still counts.
  It never fails before readiness (startup, registration retries): a restart
  there could lose the single-use bootstrap token.
  Readiness never goes back to `503`. Otherwise a backend outage would make
  Kubernetes restart the agent in a loop, interrupting actions.

## Next steps

Done: test on kind with the mock backend, the ServiceAccount's real RBAC and
registration with mTLS; cluster status with informers, checked on kind
against the real backend; `LICENSE`; identity in a Secret with a new
registration on a new bootstrap token; CI (GitHub Actions) with lint, tests,
end-to-end tests on kind and the multi-arch image on ghcr.io; health probes
and Prometheus metrics; Helm chart (OCI on ghcr.io), deploy/agent.yaml
generated from it.

1. Status: events and real usage from metrics-server, when the dashboard needs them (out of the MVP).
