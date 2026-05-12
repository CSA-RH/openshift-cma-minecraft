package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// config bundles every tunable knob. Each field is also bindable to an
// environment variable so the Deployment can configure it without touching the
// container args.
type config struct {
	ListenAddr      string
	MetricsAddr     string
	UpstreamAddr    string
	ProbeInterval   time.Duration
	DialTimeout     time.Duration
	WakeHoldFor     time.Duration
	SleepingMOTD    string
	DisconnectMsg   string
	ProtocolVersion int
	VersionName     string
	MaxPlayers      int
	LogLevel        slog.Level
}

func parseFlags() config {
	cfg := config{}
	flag.StringVar(&cfg.ListenAddr, "listen",
		envOr("WAKER_LISTEN", ":25565"),
		"TCP address the proxy listens on for Minecraft clients")
	flag.StringVar(&cfg.MetricsAddr, "metrics-listen",
		envOr("WAKER_METRICS_LISTEN", ":8080"),
		"HTTP address for /metrics, /scaler and admin endpoints")
	flag.StringVar(&cfg.UpstreamAddr, "upstream",
		envOr("WAKER_UPSTREAM", "minecraft:25565"),
		"host:port of the real Minecraft Service")
	flag.DurationVar(&cfg.ProbeInterval, "probe-interval",
		envDur("WAKER_PROBE_INTERVAL", 15*time.Second),
		"How often to probe the upstream server with an SLP request")
	flag.DurationVar(&cfg.DialTimeout, "dial-timeout",
		envDur("WAKER_DIAL_TIMEOUT", 1500*time.Millisecond),
		"Timeout when dialing or probing the upstream")
	flag.DurationVar(&cfg.WakeHoldFor, "wake-hold",
		envDur("WAKER_WAKE_HOLD", 5*time.Minute),
		"How long minecraft_wake_signal stays at 1 after a wake event")
	flag.StringVar(&cfg.SleepingMOTD, "sleeping-motd",
		envOr("WAKER_SLEEPING_MOTD",
			"§eServer is sleeping...\n§aJust hit Refresh to wake it up!"),
		"MOTD shown on the server-list while the server is asleep (§ codes accepted)")
	flag.StringVar(&cfg.DisconnectMsg, "disconnect-msg",
		envOr("WAKER_DISCONNECT_MSG",
			"Server is waking up — please reconnect in ~30 seconds."),
		"Message sent to clients that try to log in while the server is starting")
	flag.IntVar(&cfg.ProtocolVersion, "protocol-version",
		envInt("WAKER_PROTOCOL_VERSION", 769),
		"Minecraft protocol version advertised in the fake status (769 = 1.21.4)")
	flag.StringVar(&cfg.VersionName, "version-name",
		envOr("WAKER_VERSION_NAME", "Sleeping"),
		"Version label advertised in the fake status")
	flag.IntVar(&cfg.MaxPlayers, "max-players",
		envInt("WAKER_MAX_PLAYERS", 20),
		"Max-players value advertised in the fake status")

	logLevel := envOr("WAKER_LOG_LEVEL", "info")
	flag.StringVar(&logLevel, "log-level", logLevel, "log level: debug | info | warn | error")

	flag.Parse()

	cfg.LogLevel = parseLogLevel(logLevel)
	return cfg
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}
