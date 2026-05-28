// Package main is the entry point for mc-waker, a tiny TCP proxy + Minecraft
// Server List Ping (SLP) responder used to drive scale-from-zero on OpenShift
// via the Custom Metrics Autoscaler (KEDA).
//
// Behavior in one paragraph: mc-waker always listens on TCP port 25565 for
// Java clients and (optionally) UDP port 19132 for Bedrock clients. When a
// client connects, the waker tries to dial the real Minecraft server. If the
// server is up, the connection is bidirectionally proxied (Java) or simply
// acknowledged with a fake "Sleeping" entry (Bedrock — it's not a stream).
// If the server is down (because it has been scaled to zero), the waker
// answers locally with a "Server is sleeping" MOTD and raises a wake signal
// that the ScaledObject scrapes to bring the server back to one replica.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg := parseFlags()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	st := newState(cfg, log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go runProbe(ctx, st)
	go runHTTP(ctx, st)

	// Bedrock catcher is opt-in: it starts only if both listen and upstream
	// addresses are configured. Until then this is a no-op goroutine so the
	// rest of the program is unaffected.
	if cfg.bedrockEnabled() {
		go runBedrockCatcher(ctx, st)
	} else {
		log.Info("bedrock disabled",
			"reason", "WAKER_BEDROCK_LISTEN or WAKER_BEDROCK_UPSTREAM not set")
	}

	runProxy(ctx, st) // blocking — Java TCP path
}
