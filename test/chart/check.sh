#!/usr/bin/env bash
# Checks the Helm chart: lint, render with several value sets, validate the
# output with kubeconform, assert the properties the agent relies on, and
# verify that deploy/agent.yaml is up to date. Needs helm and kubeconform.
set -euo pipefail
cd "$(dirname "$0")/../.."
chart=charts/thumbops-agent
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
trap 'echo "failed at line $LINENO: $BASH_COMMAND" >&2' ERR

fail() { echo "FAIL: $*" >&2; exit 1; }
render() { # NAME ARGS...: renders to $out/NAME.yaml
  local name=$1
  shift
  helm template thumbops-agent "$chart" --namespace thumbops "$@" >"$out/$name.yaml"
}
validate() {
  kubeconform -strict -summary -ignore-missing-schemas "$out/$1.yaml"
}

echo "--- lint"
helm lint --strict "$chart"

echo "--- render and validate"
render defaults
render all-on \
  --set-string backend.caBundle="$(printf -- '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----')" \
  --set bootstrap.token=test-token \
  --set metrics.serviceMonitor.enabled=true \
  --set heartbeatInterval=30s \
  --set 'extraArgs={--log-level=debug}'
render no-rbac --set rbac.actions=false --set rbac.status=false
render existing --set bootstrap.existingSecret=my-token
for f in defaults all-on no-rbac existing; do validate "$f"; done

echo "--- assertions"
# The identity Secret is kept on uninstall and never carries data in the chart.
identity=$(awk '/^# Source: thumbops-agent\/templates\/identity-secret.yaml/{f=1} f&&/^---/{exit} f' "$out/defaults.yaml")
grep -q 'helm.sh/resource-policy: keep' <<<"$identity" || fail "identity Secret without keep policy"
! grep -qE '^(data|stringData):' <<<"$identity" || fail "identity Secret renders data"
# The agent never acts on its own namespace.
grep -A4 '"denied_namespaces"' "$out/defaults.yaml" | grep -q '"thumbops"' || fail "release namespace not denied"
# Without the status RBAC the status is turned off.
grep -q -- '--status=false' "$out/no-rbac.yaml" || fail "--status=false missing"
! grep -q 'name: thumbops-agent-status' "$out/no-rbac.yaml" || fail "status ClusterRole rendered"
! grep -q 'name: thumbops-agent-actions' "$out/no-rbac.yaml" || fail "actions ClusterRole rendered"
grep -q 'name: thumbops-agent-base' "$out/no-rbac.yaml" || fail "base ClusterRole missing"
# Everything on renders the optional objects.
grep -q -- '--backend-ca-file=/etc/thumbops/backend-ca/ca.crt' "$out/all-on.yaml" || fail "backend CA arg missing"
grep -q 'kind: ServiceMonitor' "$out/all-on.yaml" || fail "ServiceMonitor missing"
grep -q 'name: thumbops-agent-metrics' "$out/all-on.yaml" || fail "metrics Service missing"
grep -q 'name: thumbops-bootstrap' "$out/all-on.yaml" || fail "bootstrap Secret missing"
# An existing token Secret is mounted, and the chart does not create one.
grep -q 'secretName: my-token' "$out/existing.yaml" || fail "existingSecret not mounted"
! grep -q 'name: thumbops-bootstrap' "$out/existing.yaml" || fail "bootstrap Secret rendered with existingSecret"

echo "--- invalid values are rejected"
for args in "--set foo=bar" "--set policy.foo=1" "--set bootstrap.token=a --set bootstrap.existingSecret=b"; do
  # shellcheck disable=SC2086
  if helm template thumbops-agent "$chart" $args >/dev/null 2>&1; then
    fail "accepted: $args"
  fi
done

echo "--- deploy/agent.yaml up to date"
hack/gen-manifest.sh
git diff --exit-code -- deploy/agent.yaml || fail "deploy/agent.yaml is out of date: run hack/gen-manifest.sh and commit it"

echo "--- chart checks passed"
