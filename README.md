# ThumbOps agent (prototype)

The agent runs in every Kubernetes cluster managed by ThumbOps. It connects
outbound to the backend, receives the actions approved in the app and runs
them, within the limits of the cluster's local policy.

The prototype implements protocol v1, described in `protocol/protocol.md` in
the `thumbops/spec` repository: registration with a bootstrap token and mTLS,
heartbeat, long polling, claim, result reporting and certificate renewal.
There are five actions: `rollout-restart`, `scale`, `cordon`, `uncordon`,
`drain`.

## Prototype choices

- **Actions on the Kubernetes REST API, status with client-go.** The actions
  talk directly to the REST API (merge patch, Eviction API,
  SelfSubjectAccessReview) through a small client written with the standard
  library. The cluster status for the dashboard uses client-go informers, so
  building a summary never queries the API server.
- **Cluster status** (`PUT /v1/agent/status`): every 60 s
  (`--status-interval`) and right away when the backend asks for it
  (`status_requested`). Nodes (all up to 100, then only the ones with
  problems), requested versus allocatable CPU and memory, up to 20 unhealthy
  pods (crash loops, images that cannot start, failed pods, pods pending for
  more than 2 minutes) and 20 degraded deployments, sorted by severity.
  Namespaces in the policy's `status.exclude_namespaces` are never listed.
  It needs the read-only `thumbops-agent-status` ClusterRole; without it no
  status is sent and the actions keep working. `--status=false` turns it off.
- **Idempotency through annotations.** Every modified resource gets
  `thumbops.mobiletechnologies.cloud/last-action-id` and
  `.../last-action-result`, in the same patch as the change. If the agent
  restarts before sending the result, on the next delivery it resends the
  result without repeating the action.
- **Scale in a single deployment patch**, so replicas and annotations change
  together.
- **Drain like `kubectl drain`**: it skips DaemonSet, static and terminated
  pods; it stops *before* cordoning if it finds pods without a controller or
  with `emptyDir` volumes (the latter only with explicit consent); it uses the
  Eviction API, so it honors PodDisruptionBudgets, and retries blocked pods in
  rounds until the timeout.
- **Local policy in JSON** (ConfigMap). Without a file the policy denies
  everything. Besides action types, namespaces and a replica maximum, it
  protects control plane nodes by default.
- **Certificate rotation without a restart.** The client certificate is
  presented only at the TLS handshake: after registration and renewal the
  agent reopens its connections to the backend. (A test covers it: without
  this step the agent was rejected right after registering.)
- **Identity in a Secret.** Key, certificate and the SHA-256 of the
  bootstrap token used live in the `thumbops-agent-identity` Secret, created
  empty by the manifest: the agent can only read and update that Secret. Key
  and certificate are saved together in one update. `--state-dir` keeps them
  in a directory instead, for development outside the cluster.
- **A new bootstrap token means "register again".** At startup the agent
  registers when it has no identity, or when the mounted token differs from
  the one used for the saved identity. After a `401` (cluster revoked,
  certificate expired) the fix is a new token from the app in the
  `thumbops-bootstrap` Secret and a restart. The saved identity is replaced
  only after the new registration succeeds; the same token never triggers a
  second one.
- **A rejected certificate counts as a `401`.** An expired, revoked or
  unknown-CA certificate is rejected during the TLS handshake, with no HTTP
  response: the agent stops as it does for a `401` instead of retrying
  forever.

## Health and metrics

`--http-addr` (default `:9090`, empty = disabled) serves, without
authentication:

- `/healthz`: `503` when the heartbeat loop has not completed an attempt for
  `max(5 × --heartbeat-interval, 5 min)`. A failed heartbeat still counts:
  the probe never restarts the agent because the backend or the network is
  down.
- `/readyz`: `200` once policy and identity are loaded and the main loop has
  started; it never goes back to `503`.
- `/metrics`: Prometheus metrics, prefix `thumbops_agent_`, with no resource
  names in the labels, plus the standard `go_*` and `process_*` metrics.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `info` | `version` | Always 1 |
| `backend_requests_total` | `operation`, `code` | Backend calls; `code` is the HTTP status or `error` |
| `heartbeat_last_success_timestamp_seconds` | | Alert here for "backend unreachable" |
| `heartbeat_only` | | 1 after a `426`: upgrade the agent |
| `actions_total` | `type`, `outcome` | `succeeded`, `failed`, `rejected`, `expired`, `discarded` |
| `action_duration_seconds` | `type` | Execution time (histogram) |
| `action_in_progress` | | 1 while an action runs |
| `certificate_expiry_timestamp_seconds` | | Expiry of the certificate in use |
| `certificate_renewals_total` | `result` | `success` or `error` |
| `status_ready` | | 0 until the status informers sync (e.g. missing RBAC) or with `--status=false` |
| `status_last_sent_timestamp_seconds` | | Last cluster status accepted by the backend |

## Layout

```
cmd/thumbops-agent   agent command
cmd/mock-backend     mock backend for local development (HTTP, or HTTPS with mTLS)
internal/protocol    protocol messages
internal/kube        minimal Kubernetes REST client
internal/actions     action execution
internal/policy      cluster local policy
internal/backend     backend client
internal/identity    Ed25519 key, CSR, certificate; stored in a Secret or a directory
internal/enroll      registration and renewal
internal/agent       main loop
internal/status      cluster status: informers and summary
internal/metrics     Prometheus metrics (private registry)
internal/health      liveness and readiness probes
internal/kubefake    fake API server (tests)
internal/mockbackend mock backend (tests and development)
deploy/agent.yaml    manifest: RBAC (actions and status), policy, Deployment
test/e2e             end-to-end tests on kind
```

## Tests

```
go test -race ./...
```

The tests use an in-memory fake API server and mock backend. Among other
things they cover: idempotency, drain with PDBs, emptyDir and pods without a
controller, drain timeout, policy rejections, control plane nodes, expired
actions, already claimed actions, clock skew, result retries, 401, 426,
backend unavailability, registration, single-use tokens, mTLS, certificate
renewal and rejected certificates.

### End-to-end tests on kind

`test/e2e/run.sh` installs the agent with `deploy/agent.yaml` (the real RBAC
and security context, only the image, backend URL and intervals changed) next
to the mock backend in HTTPS with mTLS. It then checks registration, scale,
rollout-restart, cordon, drain, uncordon, two policy rejections, a
certificate renewal, the agent's permissions on Secrets, the identity kept
across a restart and a new registration with a new bootstrap token. The script refuses to run unless the kubectl context is
`kind-thumbops-e2e`.

```
kind create cluster --name thumbops-e2e --config test/e2e/kind.yaml
test/e2e/run.sh
kind delete cluster --name thumbops-e2e
```

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, `go test -race` and
the end-to-end tests on every pull request, and builds the multi-arch image.
Pushes to `main` publish `ghcr.io/thumbops/agent:edge`, and `v*` tags publish
the version.

## Trying it on a kind cluster

You need a test cluster: **never use a production cluster.**

```
kind create cluster --name thumbops
kubectl create deployment web --image=nginx --replicas=2

# 1. mock backend (HTTP, no authentication)
go run ./cmd/mock-backend -addr 127.0.0.1:8080

# 2. agent outside the cluster, in development mode
kubectl proxy --port 8001 &
echo '{"allowed_actions":["rollout-restart","scale","cordon","uncordon","drain"],"denied_namespaces":["kube-system"],"max_replicas":10}' > /tmp/policy.json
go run ./cmd/thumbops-agent --dev-insecure \
  --backend-url http://127.0.0.1:8080 \
  --kube-api http://127.0.0.1:8001 \
  --policy-file /tmp/policy.json

# 3. actions, as if they had been approved in the app
curl -X POST 127.0.0.1:8080/debug/actions \
  -d '{"type":"scale","params":{"namespace":"default","deployment":"web","replicas":4}}'
curl -X POST 127.0.0.1:8080/debug/actions \
  -d '{"type":"drain","params":{"node":"thumbops-control-plane","timeout_seconds":60}}'
curl 127.0.0.1:8080/debug/actions   # state and results
```

The drain of kind's only node is rejected because it is a control plane node:
that is the expected behavior. To try a real drain, create the cluster with
worker nodes. With `kubectl proxy` the agent uses your credentials; to try the
ServiceAccount's real permissions:

```
kubectl apply -f deploy/agent.yaml
TOKEN=$(kubectl -n thumbops create token thumbops-agent)
kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > /tmp/kind-ca.crt
go run ./cmd/thumbops-agent --dev-insecure --backend-url http://127.0.0.1:8080 \
  --kube-api "$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')" \
  --kube-token "$TOKEN" --kube-ca-file /tmp/kind-ca.crt --policy-file /tmp/policy.json
```

### Registration, mTLS and renewal

With `-tls-cert` and `-tls-key` the mock backend serves HTTPS like the real
one: single-use bootstrap token, mTLS required on `/v1/agent/*`, certificate
renewal. With a short `-cert-lifetime` the renewal happens after a few
seconds (when less than a third of the validity is left, checked at every
heartbeat). The client certificate CA is regenerated at every start of the
mock backend: after it restarts, the agent must register again with a new
token (or an empty `--state-dir`). `POST /debug/bootstrap-tokens` accepts one
more single-use token, to try a new registration without restarting it.

```
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 7 -subj /CN=mock-backend \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" -keyout /tmp/server.key -out /tmp/server.crt
go run ./cmd/mock-backend -addr 127.0.0.1:8443 -tls-cert /tmp/server.crt -tls-key /tmp/server.key \
  -bootstrap-token test-secret -cert-lifetime 90s
echo test-secret > /tmp/bootstrap-token
go run ./cmd/thumbops-agent --backend-url https://127.0.0.1:8443 --backend-ca-file /tmp/server.crt \
  --bootstrap-token-file /tmp/bootstrap-token --state-dir /tmp/thumbops-state --heartbeat-interval 10s \
  --kube-api http://127.0.0.1:8001 --policy-file /tmp/policy.json
curl --cacert /tmp/server.crt https://127.0.0.1:8443/debug/actions
```

## Missing for production

- Intermediate results for long drains (an open question in the protocol).

## License

Apache 2.0, see [LICENSE](LICENSE).
