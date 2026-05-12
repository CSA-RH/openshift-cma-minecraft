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
	// Live player count, refreshed by the probe loop. 0 while the upstream
	// is down (so the scaler falls back to the wake signal alone).
	players := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "minecraft_players_online",
		Help: "Number of players currently online on the upstream Minecraft server (0 if upstream is down).",
	}, func() float64 {
		up, p, _ := s.getUpstream()
		if !up {
			return 0
		}
		return float64(p)
	})

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

	upGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "minecraft_upstream_up",
		Help: "1 if the last SLP probe to the upstream succeeded, 0 otherwise.",
	}, func() float64 {
		up, _, _ := s.getUpstream()
		if up {
			return 1
		}
		return 0
	})

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

	// The convenience metric the ScaledObject uses: 1 if the server should
	// be running (players online OR a wake is pending), else 0. Built into
	// the waker so the trigger query can stay trivial.
	desired := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "minecraft_desired_replicas",
		Help: "1 if the server should be running (players online OR wake pending), else 0.",
	}, func() float64 {
		return float64(desiredReplicas(s))
	})

	reg.MustRegister(players, wake, upGauge, activeConns, wakeEvents, proxyOpens, desired)
}

// desiredReplicas computes the same value the /scaler endpoint returns and
// the minecraft_desired_replicas gauge exposes. Centralising it keeps both
// triggers (Prometheus and metrics-api) perfectly in sync.
func desiredReplicas(s *state) int {
	up, p, _ := s.getUpstream()
	if (up && p > 0) || s.wakeActive() {
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
		up, p, age := s.getUpstream()
		body := map[string]any{
			"upstream_up":      up,
			"players_online":   p,
			"last_probe_age_s": age.Seconds(),
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
