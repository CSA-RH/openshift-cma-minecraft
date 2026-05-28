package main

// Bedrock RakNet UDP proxy + wake-catcher.
//
// This file is the UDP counterpart to proxy.go (which proxies Java TCP).
// On every inbound UDP datagram from a Bedrock client:
//
//   * If we have no session yet, allocate one: a dedicated UDP socket
//     dialed at the upstream Bedrock service. The kernel uses that socket's
//     source-port to uniquely identify replies for THIS client — no need
//     to parse RakNet session IDs ourselves.
//   * Forward the datagram upstream.
//   * A per-session reader goroutine forwards every reply back to the
//     client via the shared public listener.
//   * Signal a wake on the first datagram from a new client (covers cached
//     server-list entries that go straight to OpenConnectionRequest1
//     without re-pinging first).
//   * If the upstream is DOWN, fall back to the original behaviour: when
//     the datagram is an Unconnected Ping, reply locally with a canned
//     "Sleeping" Pong; everything else is silently dropped (client retries).
//
// Sessions are evicted by a janitor goroutine after SessionIdleTimeout of
// inactivity in EITHER direction.
//
// Reference: https://wiki.vg/Raknet_Protocol  (Unconnected Ping = 0x01,
// Unconnected Pong = 0x1C, magic = the fixed 16-byte sequence below)

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	raknetIDUnconnectedPing = 0x01
	raknetIDUnconnectedPong = 0x1C

	// raknetMinPingLen = 1 (id) + 8 (time) + 16 (magic) + 8 (clientGUID)
	raknetMinPingLen = 1 + 8 + 16 + 8

	// sessionIdleTimeout is how long a per-client session sits with no
	// traffic before the janitor closes it. Bedrock heartbeats are frequent
	// (subsecond during play, every few seconds on the server-list browse),
	// so 60s is generous in both directions.
	sessionIdleTimeout = 60 * time.Second
	sessionJanitorTick = 30 * time.Second

	// udpReadBufSize is generous for any single RakNet datagram, which is
	// MTU-bounded in practice.
	udpReadBufSize = 2048
)

// raknetMagic is the fixed 16-byte "magic" sequence every RakNet Open /
// Connected packet carries. Used to discriminate the Unconnected Ping
// from other RakNet packet IDs that happen to also start with 0x01.
var raknetMagic = []byte{
	0x00, 0xFF, 0xFF, 0x00, 0xFE, 0xFE, 0xFE, 0xFE,
	0xFD, 0xFD, 0xFD, 0xFD, 0x12, 0x34, 0x56, 0x78,
}

// udpSession tracks one client's "connection" as far as the proxy is
// concerned. The upstreamConn is dialed (not Listen'd) so the kernel writes
// our source-port into the upstream packets, and replies come back on this
// socket only — that's our demux key.
type udpSession struct {
	clientAddr   net.Addr
	upstreamConn *net.UDPConn
	lastSeenMu   sync.Mutex
	lastSeen     time.Time
}

func (sess *udpSession) touch() {
	sess.lastSeenMu.Lock()
	sess.lastSeen = time.Now()
	sess.lastSeenMu.Unlock()
}

func (sess *udpSession) idleFor() time.Duration {
	sess.lastSeenMu.Lock()
	defer sess.lastSeenMu.Unlock()
	return time.Since(sess.lastSeen)
}

// bedrockProxy is the long-lived state for the UDP listener.
type bedrockProxy struct {
	s            *state
	pc           net.PacketConn // public listener (client-facing)
	upstreamAddr *net.UDPAddr
	serverGUID   int64

	sessMu   sync.RWMutex
	sessions map[string]*udpSession
}

// runBedrockCatcher starts the UDP listener. Despite the legacy name, it now
// runs a full proxy plus a sleeping-mode wake catcher.
func runBedrockCatcher(ctx context.Context, s *state) {
	addr := s.cfg.BedrockListenAddr
	if addr == "" {
		s.log.Info("bedrock proxy disabled (no listen addr)")
		return
	}
	if s.cfg.BedrockUpstreamAddr == "" {
		s.log.Warn("bedrock listen set but upstream is empty; refusing to start")
		return
	}

	upstreamAddr, err := net.ResolveUDPAddr("udp", s.cfg.BedrockUpstreamAddr)
	if err != nil {
		s.log.Error("bedrock upstream resolve failed",
			"upstream", s.cfg.BedrockUpstreamAddr, "err", err)
		return
	}

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		s.log.Error("bedrock listen failed", "addr", addr, "err", err)
		return
	}
	s.log.Info("bedrock proxy listening",
		"addr", addr,
		"upstream", upstreamAddr.String(),
		"idle_timeout", sessionIdleTimeout.String())

	bp := &bedrockProxy{
		s:            s,
		pc:           pc,
		upstreamAddr: upstreamAddr,
		serverGUID:   time.Now().UnixNano(),
		sessions:     make(map[string]*udpSession),
	}

	// Shutdown plumbing.
	go func() {
		<-ctx.Done()
		_ = pc.Close()
		bp.closeAllSessions()
	}()

	// Session janitor.
	go bp.runJanitor(ctx)

	// Main read loop.
	buf := make([]byte, udpReadBufSize)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Debug("bedrock read error", "err", err)
			continue
		}
		// Copy the slice — buf is reused next iteration, but we hand the
		// data off to a goroutine.
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		go bp.handleClientPacket(src, pkt)
	}
}

// handleClientPacket is the per-datagram entry point from the public
// listener. It implements the catch-vs-forward decision and ensures a
// session exists when we forward.
func (bp *bedrockProxy) handleClientPacket(src net.Addr, pkt []byte) {
	s := bp.s

	// Every inbound packet is a heartbeat for the wake counter — clients
	// that already cached the server-list entry may skip pings entirely
	// and go straight to Open Connection Request 1, so we cannot rely on
	// the ping path alone for waking.
	s.signalWake()

	// Track pings separately for diagnostics.
	if isUnconnectedPing(pkt) {
		s.bedrockPings.Add(1)
	}

	// Decide based on upstream readiness (the same idea proxy.go uses for
	// the Java TCP path). If the probe says upstream is up and recent,
	// forward. Otherwise, answer the ping locally (sleeping behaviour);
	// drop everything else and let the client retry once the pod wakes.
	up, _, age := s.getBedrockUpstream()
	upstreamReady := up && age < 2*s.cfg.ProbeInterval

	if !upstreamReady {
		if isUnconnectedPing(pkt) {
			bp.replyCannedPong(src, pkt)
		}
		// Non-ping packets while upstream is down: drop. The client will
		// retry after the pod scales up; meanwhile our wake signal is
		// already racing the autoscaler.
		return
	}

	// Upstream is ready — forward.
	sess, err := bp.getOrCreateSession(src)
	if err != nil {
		s.log.Warn("bedrock session open failed", "client", src, "err", err)
		return
	}
	sess.touch()
	if _, err := sess.upstreamConn.Write(pkt); err != nil {
		s.log.Debug("bedrock upstream write failed", "client", src, "err", err)
		// Don't tear the session down on a single write failure — UDP is
		// best-effort; the janitor will reap stale sessions.
		return
	}
	bp.s.bedrockForwardedUp.Add(1)
}

// replyCannedPong sends a "Sleeping" Unconnected Pong back to the client.
// Echoes the client's ping timestamp so latency math works.
func (bp *bedrockProxy) replyCannedPong(src net.Addr, pkt []byte) {
	if len(pkt) < 9 {
		return
	}
	pingTime := binary.BigEndian.Uint64(pkt[1:9])
	resp := buildUnconnectedPong(pingTime, bp.serverGUID, buildBedrockMOTD(bp.s.cfg))
	_ = bp.pc.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := bp.pc.WriteTo(resp, src); err != nil {
		bp.s.log.Debug("bedrock pong write failed", "src", src, "err", err)
	}
}

// getOrCreateSession returns an existing session for src, or creates a new
// one. Creation dials a fresh UDP socket to the upstream and launches the
// reverse-direction reader goroutine.
func (bp *bedrockProxy) getOrCreateSession(src net.Addr) (*udpSession, error) {
	key := src.String()

	bp.sessMu.RLock()
	sess, ok := bp.sessions[key]
	bp.sessMu.RUnlock()
	if ok {
		return sess, nil
	}

	// Slow path: open the socket WITHOUT holding the write lock.
	upConn, err := net.DialUDP("udp", nil, bp.upstreamAddr)
	if err != nil {
		return nil, fmt.Errorf("dial upstream: %w", err)
	}

	bp.sessMu.Lock()
	if existing, ok := bp.sessions[key]; ok {
		// A racing goroutine beat us to it. Drop ours, use theirs.
		bp.sessMu.Unlock()
		_ = upConn.Close()
		return existing, nil
	}
	sess = &udpSession{
		clientAddr:   src,
		upstreamConn: upConn,
		lastSeen:     time.Now(),
	}
	bp.sessions[key] = sess
	bp.sessMu.Unlock()

	bp.s.bedrockSessionsActive.Add(1)
	bp.s.log.Debug("bedrock session opened",
		"client", src, "upstream_local", upConn.LocalAddr())

	go bp.pumpUpstreamToClient(sess)
	return sess, nil
}

// pumpUpstreamToClient runs for the life of one session: read replies from
// the per-client upstream socket, forward them back to the client via the
// shared public listener.
func (bp *bedrockProxy) pumpUpstreamToClient(sess *udpSession) {
	defer func() {
		_ = sess.upstreamConn.Close()
		bp.sessMu.Lock()
		delete(bp.sessions, sess.clientAddr.String())
		bp.sessMu.Unlock()
		bp.s.bedrockSessionsActive.Add(-1)
		bp.s.log.Debug("bedrock session closed", "client", sess.clientAddr)
	}()

	buf := make([]byte, udpReadBufSize)
	for {
		// A 1-second deadline lets the janitor's idle-eviction take effect:
		// when the janitor closes upstreamConn, the next Read returns and
		// we exit cleanly. Meanwhile a fresh datagram beats the deadline
		// almost always.
		_ = sess.upstreamConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := sess.upstreamConn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Deadline exceeded is normal — loop again unless the session
			// has been evicted (which closes the socket → next Read errors
			// with ErrClosed and we exit above).
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if sess.idleFor() > sessionIdleTimeout {
					return
				}
				continue
			}
			bp.s.log.Debug("bedrock upstream read err",
				"client", sess.clientAddr, "err", err)
			return
		}
		sess.touch()
		_ = bp.pc.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		if _, werr := bp.pc.WriteTo(buf[:n], sess.clientAddr); werr != nil {
			bp.s.log.Debug("bedrock client write failed",
				"client", sess.clientAddr, "err", werr)
			// Don't tear the session down on one failed write.
			continue
		}
		bp.s.bedrockForwardedDown.Add(1)
	}
}

// runJanitor evicts sessions idle longer than sessionIdleTimeout. Closing
// the upstreamConn unblocks the pump goroutine's next Read, which then
// runs its deferred cleanup.
func (bp *bedrockProxy) runJanitor(ctx context.Context) {
	t := time.NewTicker(sessionJanitorTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			bp.reapIdleSessions()
		}
	}
}

func (bp *bedrockProxy) reapIdleSessions() {
	bp.sessMu.RLock()
	stale := make([]*udpSession, 0)
	for _, sess := range bp.sessions {
		if sess.idleFor() > sessionIdleTimeout {
			stale = append(stale, sess)
		}
	}
	bp.sessMu.RUnlock()
	for _, sess := range stale {
		// Closing the upstream socket is enough: pumpUpstreamToClient sees
		// ErrClosed on its next Read and cleans up the map entry itself.
		_ = sess.upstreamConn.Close()
	}
	if len(stale) > 0 {
		bp.s.log.Debug("bedrock janitor reaped sessions", "count", len(stale))
	}
}

func (bp *bedrockProxy) closeAllSessions() {
	bp.sessMu.RLock()
	all := make([]*udpSession, 0, len(bp.sessions))
	for _, sess := range bp.sessions {
		all = append(all, sess)
	}
	bp.sessMu.RUnlock()
	for _, sess := range all {
		_ = sess.upstreamConn.Close()
	}
}

// --- protocol helpers (unchanged from the wake-catcher version) -------------

func isUnconnectedPing(pkt []byte) bool {
	if len(pkt) < raknetMinPingLen {
		return false
	}
	if pkt[0] != raknetIDUnconnectedPing {
		return false
	}
	return bytesEqual(pkt[9:9+16], raknetMagic)
}

// buildUnconnectedPong constructs the wire-format Pong:
//
//	id(1)=0x1C | time(8) | serverGUID(8) | magic(16) | motdLen(uint16) | motd
func buildUnconnectedPong(pingTime uint64, serverGUID int64, motd string) []byte {
	out := make([]byte, 0, 1+8+8+16+2+len(motd))
	out = append(out, raknetIDUnconnectedPong)

	var tb [8]byte
	binary.BigEndian.PutUint64(tb[:], pingTime)
	out = append(out, tb[:]...)

	var gb [8]byte
	binary.BigEndian.PutUint64(gb[:], uint64(serverGUID))
	out = append(out, gb[:]...)

	out = append(out, raknetMagic...)

	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(len(motd)))
	out = append(out, lb[:]...)
	out = append(out, motd...)
	return out
}

// buildBedrockMOTD constructs the semicolon-delimited MOTD payload the
// Bedrock client expects for the sleeping reply. Fields, in order:
//
//	0  edition        ("MCPE" for Bedrock)
//	1  motd line 1
//	2  protocol version
//	3  game version string
//	4  players online
//	5  players max
//	6  server GUID (string)
//	7  motd line 2 / world name
//	8  gamemode name
//	9  gamemode numeric
//	10 IPv4 port
//	11 IPv6 port
func buildBedrockMOTD(cfg config) string {
	motd := sanitizeMOTD(cfg.SleepingMOTD)
	parts := []string{
		"MCPE",
		motd,
		"729",
		"1.21.0",
		"0",
		fmt.Sprintf("%d", cfg.MaxPlayers),
		"0",
		"Sleeping",
		"Survival",
		"1",
		"19132",
		"19133",
	}
	return strings.Join(parts, ";")
}

func sanitizeMOTD(s string) string {
	s = strings.ReplaceAll(s, ";", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
