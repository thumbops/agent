#!/usr/bin/env bash
# Regenerates deploy/agent.yaml from the Helm chart with the default values.
# Run it after every chart change; test/chart/check.sh fails when the
# committed manifest is out of date.
set -euo pipefail
cd "$(dirname "$0")/.."

{
  cat <<'HEADER'
# ThumbOps agent installation without Helm.
#
# GENERATED from charts/thumbops-agent by hack/gen-manifest.sh: do not edit,
# change the chart and run the script again.
#
#   kubectl apply -f deploy/agent.yaml
#   kubectl -n thumbops create secret generic thumbops-bootstrap --from-literal=token=<SINGLE-USE TOKEN>
#
# ClusterRoles: thumbops-agent-base (always needed: kube-system UID at
# registration, node counts in the heartbeat), thumbops-agent-actions (only
# what the five actions need), thumbops-agent-status (read-only, for the
# dashboard's cluster status). The Role thumbops-agent-identity lets the agent
# read and update only the Secret holding its key and certificate.
#
# To register again (cluster revoked, certificate expired), put a new
# single-use token in thumbops-bootstrap and restart the agent: a token
# different from the one used for the saved identity triggers a new
# registration. For other options (dashboard only, no actions, backend CA,
# metrics Service) install with Helm: see charts/thumbops-agent/README.md.
#
# The resources carry Helm labels (app.kubernetes.io/managed-by: Helm) because
# they are rendered from the chart; applied with kubectl they are not managed
# by Helm.
apiVersion: v1
kind: Namespace
metadata:
  name: thumbops
HEADER
  helm template thumbops-agent charts/thumbops-agent --namespace thumbops
} > deploy/agent.yaml
