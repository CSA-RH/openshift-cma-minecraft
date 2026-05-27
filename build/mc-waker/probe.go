package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// runProbe periodically asks the upstream Minecraft server(s) for their player
// counts. The result is cached on `state` and exposed via Prometheus + the
// /scaler endpoint, where KEDA can pick it up.
//
// Both Java and Bedrock are probed in the same tick. Bedrock probing is
// skipped entirely if cfg.BedrockUpstreamAddr is empty.
func runProbe(ctx context.Context, s *state) {
	t := time.NewTicker(s.cfg.ProbeInterval)
	defer t.Stop()

	probeOnce(s) // probe immediately so the first metric scrape is meaningful

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probeOnce(s)
		}
	}
}

func probeOnce(s *state) {
	probeJava(s)
	if s.cfg.BedrockUpstreamAddr != "" {
		probeBedrock(s)
	}
}

// probeJava asks mc-monitor for the Java upstream's status. If mc-monitor is
// missing or fails to execute (not "server down" — actually broken binary),
// it falls back to the hand-rolled SLP requester in slp.go so a misconfigured
// image doesn't silently disable Java probing.
func probeJava(s *state) {
	if s.cfg.McMonitorPath != "" && mcMonitorUsable(s.cfg.McMonitorPath) {
		online, err := mcMonitorStatus(s.cfg, "status", s.cfg.UpstreamAddr)
		if err == nil {
			s.setUpstream(true, online)
			s.log.Debug("probe(java): mc-monitor reports up", "players", online)
			return
		}
		// mc-monitor returned non-zero. That usually means the server is
		// actually down. Don't fall back — falling back would mask a real
		// "server down" with a parallel real "server down".
		if isExecError(err) {
			s.log.Warn("probe(java): mc-monitor failed to execute, falling back to SLP",
				"err", err)
			fallbackJavaSLP(s)
			return
		}
		s.setUpstream(false, 0)
		s.log.Debug("probe(java): mc-monitor says upstream down", "err", err)
		return
	}
	// No mc-monitor configured or binary missing — use the hand-rolled probe.
	fallbackJavaSLP(s)
}

func fallbackJavaSLP(s *state) {
	online, err := probeUpstream(s.cfg.UpstreamAddr, s.cfg.DialTimeout)
	if err != nil {
		s.setUpstream(false, 0)
		s.log.Debug("probe(java/slp): upstream down", "err", err)
		return
	}
	s.setUpstream(true, online)
	s.log.Debug("probe(java/slp): upstream up", "players", online)
}

// probeBedrock asks mc-monitor for the Bedrock upstream's status. There is no
// in-Go fallback here — Bedrock probing is opt-in via configuration, so if
// mc-monitor is missing we simply log and mark the upstream down.
func probeBedrock(s *state) {
	if s.cfg.McMonitorPath == "" || !mcMonitorUsable(s.cfg.McMonitorPath) {
		s.setBedrockUpstream(false, 0)
		s.log.Warn("probe(bedrock): mc-monitor unavailable; cannot probe Bedrock")
		return
	}
	online, err := mcMonitorStatus(s.cfg, "status-bedrock", s.cfg.BedrockUpstreamAddr)
	if err != nil {
		s.setBedrockUpstream(false, 0)
		s.log.Debug("probe(bedrock): upstream down", "err", err)
		return
	}
	s.setBedrockUpstream(true, online)
	s.log.Debug("probe(bedrock): upstream up", "players", online)
}

// mcMonitorStatus invokes `mc-monitor <subcmd> --host H --port P
// --use-proxy=false --show-player-count` and parses stdout for the online
// player count. Subcmd is either "status" (Java SLP/TCP) or "status-bedrock"
// (RakNet/UDP). Returns the online count on success.
//
// mc-monitor's JSON output mode shape (when --use-json-output is supported)
// is preferred; otherwise it prints a one-liner like
//   "<host>:<port> : online=true ... players=3/20 version=1.21.4"
// from which we extract the numerator.
func mcMonitorStatus(cfg config, subcmd, addr string) (int, error) {
	host, port, err := splitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("bad upstream addr %q: %w", addr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.McMonitorTimeout)
	defer cancel()

	// Try JSON output first.
	jsonArgs := []string{
		subcmd,
		"--host", host,
		"--port", fmt.Sprintf("%d", port),
		"--show-player-count",
		"--use-json-output",
	}
	out, err := exec.CommandContext(ctx, cfg.McMonitorPath, jsonArgs...).Output()
	if err == nil {
		if n, ok := parseMcMonitorJSON(out); ok {
			return n, nil
		}
		// fall through and retry without JSON, in case this build doesn't
		// support the flag
	}

	// Plain output.
	plainArgs := []string{
		subcmd,
		"--host", host,
		"--port", fmt.Sprintf("%d", port),
		"--show-player-count",
	}
	out, err = exec.CommandContext(ctx, cfg.McMonitorPath, plainArgs...).Output()
	if err != nil {
		return 0, err
	}
	return parseMcMonitorPlain(out), nil
}

// parseMcMonitorJSON extracts players.online from a JSON document.
// Tolerates either the top-level `{online: 3, ...}` shape or the nested
// `{players: {online: 3}}` shape mc-monitor has used at different times.
func parseMcMonitorJSON(b []byte) (int, bool) {
	var any1 struct {
		Online  *int `json:"online"`
		Players struct {
			Online *int `json:"online"`
		} `json:"players"`
	}
	if err := json.Unmarshal(b, &any1); err != nil {
		return 0, false
	}
	if any1.Players.Online != nil {
		return *any1.Players.Online, true
	}
	if any1.Online != nil {
		return *any1.Online, true
	}
	return 0, false
}

// parseMcMonitorPlain hunts for "players=N/M" in the one-liner output and
// returns N. If the substring isn't there, returns 0 — which is safe: a
// successful probe with zero players is the steady state anyway.
func parseMcMonitorPlain(b []byte) int {
	s := string(b)
	idx := strings.Index(s, "players=")
	if idx < 0 {
		return 0
	}
	rest := s[idx+len("players="):]
	slash := strings.IndexAny(rest, "/ \n\t")
	if slash > 0 {
		rest = rest[:slash]
	}
	var n int
	if _, err := fmt.Sscanf(rest, "%d", &n); err != nil {
		return 0
	}
	return n
}

// mcMonitorUsable returns true if the binary at path exists and is
// executable. We cache nothing — the check is one stat, cheap on every probe.
func mcMonitorUsable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}

// isExecError distinguishes "couldn't launch mc-monitor at all" from "ran it
// and it reported the server down". The former is an *ExitError when the
// process never started, or a PathError; we treat anything that's NOT an
// ExitError with a non-zero status as exec failure.
func isExecError(err error) bool {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// It ran; it just reported failure. That's a real "server down".
		return false
	}
	return true
}
