package main

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// upstreamState is a small, lockable snapshot of one upstream's health.
// We keep one of these per protocol (Java + Bedrock) so the two protocols
// never mask each other's state.
type upstreamState struct {
	mu          sync.RWMutex
	up          bool
	players     int
	lastProbeAt time.Time
}

func (u *upstreamState) set(up bool, players int) {
	u.mu.Lock()
	u.up = up
	u.players = players
	u.lastProbeAt = time.Now()
	u.mu.Unlock()
}

// snapshot returns the cached state plus how stale it is. If we've never
// probed, age is reported as a large value so callers treat it as untrusted.
func (u *upstreamState) snapshot() (up bool, players int, age time.Duration) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.lastProbeAt.IsZero() {
		return false, 0, time.Hour
	}
	return u.up, u.players, time.Since(u.lastProbeAt)
}

// state holds everything the proxy, the probe loop, and the metrics endpoint
// need to share.
type state struct {
	cfg config
	log *slog.Logger

	java    upstreamState
	bedrock upstreamState

	wakeMu    sync.Mutex
	wakeUntil time.Time

	activeConns atomic.Int64
	wakeEvents  atomic.Int64
	proxyOpens  atomic.Int64
	proxyErrors atomic.Int64

	// bedrockPings counts UDP Unconnected Pings received from clients.
	// Useful for sanity-checking that wake traffic is reaching the waker.
	bedrockPings atomic.Int64
}

func newState(cfg config, log *slog.Logger) *state {
	return &state{cfg: cfg, log: log}
}

// --- Java accessors (preserved so existing call sites keep working) ---

func (s *state) setUpstream(up bool, players int) {
	s.java.set(up, players)
}

func (s *state) getUpstream() (up bool, players int, age time.Duration) {
	return s.java.snapshot()
}

// --- Bedrock accessors ---

func (s *state) setBedrockUpstream(up bool, players int) {
	s.bedrock.set(up, players)
}

func (s *state) getBedrockUpstream() (up bool, players int, age time.Duration) {
	return s.bedrock.snapshot()
}

// --- Wake signal (shared across protocols) ---

// signalWake bumps the wake deadline forward and increments the wake counter.
// While the deadline is in the future, minecraft_wake_signal reports 1.
func (s *state) signalWake() {
	s.wakeMu.Lock()
	s.wakeUntil = time.Now().Add(s.cfg.WakeHoldFor)
	s.wakeMu.Unlock()
	s.wakeEvents.Add(1)
}

func (s *state) wakeActive() bool {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	return time.Now().Before(s.wakeUntil)
}
