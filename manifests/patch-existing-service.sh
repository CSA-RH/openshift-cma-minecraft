#!/usr/bin/env bash
# Repoints the EXISTING NodePort Service "mc-ragnarok" away from the
# minecraft pod and toward the waker pod.
#
# Why this approach: the Service object itself is preserved — same name,
# same ClusterIP, same nodePort number, same TLS/RBAC/NetworkPolicy
# references that may already point at it. Only its selector changes.
# Anything outside the cluster (router port-forward, firewall rule,
# server-list bookmark in your Minecraft client) keeps working as-is.
#
# Rollback: ./patch-existing-service.sh --rollback
set -euo pipefail

NS="${NAMESPACE:-minecraft}"
SVC="${SERVICE:-mc-ragnarok-nodeport}"

usage() {
  cat <<EOF
Usage:
  $(basename "$0")              flip selector to the waker
  $(basename "$0") --rollback   restore selector to the original minecraft pod

Environment overrides:
  NAMESPACE   (default: minecraft)
  SERVICE     (default: mc-ragnarok-nodeport)
EOF
}

snapshot_selector() {
  oc -n "$NS" get svc "$SVC" -o json \
    | python3 -c 'import json,sys; print(json.dumps(json.load(sys.stdin)["spec"].get("selector",{})))'
}

# 1) Capture the current selector into an annotation so we can restore it.
case "${1:-}" in
  -h|--help) usage; exit 0;;
  --rollback)
    PRIOR="$(oc -n "$NS" get svc "$SVC" -o jsonpath='{.metadata.annotations.cma-demo\.redhat\.com/original-selector}')"
    if [[ -z "$PRIOR" ]]; then
      echo "No saved original selector found on $SVC. Aborting." >&2
      exit 1
    fi
    echo "Restoring original selector on Service/$SVC: $PRIOR"
    oc -n "$NS" patch svc "$SVC" --type=json \
      -p "[{\"op\":\"replace\",\"path\":\"/spec/selector\",\"value\":$PRIOR}]"
    oc -n "$NS" annotate svc "$SVC" cma-demo.redhat.com/original-selector- --overwrite
    exit 0
    ;;
esac

CURRENT="$(snapshot_selector)"
echo "Current selector on Service/$SVC in $NS: $CURRENT"

# Save the original selector so --rollback can restore it.
oc -n "$NS" annotate svc "$SVC" \
  "cma-demo.redhat.com/original-selector=${CURRENT}" --overwrite >/dev/null

NEW_SELECTOR='{"app.kubernetes.io/name":"mc-ragnarok-waker"}'
echo "Replacing selector -> $NEW_SELECTOR"
# Use --type=json with a `replace` op so the new selector fully REPLACES the
# old one. A merge patch would preserve old keys (e.g. `app: mc-ragnarok`)
# and Service selectors are AND-ed, leaving you with empty endpoints.
oc -n "$NS" patch svc "$SVC" --type=json \
  -p "[{\"op\":\"replace\",\"path\":\"/spec/selector\",\"value\":$NEW_SELECTOR}]"

echo
echo "Done. External traffic to Service/$SVC now lands on the waker pod."
echo "Endpoints check:"
oc -n "$NS" get endpoints "$SVC"
