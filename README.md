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

- **Go standard library only.** The agent talks directly to the Kubernetes
  REST API (merge patch, Eviction API, SelfSubjectAccessReview). No external
  dependencies: the code is small and easy to review. The dashboard will need
  client-go (informers), which can be introduced without changing the
  protocol or the action logic.
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
- **A rejected certificate counts as a `401`.** An expired, revoked or
  unknown-CA certificate is rejected during the TLS handshake, with no HTTP
  response: the agent stops as it does for a `401` instead of retrying
  forever.

## Layout

```
cmd/thumbops-agent   agent command
cmd/mock-backend     mock backend for local development (HTTP, or HTTPS with mTLS)
internal/protocol    protocol messages
internal/kube        minimal Kubernetes REST client
internal/actions     action execution
internal/policy      cluster local policy
internal/backend     backend client
internal/identity    Ed25519 key, CSR, certificate
internal/enroll      registration and renewal
internal/agent       main loop
internal/kubefake    fake API server (tests)
internal/mockbackend mock backend (tests and development)
deploy/agent.yaml    manifest: RBAC, policy, Deployment
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
mock backend: after it restarts, the agent must register again with an empty
`--state-dir`.

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

- Cluster status for the dashboard (`PUT /v1/agent/status`, with informers).
- Key and certificate in a Secret instead of a volume.
- Prometheus metrics for the agent and liveness probes.
- Tests on real clusters (kind in CI) besides the ones with the fake API.
- Intermediate results for long drains (an open question in the protocol).
