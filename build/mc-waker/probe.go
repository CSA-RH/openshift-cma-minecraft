package main

import (
	"context"
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

// mcMonitorStatus invokes the itzg `mc-monitor` binary with the correct
// single-dash flag syntax it actually uses:
//
//   mc-monitor status         -host H -port P
//   mc-monitor status-bedrock -host H -port P
//
// Both subcommands print a one-liner to stdout that always contains the
// player count as `online=N` and the cap as `max=M`. Exit code 0 means
// "server answered"; non-zero means it didn't. There is no JSON output
// mode in current builds, so we don't try.
//
// Example outputs (observed):
//   mc-ragnarok:25565         : version=1.21.11 online=0 max=20 motd='Welcome to the CMA Workshop!'
//   mc-ragnarok-bedrock:19132 : version=1.26.23 online=0 max=5
func mcMonitorStatus(cfg config, subcmd, addr string) (int, error) {
	host, port, err := splitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("bad upstream addr %q: %w", addr, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.McMonitorTimeout)
	defer cancel()

	args := []string{
		subcmd,
		"-host", host,
		"-port", fmt.Sprintf("%d", port),
	}
	cmd := exec.CommandContext(ctx, cfg.McMonitorPath, args...)
	// Combine stdout+stderr so a misconfigured invocation surfaces something
	// useful in the wrapped error rather than a silent empty string.
	out, err := cmd.CombinedOutput()
	if err != nil {
		// A non-zero exit from mc-monitor is a real "server down" signal,
		// not a tool failure. Wrap the error with the head of its output so
		// debug logging is actually actionable.
		return 0, fmt.Errorf("mc-monitor %s exited: %w (output=%s)",
			subcmd, err, trimForLog(out))
	}
	n, ok := parseMcMonitorPlain(out)
	if !ok {
		return 0, fmt.Errorf("mc-monitor %s: could not parse 'online=' from output: %s",
			subcmd, trimForLog(out))
	}
	return n, nil
}

// parseMcMonitorPlain extracts the value of the `online=N` token from
// mc-monitor's one-liner output. Returns (count, true) on success, or
// (0, false) if the token wasn't found.
//
// We deliberately ignore `max=` — the autoscaler only cares about the
// live count.
func parseMcMonitorPlain(b []byte) (int, bool) {
	s := string(b)
	// Find `online=` as a whole token (preceded by start-of-string, space,
	// or one of the punctuation chars mc-monitor uses).
	idx := strings.Index(s, "online=")
	if idx < 0 {
		return 0, false
	}
	rest := s[idx+len("online="):]
	// The value ends at the next whitespace, comma, or end of string.
	end := strings.IndexAny(rest, " \t\n\r,")
	if end > 0 {
		rest = rest[:end]
	}
	var n int
	if _, err := fmt.Sscanf(rest, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// trimForLog shortens mc-monitor output for inclusion in error messages.
// The full output is rarely more than 200 chars but we cap defensively.
func trimForLog(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
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
