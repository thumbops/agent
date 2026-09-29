# thumbops-agent Helm chart

Installs the ThumbOps agent: it receives actions approved in the ThumbOps
app and runs them in the cluster within the local policy.

```
helm install thumbops-agent oci://ghcr.io/thumbops/charts/thumbops-agent \
  --version <version> --namespace thumbops --create-namespace \
  --set bootstrap.token=<SINGLE-USE TOKEN>
```

The bootstrap token comes from the app when the cluster is created; it is
single-use and valid for one hour. It is needed only for the first
registration. To keep it out of the Helm release, create the Secret yourself
and pass `--set bootstrap.existingSecret=<name>` instead.

## Values

| Value | Default | Description |
| --- | --- | --- |
| `image.repository` | `ghcr.io/thumbops/agent` | Agent image |
| `image.tag` | chart `appVersion` | |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `backend.url` | `https://agent.thumbops.mobiletechnologies.cloud` | Backend URL |
| `backend.caBundle` | `""` | PEM of the backend CA; empty = system CAs |
| `bootstrap.token` | `""` | Single-use token; the chart creates the `thumbops-bootstrap` Secret |
| `bootstrap.existingSecret` | `""` | Existing Secret with the token (not together with `bootstrap.token`) |
| `bootstrap.key` | `token` | Key of the token in `existingSecret` |
| `identity.secretName` | `thumbops-agent-identity` | Secret with the agent's key and certificate |
| `policy` | see `values.yaml` | Local policy; the release namespace is always denied |
| `rbac.actions` | `true` | Permissions for the five actions |
| `rbac.status` | `true` | Read-only permissions for the cluster status; `false` turns the status off |
| `heartbeatInterval`, `statusInterval` | `""` (60s) | |
| `logLevel` | `info` | `debug`, `info`, `warn`, `error` |
| `extraArgs` | `[]` | Extra agent flags |
| `http.port` | `9090` | `/healthz`, `/readyz`, `/metrics` |
| `livenessProbe`, `readinessProbe` | see `values.yaml` | |
| `metrics.service.enabled` | `false` | ClusterIP Service for `/metrics` |
| `metrics.serviceMonitor.enabled` | `false` | ServiceMonitor (Prometheus Operator); implies the Service |
| `metrics.serviceMonitor.interval`, `.labels` | `30s`, `{}` | |
| `podAnnotations` | `prometheus.io/*` | |
| `podLabels`, `resources`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName` | | Usual pod settings |
| `podSecurityContext`, `securityContext` | non-root, read-only root filesystem, no capabilities | |

Unknown values are rejected (`values.schema.json`), as unknown policy fields
are rejected by the agent.

## The identity Secret

The agent keeps its private key and certificate in the
`identity.secretName` Secret. The chart creates it empty with
`helm.sh/resource-policy: keep`:

- `helm upgrade` never touches what the agent wrote;
- `helm uninstall` leaves the Secret in place, and a later `helm install`
  **with the same release name in the same namespace** adopts it: the agent
  keeps its identity and does not register again;
- with a different release name Helm refuses to install ("exists and cannot
  be imported"): reuse the release name, or delete the Secret and register
  again with a new bootstrap token.

To register again (cluster revoked, certificate expired), set a new token; the
upgrade restarts the agent by itself (a change of the token or of
`backend.caBundle` changes a pod annotation):

```
helm upgrade thumbops-agent <chart> -n thumbops --reset-then-reuse-values --set bootstrap.token=<NEW TOKEN>
```

With `bootstrap.existingSecret` put the new token in that Secret and restart
the agent instead of setting `bootstrap.token`: Helm cannot see the content
of your Secret, so rotating the token there does not restart the agent.

```
kubectl -n thumbops rollout restart deploy/thumbops-agent
```

Argo CD does not honour `helm.sh/resource-policy: keep`. If you deploy the
chart with Argo CD, annotate the identity Secret with
`argocd.argoproj.io/sync-options: Delete=false`, or exclude it from pruning,
so a sync never deletes the agent's identity.

### Moving from deploy/agent.yaml to Helm

The identity Secret created by `kubectl apply -f deploy/agent.yaml` has no
Helm ownership metadata. Delete the other manifest resources, then either
delete the Secret too (the agent registers again with a new token), or let
Helm adopt it. The `thumbops-bootstrap` Secret you created by hand must go
too, or be reused with `--set bootstrap.existingSecret=thumbops-bootstrap`;
otherwise `helm install --set bootstrap.token=...` fails with "exists and
cannot be imported".

```
kubectl -n thumbops label secret thumbops-agent-identity app.kubernetes.io/managed-by=Helm
kubectl -n thumbops annotate secret thumbops-agent-identity \
  meta.helm.sh/release-name=thumbops-agent meta.helm.sh/release-namespace=thumbops
```
