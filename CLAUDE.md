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

Everything written into the repository is in English: code comments, docs,
log and error messages, action result messages, tests, commit messages and PR
descriptions.

## Commands

```
go test -race ./...     # all tests (fake API server and mock backend)
go vet ./...
gofmt -l .              # must print nothing
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
| `internal/kube` | Minimal Kubernetes REST client (standard library only) |
| `internal/actions` | Runs rollout-restart, scale, cordon, uncordon, drain |
| `internal/policy` | Cluster local policy (JSON from a ConfigMap) |
| `internal/backend` | Backend HTTP client |
| `internal/identity` | Ed25519 key, CSR, certificate on disk |
| `internal/enroll` | Registration and certificate renewal |
| `internal/agent` | Main loop: heartbeat, poll, claim, execution, result |
| `internal/kubefake` | Fake API server for tests |
| `internal/mockbackend` | Mock backend for tests and development |

## Why no client-go (for now)

The prototype was written in an environment without access to
proxy.golang.org, so it uses only the standard library. This is not a design
choice to defend: client-go should be introduced for the cluster status
(informers), and migrating the actions too is fine if it simplifies the code.
In that case the `kubefake` tests should be replaced or complemented by tests
with the fake clientset or envtest.

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
  node stays cordoned and the result lists the remaining pods.
- **Results are never lost.** Sending the result is retried with backoff
  until the backend confirms (except `400`/`409`/`410`).
- **Certificate change = new connections.** After registration and renewal
  `backend.Client.ResetConnections()` must be called (`enroll.Activate` does
  it): the client certificate is presented only at the handshake, and long
  polling keeps the connection always active. This bug has been hit once.
- **`401` stops the agent** (`ErrUnauthorized`); `426` leaves it in
  heartbeat-only mode. An expired or rejected certificate produces no `401`
  but a TLS alert at the handshake: `backend.Unauthorized` treats both cases
  the same way, otherwise the agent would retry forever. This was also found
  on kind.

## Next steps

Done: test on kind with the mock backend, the ServiceAccount's real RBAC and
registration with mTLS.

1. Cluster status: `PUT /v1/agent/status` every 60 s and on `status_requested`, with informers; format and limits in `../spec/protocol/protocol.md` ("Cluster status" section). Honor the policy's `status.exclude_namespaces`. Separate read-only ClusterRole.
2. Key and certificate in a Secret managed by the agent instead of a PVC. Decide at the same time whether the agent should re-register by itself when it stops with `ErrUnauthorized` and finds a new bootstrap token (today the state must be wiped by hand).
3. Helm chart (replaces `deploy/agent.yaml`), liveness/readiness probes, Prometheus metrics.
4. CI (GitHub Actions): `go test -race`, `go vet`, `gofmt`, end-to-end tests on kind; image build.
5. Add the `LICENSE` file (Apache 2.0).
