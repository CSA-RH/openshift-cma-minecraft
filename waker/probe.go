package main

import (
	"context"
	"time"
)

// runProbe periodically asks the upstream Minecraft server for its player
// count using the SLP protocol. The result is cached on `state` and exposed
// via Prometheus + the /scaler endpoint, where KEDA can pick it up.
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
	online, err := probeUpstream(s.cfg.UpstreamAddr, s.cfg.DialTimeout)
	if err != nil {
		s.setUpstream(false, 0)
		s.log.Debug("probe: upstream down", "err", err)
		return
	}
	s.setUpstream(true, online)
	s.log.Debug("probe: upstream up", "players", online)
}
