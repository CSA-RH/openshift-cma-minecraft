package main

// Bedrock RakNet Unconnected Ping / Pong handling.
//
// This file is the UDP counterpart to slp.go's TCP sleeping handler. It
// listens on UDP for Bedrock client pings, signals a wake, and answers with
// a minimal Unconnected Pong so the client's server-list entry resolves with
// our "Sleeping" MOTD instead of "can't reach server".
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
	"time"
)

const (
	raknetIDUnconnectedPing = 0x01
	raknetIDUnconnectedPong = 0x1C

	// minPingLen = 1 (id) + 8 (time) + 16 (magic) + 8 (clientGUID)
	raknetMinPingLen = 1 + 8 + 16 + 8
)

// raknetMagic is the fixed 16-byte "magic" sequence every RakNet packet
// carries. Used to discriminate Bedrock pings from random UDP traffic.
var raknetMagic = []byte{
	0x00, 0xFF, 0xFF, 0x00, 0xFE, 0xFE, 0xFE, 0xFE,
	0xFD, 0xFD, 0xFD, 0xFD, 0x12, 0x34, 0x56, 0x78,
}

// runBedrockCatcher starts the UDP listener for Bedrock client pings.
// It is a no-op if Bedrock support isn't enabled in the config — the caller
// should check cfg.bedrockEnabled() first.
//
// One server-side GUID is generated at startup; the same value is reported in
// every Pong so clients see a stable identity.
func runBedrockCatcher(ctx context.Context, s *state) {
	addr := s.cfg.BedrockListenAddr
	if addr == "" {
		s.log.Info("bedrock catcher disabled (no listen addr)")
		return
	}

	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		s.log.Error("bedrock listen failed", "addr", addr, "err", err)
		return
	}
	s.log.Info("bedrock catcher listening",
		"addr", addr,
		"upstream", s.cfg.BedrockUpstreamAddr)

	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()

	// Stable server GUID for the lifetime of this waker process. Real
	// Bedrock servers persist this; for a stateless waker, per-process is
	// fine — restarts are infrequent and the field is cosmetic.
	serverGUID := int64(time.Now().UnixNano())

	buf := make([]byte, 1500) // one UDP MTU is plenty for an Unconnected Ping
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Debug("bedrock read error", "err", err)
			continue
		}
		handleBedrockDatagram(s, pc, src, buf[:n], serverGUID)
	}
}

// handleBedrockDatagram processes one UDP packet. If it's an Unconnected
// Ping, we bump the wake signal and reply with an Unconnected Pong. Any
// other packet is ignored — we are explicitly not a full RakNet server.
func handleBedrockDatagram(s *state, pc net.PacketConn, src net.Addr, pkt []byte, serverGUID int64) {
	if !isUnconnectedPing(pkt) {
		return
	}
	s.bedrockPings.Add(1)
	s.signalWake()

	// Echo back the client's ping time so it can compute latency.
	pingTime := binary.BigEndian.Uint64(pkt[1:9])

	resp := buildUnconnectedPong(pingTime, serverGUID, buildBedrockMOTD(s.cfg))
	_ = pc.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := pc.WriteTo(resp, src); err != nil {
		s.log.Debug("bedrock pong write failed", "src", src, "err", err)
	}
}

func isUnconnectedPing(pkt []byte) bool {
	if len(pkt) < raknetMinPingLen {
		return false
	}
	if pkt[0] != raknetIDUnconnectedPing {
		return false
	}
	// Magic begins at offset 9 (after 1-byte id + 8-byte time).
	return bytesEqual(pkt[9:9+16], raknetMagic)
}

// buildUnconnectedPong constructs the wire-format Pong:
//
//	id(1)=0x1C | time(8) | serverGUID(8) | magic(16) | motdLen(uint16) | motd
//
// The MOTD is a single semicolon-delimited string with a strict field order
// the Bedrock client expects.
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
// Bedrock client expects. Fields, in order:
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
//
// Many clients tolerate missing trailing fields, but we ship all 12 to be
// safe and to make the entry render predictably across client versions.
func buildBedrockMOTD(cfg config) string {
	motd := sanitizeMOTD(cfg.SleepingMOTD)
	// 729 is the Bedrock protocol number around 1.21.x; clients tolerate a
	// reasonable advertised number. The cosmetic version string is what
	// shows in the server list.
	parts := []string{
		"MCPE",
		motd,
		"729",
		"1.21.0",
		"0", // players online — we're asleep
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

// sanitizeMOTD strips characters that break the semicolon-delimited
// envelope: semicolons themselves, and newlines which the Bedrock server
// list can't render anyway.
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

