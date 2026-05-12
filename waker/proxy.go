package main

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// runProxy starts the TCP listener that fronts the Minecraft service.
// It is the only blocking goroutine in the program; the rest run in the
// background.
func runProxy(ctx context.Context, s *state) {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		s.log.Error("listen failed", "addr", s.cfg.ListenAddr, "err", err)
		return
	}
	s.log.Info("waker listening",
		"addr", s.cfg.ListenAddr,
		"upstream", s.cfg.UpstreamAddr)

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("accept error", "err", err)
			continue
		}
		go handleConn(ctx, s, conn)
	}
}

// handleConn implements the core decision tree:
//
//  1. Trust the most recent probe. If it succeeded recently, attempt to
//     dial the upstream and proxy bytes both directions.
//  2. If the probe says the upstream is down (or stale), do NOT dial — the
//     pod might still be in early startup where TCP connects but the SLP
//     server hasn't bound yet, which would hang the client. Instead, answer
//     the SLP locally with a "sleeping" status and signal a wake.
func handleConn(ctx context.Context, s *state, client net.Conn) {
	defer client.Close()
	s.activeConns.Add(1)
	defer s.activeConns.Add(-1)

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
		// The probe was wrong — pod likely just died. Fall through to
		// the sleeping path so the next probe corrects state.
		s.log.Warn("upstream dial failed though marked up", "err", err)
		s.proxyErrors.Add(1)
		s.setUpstream(false, 0)
	}

	s.signalWake()
	if err := handleSleepingClient(client, s.cfg); err != nil {
		// Clients drop SLP connections aggressively; only log unexpected
		// failures at debug level.
		if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			s.log.Debug("sleeping handler ended", "err", err)
		}
	}
}

// proxyBidir wires the two sockets together and waits for the first half to
// finish. Once it does, the writer side of the still-open half is closed
// (CloseWrite) so the peer sees EOF and we don't leak goroutines if the
// remote end forgets to close.
func proxyBidir(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if t, ok := dst.(*net.TCPConn); ok {
			_ = t.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(b, a)
	go cp(a, b)
	<-done
	// Give the second direction a brief window to drain, then bail.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}
