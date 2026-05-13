#!/usr/bin/env bash
# Creates the Secret + RBAC needed for KEDA's prometheus trigger to query the
# OpenShift User Workload Monitoring stack (Thanos Querier).
#
# Only run this if you're using 21-scaledobject-prometheus.yaml.
# 20-scaledobject-metricsapi.yaml has no such prerequisite.
#
# Usage:
#   ./setup-trigger-auth.sh [namespace]    (default: minecraft)
set -euo pipefail

NS="${1:-minecraft}"
SA="thanos-reader"

oc get ns "$NS" >/dev/null 2>&1 || oc new-project "$NS"

oc -n "$NS" create sa "$SA" --dry-run=client -o yaml | oc apply -f -

# Allow the SA to read metrics from the cluster monitoring stack.
oc adm policy add-cluster-role-to-user cluster-monitoring-view \
  -z "$SA" -n "$NS"

# 1-year token; rotate as needed.
TOKEN="$(oc -n "$NS" create token "$SA" --duration=8760h)"

# Pull the cluster's serving CA bundle so KEDA can verify Thanos's TLS cert.
CA_BUNDLE="$(oc get cm -n openshift-config-managed kube-root-ca.crt -o jsonpath='{.data.ca\.crt}')"

oc -n "$NS" create secret generic keda-prometheus-token \
  --from-literal=token="$TOKEN" \
  --from-literal=ca.crt="$CA_BUNDLE" \
  --dry-run=client -o yaml | oc apply -f -

echo "Secret keda-prometheus-token created/updated in namespace $NS."
echo "You can now apply manifests/keda/scaledobject-prometheus.yaml."
