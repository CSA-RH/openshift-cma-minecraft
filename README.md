# OpenShift Custom Metrics Autoscaler — Workshop

A hands-on workshop on the **OpenShift Custom Metrics Autoscaler** (CMA / KEDA). You'll scale a Minecraft Java server from **0 to 1 and back** based on player intent — a custom metric the workload itself can't possibly report while it's at zero replicas.

## What it teaches

- `ScaledObject` mechanics: `threshold` vs `activationThreshold`, `pollingInterval`, `cooldownPeriod`, `fallback`.
- Two trigger flavors side-by-side: **`metrics-api`** (direct HTTP scrape) and **`prometheus`** (UWM / Thanos).
- Why a custom metric for a scale-to-zero workload has to be *sourced from outside* the workload.

## How it works

```
player ──► NodePort ──► waker pod (always 1) ──► ClusterIP ──► mc-ragnarok pod (0..1, KEDA-scaled)
                            │
                            └── publishes minecraft_desired_replicas (0/1) ──► ScaledObject
```

The **waker** is a small Go service that always listens on TCP `25565`. When `mc-ragnarok` is up, it transparently proxies. When it's at zero replicas, it answers the Minecraft client's *Refresh* with a "Sleeping" MOTD and raises the metric to `1`. KEDA picks that up, scales the Minecraft Deployment to `1`, and the next *Refresh* gets the live server. After the last player leaves and `cooldownPeriod` elapses, KEDA scales it back to `0`.

Source walkthrough for the waker: **[`docs/mc-waker-internals.md`](docs/mc-waker-internals.md)**.

## Repo layout

```
build/mc-waker/                    Go source for the waker + Containerfile + Makefile
manifests/
├── keda/                          KedaController + the two ScaledObject variants
├── minecraft/                     Deployment, PVC, Services, Secrets
└── waker/                         waker Deployment + ServiceMonitor
scripts/
├── bootstrap.sh                   one-shot installer (NAMESPACE/TRIGGER as env vars)
└── setup-trigger-auth.sh          (Prometheus path only) provisions the bearer-token Secret
docs/mc-waker-internals.md         walkthrough of the Go program
```

## Prerequisites

- This project has been tested on OpenShift 4.21.14, it should be fine with other version, though your mileage may vary.
- A user with cluster-admin privileges is needed for the initial install.
- The **Custom Metrics Autoscaler Operator** installed (OperatorHub → "Custom Metrics Autoscaler").
- For the Prometheus variant: cluster-wide [User Workload Monitoring](https://docs.openshift.com/container-platform/latest/observability/monitoring/enabling-monitoring-for-user-defined-projects.html) enabled.

## Build the waker image (manual / external registry)

`bootstrap.sh` runs `make ocp-build` for you, which pushes into the cluster's internal ImageStream. If you'd rather build locally and push to an external registry (quay.io, etc.):

```bash
cd build/mc-waker
make image IMG=quay.io/<you>/mc-waker:v0.1.0  &&  make push IMG=quay.io/<you>/mc-waker:v0.1.0
```

Update the `image:` field in `manifests/waker/mc-waker.yaml` to match, then run `./scripts/bootstrap.sh --skip-build`.

## Deploy

> Both Secrets in `manifests/minecraft/` ship with `CHANGEME!` placeholder values. Replace at least the Curseforge one if you want modpack downloads.

End-to-end install is one command:

```bash
# Defaults: NAMESPACE=minecraft, TRIGGER=metrics-api
./scripts/bootstrap.sh

# Override either:
NAMESPACE=acme TRIGGER=prometheus ./scripts/bootstrap.sh

# Render-only (no oc apply, useful for diff'ing or piping to oc diff):
./scripts/bootstrap.sh --dry-run

# Already pushed the waker image somewhere (and updated `image:` in
# manifests/waker/mc-waker.yaml)? Skip the in-cluster build:
./scripts/bootstrap.sh --skip-build
```

The script applies the `KedaController`, creates the namespace, runs `make ocp-build` to push the waker image into the namespace's ImageStream, applies the Minecraft + waker workloads, and applies the chosen `ScaledObject`. Run `./scripts/bootstrap.sh --help` to see every flag.

`mc-ragnarok` will scale to `0` within ~15s of being idle.

## Test

`bootstrap.sh` creates a TLS-terminated Route in front of the waker's HTTP API, so no port-forward is needed. Grab the URL once, then hit `/scaler`, `/wake`, or `/status`:

```bash
ROUTE="https://$(oc -n minecraft get route mc-ragnarok-waker -o jsonpath='{.spec.host}')"

curl -s   "$ROUTE/scaler"        # {"value":0}  when sleeping, {"value":1} when awake
curl -sX POST "$ROUTE/wake"      # manual wake — flips the metric to 1
curl -s   "$ROUTE/status" | jq   # full state JSON (upstream_up, players_online, wake_active, ...)

oc -n minecraft get scaledobject mc-ragnarok   # ACTIVE=True within ~15s of a wake
oc -n minecraft get deploy mc-ragnarok         # READY=2/2 shortly after
```

End-to-end: connect a Minecraft client to `<node-ip>:<nodePort>`. First *Refresh* shows "Ragnarok is Sleeping"; refresh again ~30s later for the live server.

## Tuning

CMA-side knobs live on the `ScaledObject` (`pollingInterval`, `cooldownPeriod`, `fallback`, the `behavior` block). Waker-side knobs are `WAKER_*` env vars in `manifests/waker/mc-waker.yaml` — the most demo-relevant is `WAKER_WAKE_HOLD` (default `5m`; drop to `30s` if you want to demo the cooldown faster).

## Cleanup

```bash
# Remove the workshop resources, keep the namespace (safe in shared projects):
./scripts/bootstrap.sh --uninstall

# Or, also delete the namespace itself:
./scripts/bootstrap.sh --uninstall --delete-namespace
```

The cluster-scoped `KedaController` (in `openshift-keda`) is left alone in either case — delete it manually if you no longer want CMA on the cluster.

## References

- OpenShift docs — [Custom Metrics Autoscaler Operator](https://docs.openshift.com/container-platform/latest/nodes/cma/nodes-cma-autoscaling-custom.html)
- KEDA docs — [`ScaledObject` concepts](https://keda.sh/docs/latest/concepts/scaling-deployments/), [`metrics-api` trigger](https://keda.sh/docs/latest/scalers/metrics-api/), [`prometheus` trigger](https://keda.sh/docs/latest/scalers/prometheus/)
