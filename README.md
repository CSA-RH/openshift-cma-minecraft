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
├── bootstrap.sh                   (WIP) one-shot installer
└── setup-trigger-auth.sh          (Prometheus path only) provisions the bearer-token Secret
docs/mc-waker-internals.md         walkthrough of the Go program
```

## Prerequisites

- OpenShift 4.12+ with cluster-admin for the initial install.
- The **Custom Metrics Autoscaler Operator** installed (OperatorHub → "Custom Metrics Autoscaler").
- For the Prometheus variant: cluster-wide [User Workload Monitoring](https://docs.openshift.com/container-platform/latest/observability/monitoring/enabling-monitoring-for-user-defined-projects.html) enabled.

## Build the waker image

```bash
cd build/mc-waker
make image IMG=quay.io/<you>/mc-waker:v0.1.0  &&  make push IMG=quay.io/<you>/mc-waker:v0.1.0
# or, in-cluster build (no external registry needed):
make ocp-build NAMESPACE=minecraft
```

Then update the `image:` field in `manifests/waker/mc-waker.yaml` to match.

## Deploy

> Both Secrets in `manifests/minecraft/` ship with `CHANGEME!` placeholder values. Replace at least the Curseforge one if you want modpack downloads.

```bash
# 0. Cluster-wide (one-time)
oc apply -f manifests/keda/kedacontroller.yaml

# 1. Namespace + workload
oc new-project minecraft
oc apply -f manifests/minecraft/
oc rollout status deploy/mc-ragnarok

# 2. Waker
oc apply -f manifests/waker/mc-waker.yaml
oc rollout status deploy/mc-ragnarok-waker

# 3. Pick ONE ScaledObject variant
oc apply -f manifests/keda/scaledobject-metricsapi.yaml
# or:
./scripts/setup-trigger-auth.sh minecraft
oc apply -f manifests/waker/servicemonitor.yaml
oc apply -f manifests/keda/scaledobject-prometheus.yaml
```

`mc-ragnarok` will scale to `0` within ~15s of being idle.

## Test

```bash
# From inside the cluster — no Minecraft client needed:
oc -n minecraft port-forward svc/mc-ragnarok-waker 8080:8080 &
curl -s   localhost:8080/scaler          # {"value":0}
curl -sX POST localhost:8080/wake        # manual wake
oc -n minecraft get scaledobject mc-ragnarok   # ACTIVE=True within 15s
oc -n minecraft get deploy mc-ragnarok         # READY=1/1 shortly after
```

End-to-end: connect a Minecraft client to `<node-ip>:<nodePort>`. First *Refresh* shows "Ragnarok is Sleeping"; refresh again ~30s later for the live server.

## Tuning

CMA-side knobs live on the `ScaledObject` (`pollingInterval`, `cooldownPeriod`, `fallback`, the `behavior` block). Waker-side knobs are `WAKER_*` env vars in `manifests/waker/mc-waker.yaml` — the most demo-relevant is `WAKER_WAKE_HOLD` (default `5m`; drop to `30s` if you want to demo the cooldown faster).

## Cleanup

```bash
oc delete -f manifests/keda/   --ignore-not-found
oc delete -f manifests/waker/  --ignore-not-found
oc delete project minecraft
```

## References

- OpenShift docs — [Custom Metrics Autoscaler Operator](https://docs.openshift.com/container-platform/latest/nodes/cma/nodes-cma-autoscaling-custom.html)
- KEDA docs — [`ScaledObject` concepts](https://keda.sh/docs/latest/concepts/scaling-deployments/), [`metrics-api` trigger](https://keda.sh/docs/latest/scalers/metrics-api/), [`prometheus` trigger](https://keda.sh/docs/latest/scalers/prometheus/)
