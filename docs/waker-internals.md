# mc-waker — how the Go program works

> A walkthrough of the waker's source, aimed at someone reading the code for the first time. The whole thing is ~950 lines split across eight files in `waker/`; this doc explains *why* each piece exists and how they fit together.

---

## Table of contents

- [The job in one sentence](#the-job-in-one-sentence)
- [Three goroutines, one shared `state`](#three-goroutines-one-shared-state)
- [`proxy.go` — the TCP decision tree](#proxygo--the-tcp-decision-tree)
- [`slp.go` — Minecraft's wire protocol, the small slice we need](#slpgo--minecrafts-wire-protocol-the-small-slice-we-need)
- [`probe.go` — the boring 30-line file](#probego--the-boring-30-line-file)
- [`metrics.go` — Prometheus + the scaler endpoint](#metricsgo--prometheus--the-scaler-endpoint)
- [`config.go` — flags + env, one struct out](#configgo--flags--env-one-struct-out)
- [The wake-signal "hold" — small but load-bearing](#the-wake-signal-hold--small-but-load-bearing)
- [Why scale-to-zero needs the waker in its own Deployment](#why-scale-to-zero-needs-the-waker-in-its-own-deployment)
- [Tests](#tests)
- [Recommended reading order](#recommended-reading-order)

---

## The job in one sentence

Be the always-on thing that sits in front of a sleeping Minecraft pod, so that when a player tries to connect there's *something* on TCP `25565` to either (a) forward their bytes to the live server, or (b) answer them politely and trigger the scale-up.

---

## Three goroutines, one shared `state`

`main.go` is tiny on purpose — it just builds a `state`, then launches three concurrent loops that share it:

```go
go runProbe(ctx, st)   // periodic SLP probe of the real server
go runHTTP(ctx, st)    // /metrics, /scaler, /status, /wake on :8080
runProxy(ctx, st)      // TCP listener on :25565 (blocks; this is the main goroutine)
```

Everything those three loops need to know about each other lives on a single struct in `state.go`:

```go
type state struct {
    upMu          sync.RWMutex   // protects upstreamUp / playersOnline / lastProbeAt
    upstreamUp    bool
    playersOnline int
    lastProbeAt   time.Time

    wakeMu        sync.Mutex     // protects wakeUntil
    wakeUntil     time.Time

    activeConns   atomic.Int64   // counters: cheap, lock-free
    wakeEvents    atomic.Int64
    proxyOpens    atomic.Int64
    proxyErrors   atomic.Int64
}
```

Two design choices worth flagging:

- **Two separate mutexes.** Upstream readiness is read on *every* TCP accept, so it gets an `RWMutex` (cheap parallel reads). The wake deadline gets a regular `Mutex` because it's only touched on wake events and on every metric scrape — low contention.
- **Atomic counters for stats.** No locking needed for "how many connections have we ever proxied?" — these are write-rarely, read-rarely numbers that just need to be monotonic, so `atomic.Int64` is enough.

---

## `proxy.go` — the TCP decision tree

This is the only file that talks to players. `runProxy` does `net.Listen("tcp", ":25565")` and spawns a goroutine per accepted connection. The interesting code is `handleConn`:

```go
up, _, age := s.getUpstream()
upstreamReady := up && age < 2*s.cfg.ProbeInterval

if upstreamReady {
    upstream, err := net.DialTimeout("tcp", s.cfg.UpstreamAddr, s.cfg.DialTimeout)
    if err == nil {
        s.proxyOpens.Add(1)
        defer upstream.Close()
        proxyBidir(client, upstream)
        return
    }
    s.setUpstream(false, 0)   // cached state was a lie; fall through
}

s.signalWake()
handleSleepingClient(client, s.cfg)
```

There are two ways a connection gets handled:

**Path A — transparent proxy.** The probe says the upstream is up *and* the last probe is recent. We dial the real server and run `proxyBidir`, which is just two `io.Copy` goroutines (one in each direction) racing to EOF. When the first one finishes, we `CloseWrite()` on the still-open side so the peer sees EOF and doesn't hang forever, then wait up to 2s for the other half to drain.

**Path B — sleeping handler.** The probe says the upstream is down (or the cached state is stale, or Path A's dial failed). We bump the wake deadline and hand the connection to `handleSleepingClient` in `slp.go`, which speaks just enough of the Minecraft protocol to send back a fake "server-list" entry.

> **Why not always dial first?** Because during pod startup the kernel accepts TCP on 25565 *before* the JVM has bound its socket — we'd connect, proxy, and the player would hang. Trusting the cached probe (which checks via SLP, not raw TCP) avoids that.

---

## `slp.go` — Minecraft's wire protocol, the small slice we need

This is the meatiest file (~310 lines) but conceptually small. It implements just three things from the [Java Edition Server List Ping protocol](https://wiki.vg/Server_List_Ping):

1. **Wire helpers** — `VarInt` encoding (variable-length integer, 7 data bits per byte, MSB = "more bytes coming"), length-prefixed strings, and the packet framing rule (`VarInt length | VarInt packetID | payload`).
2. **Handshake parsing** — every client connection starts with a `Handshake` packet that tells us whether the next phase is *Status* (server-list refresh) or *Login* (player trying to join).
3. **Two response paths** — `handleSleepingStatus` answers a status request with our fake "Sleeping" MOTD and echoes the optional Ping/Pong so the server-list shows a believable latency; `handleSleepingLogin` answers a login attempt with a kick-message disconnect.

### Why the protocol detour matters

We don't *need* to fake an SLP response. We could just close the TCP connection and rely on retry. But two things make the SLP path much better UX:

- The Minecraft client's *Refresh* button works visually. The player sees a server, with a custom MOTD, in their server list — they know it exists and is sleeping, not down.
- It gives us a clean, lightweight wake trigger. "Client did an SLP refresh" is a much better signal than "TCP connect to 25565 failed", because nothing else in the wild does SLP — there's no port-scanner background noise to filter out.

### The probe

`probeUpstream(addr, timeout)` lives in this file too — it's a complete client-side SLP round trip:

```text
TCP dial
    -> send Handshake (next state = 1 / Status)
    -> send Status Request (empty payload)
    <- read Status Response (JSON in a length-prefixed string)
    parse out players.online
```

> **A subtle bug we hit during the workshop test (now fixed).** Different servers serialize the JSON `description` field in incompatible shapes (plain string, `{"text": "..."}`, or a full chat-component tree). Trying to parse it all with one struct fails on whichever shape isn't covered. The probe now uses a minimal struct that only reads `players.online` and ignores everything else:
>
> ```go
> var minimal struct {
>     Players struct {
>         Online int `json:"online"`
>     } `json:"players"`
> }
> ```
>
> A good reminder for any code that consumes loosely-typed JSON from third-party servers: parse only what you actually need.

---

## `probe.go` — the boring 30-line file

A `time.Ticker`-driven loop that calls `probeUpstream` every `ProbeInterval` (default 15s) and writes the result into `state` via `setUpstream`. Probes immediately on startup so the first metric scrape isn't a lie.

That's it. It could be inlined into `state.go`, but it's its own file because conceptually it owns one of the three goroutines.

---

## `metrics.go` — Prometheus + the scaler endpoint

Two surfaces here.

### The Prometheus surface (`/metrics`)

Uses `prometheus.NewGaugeFunc` / `NewCounterFunc`. The "Func" variant is important — instead of having to push updates into a metric whenever state changes, the metric is a *callback* that the Prometheus library invokes on each scrape:

```go
players := prometheus.NewGaugeFunc(prometheus.GaugeOpts{...},
    func() float64 {
        up, p, _ := s.getUpstream()
        if !up { return 0 }
        return float64(p)
    })
```

This means there's exactly one source of truth (`state`) and the metrics layer just reads from it — no risk of metrics drifting from reality.

The most important metric is `minecraft_desired_replicas`, a synthetic 0/1 gauge computed by `desiredReplicas(s)`:

```go
func desiredReplicas(s *state) int {
    up, p, _ := s.getUpstream()
    if (up && p > 0) || s.wakeActive() {
        return 1
    }
    return 0
}
```

That's the entire scaling decision, condensed into one function. Both ScaledObject variants (`metrics-api` and `prometheus`) trigger on this number.

### The plain-HTTP surface

Four endpoints used by humans and KEDA's `metrics-api` trigger:

| Endpoint | Used by | Returns |
|---|---|---|
| `/scaler` | KEDA `metrics-api` trigger | `{"value": 0 \| 1}` |
| `/status` | humans (curl/debug) | JSON snapshot of `state` |
| `/wake` (POST) | demos, "wake-the-server" buttons | bumps the wake deadline |
| `/healthz`, `/readyz` | Kubernetes probes | `200 OK` |

`/scaler` and `minecraft_desired_replicas` both call the same `desiredReplicas(s)` helper, so the two scaling paths are guaranteed identical — there's no way for them to drift apart.

---

## `config.go` — flags + env, one struct out

Standard `flag` package wiring. Each tunable is exposed as both a command-line flag *and* an environment variable (`WAKER_*`). Env wins by being the flag's default, so you can override it in the Deployment without touching the container args. The helpers `envOr`, `envDur`, `envInt`, `parseLogLevel` are mechanical.

The `LogLevel` field is mapped to `slog.Level` and fed into `slog.NewJSONHandler` back in `main.go`, so the whole program logs structured JSON to stdout — easy to grep, easy for OpenShift's log aggregator.

---

## The wake-signal "hold" — small but load-bearing

When `signalWake()` fires, we set `wakeUntil = now + WakeHoldFor` (default 5 min). While `time.Now().Before(wakeUntil)` is true, `minecraft_wake_signal` reports `1`.

Why a five-minute hold? Several races collide if it's too short:

- KEDA's `pollingInterval` is 15s — it might not see a transient `1` between scrapes.
- The Minecraft pod's cold-start is 30–60s.
- HPA has a "scale-down stabilization window" (we set 60s) before it'll reduce replicas.

A five-minute hold guarantees that one wake event reliably scales the workload up, the pod fully starts, *and* at least one player has time to actually connect — at which point `minecraft_players_online > 0` takes over as the reason to stay scaled up. After both signals go back to zero, the `cooldownPeriod` on the ScaledObject (60s in the demo, 600s+ in production) is what eventually triggers scale-to-zero.

---

## Why scale-to-zero needs the waker in its own Deployment

This is the architectural insight that drives everything else. If the waker were a sidecar inside the Minecraft pod, scaling that pod to zero would also kill the waker — nothing left to receive the player's TCP connection, nothing to signal the scale-up, dead loop. So the waker is its own one-replica Deployment that is *never* scaled by KEDA. The thing KEDA scales is the Minecraft Deployment itself, between 0 and 1, driven by a metric the waker publishes about something it observes externally (probe + connection attempts).

The Minecraft pod has no idea any of this is happening. It just runs, gets traffic, or doesn't. That's deliberate — it means the same setup works for any TCP service, not just Minecraft, with only `slp.go` swapped out for whatever protocol-fronting code that service needs.

---

## Tests

`slp_test.go` covers the wire format — VarInts and length-prefixed strings round-trip, packets parse back to what they came from, and `buildSleepingStatus` produces JSON containing the configured MOTD. Run with `go test ./...` in `waker/`. They're not exhaustive, but they catch the kind of bug that would otherwise only surface when a Minecraft client refuses to talk to us.

---

## Recommended reading order

1. **`main.go`** — 10 lines of glue, sets the stage.
2. **`state.go`** — what the three loops share.
3. **`proxy.go`** — the decision tree (start here for the "how does scale-from-zero actually trigger" question).
4. **`slp.go`** — wire protocol; the trickiest file, worth reading slowly.
5. **`probe.go`** — short and obvious.
6. **`metrics.go`** — what KEDA actually consumes.
7. **`config.go`** — the knobs.
8. **`slp_test.go`** — sanity checks for the protocol code.
