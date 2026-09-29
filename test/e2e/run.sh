#!/usr/bin/env bash
# End-to-end test on kind: the agent installed with the Helm chart (real
# RBAC) registers with the mock backend over mTLS, runs approved actions,
# rejects the ones outside the policy, renews its certificate, exposes health
# probes and metrics, keeps its identity (in a Secret) across restarts,
# survives helm upgrade, uninstall and reinstall with its identity, and
# registers again with a new bootstrap token.
#
#   export KUBECONFIG="$PWD/.kind-kubeconfig"
#   kind create cluster --name thumbops-e2e --config test/e2e/kind.yaml
#   test/e2e/run.sh
#   kind delete cluster --name thumbops-e2e
#
# KUBECONFIG must point to a dedicated file, never ~/.kube/config: kind
# switches the current context of the file it writes to, and clears it on
# delete, which would break whatever else is using kubectl meanwhile. In CI
# (CI=true, a throwaway runner) the default kubeconfig is accepted.
# KIND_CLUSTER selects the cluster (default thumbops-e2e). The script refuses
# to run unless the current context of that file is kind-$KIND_CLUSTER.
# Never point it at a real cluster.
set -euo pipefail

cd "$(dirname "$0")/../.."
KIND_CLUSTER=${KIND_CLUSTER:-thumbops-e2e}
LOCAL_PORT=${LOCAL_PORT:-18443}
work=$(mktemp -d)
pf_pid=""
agent_pf_pid=""

if [[ ${CI:-} != true ]]; then
  if [[ -z ${KUBECONFIG:-} || $KUBECONFIG == *:* || $KUBECONFIG -ef "$HOME/.kube/config" ]]; then
    echo "set KUBECONFIG to a dedicated file, e.g. export KUBECONFIG=\"\$PWD/.kind-kubeconfig\"," >&2
    echo "and create the kind cluster with it: never use ~/.kube/config" >&2
    exit 1
  fi
fi
if [[ $(kubectl config current-context) != "kind-$KIND_CLUSTER" ]]; then
  echo "the current kubectl context is not kind-$KIND_CLUSTER: refusing to run" >&2
  exit 1
fi

log() { echo "--- $*"; }

dump() {
  log "agent logs"
  kubectl -n thumbops logs deploy/thumbops-agent --tail=200 || true
  log "mock backend logs"
  kubectl -n thumbops-e2e logs deploy/mock-backend --tail=100 || true
  log "actions on the mock backend"
  backend GET /debug/actions | jq . || true
  kubectl get nodes,pods -A -o wide || true
}

cleanup() {
  local rc=$?
  [[ $rc -ne 0 ]] && dump
  [[ -n $pf_pid ]] && kill "$pf_pid" 2>/dev/null || true
  [[ -n $agent_pf_pid ]] && kill "$agent_pf_pid" 2>/dev/null || true
  rm -rf "$work"
  exit $rc
}
trap cleanup EXIT
trap 'echo "failed at line $LINENO: $BASH_COMMAND" >&2' ERR

backend() { # METHOD PATH [BODY]
  curl -fsS --cacert "$work/tls.crt" -X "$1" "https://localhost:$LOCAL_PORT$2" ${3:+-d "$3"}
}

# wait_for DESCRIPTION TIMEOUT_SECONDS COMMAND...
wait_for() {
  local what=$1 timeout=$2
  shift 2
  for ((i = 0; i < timeout; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "timed out after ${timeout}s waiting for: $what" >&2
  return 1
}

# run_action EXPECTED_STATUS JSON: enqueues an action as if approved in the
# app and waits for the agent's result.
run_action() {
  local want=$1 body=$2 id status
  id=$(backend POST /debug/actions "$body" | jq -r .action_id)
  wait_for "result of $id" 120 has_result "$id"
  status=$(backend GET /debug/actions | jq -r --arg id "$id" '.[] | select(.action.action_id == $id) | .result.status')
  echo "$(jq -c '{type, params}' <<<"$body") -> $status"
  if [[ $status != "$want" ]]; then
    backend GET /debug/actions | jq --arg id "$id" '.[] | select(.action.action_id == $id)'
    echo "expected $want, got $status" >&2
    return 1
  fi
}

has_result() {
  backend GET /debug/actions | jq -e --arg id "$1" '.[] | select(.action.action_id == $id) | .result'
}

agent_logged() {
  kubectl -n thumbops logs deploy/thumbops-agent | grep -q "$1"
}

METRICS_PORT=${METRICS_PORT:-19090}

# metric SERIES: value of an exact series, e.g. 'thumbops_agent_status_ready'.
metric() {
  curl -fsS "http://localhost:$METRICS_PORT/metrics" | awk -v s="$1" '$1 == s { print $2 }'
}

# helm_agent install|upgrade [ARGS...]: the agent release with the e2e values.
helm_agent() {
  local cmd=$1
  shift
  helm "$cmd" thumbops-agent charts/thumbops-agent -n thumbops -f test/e2e/values.yaml \
    --set-file backend.caBundle="$work/tls.crt" --wait --timeout 3m "$@"
}

agent_restarts() {
  kubectl -n thumbops get pods -l app.kubernetes.io/name=thumbops-agent --field-selector=status.phase=Running -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}'
}

log "building and loading the images"
docker build -q -t thumbops/agent:e2e --build-arg VERSION=0.1.0-e2e .
docker build -q -t thumbops/mock-backend:e2e -f test/e2e/Dockerfile.mock-backend .
kind load docker-image --name "$KIND_CLUSTER" thumbops/agent:e2e thumbops/mock-backend:e2e

log "starting the mock backend"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=mock-backend \
  -addext "subjectAltName=DNS:mock-backend.thumbops-e2e.svc,DNS:localhost" \
  -keyout "$work/tls.key" -out "$work/tls.crt" 2>/dev/null
kubectl apply -f test/e2e/mock-backend.yaml
kubectl -n thumbops-e2e create secret tls mock-backend-tls --cert "$work/tls.crt" --key "$work/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n thumbops-e2e rollout status deploy/mock-backend --timeout=120s
kubectl -n thumbops-e2e port-forward svc/mock-backend "$LOCAL_PORT:8443" >/dev/null &
pf_pid=$!
wait_for "port-forward to the mock backend" 30 backend GET /debug/actions

log "installing the agent"
helm_agent install --create-namespace
wait_for "agent registration" 60 agent_logged '"agent registered"'

# The agent reads and updates only its identity Secret: no other Secret, no create.
sa=system:serviceaccount:thumbops:thumbops-agent
for check in "yes get secret/thumbops-agent-identity" "yes update secret/thumbops-agent-identity" \
  "no get secret/thumbops-bootstrap" "no create secrets" "no list secrets"; do
  read -r want verb resource <<<"$check"
  got=$(kubectl auth can-i "$verb" "$resource" -n thumbops --as="$sa" || true)
  [[ $got == "$want" ]] || { echo "can-i $verb $resource: expected $want, got $got" >&2; exit 1; }
done

log "test workload"
kubectl create deployment web --image=registry.k8s.io/pause:3.10 --replicas=2 --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout status deploy/web --timeout=120s
workload_node=$(kubectl get nodes -l thumbops-e2e=workload -o jsonpath='{.items[0].metadata.name}')
control_plane=$(kubectl get nodes -l node-role.kubernetes.io/control-plane -o jsonpath='{.items[0].metadata.name}')

log "actions"
run_action succeeded '{"type":"scale","params":{"namespace":"default","deployment":"web","replicas":4}}'
[[ $(kubectl get deploy web -o jsonpath='{.spec.replicas}') == 4 ]]
[[ -n $(kubectl get deploy web -o jsonpath='{.metadata.annotations.thumbops\.mobiletechnologies\.cloud/last-action-id}') ]]
kubectl rollout status deploy/web --timeout=120s

run_action succeeded '{"type":"rollout-restart","params":{"namespace":"default","deployment":"web"}}'
kubectl rollout status deploy/web --timeout=120s

run_action succeeded "{\"type\":\"cordon\",\"params\":{\"node\":\"$workload_node\"}}"
[[ $(kubectl get node "$workload_node" -o jsonpath='{.spec.unschedulable}') == true ]]

run_action succeeded "{\"type\":\"drain\",\"params\":{\"node\":\"$workload_node\",\"timeout_seconds\":90}}"
remaining=$(kubectl get pods -n default -l app=web --field-selector "spec.nodeName=$workload_node" -o name)
[[ -z $remaining ]]

log "policy rejections"
run_action rejected '{"type":"scale","params":{"namespace":"kube-system","deployment":"coredns","replicas":5}}'
run_action rejected "{\"type\":\"cordon\",\"params\":{\"node\":\"$control_plane\"}}"
[[ $(kubectl get node "$control_plane" -o jsonpath='{.spec.unschedulable}') != true ]]

log "certificate renewal"
wait_for "certificate renewal" 90 agent_logged '"certificate renewed"'
# An action after the renewal proves the new certificate is used (new connections).
run_action succeeded "{\"type\":\"uncordon\",\"params\":{\"node\":\"$workload_node\"}}"
[[ $(kubectl get node "$workload_node" -o jsonpath='{.spec.unschedulable}') != true ]]

log "health and metrics"
kubectl -n thumbops port-forward deploy/thumbops-agent "$METRICS_PORT:9090" >/dev/null &
agent_pf_pid=$!
wait_for "port-forward to the agent" 30 curl -fsS "http://localhost:$METRICS_PORT/readyz"
curl -fsS "http://localhost:$METRICS_PORT/healthz" >/dev/null
for series in \
  'thumbops_agent_actions_total{outcome="succeeded",type="scale"}' \
  'thumbops_agent_actions_total{outcome="succeeded",type="rollout-restart"}' \
  'thumbops_agent_actions_total{outcome="succeeded",type="cordon"}' \
  'thumbops_agent_actions_total{outcome="succeeded",type="drain"}' \
  'thumbops_agent_actions_total{outcome="succeeded",type="uncordon"}' \
  'thumbops_agent_actions_total{outcome="rejected",type="scale"}' \
  'thumbops_agent_actions_total{outcome="rejected",type="cordon"}' \
  'thumbops_agent_status_ready'; do
  [[ $(metric "$series") == 1 ]] || { echo "$series = $(metric "$series"), expected 1" >&2; exit 1; }
done
(( $(metric 'thumbops_agent_certificate_renewals_total{result="success"}') >= 1 ))
# Large gauges come in exponent form (1.7e+09): round with awk, whose parsing
# does not depend on the locale.
last_hb=$(metric thumbops_agent_heartbeat_last_success_timestamp_seconds | awk '{ printf "%.0f", $1 }')
(( $(date +%s) - last_hb < 60 ))
[[ $(agent_restarts) == 0 ]] # the liveness probe never fired, drain included
kill "$agent_pf_pid"
agent_pf_pid=""

identity_field() {
  kubectl -n thumbops get secret thumbops-agent-identity -o jsonpath="{.data.$1}"
}

restart_agent() {
  local old
  old=$(kubectl -n thumbops get pods -l app.kubernetes.io/name=thumbops-agent -o name)
  kubectl -n thumbops rollout restart deploy/thumbops-agent
  kubectl -n thumbops rollout status deploy/thumbops-agent --timeout=120s
  # The logs checked next must come from the new pod only.
  kubectl -n thumbops wait --for=delete $old --timeout=120s
  wait_for "agent start" 60 agent_logged '"agent started"'
}

log "restart: the identity in the Secret is kept"
key_before=$(identity_field 'key\.pem')
helm_agent upgrade >/dev/null # an upgrade must not erase it
[[ $(identity_field 'key\.pem') == "$key_before" ]]
restart_agent
if agent_logged '"agent registered"'; then
  echo "the agent registered again after a plain restart" >&2
  exit 1
fi
[[ $(identity_field 'key\.pem') == "$key_before" ]]
run_action succeeded '{"type":"scale","params":{"namespace":"default","deployment":"web","replicas":2}}'

log "uninstall and install again: the identity Secret is kept and adopted"
old=$(kubectl -n thumbops get pods -l app.kubernetes.io/name=thumbops-agent -o name)
helm uninstall thumbops-agent -n thumbops --wait
kubectl -n thumbops wait --for=delete $old --timeout=120s
[[ $(identity_field 'key\.pem') == "$key_before" ]]
helm_agent install
wait_for "agent start" 60 agent_logged '"agent started"'
if agent_logged '"agent registered"'; then
  echo "the agent registered again after a reinstall" >&2
  exit 1
fi
[[ $(identity_field 'key\.pem') == "$key_before" ]]
run_action succeeded '{"type":"scale","params":{"namespace":"default","deployment":"web","replicas":1}}'

log "new bootstrap token: the agent registers again"
backend POST /debug/bootstrap-tokens '{"token":"e2e-bootstrap-token-2"}' >/dev/null
# The token is part of the pod template (checksum/bootstrap): the upgrade
# replaces the pod by itself, so no restart is needed.
old=$(kubectl -n thumbops get pods -l app.kubernetes.io/name=thumbops-agent -o name)
helm_agent upgrade --reuse-values --set bootstrap.token=e2e-bootstrap-token-2 >/dev/null
kubectl -n thumbops wait --for=delete $old --timeout=120s
wait_for "agent start" 60 agent_logged '"agent started"'
agent_logged '"agent registered"'
[[ $(identity_field 'key\.pem') != "$key_before" ]]
run_action succeeded '{"type":"scale","params":{"namespace":"default","deployment":"web","replicas":3}}'

[[ $(agent_restarts) == 0 ]]

log "end-to-end tests passed"
