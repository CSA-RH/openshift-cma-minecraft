# openshift-cma-minecraft

A scale-from-zero demo for the **OpenShift Custom Metrics Autoscaler** (CMA / KEDA), built around an existing Minecraft Java Edition server (`mc-ragnarok`) without disrupting how it's currently exposed.

> This README is the working scaffolding — the "fancy markdown" pass will come later.

## What it does

A tiny "waker" service is inserted on the network path in front of the Minecraft pod and:

- Always listens on TCP `25565`.
- Forwards traffic to the real server when it's up.
- When the server has been scaled to **0 replicas**, answers the Minecraft client's *Refresh* itself with a "Server is sleeping" MOTD, and signals a wake-up.
- Probes the running server every 15s for the live player count via the Server List Ping (SLP) protocol.
- Exposes both Prometheus metrics (`/metrics`) and a tiny JSON scaler endpoint (`/scaler`).

A KEDA `ScaledObject` reads either of those metrics and scales the `mc-ragnarok` Deployment between 0 and 1.

## Topology

```
[Player on internet]
      |
      v
  EXISTING NodePort Service "mc-ragnarok"   <-- name, IP, nodePort UNCHANGED
      |  (selector flipped to point at the waker; see patch-existing-service.sh)
      v
  Pod mc-ragnarok-waker        <-- always 1 replica (new Deployment)
      |
      v
  ClusterIP Service "mc-ragnarok-backend"   <-- new, internal-only
      |
      v
  Pod mc-ragnarok              <-- existing Deployment, scaled 0..1 by KEDA
```

The existing `Service/mc-ragnarok` object is preserved end-to-end: same name, same ClusterIP, same `nodePort` value, same external endpoint that any router or firewall rule already points at. **Only its `selector` changes** (one `oc patch`) so traffic now lands on the waker pod instead of directly on the minecraft pod. From a player's point of view nothing has moved.

## Repo layout

```
openshift-cma-minecraft/
  waker/                                  # Go source for the proxy + metrics service
    main.go config.go state.go
    proxy.go slp.go probe.go metrics.go
    slp_test.go
    Containerfile  Makefile  go.mod
  manifests/
    00-waker.yaml                         # Deployment + ClusterIP Service for the waker
    01-backend-service.yaml               # NEW internal Service the waker dials
    10-servicemonitor.yaml                # (Prometheus path only) ServiceMonitor
    20-scaledobject-metricsapi.yaml       # Variant A: KEDA metrics-api trigger
    21-scaledobject-prometheus.yaml       # Variant B: KEDA prometheus trigger
    setup-trigger-auth.sh                 # (Prometheus path only) RBAC + Secret
    patch-existing-service.sh             # selector flip (and rollback)
  docs/                                   # placeholder for the polished workshop docs
```

## Prerequisites

- An OpenShift cluster (4.12+).
- The **Custom Metrics Autoscaler Operator** installed (it ships KEDA's CRDs: `ScaledObject`, `TriggerAuthentication`, …).
- Your existing setup, assumed to be:
  - Namespace: `minecraft`
  - Deployment: `mc-ragnarok` whose pods carry the label `app: mc-ragnarok`
  - Service: `mc-ragnarok` of type `NodePort`, selector `app: mc-ragnarok`, port `25565`
  
  If any of these names differ, do a global find-and-replace on the manifests and the patch script.

Confirm the assumption above with:

```bash
oc -n minecraft get svc mc-ragnarok -o jsonpath='{.spec.selector}{"\n"}'
oc -n minecraft get deploy mc-ragnarok -o jsonpath='{.spec.template.metadata.labels}{"\n"}'
```

## Build

```bash
cd waker

# Local build with podman/docker:
make image IMG=quay.io/<you>/mc-waker:v0.1.0
make push  IMG=quay.io/<you>/mc-waker:v0.1.0

# Or build inside the cluster (no external registry needed):
make ocp-build NAMESPACE=minecraft
```

Then update `manifests/00-waker.yaml` so the `image:` field points at the result.

## Deploy — quick path (metrics-api trigger)

```bash
oc -n minecraft apply -f manifests/00-waker.yaml
oc -n minecraft apply -f manifests/01-backend-service.yaml
oc -n minecraft apply -f manifests/20-scaledobject-metricsapi.yaml

# Wait until the waker pod is Ready, then flip the selector on the
# existing NodePort Service:
oc -n minecraft rollout status deploy/mc-ragnarok-waker
./manifests/patch-existing-service.sh
```

The `mc-ragnarok` Deployment will scale to 0 within ~15s (assuming no players are connected and no wake is pending).

## Deploy — production-ish path (Prometheus trigger)

```bash
# Prereq: User Workload Monitoring is enabled cluster-wide.
./manifests/setup-trigger-auth.sh minecraft

oc -n minecraft apply -f manifests/00-waker.yaml
oc -n minecraft apply -f manifests/01-backend-service.yaml
oc -n minecraft apply -f manifests/10-servicemonitor.yaml
oc -n minecraft apply -f manifests/21-scaledobject-prometheus.yaml

oc -n minecraft rollout status deploy/mc-ragnarok-waker
./manifests/patch-existing-service.sh
```

Pick *one* of `20-` or `21-`, not both.

## Rollback

The cutover is a single Service-selector change, so undoing it is one command:

```bash
./manifests/patch-existing-service.sh --rollback
```

That restores the original selector that the script saved as an annotation on the Service the first time it ran. Traffic immediately flows back to `mc-ragnarok` directly. You can then `oc delete -f manifests/` at your leisure.

## Testing the demo

While `mc-ragnarok` is at 0 replicas, exercise the scaler from inside the cluster:

```bash
oc -n minecraft port-forward svc/mc-ragnarok-waker 8080:8080 &
curl -s localhost:8080/scaler            # -> {"value":0}
curl -s localhost:8080/status            # JSON with full state
curl -sX POST localhost:8080/wake        # signal a manual wake
curl -s localhost:8080/scaler            # -> {"value":1}
# ...the ScaledObject scales the mc-ragnarok Deployment to 1 within ~15s.
```

End-to-end with a real client: just connect your Minecraft client to the same `<node>:<nodePort>` you've always used. The first *Refresh* shows the "Sleeping (Ragnarok)" MOTD and triggers the wake. Refresh again ~30s later and you'll see the real server; clicking *Join* will land you on the live world.

## Tuning

All knobs are environment variables on the waker (see `00-waker.yaml`):

| Var | Default | What it controls |
|---|---|---|
| `WAKER_UPSTREAM` | `mc-ragnarok-backend.minecraft.svc.cluster.local:25565` | host:port of the real Minecraft Service |
| `WAKER_PROBE_INTERVAL` | `15s` | How often to query the running server for player count |
| `WAKER_DIAL_TIMEOUT` | `1500ms` | TCP dial timeout (proxy + probe) |
| `WAKER_WAKE_HOLD` | `5m` | Window during which `minecraft_wake_signal` stays at 1 after a wake |
| `WAKER_SLEEPING_MOTD` | "Server is sleeping…" | MOTD shown on the in-game server list |
| `WAKER_DISCONNECT_MSG` | "Server is waking up…" | Shown if a client tries to *Join* during startup |
| `WAKER_PROTOCOL_VERSION` | `769` | Minecraft protocol version advertised in the fake status (769 = 1.21.4) |

The cooldown before scaling back to 0 is set on the `ScaledObject` (`cooldownPeriod`, default 600s).

## Metrics published

```
minecraft_players_online              # gauge: live player count when up, 0 when down
minecraft_wake_signal                 # gauge: 1 while a wake is being held, else 0
minecraft_upstream_up                 # gauge: 1 if last SLP probe succeeded
minecraft_proxy_active_connections    # gauge: in-flight TCP connections through the waker
minecraft_wake_events_total           # counter: total wake-ups triggered
minecraft_proxy_opens_total           # counter: total connections proxied to upstream
minecraft_desired_replicas            # gauge: 1 if the server should be running, else 0
```

`minecraft_desired_replicas` is what both ScaledObject variants drive on, so the trigger query stays a one-liner.

## Adding a second world later

The whole pattern is keyed on the world name. To add `mc-asgard` alongside `mc-ragnarok`, copy `manifests/` to `manifests-asgard/`, do a global rename of `mc-ragnarok` → `mc-asgard`, and apply. Each world gets its own waker, its own backend Service, and its own ScaledObject.
