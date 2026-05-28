# mc-waker — how the Go program works

> A walkthrough of the waker's source, aimed at someone reading the code for the first time. The whole thing is ~1500 lines split across nine files in `build/mc-waker/`; this doc explains *why* each piece exists and how they fit together.

The waker handles **both Minecraft editions**: Java (TCP, SLP wire protocol) and Bedrock (UDP, RakNet). Java traffic always flows through; Bedrock is opt-in via two env vars. Wherever something differs between the two, this doc calls it out.

---

## Table of contents

- [The job in one sentence](#the-job-in-one-sentence)
- [Four goroutines, one shared `state`](#four-goroutines-one-shared-state)
- [`proxy.go` — the TCP decision tree (Java)](#proxygo--the-tcp-decision-tree-java)
- [`bedrock.go` — the UDP proxy + wake catcher (Bedrock)](#bedrockgo--the-udp-proxy--wake-catcher-bedrock)
- [`slp.go` — Minecraft's Java wire protocol, the small slice we need](#slpgo--minecrafts-java-wire-protocol-the-small-slice-we-need)
- [`probe.go` — mc-monitor with an SLP fallback](#probego--mc-monitor-with-an-slp-fallback)
- [`metrics.go` — Prometheus + the scaler endpoint](#metricsgo--prometheus--the-scaler-endpoint)
- [`config.go` — flags + env, one struct out](#configgo--flags--env-one-struct-out)
- [The wake-signal "hold" — small but load-bearing](#the-wake-signal-hold--small-but-load-bearing)
- [Why scale-to-zero needs the waker in its own Deployment](#why-scale-to-zero-needs-the-waker-in-its-own-deployment)
- [Tests](#tests)
- [Recommended reading order](#recommended-reading-order)

---

## The job in one sentence

Be the always-on thing that sits in front of a sleeping Minecraft pod, so that when a player tries to connect — whether on Java (TCP 25565) or Bedrock (UDP 19132) — there's *something* listening to either (a) forward their bytes to the live server, or (b) answer them politely and trigger the scale-up.

---

## Four goroutines, one shared `state`

`main.go` is tiny on purpose — it just builds a `state`, then launches concurrent loops that share it:

```go
go runProbe(ctx, st)             // periodic upstream probe (Java + Bedrock)
go runHTTP(ctx, st)              // /metrics, /scaler, /status, /wake on :8080
if cfg.bedrockEnabled() {
    go runBedrockCatcher(ctx, st) // UDP listener + proxy on :19132
}
runProxy(ctx, st)                // TCP listener on :25565 (blocks; main goroutine)
```

The Bedrock catcher is conditional: if either `WAKER_BEDROCK_LISTEN` or `WAKER_BEDROCK_UPSTREAM` is unset, the goroutine never starts and the binary behaves exactly like a Java-only waker.

Everything those loops need to know about each other lives on `state.go`:

```go
type state struct {
    cfg config
    log *slog.Logger

    java    upstreamState   // probe-loop cache for the Java upstream
    bedrock upstreamState   // probe-loop cache for the Bedrock upstream

    wakeMu    sync.Mutex
    wakeUntil time.Time

    activeConns          atomic.Int64
    wakeEvents           atomic.Int64
    proxyOpens           atomic.Int64
    proxyErrors          atomic.Int64
    bedrockPings         atomic.Int64   // RakNet Unconnected Pings observed
    bedrockSessionsActive atomic.Int64  // open UDP sessions in the proxy table
    bedrockForwardedUp   atomic.Int64   // datagrams client → server
    bedrockForwardedDown atomic.Int64   // datagrams server → client
}
```

Three design choices worth flagging:

- **Per-protocol upstream caches.** `upstreamState` is a tiny struct (mutex + up flag + player count + last-probe timestamp). Two instances on `state` mean a Java outage cannot mask Bedrock state and vice versa.
- **One wake signal, shared.** A wake event from either protocol bumps the same `wakeUntil` deadline. The autoscaler is binary (0 or 1) and pod-level, so it doesn't matter which protocol triggered the wake.
- **Atomic counters for stats.** No locking needed for monotonic counters that are read on every Prometheus scrape; `atomic.Int64` is enough.

---

## `proxy.go` — the TCP decision tree (Java)

`runProxy` does `net.Listen("tcp", ":25565")` and spawns a goroutine per accepted connection. The interesting code is `handleConn`:

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

> **Why not always dial first?** Because during pod startup the kernel accepts TCP on 25565 *before* the JVM has bound its socket — we'd connect, proxy, and the player would hang. Trusting the cached probe (which checks via SLP/RakNet, not raw TCP) avoids that.

---

## `bedrock.go` — the UDP proxy + wake catcher (Bedrock)

The Bedrock equivalent of `proxy.go`, but UDP changes the shape of the code in ways worth understanding.

### Why UDP needs more code than TCP

TCP is a stream. Once you've called `Accept()`, you have a single `net.Conn` representing one client; everything from that client arrives on the same socket, in order. Forwarding is just two `io.Copy` calls.

UDP is datagrams. Every `ReadFrom` returns one packet plus the sender's address. The same shared listener socket receives packets from *every* client — there's no per-client connection state for the kernel to hand you. If we naively forwarded each datagram to the upstream and then read replies, we wouldn't know which client a reply was for.

The waker solves this with a **per-client session table**: `map[string]*udpSession`, keyed by the client's address. Each `udpSession` owns a dedicated `net.DialUDP` socket to the upstream. Because that socket is *dialed* (not Listen'd), the kernel writes our local source-port into outgoing packets, and replies come back on that socket only. The kernel does the client demux for us — no need to parse RakNet session IDs.

### The session lifecycle

```
client UDP datagram arrives on the public listener
        │
        ▼
signalWake()  ─── always bump wake on any datagram (covers cached server-list entries)
        │
        ▼
is upstream up & recent?
        │
   ┌────┴────┐
   no       yes
   │         │
   ▼         ▼
  is ping?  getOrCreateSession(clientAddr)
   │         │
   yes       ▼
   │       upstreamConn.Write(datagram)
   ▼         │
  reply       └── per-session goroutine reads upstream replies,
  canned          forwards each one back to clientAddr via the
  "Sleeping"      shared public listener
  Pong
```

### The janitor

A small ticker goroutine (`runJanitor`, every 30s) walks the session map and evicts entries that have been idle for more than 60s in either direction. Eviction simply closes the upstream socket; the per-session reader goroutine then sees `net.ErrClosed` on its next read and runs its own deferred cleanup (removes from map, decrements gauge, logs).

The 2-second read deadline on the per-session reader is what makes this work cleanly: when the janitor closes the socket, the next read returns within at most 2s and the goroutine exits — no goroutine leak, no need for a separate cancellation channel per session.

### The catch-vs-forward switch

When the upstream is **down**, the proxy behaves like the old wake-only catcher: any Unconnected Ping (RakNet packet ID `0x01` with the fixed 16-byte magic at offset 9) gets a canned "Sleeping" Pong, signaled by the same shared wake mechanism. Non-ping packets are dropped (the client will retry once the pod is up).

When the upstream is **up**, even pings are forwarded — so the client's server-list entry shows the real server's version, MOTD, and player count, exactly as if the waker weren't there. This is symmetric with the Java side, where SLP refreshes against a live upstream are proxied through to get the real status.

### Concurrency safety

- `net.PacketConn` is documented as safe for concurrent writes, so many session readers can `WriteTo` the public listener at once without an extra mutex.
- The session map is guarded by `sync.RWMutex` with a double-check pattern in `getOrCreateSession` so two goroutines racing for a new client open at most one upstream socket.

---

## `slp.go` — Minecraft's Java wire protocol, the small slice we need

This file (~320 lines) implements just three things from the [Java Edition Server List Ping protocol](https://wiki.vg/Server_List_Ping):

1. **Wire helpers** — `VarInt` encoding (variable-length integer, 7 data bits per byte, MSB = "more bytes coming"), length-prefixed strings, and the packet framing rule (`VarInt length | VarInt packetID | payload`).
2. **Handshake parsing** — every client connection starts with a `Handshake` packet that tells us whether the next phase is *Status* (server-list refresh) or *Login* (player trying to join).
3. **Two response paths** — `handleSleepingStatus` answers a status request with our fake "Sleeping" MOTD and echoes the optional Ping/Pong so the server-list shows a believable latency; `handleSleepingLogin` answers a login attempt with a kick-message disconnect.

The file also still contains `probeUpstream(addr, timeout)` — a complete client-side SLP round trip — but it's now used as a **fallback** for the Java probe when `mc-monitor` is unavailable (see [`probe.go`](#probego--mc-monitor-with-an-slp-fallback) below).

### Why the protocol detour matters

We don't *need* to fake an SLP response. We could just close the TCP connection and rely on retry. But two things make the SLP path much better UX:

- The Minecraft client's *Refresh* button works visually. The player sees a server, with a custom MOTD, in their server list — they know it exists and is sleeping, not down.
- It gives us a clean, lightweight wake trigger. "Client did an SLP refresh" is a much better signal than "TCP connect to 25565 failed", because nothing else in the wild does SLP — there's no port-scanner background noise to filter out.

The Bedrock equivalent of all this is in `bedrock.go` — the canned Unconnected Pong serves the same UX role: a visible "Sleeping" entry in the server list with a working refresh.

---

## `probe.go` — mc-monitor with an SLP fallback

The probe loop drives one of the most important state transitions in the system: "is the upstream actually answering?". Wrong here and the proxy refuses to forward when it should (player can't connect) or forwards when it shouldn't (player hangs on a dead socket).

We use **`mc-monitor`** — a small Go binary from the itzg project that the Minecraft image itself uses for its liveness probe. The waker's Containerfile copies it out of `itzg/minecraft-server` and into the runtime image:

```dockerfile
FROM docker.io/itzg/minecraft-server:latest AS mc-monitor
# ... runtime stage ...
COPY --from=mc-monitor /usr/local/bin/mc-monitor /usr/local/bin/mc-monitor
```

This means the waker probes its upstreams with the *same* tool that the Minecraft pod uses to decide it's alive — there's no risk of the two disagreeing about what "up" means.

### What gets probed

Every `ProbeInterval` (default 15s), `probeOnce(s)` runs:

```go
probeJava(s)
if s.cfg.BedrockUpstreamAddr != "" {
    probeBedrock(s)
}
```

Both call out to `mc-monitor`:

```bash
mc-monitor status         -host mc-ragnarok         -port 25565
mc-monitor status-bedrock -host mc-ragnarok-bedrock -port 19132
```

Each prints a one-liner like `mc-ragnarok:25565 : version=1.21.11 online=0 max=20 motd='...'`. We grep for `online=N` and that's our player count. Exit 0 means the server answered; non-zero means it didn't.

### The Java fallback

`probeJava` tries `mc-monitor` first; if the binary is missing or fails to execute (a `*exec.PathError`, *not* a non-zero exit, which is a legitimate "server down" signal), it falls back to the hand-rolled `probeUpstream` in `slp.go`. This means the waker still works if you build it without `mc-monitor` baked in — though Bedrock probing has no such fallback (RakNet client logic isn't in the codebase, and shouldn't be).

The distinction between "couldn't execute" and "ran but reported down" is done with `errors.As`:

```go
var exitErr *exec.ExitError
if errors.As(err, &exitErr) {
    // It ran; it just reported failure. That's a real "server down".
    return false
}
return true  // Genuine exec failure; fall back.
```

### Why an external probe and not just "did anyone connect lately?"

Because of the chicken-and-egg: when the pod is at zero replicas, *no one is connecting* — by definition. The probe is the only signal that tells us the pod has come back up and is ready to be proxied to. Without it, the proxy decision tree in `proxy.go` and `bedrock.go` has no way to know when to switch from "answer with sleeping reply" to "forward to upstream".

---

## `metrics.go` — Prometheus + the scaler endpoint

Two surfaces here.

### The Prometheus surface (`/metrics`)

The waker exposes per-protocol gauges and counters. The set you care about:

| Metric | Type | Meaning |
|---|---|---|
| `minecraft_players_online{protocol="java"}` | gauge | Live player count on the Java upstream (0 if down) |
| `minecraft_players_online{protocol="bedrock"}` | gauge | Same for Bedrock |
| `minecraft_upstream_up{protocol="java"\|"bedrock"}` | gauge | 1 if last probe succeeded, 0 otherwise |
| `minecraft_wake_signal` | gauge | 1 while a wake-up is being held (see next section) |
| `minecraft_desired_replicas` | gauge | The synthetic 0/1 the ScaledObject triggers on |
| `minecraft_wake_events_total` | counter | Total wake-ups triggered (any protocol, or POST /wake) |
| `minecraft_proxy_opens_total` | counter | Java TCP proxy connections opened |
| `minecraft_bedrock_pings_total` | counter | RakNet Unconnected Pings observed |
| `minecraft_bedrock_sessions_active` | gauge | Open UDP sessions in the proxy table |
| `minecraft_bedrock_forwarded_packets_total{direction="up"\|"down"}` | counter | Datagrams forwarded each direction |

The most important metric is `minecraft_desired_replicas`, a 0/1 gauge computed by `desiredReplicas(s)`:

```go
func desiredReplicas(s *state) int {
    jUp, jP, _ := s.getUpstream()
    bUp, bP, _ := s.getBedrockUpstream()
    if (jUp && jP > 0) || (bUp && bP > 0) || s.wakeActive() {
        return 1
    }
    return 0
}
```

Either protocol having players online OR a wake being pending is enough to keep the workload at 1. Both ScaledObject variants (`metrics-api` and `prometheus`) trigger on this number.

### The plain-HTTP surface

| Endpoint | Used by | Returns |
|---|---|---|
| `/scaler` | KEDA `metrics-api` trigger | `{"value": 0 \| 1}` |
| `/status` | humans (curl/debug) | JSON snapshot of `state` — per-protocol upstream health, wake state, session counts |
| `/wake` (POST) | demos, "wake-the-server" buttons | bumps the wake deadline |
| `/healthz`, `/readyz` | Kubernetes probes | `200 OK` |

`/scaler` and `minecraft_desired_replicas` both call the same `desiredReplicas(s)` helper, so the two scaling paths are guaranteed identical — there's no way for them to drift apart.

---

## `config.go` — flags + env, one struct out

Standard `flag` package wiring. Each tunable is exposed as both a command-line flag *and* an environment variable (`WAKER_*`). Env wins by being the flag's default, so you can override it in the Deployment without touching the container args.

The Bedrock-relevant knobs:

| Env var | Default | Meaning |
|---|---|---|
| `WAKER_BEDROCK_LISTEN` | `""` | UDP address to bind for Bedrock catcher (`:19132` to enable) |
| `WAKER_BEDROCK_UPSTREAM` | `""` | host:port of the real Bedrock service |
| `WAKER_MC_MONITOR` | `/usr/local/bin/mc-monitor` | Path to the mc-monitor binary |
| `WAKER_MC_MONITOR_TIMEOUT` | `3s` | How long to wait for one mc-monitor invocation |

The helper `cfg.bedrockEnabled()` returns true only when both `BedrockListenAddr` and `BedrockUpstreamAddr` are set — either alone is a misconfiguration we tolerate by treating Bedrock as fully disabled.

---

## The wake-signal "hold" — small but load-bearing

When `signalWake()` fires, we set `wakeUntil = now + WakeHoldFor` (default 3 min). While `time.Now().Before(wakeUntil)` is true, `minecraft_wake_signal` reports `1`.

Why a multi-minute hold? Several races collide if it's too short:

- KEDA's `pollingInterval` is 15s — it might not see a transient `1` between scrapes.
- The Minecraft pod's cold-start is 30–60s.
- HPA has a "scale-down stabilization window" (we set 60s) before it'll reduce replicas.

A 3-minute hold guarantees that one wake event reliably scales the workload up, the pod fully starts, *and* at least one player has time to actually connect — at which point `minecraft_players_online > 0` takes over as the reason to stay scaled up. After both signals go back to zero, the `cooldownPeriod` on the ScaledObject (60s in the demo, 600s+ in production) is what eventually triggers scale-to-zero.

---

## Why scale-to-zero needs the waker in its own Deployment

This is the architectural insight that drives everything else. If the waker were a sidecar inside the Minecraft pod, scaling that pod to zero would also kill the waker — nothing left to receive the player's TCP connection or UDP datagram, nothing to signal the scale-up, dead loop. So the waker is its own one-replica Deployment that is *never* scaled by KEDA. The thing KEDA scales is the Minecraft Deployment itself, between 0 and 1, driven by a metric the waker publishes about something it observes externally (probe + connection attempts on both protocols).

The Minecraft pod has no idea any of this is happening. It just runs, gets traffic, or doesn't. That's deliberate — it means the same setup works for any TCP or UDP service, not just Minecraft, with only `slp.go`/`bedrock.go` swapped out for whatever protocol-fronting code that service needs.

---

## Tests

`slp_test.go` covers the Java wire format — VarInts and length-prefixed strings round-trip, packets parse back to what they came from, and `buildSleepingStatus` produces JSON containing the configured MOTD. Run with `go test ./...` in `build/mc-waker/`. They're not exhaustive, but they catch the kind of bug that would otherwise only surface when a Minecraft client refuses to talk to us.

The Bedrock side is not (yet) covered by unit tests — its correctness was established by running real Bedrock clients against the proxy and watching `minecraft_bedrock_forwarded_packets_total{direction="up"}` and `{direction="down"}` grow roughly in lockstep. A future addition would be RakNet packet-construction tests analogous to `slp_test.go`.

---

## Recommended reading order

1. **`main.go`** — 50 lines of glue, sets the stage.
2. **`state.go`** — what the goroutines share.
3. **`proxy.go`** — the Java TCP decision tree (start here for the "how does scale-from-zero actually trigger" question).
4. **`bedrock.go`** — the UDP proxy + wake catcher; the most code, but well-commented.
5. **`slp.go`** — Java wire protocol; the trickiest file, worth reading slowly.
6. **`probe.go`** — mc-monitor wiring and the SLP fallback.
7. **`metrics.go`** — what KEDA actually consumes.
8. **`config.go`** — the knobs.
9. **`slp_test.go`** — sanity checks for the protocol code.