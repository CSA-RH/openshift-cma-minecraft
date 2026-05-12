# OpenShift Custom Metrics Autoscaler — Workshop

> A hands-on workshop on the **OpenShift Custom Metrics Autoscaler** (CMA / KEDA). You'll scale a real workload from **0 to 1 and back** using a metric that the workload itself cannot report — exercising every interesting corner of the CMA's `ScaledObject` resource.

> **Scenario.** A Minecraft Java Edition server (`mc-ragnarok`) that sleeps when nobody's playing and wakes the moment a player clicks *Refresh* in their server list. Minecraft is a deliberate pick: it's a TCP protocol (so no HTTP shortcuts), and a workload at 0 replicas obviously can't report "someone is trying to reach me" — which is exactly where the CMA pattern earns its keep.

---

## Table of contents

- [What you'll learn](#what-youll-learn)
- [The CMA event flow](#the-cma-event-flow)
- [`ScaledObject` mechanics, walked through](#scaledobject-mechanics-walked-through)
- [Two trigger flavors — and when to pick which](#two-trigger-flavors--and-when-to-pick-which)
- [Topology of the workshop environment](#topology-of-the-workshop-environment)
- [The auxiliary waker service](#the-auxiliary-waker-service)
- [Prerequisites](#prerequisites)
- [Build the waker image](#build-the-waker-image)
- [Deploy the workshop](#deploy-the-workshop)
- [Rollback](#rollback)
- [Testing the demo](#testing-the-demo)
- [Tuning](#tuning)
- [Adding a second world later](#adding-a-second-world-later)
- [Further reading](#further-reading)

---

## What you'll learn

By the end of the workshop you will have hands-on experience with:

- **The `ScaledObject` resource** — its lifecycle, reconciliation, and how it integrates with Kubernetes HPA.
- **Custom triggers** — the `metrics-api` and `prometheus` trigger types, with two complete deployments to compare.
- **`threshold` vs `activationThreshold`** — the most commonly misunderstood pair of fields in KEDA, and the rule that prevents a 0/1 metric from "getting stuck at zero".
- **Scale-to-zero semantics** — `minReplicaCount: 0`, `cooldownPeriod`, and how a workload returns from zero.
- **HPA integration** — `advanced.horizontalPodAutoscalerConfig.behavior` for shaping stabilization windows.
- **`fallback`** — defining safe behavior when the metric source itself goes away.
- **Sourcing a custom metric from outside the scaled workload** — the architectural reason CMA exists at all.

---

## The CMA event flow

The hero diagram of the workshop. Read top to bottom; this is one full sleep → wake → sleep cycle:

```text
┌──────────────────────────────────────────────────────────────────────────┐
│                                                                          │
│   t=0      mc-ragnarok @ replicas=0                                      │
│            waker reports metric = 0                                      │
│            ScaledObject:  value <= activationThreshold(0) -> INACTIVE    │
│                                                                          │
│   t=5s     Player opens client → Refresh → SLP request hits the waker    │
│            waker responds with "Sleeping" MOTD                           │
│            waker bumps wakeUntil = now + 5m  →  metric = 1               │
│                                                                          │
│   t=20s    ScaledObject polls every 15s, observes metric = 1             │
│            value > activationThreshold → ACTIVE                          │
│            HPA: desired = ceil(metric / threshold) = ceil(1/1) = 1       │
│            mc-ragnarok scales 0 -> 1                                     │
│                                                                          │
│   t=50s    JVM finishes startup, binds 25565                             │
│            waker's SLP probe succeeds → upstream_up = true               │
│            metric stays at 1 (wake-hold not yet expired)                 │
│                                                                          │
│   t=60s    Player clicks Refresh again → waker now PROXIES → real MOTD   │
│            Player clicks Join → real game session begins                 │
│                                                                          │
│   t=...    players_online > 0 → metric stays 1 regardless of wake-hold   │
│                                                                          │
│   t=L      Last player disconnects.  After wakeHoldFor expires:          │
│            players_online = 0 AND wake_signal = 0  →  metric = 0         │
│                                                                          │
│   t=L+60s  cooldownPeriod elapses. ScaledObject scales 1 -> 0.           │
│            We are back where we started.                                 │
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
```

Three CMA primitives govern the timing in that diagram: `pollingInterval` (how often the trigger is evaluated), `activationThreshold` (the gate that decides 0 vs non-zero), and `cooldownPeriod` (how long the metric must stay at 0 before scaling back down). Everything else is plumbing.

---

## `ScaledObject` mechanics, walked through

A `ScaledObject` says, in plain English: *"Watch this metric. Translate its value into a replica count for this Deployment. Don't go below this floor, don't go above this ceiling, and be patient about both directions."*

The replica-count math is HPA's, not KEDA's. Given a metric value `M` and a threshold `T`:

```
desiredReplicas = ceil(M / T)
```

Clamped between `minReplicaCount` and `maxReplicaCount`. So with our 0/1 metric and `threshold: "1"`, the result is always either `0` or `1` — perfect for this demo.

But HPA on its own can't go below 1. KEDA's contribution to scale-to-zero is a separate decision *layer* on top:

```
if metric <= activationThreshold:  replicas = 0          (KEDA decides)
else:                              replicas = HPA(metric)  (HPA decides)
```

That's why a 0/1 metric **requires `activationThreshold: "0"`**. With any higher value, the metric — which never exceeds 1 — would never satisfy "strictly greater than activationThreshold", and the workload would never come back from zero.

The other knobs you'll touch:

| Field | What it does | Demo value | Notes |
|---|---|---|---|
| `minReplicaCount` | Floor that KEDA respects | `0` | The whole point of CMA |
| `maxReplicaCount` | Ceiling | `1` | Minecraft has a single replica by nature |
| `pollingInterval` | How often the trigger is evaluated | `15` (seconds) | Lower = snappier; higher = quieter |
| `cooldownPeriod` | Seconds at value=0 before scaling down to `minReplicaCount` | `60` | Workshop default; bump to 600+ in production |
| `fallback.replicas` | Replicas to use if the trigger itself fails for `failureThreshold` consecutive polls | `1` | Fail open: a broken waker doesn't black-hole the server |
| `advanced.horizontalPodAutoscalerConfig.behavior.scaleDown.stabilizationWindowSeconds` | HPA's own debouncing on scale-down | `60` | Composes with `cooldownPeriod` |

---

## Two trigger flavors — and when to pick which

The repo ships two complete `ScaledObject` manifests. They scale on the exact same number (`minecraft_desired_replicas`), just sourced differently. Apply **one** of them.

| | `metrics-api` (file `20-…yaml`) | `prometheus` (file `21-…yaml`) |
|---|---|---|
| Where the metric comes from | Direct HTTP scrape of `/scaler` on the waker | The cluster's Prometheus (OpenShift UWM via Thanos Querier) |
| Cluster prerequisites | None | User Workload Monitoring must be enabled |
| Auth | None | Bearer token + CA bundle (`setup-trigger-auth.sh` provisions both) |
| Best for | Workshops, dev clusters, offline demos | Production deployments, environments already on UWM |
| What it teaches | Simplest possible custom-metric integration | The real-world pattern most customers will use |

The workshop will run start-to-finish with either. Most attendees will get more out of switching from `20-` to `21-` partway through, to see both flavors live.

---

## Topology of the workshop environment

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
              │ Pod  mc-ragnarok-waker   │  always 1 replica
              │ publishes the metric ────┼──► ScaledObject (CMA)
              └────────────────┬─────────┘                │
                               │                          │ scales
                               ▼                          ▼
       ┌───────────────────────────────────────────────────┐
       │ ClusterIP Service "mc-ragnarok-backend"           │
       └────────────────┬──────────────────────────────────┘
                        │
                        ▼
              ┌──────────────────────────┐
              │ Pod  mc-ragnarok         │  scaled 0..1 by KEDA
              └──────────────────────────┘
```

The existing `mc-ragnarok-nodeport` Service object is preserved end-to-end — only its `selector` changes, so nothing outside the cluster (router port-forwards, firewall rules, the bookmark in the Minecraft client) needs to move. The full reasoning is in *Cutover and rollback* below.

---

## The auxiliary waker service

The waker is a small Go program that publishes the custom metric the `ScaledObject` reads. It is *not* the topic of the workshop — but it is what makes the topic demonstrable, because the workload that gets scaled (the Minecraft pod itself) cannot report "someone is trying to reach me" while it's at zero replicas. That observation has to come from outside.

In short, the waker:

1. Sits on TCP `25565` (the NodePort traffic lands on it).
2. Forwards bytes when `mc-ragnarok` is up; otherwise speaks just enough of the Minecraft Server List Ping protocol to answer with a *"Sleeping"* MOTD and raise a wake signal.
3. Periodically probes the upstream for the live player count.
4. Exposes `/metrics` (Prometheus) and `/scaler` (JSON) — the metric `minecraft_desired_replicas` is what both `ScaledObject` variants drive on.

Treat it as a black box for the workshop's narrative. If you do want to read the source, the full walkthrough is in **[`docs/waker-internals.md`](docs/waker-internals.md)** — three goroutines, a small slice of the SLP wire protocol, and the decision tree that picks between proxy-mode and sleeping-mode.

---

## Prerequisites

- An OpenShift cluster (4.12+).
- The **Custom Metrics Autoscaler Operator** installed — it ships the KEDA CRDs (`ScaledObject`, `TriggerAuthentication`, `KedaController`, …) and a default `KedaController` in `openshift-keda`.
- An existing Minecraft Deployment + NodePort Service in some namespace. The defaults assume:

  | Object | Name | Notes |
  |---|---|---|
  | Namespace | `minecraft` | |
  | Deployment | `mc-ragnarok` | pods carry label `app: mc-ragnarok` |
  | Service | `mc-ragnarok-nodeport` | type `NodePort`, selector `app: mc-ragnarok`, port `25565` |

Verify:

```bash
oc -n minecraft get svc mc-ragnarok-nodeport -o jsonpath='{.spec.selector}{"\n"}'
oc -n minecraft get deploy mc-ragnarok -o jsonpath='{.spec.template.metadata.labels}{"\n"}'
```

---

## Build the waker image

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

## Deploy the workshop

### Path A — `metrics-api` trigger (recommended for the workshop)

```bash
oc -n minecraft apply -f manifests/00-waker.yaml
oc -n minecraft apply -f manifests/01-backend-service.yaml
oc -n minecraft apply -f manifests/20-scaledobject-metricsapi.yaml

# Wait until the waker pod is Ready, then flip the selector on the
# existing NodePort Service:
oc -n minecraft rollout status deploy/mc-ragnarok-waker
./manifests/patch-existing-service.sh
```

`mc-ragnarok` will scale to 0 within ~15s, assuming no players are connected and no wake is pending.

### Path B — `prometheus` trigger (production-leaning variant)

Same flow plus three extra steps to wire up Prometheus auth and the ServiceMonitor:

```bash
./manifests/setup-trigger-auth.sh minecraft

oc -n minecraft apply -f manifests/00-waker.yaml
oc -n minecraft apply -f manifests/01-backend-service.yaml
oc -n minecraft apply -f manifests/10-servicemonitor.yaml
oc -n minecraft apply -f manifests/21-scaledobject-prometheus.yaml

oc -n minecraft rollout status deploy/mc-ragnarok-waker
./manifests/patch-existing-service.sh
```

Pick **one** path, not both.

---

## Rollback

The cutover is a single Service-selector change, so undoing it is one command:

```bash
./manifests/patch-existing-service.sh --rollback
```

The script restores the original selector it saved as an annotation on the Service the first time it ran. Traffic flows back to `mc-ragnarok` directly. You can then `oc delete -f manifests/` at your leisure.

---

## Testing the demo

Two ways. Either exercise the `ScaledObject` directly from inside the cluster (great for showing what KEDA reacts to without needing a Minecraft client on hand):

```bash
oc -n minecraft port-forward svc/mc-ragnarok-waker 8080:8080 &
curl -s localhost:8080/scaler                 # -> {"value":0}
curl -sX POST localhost:8080/wake             # signal a manual wake
curl -s localhost:8080/scaler                 # -> {"value":1}
oc -n minecraft get scaledobject mc-ragnarok  # ACTIVE=True within ~15s
oc -n minecraft get deploy mc-ragnarok        # READY=1/1 shortly after
```

Or end-to-end with a real client: connect your Minecraft client to the same `<node>:<nodePort>` you've always used. The first *Refresh* shows the *"Sleeping (Ragnarok)"* MOTD and triggers the wake. Refresh again ~30s later and you'll see the real server; clicking *Join* lands you on the live world.

---

## Tuning

### CMA-side (the workshop's actual subject)

All set on the `ScaledObject`:

| Field | Demo value | Production value | What it changes |
|---|---|---|---|
| `pollingInterval` | `15` | `15–30` | Time between trigger evaluations |
| `cooldownPeriod` | `60` | `600+` | Seconds at value=0 before scaling down |
| `fallback.failureThreshold` | `3` | `3–5` | How many failed polls before fail-open |
| `fallback.replicas` | `1` | `1` | Fail-open replica count |
| `advanced.horizontalPodAutoscalerConfig.behavior.scaleDown.stabilizationWindowSeconds` | `60` | `300+` | HPA's debouncing on the way down |

### Waker-side (the auxiliary service)

Less interesting for the workshop, but worth knowing. Full list of `WAKER_*` env vars is in **[`docs/waker-internals.md`](docs/waker-internals.md)**. The most relevant ones during a live demo are:

| Variable | Default | Why you might change it |
|---|---|---|
| `WAKER_WAKE_HOLD` | `5m` | How long a single wake event holds the metric at 1. Reduce to ~`30s` to make the cooldown easier to demo. |
| `WAKER_PROBE_INTERVAL` | `15s` | How often the waker checks the upstream for the live player count. |

---

## Adding a second world later

The whole pattern is keyed on the world name. To add `mc-asgard` alongside `mc-ragnarok`:

1. Copy `manifests/` to `manifests-asgard/`.
2. Do a global rename of `mc-ragnarok` → `mc-asgard` inside it.
3. Apply.

Each world gets its own waker, its own backend Service, and its own `ScaledObject`. Nothing about `mc-ragnarok` is affected.

---

## Further reading

- **OpenShift docs — [Custom Metrics Autoscaler Operator](https://docs.openshift.com/container-platform/latest/nodes/cma/nodes-cma-autoscaling-custom.html)**
- **KEDA docs — [Concepts: ScaledObject](https://keda.sh/docs/latest/concepts/scaling-deployments/)**, [Triggers: metrics-api](https://keda.sh/docs/latest/scalers/metrics-api/), [Triggers: prometheus](https://keda.sh/docs/latest/scalers/prometheus/)
- **[`docs/waker-internals.md`](docs/waker-internals.md)** — the Go program that publishes the custom metric (three-goroutine architecture, the SLP wire protocol, the metric callback design).
