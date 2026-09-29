# Helm chart for the agent

Date: 2026-09-29. Status: approved design, to be implemented.

## Goal

Install the agent with Helm, as the protocol expects ("the agent is installed
(Helm chart)"), without losing what the static manifest guarantees today:
minimal RBAC, restrictive security context, local policy, probes, and an
identity Secret the agent can only read and update.

- The chart is the single source of the installation. `deploy/agent.yaml` is
  generated from it and kept for users without Helm.
- The identity Secret survives `helm upgrade` and `helm uninstall`.
- The chart is published as an OCI artifact on ghcr.io next to the image.
- The end-to-end tests on kind install the agent with the chart.

Out of scope: a Helm test hook, multiple agents per cluster, NetworkPolicy,
an automated migration from the static manifest to Helm. The chart README
documents the manual one: an identity Secret created by `kubectl apply` has
no Helm ownership metadata, so `helm install` refuses it until it is either
deleted (and the agent registers again with a new token) or labeled
`app.kubernetes.io/managed-by=Helm` and annotated
`meta.helm.sh/release-name` / `meta.helm.sh/release-namespace` for adoption.

## Layout

- `charts/thumbops-agent/`: `Chart.yaml`, `values.yaml`,
  `values.schema.json`, `templates/`, `README.md` (values reference).
- `hack/gen-manifest.sh`: regenerates `deploy/agent.yaml`.
- `test/chart/check.sh`: chart checks (lint, render, kubeconform, assertions,
  schema, manifest up to date).
- `test/e2e/values.yaml`: values for the end-to-end tests;
  `test/e2e/agent/kustomization.yaml` is removed.

`Chart.yaml`: `apiVersion: v2`, `name: thumbops-agent`, `type: application`,
`version` and `appVersion` equal (the repository keeps `0.1.0`; the tag
pipeline overrides both). `kubeVersion` is not constrained.

## Names and namespace

- Resources go into the release namespace (`helm install -n thumbops
  --create-namespace`); nothing hardcodes `thumbops`.
- Resource names come from a `fullname` helper: the release name, or
  `nameOverride`/`fullnameOverride`. With the release name `thumbops-agent`
  the names are the ones of today's manifest (ServiceAccount, Deployment,
  ClusterRoles `thumbops-agent-actions` / `thumbops-agent-status`, their
  bindings, Role and RoleBinding `thumbops-agent-identity`), plus the new
  ClusterRole `thumbops-agent-base` (see "RBAC").
- Two names are fixed by value, not by release: the identity Secret
  (`identity.secretName`, default `thumbops-agent-identity`) and the
  bootstrap Secret (`thumbops-bootstrap` unless `bootstrap.existingSecret`).
  The policy ConfigMap is `<fullname>-policy` (today's `thumbops-policy`
  becomes `thumbops-agent-policy` with the default release name).
- Standard labels on every resource: `app.kubernetes.io/name`,
  `app.kubernetes.io/instance`, `app.kubernetes.io/version`,
  `app.kubernetes.io/managed-by`, `helm.sh/chart`. The Deployment selector
  uses only `app.kubernetes.io/name` and `app.kubernetes.io/instance`.

## Values

| Value | Default | Effect |
| --- | --- | --- |
| `image.repository` | `ghcr.io/thumbops/agent` | |
| `image.tag` | `""` (= `appVersion`) | |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `nameOverride`, `fullnameOverride` | `""` | |
| `backend.url` | `https://agent.thumbops.mobiletechnologies.cloud` | `--backend-url` |
| `backend.caBundle` | `""` | PEM; if set, ConfigMap `<fullname>-backend-ca` mounted and `--backend-ca-file` passed |
| `bootstrap.token` | `""` | If set, the chart creates Secret `thumbops-bootstrap` with key `token` |
| `bootstrap.existingSecret` | `""` | Secret to mount instead (must not be set together with `bootstrap.token`: render error) |
| `bootstrap.key` | `token` | Key in `existingSecret` |
| `identity.secretName` | `thumbops-agent-identity` | Identity Secret name, passed as `--state-secret` |
| `policy` | today's policy (below) | Serialized to the policy ConfigMap |
| `rbac.actions` | `true` | ClusterRole/Binding for the five actions |
| `rbac.status` | `true` | ClusterRole/Binding for the cluster status; `false` also passes `--status=false` |
| `heartbeatInterval` | `""` (agent default 60s) | `--heartbeat-interval` when set |
| `statusInterval` | `""` (agent default 60s) | `--status-interval` when set |
| `logLevel` | `info` | `--log-level` |
| `extraArgs` | `[]` | Appended to the args |
| `http.port` | `9090` | `--http-addr=:<port>`, container port `http` |
| `livenessProbe` | `httpGet /healthz`, `periodSeconds: 30`, `failureThreshold: 3` | Rendered as is |
| `readinessProbe` | `httpGet /readyz`, `periodSeconds: 5` | Rendered as is |
| `metrics.service.enabled` | `false` | ClusterIP Service `<fullname>-metrics`, port `http` |
| `metrics.serviceMonitor.enabled` | `false` | ServiceMonitor (implies the Service) |
| `metrics.serviceMonitor.interval` | `30s` | |
| `metrics.serviceMonitor.labels` | `{}` | For the Prometheus selector |
| `podAnnotations` | `prometheus.io/scrape: "true"`, `prometheus.io/port: "9090"`, `prometheus.io/path: /metrics` | |
| `podLabels` | `{}` | |
| `resources` | requests `cpu: 10m, memory: 32Mi`; limits `memory: 128Mi` | |
| `podSecurityContext` | `runAsNonRoot: true`, `runAsUser: 65532`, `fsGroup: 65532`, `seccompProfile: RuntimeDefault` | |
| `securityContext` | `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, `capabilities.drop: [ALL]` | |
| `nodeSelector`, `tolerations`, `affinity` | empty | |
| `priorityClassName` | `""` | |

Default `policy` (the JSON field names of the agent's policy, so values and
file map one to one):

```yaml
policy:
  allowed_actions: [rollout-restart, scale, cordon, uncordon, drain]
  denied_namespaces: [kube-system]
  max_replicas: 20
  allow_delete_emptydir_data: false
  allow_control_plane_nodes: false
  status:
    exclude_namespaces: []
```

The release namespace is always appended to `denied_namespaces` if missing:
the agent never acts on its own namespace. With the defaults the rendered
policy is identical to today's (`kube-system`, `thumbops`).

The ConfigMap renders the policy with `toPrettyJson`, indented under
`policy.json: |`, so `internal/policy/manifest_test.go` keeps reading it
from the generated manifest (and thereby checks that the chart's default
policy loads in the agent).

`values.schema.json` sets `additionalProperties: false` at every level of
the chart's own objects (not inside free-form maps: `podAnnotations`,
`podLabels`, `nodeSelector`, `affinity`, `tolerations`, `resources`,
security contexts, probes, `metrics.serviceMonitor.labels`). The `policy`
object lists exactly the agent's policy fields. A misspelled value is a
`helm install` error, as unknown policy fields are an agent startup error.

## RBAC

- `<fullname>-base` (always rendered): `get` on the `kube-system` namespace
  (cluster identification at registration) and `list` on nodes (node counts
  in the heartbeat). Today these rules live in the actions ClusterRole, so
  an install without it (which the current manifest comment suggests for a
  dashboard-only agent) could not even register.
- `<fullname>-actions` (`rbac.actions`): deployments `get`/`patch`; nodes
  `get`/`list`/`patch`; pods `get`/`list`; `pods/eviction` `create`.
- `<fullname>-status` (`rbac.status`): nodes and pods, deployments
  `get`/`list`/`watch`.
- Role `<fullname>-identity`: see below.

## Identity Secret

- Rendered empty (no `data`, `type: Opaque`) with the annotation
  `helm.sh/resource-policy: keep`.
- `helm upgrade` never touches the data the agent wrote: the template has no
  `data`, so neither a three-way merge nor Helm's server-side apply removes
  it.
- `helm uninstall` leaves it in place. A later `helm install` in the same
  namespace with the same release name adopts it (its Helm ownership
  annotations match) and the agent starts with the same identity, without
  registering again. With a different release name Helm refuses to install
  ("exists and cannot be imported"): reuse the name, or delete the Secret
  and register again with a new bootstrap token. The chart README says so.
- Role and RoleBinding give the ServiceAccount only `get` and `update` on
  that Secret (`resourceNames`), as today.
- The `SecretStore` not-found error message mentions the chart and
  `deploy/agent.yaml`.

## Deployment

- One replica, `strategy: Recreate`, as today.
- Args: `--backend-url`, `--policy-file=/etc/thumbops/policy/policy.json`,
  `--bootstrap-token-file=/etc/thumbops/bootstrap/<key>`,
  `--state-secret=<identity.secretName>`, `--http-addr=:<http.port>`,
  `--log-level`, plus the optional ones above and `extraArgs`.
- Volumes: policy ConfigMap; bootstrap Secret (`optional: true`, the chart's
  or `existingSecret`); backend CA ConfigMap when `backend.caBundle` is set.
- A `checksum/policy` pod annotation (hash of the rendered policy) restarts
  the agent when the policy changes, since the agent reads it at startup.
- Probes, ports, security contexts and resources from the values.

## Generated manifest

`hack/gen-manifest.sh` runs `helm template thumbops-agent
charts/thumbops-agent --namespace thumbops` with the default values and
writes `deploy/agent.yaml` with:

- a header comment: generated from the chart, do not edit; how to install
  (`kubectl apply -f`, then `kubectl -n thumbops create secret generic
  thumbops-bootstrap --from-literal=token=<SINGLE-USE TOKEN>`); how to
  regenerate;
- a `Namespace` object for `thumbops` first;
- then the rendered resources.

No bootstrap Secret is rendered (no token in the defaults).

## Chart checks (`test/chart/check.sh`)

Run in CI and locally; fails on the first error.

1. `helm lint --strict charts/thumbops-agent`.
2. `helm template` with three value sets, each piped to `kubeconform -strict
   -summary` (`-ignore-missing-schemas` for the ServiceMonitor):
   defaults; everything on (`backend.caBundle`, `bootstrap.token`,
   `metrics.serviceMonitor.enabled`, `heartbeatInterval`, `extraArgs`);
   `rbac.actions=false,rbac.status=false`; `bootstrap.existingSecret`.
3. Assertions on the rendered output:
   - the identity Secret has `helm.sh/resource-policy: keep` and no `data`;
   - with `rbac.actions=false,rbac.status=false` the args contain
     `--status=false`, there is no actions or status ClusterRole, and the
     base ClusterRole is still there;
   - with `bootstrap.existingSecret=my-token` the bootstrap volume uses
     `my-token` and no `thumbops-bootstrap` Secret is rendered;
   - with `bootstrap.token` and `bootstrap.existingSecret` both set, the
     render fails;
   - the release namespace is in the rendered `denied_namespaces`.
4. An unknown value (`--set foo=bar`) and an unknown policy field
   (`--set policy.foo=1`) make `helm template` fail.
5. `hack/gen-manifest.sh` followed by `git diff --exit-code
   deploy/agent.yaml`: the committed manifest is up to date.

## End-to-end tests on kind

`test/e2e/run.sh` installs the agent with `helm install thumbops-agent
charts/thumbops-agent -n thumbops --create-namespace -f
test/e2e/values.yaml --set-file backend.caBundle=<mock CA>`. The values set
image `thumbops/agent:e2e` with `pullPolicy: Never`, the mock backend URL,
`bootstrap.token`, `heartbeatInterval: 5s`, `statusInterval: 10s` and the
`thumbops-e2e: infra` node selector.

Changes to the scenario:

- "applying the manifest again" becomes `helm upgrade` with the same values:
  the identity key is unchanged.
- New step: `helm uninstall`, check that the identity Secret is still there,
  `helm install` again with the same values; the agent does not register
  again (no "agent registered" in the new pod's logs) and an action
  succeeds.
- The new-token step uses `helm upgrade --reuse-values --set
  bootstrap.token=<new token>` and a rollout restart.
- All other checks stay: actions, policy rejections, certificate renewal,
  Secret RBAC, health and metrics, `restartCount` 0.

## CI

- New job `chart`: installs Helm and kubeconform, runs `test/chart/check.sh`.
- The `e2e` job needs Helm (preinstalled on `ubuntu-latest`; otherwise
  `azure/setup-helm`).
- New job `chart-publish` on `v*` tags, after `image`: `helm registry login
  ghcr.io` with `GITHUB_TOKEN`, `helm package charts/thumbops-agent
  --version <tag without v> --app-version <tag without v>`, `helm push` to
  `oci://ghcr.io/thumbops/charts`. Permission `packages: write`.

## Documentation

- README: "Installing" section (Helm from OCI, the static manifest as the
  alternative), the main values, the identity Secret notes, the updated
  e2e description and layout.
- `charts/thumbops-agent/README.md`: values reference and the identity
  Secret notes.
- CLAUDE.md: layout (`charts/`, `hack/`, `test/chart`), commands
  (`hack/gen-manifest.sh`, `test/chart/check.sh`), a new invariant
  ("`deploy/agent.yaml` is generated from the chart: change the chart and
  regenerate"), next steps updated.
