package main

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// state holds everything the proxy, the probe loop, and the metrics endpoint
// need to share. Locks are intentionally fine-grained: a fast RWMutex for
// upstream readiness (which is read on every TCP accept) and a separate Mutex
// for the wake-signal deadline.
type state struct {
	cfg config
	log *slog.Logger

	upMu          sync.RWMutex
	upstreamUp    bool
	playersOnline int
	lastProbeAt   time.Time

	wakeMu    sync.Mutex
	wakeUntil time.Time

	activeConns atomic.Int64
	wakeEvents  atomic.Int64
	proxyOpens  atomic.Int64
	proxyErrors atomic.Int64
}

func newState(cfg config, log *slog.Logger) *state {
	return &state{cfg: cfg, log: log}
}

// setUpstream records the result of a probe.
func (s *state) setUpstream(up bool, players int) {
	s.upMu.Lock()
	s.upstreamUp = up
	s.playersOnline = players
	s.lastProbeAt = time.Now()
	s.upMu.Unlock()
}

// getUpstream returns the cached upstream status plus how stale it is.
// Callers should consider data older than ~2x the probe interval as
// untrustworthy.
func (s *state) getUpstream() (up bool, players int, age time.Duration) {
	s.upMu.RLock()
	defer s.upMu.RUnlock()
	age = time.Since(s.lastProbeAt)
	if s.lastProbeAt.IsZero() {
		// Never probed yet; treat as very stale.
		age = time.Hour
	}
	return s.upstreamUp, s.playersOnline, age
}

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
