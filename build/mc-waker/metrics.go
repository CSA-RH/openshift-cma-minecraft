package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// runHTTP starts the HTTP server that serves the Prometheus /metrics
// endpoint, the /scaler JSON endpoint (for the KEDA metrics-api trigger),
// and small admin endpoints (/healthz, /readyz, /status, /wake).
func runHTTP(ctx context.Context, s *state) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	registerWakerMetrics(reg, s)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("/healthz", textOK("ok"))
	mux.HandleFunc("/readyz", textOK("ready"))
	mux.HandleFunc("/status", statusHandler(s))
	mux.HandleFunc("/scaler", scalerHandler(s))
	mux.HandleFunc("/wake", wakeHandler(s))

	srv := &http.Server{
		Addr:              s.cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	s.log.Info("metrics listening", "addr", s.cfg.MetricsAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.log.Error("metrics server error", "err", err)
	}
}

func registerWakerMetrics(reg *prometheus.Registry, s *state) {
	// Per-protocol player counts. Labelled by protocol so a Bedrock outage
	// doesn't mask Java players and vice versa.
	playersJava := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "minecraft_players_online",
		Help: "Number of players currently online on an upstream Minecraft server (0 if upstream is down).",
	}, []string{"protocol"})
	reg.MustRegister(playersJava)

	// 1 while a wake-up has been requested and is still being held; 0
	// otherwise. Provides a brief window during which the ScaledObject can
	// scale 0 -> 1 even though no player is "online" yet.
	wake := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "minecraft_wake_signal",
		Help: "1 while a wake-up has been requested and is still being held; 0 otherwise.",
	}, func() float64 {
		if s.wakeActive() {
			return 1
		}
		return 0
	})

	// Per-protocol upstream up gauge.
	upGauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "minecraft_upstream_up",
		Help: "1 if the last probe to the upstream succeeded, 0 otherwise.",
	}, []string{"protocol"})
	reg.MustRegister(upGauge)

	activeConns := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "minecraft_proxy_active_connections",
		Help: "Active client connections currently being handled by the waker.",
	}, func() float64 {
		return float64(s.activeConns.Load())
	})

	wakeEvents := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "minecraft_wake_events_total",
		Help: "Total number of wake-ups triggered by client connections (or POST /wake).",
	}, func() float64 {
		return float64(s.wakeEvents.Load())
	})

	proxyOpens := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "minecraft_proxy_opens_total",
		Help: "Total number of client connections proxied to the upstream server.",
	}, func() float64 {
		return float64(s.proxyOpens.Load())
	})

	bedrockPings := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "minecraft_bedrock_pings_total",
		Help: "Total number of Bedrock Unconnected Pings received from clients.",
	}, func() float64 {
		return float64(s.bedrockPings.Load())
	})

	// The convenience metric the ScaledObject uses: 1 if the server should
	// be running (any protocol has players online OR a wake is pending),
	// else 0. Built into the waker so the trigger query can stay trivial.
	desired := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "minecraft_desired_replicas",
		Help: "1 if any upstream should be running (players online OR wake pending), else 0.",
	}, func() float64 {
		return float64(desiredReplicas(s))
	})

	reg.MustRegister(wake, activeConns, wakeEvents, proxyOpens, bedrockPings, desired)

	// Re-publish gauges on every scrape by hooking a custom collector.
	// Simpler: register a single Collect callback via a tiny lambda gauge.
	// We piggy-back on a NewGaugeFunc that updates the vec just before
	// Prometheus reads it. (NewGaugeVec doesn't have a "Func" variant.)
	hook := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "minecraft_metrics_refresh_hook",
		Help: "Internal: 1; side-effect refreshes per-protocol gauges. Safe to ignore.",
	}, func() float64 {
		jUp, jPlayers, _ := s.getUpstream()
		playersJava.WithLabelValues("java").Set(playerOrZero(jUp, jPlayers))
		upGauge.WithLabelValues("java").Set(boolToFloat(jUp))

		bUp, bPlayers, _ := s.getBedrockUpstream()
		playersJava.WithLabelValues("bedrock").Set(playerOrZero(bUp, bPlayers))
		upGauge.WithLabelValues("bedrock").Set(boolToFloat(bUp))
		return 1
	})
	reg.MustRegister(hook)
}

func playerOrZero(up bool, n int) float64 {
	if !up {
		return 0
	}
	return float64(n)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// desiredReplicas computes the same value the /scaler endpoint returns and
// the minecraft_desired_replicas gauge exposes. Returns 1 if EITHER
// protocol has players online OR a wake is pending.
func desiredReplicas(s *state) int {
	jUp, jP, _ := s.getUpstream()
	bUp, bP, _ := s.getBedrockUpstream()
	if (jUp && jP > 0) || (bUp && bP > 0) || s.wakeActive() {
		return 1
	}
	return 0
}

func textOK(msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(msg))
	}
}

func statusHandler(s *state) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		jUp, jP, jAge := s.getUpstream()
		bUp, bP, bAge := s.getBedrockUpstream()
		body := map[string]any{
			"java": map[string]any{
				"upstream_up":      jUp,
				"players_online":   jP,
				"last_probe_age_s": jAge.Seconds(),
			},
			"bedrock": map[string]any{
				"enabled":          s.cfg.bedrockEnabled(),
				"upstream_up":      bUp,
				"players_online":   bP,
				"last_probe_age_s": bAge.Seconds(),
				"pings_received":   s.bedrockPings.Load(),
			},
			"wake_active":      s.wakeActive(),
			"active_conns":     s.activeConns.Load(),
			"desired_replicas": desiredReplicas(s),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

// scalerHandler is consumed by the KEDA metrics-api trigger.
// Returned shape: {"value": 0|1}
//
// The trigger config sets:
//
//	valueLocation: "value"
//	targetValue:   "1"
//
// so ceil(value/target) yields the desired replica count.
func scalerHandler(s *state) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int{
			"value": desiredReplicas(s),
		})
	}
}

// wakeHandler lets you bring the server up without a Minecraft client —
// invaluable for demos, smoke tests, or a "Wake the server" web button.
func wakeHandler(s *state) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		s.signalWake()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("wake signaled\n"))
	}
}
