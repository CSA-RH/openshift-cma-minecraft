# openshift-cma-minecraft

> A scale-from-zero demo for the **OpenShift Custom Metrics Autoscaler** (CMA / KEDA), built around a Minecraft Java Edition server. The on-call workload sleeps when nobody's playing and wakes up the moment someone clicks *Refresh* in their server list.

---

## Table of contents

- [What it does](#what-it-does)
- [Topology](#topology)
- [Repo layout](#repo-layout)
- [Prerequisites](#prerequisites)
- [Build](#build)
- [Deploy — quick path (metrics-api trigger)](#deploy--quick-path-metrics-api-trigger)
- [Deploy — production-ish path (Prometheus trigger)](#deploy--production-ish-path-prometheus-trigger)
- [Rollback](#rollback)
- [Testing the demo](#testing-the-demo)
- [Tuning](#tuning)
- [Metrics published](#metrics-published)
- [Adding a second world later](#adding-a-second-world-later)
- [Further reading](#further-reading)

---

## What it does

A small "waker" service is inserted on the network path in front of the Minecraft pod and:

- Always listens on TCP **`25565`**.
- Forwards traffic to the real server when it's up (transparent proxy).
- When the server is at **0 replicas**, answers the Minecraft client's *Refresh* itself with a *"Server is sleeping"* MOTD and raises a wake signal.
- Probes the running server every 15s for the live player count via the Server List Ping (SLP) protocol.
- Exposes both a Prometheus `/metrics` endpoint and a tiny `/scaler` JSON endpoint.

A KEDA `ScaledObject` reads one of those metrics and scales the Minecraft Deployment between **0** and **1**.

---

## Topology

```text
              ┌────────────────────────────────────────────┐
              │ Player on the internet                     │
              └────────────────┬───────────────────────────┘
                               │ TCP 25565
                               ▼
       ┌───────────────────────────────────────────────────┐
       │ EXISTING NodePort Service "mc-ragnarok-nodeport"  │
       │   name, ClusterIP, nodePort UNCHANGED             │
       │   only its selector is repointed to the waker     │
       └────────────────┬──────────────────────────────────┘
                        │
                        ▼
              ┌──────────────────────────┐
              │ Pod  mc-ragnarok-waker   │  always 1 replica  (new Deployment)
              └────────────────┬─────────┘
                               │
                               ▼
       ┌───────────────────────────────────────────────────┐
       │ ClusterIP Service "mc-ragnarok-backend"  (new)    │
       └────────────────┬──────────────────────────────────┘
                        │
                        ▼
              ┌──────────────────────────┐
              │ Pod  mc-ragnarok         │  scaled 0..1 by KEDA  (existing)
              └──────────────────────────┘
```

> **Why this shape?** The existing NodePort Service object is preserved end-to-end: same name, same ClusterIP, same `nodePort`. Only its `selector` changes (one `oc patch`), so anything outside the cluster — router port-forwards, firewall rules, the bookmark in your Minecraft client — keeps working. From a player's point of view, nothing has moved.

---

## Repo layout

```text
openshift-cma-minecraft/
├── waker/                                 # Go source for the proxy + metrics service
│   ├── main.go config.go state.go
│   ├── proxy.go slp.go probe.go metrics.go
│   ├── slp_test.go
│   ├── Containerfile  Makefile  go.mod
│   └── .dockerignore
├── manifests/
│   ├── 00-waker.yaml                      # Deployment + ClusterIP Service for the waker
│   ├── 01-backend-service.yaml            # new internal Service the waker dials
│   ├── 10-servicemonitor.yaml             # (Prometheus path only) ServiceMonitor
│   ├── 20-scaledobject-metricsapi.yaml    # Variant A: KEDA metrics-api trigger
│   ├── 21-scaledobject-prometheus.yaml    # Variant B: KEDA prometheus trigger
│   ├── setup-trigger-auth.sh              # (Prometheus path only) RBAC + Secret
│   └── patch-existing-service.sh          # selector flip (and rollback)
├── docs/
│   └── waker-internals.md                 # how the Go program actually works
└── README.md
```

---

## Prerequisites

- An OpenShift cluster (4.12+).
- The **Custom Metrics Autoscaler Operator** installed — it ships the KEDA CRDs (`ScaledObject`, `TriggerAuthentication`, …).
- An existing Minecraft Deployment + NodePort Service in some namespace. The defaults assume:

  | Object | Name | Notes |
  |---|---|---|
  | Namespace | `minecraft` | |
  | Deployment | `mc-ragnarok` | pods carry label `app: mc-ragnarok` |
  | Service | `mc-ragnarok-nodeport` | type `NodePort`, selector `app: mc-ragnarok`, port `25565` |

  If any of these names differ, do a global find-and-replace on the manifests and the patch script.

Verify your setup matches:

```bash
oc -n minecraft get svc mc-ragnarok-nodeport -o jsonpath='{.spec.selector}{"\n"}'
oc -n minecraft get deploy mc-ragnarok -o jsonpath='{.spec.template.metadata.labels}{"\n"}'
```

---

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

---

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

The `mc-ragnarok` Deployment will scale to 0 within ~15s, assuming no players are connected and no wake is pending.

---

## Deploy — production-ish path (Prometheus trigger)

> Requires User Workload Monitoring to be enabled cluster-wide.

```bash
./manifests/setup-trigger-auth.sh minecraft

oc -n minecraft apply -f manifests/00-waker.yaml
oc -n minecraft apply -f manifests/01-backend-service.yaml
oc -n minecraft apply -f manifests/10-servicemonitor.yaml
oc -n minecraft apply -f manifests/21-scaledobject-prometheus.yaml

oc -n minecraft rollout status deploy/mc-ragnarok-waker
./manifests/patch-existing-service.sh
```

Pick **one** of `20-` or `21-`, not both.

---

## Rollback

The cutover is a single Service-selector change, so undoing it is one command:

```bash
./manifests/patch-existing-service.sh --rollback
```

The script restores the original selector it saved as an annotation on the Service the first time it ran. Traffic flows back to `mc-ragnarok` directly. You can then `oc delete -f manifests/` at your leisure.

---

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

End-to-end with a real client: connect your Minecraft client to the same `<node>:<nodePort>` you've always used. The first *Refresh* shows the "Sleeping (Ragnarok)" MOTD and triggers the wake. Refresh again ~30s later and you'll see the real server; clicking *Join* lands you on the live world.

---

## Tuning

All waker knobs are environment variables (see `00-waker.yaml`):

| Variable | Default | Controls |
|---|---|---|
| `WAKER_UPSTREAM` | `mc-ragnarok-backend.minecraft.svc.cluster.local:25565` | host:port of the real Minecraft Service |
| `WAKER_PROBE_INTERVAL` | `15s` | How often to query the running server for player count |
| `WAKER_DIAL_TIMEOUT` | `1500ms` | TCP dial timeout (proxy + probe) |
| `WAKER_WAKE_HOLD` | `5m` | Window during which `minecraft_wake_signal` stays at `1` after a wake |
| `WAKER_SLEEPING_MOTD` | *"Server is sleeping…"* | MOTD shown on the in-game server list |
| `WAKER_DISCONNECT_MSG` | *"Server is waking up…"* | Shown if a client tries to *Join* during startup |
| `WAKER_PROTOCOL_VERSION` | `769` | Minecraft protocol version advertised in the fake status (`769` = 1.21.4) |

The cooldown before scaling back to 0 is set on the `ScaledObject` (`cooldownPeriod`, default `60s` for the workshop; bump to `600s+` for real audiences).

---

## Metrics published

| Metric | Type | Meaning |
|---|---|---|
| `minecraft_players_online` | gauge | Live player count when up, `0` when down |
| `minecraft_wake_signal` | gauge | `1` while a wake is being held, else `0` |
| `minecraft_upstream_up` | gauge | `1` if the last SLP probe succeeded |
| `minecraft_proxy_active_connections` | gauge | In-flight TCP connections through the waker |
| `minecraft_wake_events_total` | counter | Total wake-ups triggered |
| `minecraft_proxy_opens_total` | counter | Total connections proxied to the upstream |
| `minecraft_desired_replicas` | gauge | `1` if the server should be running, else `0` |

`minecraft_desired_replicas` is what both ScaledObject variants drive on, so the trigger query stays a one-liner.

---

## Adding a second world later

The whole pattern is keyed on the world name. To add `mc-asgard` alongside `mc-ragnarok`:

1. Copy `manifests/` to `manifests-asgard/`.
2. Do a global rename of `mc-ragnarok` → `mc-asgard` inside it.
3. Apply.

Each world gets its own waker, its own backend Service, and its own ScaledObject. Nothing about `mc-ragnarok` is affected.

---

## Further reading

- **[`docs/waker-internals.md`](docs/waker-internals.md)** — a walkthrough of the Go source: three-goroutine architecture, the proxy decision tree, the slice of the SLP protocol we implement, and the design choices behind the metric.
