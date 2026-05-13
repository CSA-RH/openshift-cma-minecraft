#!/usr/bin/env bash
# bootstrap.sh — install the OpenShift CMA / Minecraft workshop end-to-end.
#
# Reads ${NAMESPACE} from the environment (default: minecraft) and substitutes
# it into every YAML in manifests/ via envsubst before applying. Only the
# ${NAMESPACE} variable is expanded — any other ${...} that happens to appear
# in a manifest is left alone.
#
# Usage:
#   ./scripts/bootstrap.sh                                  # defaults: NAMESPACE=minecraft, TRIGGER=metrics-api
#   NAMESPACE=acme TRIGGER=prometheus ./scripts/bootstrap.sh
#   ./scripts/bootstrap.sh --dry-run                        # render-only, no oc apply
#   ./scripts/bootstrap.sh --uninstall                      # remove the per-namespace install (keeps the namespace)
#   ./scripts/bootstrap.sh --uninstall --delete-namespace   # also `oc delete project $NAMESPACE`
#   ./scripts/bootstrap.sh --skip-build                     # skip the in-cluster `make ocp-build`
#
# Prerequisites (not enforced by this script):
#   * oc logged in with rights to create namespaces and cluster-scoped objects
#   * envsubst (ships with gettext on most distros)
#   * the Custom Metrics Autoscaler Operator already installed (OperatorHub)
#   * the mc-waker image already built and pushed/imagestreamed somewhere
#     reachable from the chosen namespace

set -euo pipefail

# ---- defaults -----------------------------------------------------------
: "${NAMESPACE:=minecraft}"
: "${TRIGGER:=metrics-api}"     # metrics-api | prometheus
: "${MC_WAIT_TIMEOUT:=180s}"
: "${WAKER_WAIT_TIMEOUT:=120s}"
export NAMESPACE

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." >/dev/null 2>&1 && pwd)"
MANIFESTS="$REPO_ROOT/manifests"

DRY_RUN=0
UNINSTALL=0
DELETE_NS=0
SKIP_BUILD=0
for arg in "$@"; do
  case "$arg" in
    --dry-run)   DRY_RUN=1 ;;
    --uninstall)        UNINSTALL=1 ;;
    --delete-namespace) DELETE_NS=1 ;;
    --skip-build)       SKIP_BUILD=1 ;;
    -h|--help)   sed -n '2,30p' "$0"; exit 0 ;;
    *)           echo "unknown arg: $arg (try --help)"; exit 1 ;;
  esac
done

# ---- helpers -----------------------------------------------------------
note()  { printf "\n\033[1;34m==>\033[0m %s\n" "$*"; }
warn()  { printf "\n\033[1;33mwarn:\033[0m %s\n" "$*" >&2; }
fatal() { printf "\n\033[1;31mfatal:\033[0m %s\n" "$*" >&2; exit 1; }

require() {
  command -v "$1" >/dev/null 2>&1 || fatal "$1 not found in PATH"
}

# render <yaml-file>: substitute only ${NAMESPACE}, leave all other ${...} alone
render() {
  envsubst '${NAMESPACE}' < "$1"
}

# apply <yaml-file-or-dir>: render every file (or every file in the dir) and pipe to oc apply -n $NAMESPACE
apply_namespaced() {
  local target="$1"
  if [[ -d "$target" ]]; then
    for f in "$target"/*.yaml; do apply_namespaced "$f"; done
    return
  fi
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "--- rendered $target ---"
    render "$target"
  else
    render "$target" | oc -n "$NAMESPACE" apply -f -
  fi
}

# apply_cluster <yaml-file>: same idea, but no -n flag (cluster-scoped or pre-namespaced)
apply_cluster() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "--- rendered $1 ---"
    render "$1"
  else
    render "$1" | oc apply -f -
  fi
}

# ---- preflight ---------------------------------------------------------
require oc
require envsubst

if [[ "$UNINSTALL" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
  oc whoami >/dev/null 2>&1 || fatal "not logged into an OpenShift cluster (try: oc login ...)"
fi

case "$TRIGGER" in
  metrics-api|prometheus) ;;
  *) fatal "TRIGGER must be 'metrics-api' or 'prometheus' (got: $TRIGGER)" ;;
esac

note "config: NAMESPACE=$NAMESPACE  TRIGGER=$TRIGGER  DRY_RUN=$DRY_RUN  UNINSTALL=$UNINSTALL"

# ---- uninstall path ----------------------------------------------------
if [[ "$UNINSTALL" -eq 1 ]]; then
  note "Tearing down namespace '$NAMESPACE'"
  oc delete -n "$NAMESPACE" -f "$MANIFESTS/keda/scaledobject-metricsapi.yaml" --ignore-not-found
  oc delete -n "$NAMESPACE" -f "$MANIFESTS/keda/scaledobject-prometheus.yaml" --ignore-not-found
  oc delete -n "$NAMESPACE" route mc-ragnarok-waker                           --ignore-not-found
  oc delete -n "$NAMESPACE" -f "$MANIFESTS/waker/"                            --ignore-not-found
  oc delete -n "$NAMESPACE" -f "$MANIFESTS/minecraft/"                        --ignore-not-found

  if [[ "$DELETE_NS" -eq 1 ]]; then
    note "Deleting project '$NAMESPACE' (because --delete-namespace was passed)"
    oc delete project "$NAMESPACE" --ignore-not-found
  else
    note "Namespace '$NAMESPACE' kept (pass --delete-namespace to remove it too)"
  fi
  warn "Cluster-scoped KedaController in 'openshift-keda' was left alone — delete it manually if you no longer want CMA on this cluster."
  exit 0
fi

# ---- install path ------------------------------------------------------

# 0) KedaController (cluster-scoped, pinned to openshift-keda; one-time)
note "Applying KedaController (cluster-scoped, openshift-keda)"
apply_cluster "$MANIFESTS/keda/kedacontroller.yaml"

# 1) Namespace
note "Ensuring namespace '$NAMESPACE' exists"
if [[ "$DRY_RUN" -eq 0 ]]; then
  oc get ns "$NAMESPACE" >/dev/null 2>&1 || oc new-project "$NAMESPACE" >/dev/null
fi

# 2) Build the waker image into the namespace's ImageStream (in-cluster).
#    Skip with --skip-build if you've already pushed it (e.g. to quay.io)
#    and updated `image:` in manifests/waker/mc-waker.yaml accordingly.
if [[ "$SKIP_BUILD" -eq 1 ]]; then
  note "Skipping waker image build (--skip-build was passed)"
elif [[ "$DRY_RUN" -eq 1 ]]; then
  note "Would build waker image: make -C $REPO_ROOT/build/mc-waker ocp-build NAMESPACE=$NAMESPACE"
else
  note "Building waker image into ImageStream '$NAMESPACE/mc-waker' (in-cluster build)"
  make -C "$REPO_ROOT/build/mc-waker" ocp-build NAMESPACE="$NAMESPACE"
fi

# 3) Minecraft workload (secrets, PVC, deploy, services)
note "Applying Minecraft workload"
apply_namespaced "$MANIFESTS/minecraft"

if [[ "$DRY_RUN" -eq 0 ]]; then
  oc -n "$NAMESPACE" rollout status deploy/mc-ragnarok --timeout="$MC_WAIT_TIMEOUT"
fi

# 3) Waker (SA + Deployment + Service)
note "Applying waker"
apply_namespaced "$MANIFESTS/waker/mc-waker.yaml"

if [[ "$DRY_RUN" -eq 0 ]]; then
  oc -n "$NAMESPACE" rollout status deploy/mc-ragnarok-waker --timeout="$WAKER_WAIT_TIMEOUT"
fi

# 4.5) TLS-terminated Route for the waker's HTTP API (port 8080).
#      Hostname is derived from the cluster's base domain so curl works
#      from anywhere without a port-forward.
note "Creating edge-terminated Route for the waker API"
if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "(would create: route/mc-ragnarok-waker -> svc/mc-ragnarok-waker:8080 in $NAMESPACE)"
else
  BASE_DOMAIN="$(oc get dns cluster -o jsonpath='{.spec.baseDomain}')"
  oc -n "$NAMESPACE" create route edge \
    --service=mc-ragnarok-waker \
    --port=8080 \
    --hostname="mc-ragnarok-waker-api.apps.${BASE_DOMAIN}" \
    --dry-run=client -o yaml \
    | oc -n "$NAMESPACE" apply -f -
  ROUTE_HOST="$(oc -n "$NAMESPACE" get route mc-ragnarok-waker -o jsonpath='{.spec.host}')"
  echo "    Route host: https://${ROUTE_HOST}"
fi

# 4) Trigger variant
case "$TRIGGER" in
  metrics-api)
    note "Applying ScaledObject (metrics-api trigger)"
    apply_namespaced "$MANIFESTS/keda/scaledobject-metricsapi.yaml"
    ;;
  prometheus)
    note "Provisioning Prometheus trigger auth (RBAC + token Secret)"
    if [[ "$DRY_RUN" -eq 0 ]]; then
      "$SCRIPT_DIR/setup-trigger-auth.sh" "$NAMESPACE"
    else
      echo "(skipped in --dry-run: $SCRIPT_DIR/setup-trigger-auth.sh $NAMESPACE)"
    fi
    note "Applying ServiceMonitor + ScaledObject (prometheus trigger)"
    apply_namespaced "$MANIFESTS/waker/servicemonitor.yaml"
    apply_namespaced "$MANIFESTS/keda/scaledobject-prometheus.yaml"
    ;;
esac

note "Done. Workshop installed in namespace '$NAMESPACE' with trigger '$TRIGGER'."
if [[ "$DRY_RUN" -eq 0 ]]; then
  echo
  echo "Quick smoke test:"
  echo "  oc -n $NAMESPACE port-forward svc/mc-ragnarok-waker 8080:8080 &"
  echo "  curl -s   localhost:8080/scaler"
  echo "  curl -sX POST localhost:8080/wake"
fi
