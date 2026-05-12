package main

// Minimal Java Edition Server List Ping (SLP) implementation — just enough
// to answer the client's "Refresh" while the real server is asleep, and to
// poll the real server for a player count when it's awake.
//
// Reference: https://wiki.vg/Server_List_Ping

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	pktHandshake    = 0x00
	pktStatusReq    = 0x00
	pktStatusResp   = 0x00
	pktPing         = 0x01
	pktPong         = 0x01
	pktLoginStart   = 0x00
	pktDisconnect   = 0x00
	nextStateStatus = 1
	nextStateLogin  = 2

	maxPacketSize = 1 << 20 // 1 MiB hard cap; SLP packets are tiny
)

type handshake struct {
	ProtocolVersion int32
	ServerAddress   string
	ServerPort      uint16
	NextState       int32
}

// handleSleepingClient services a client that connected while the upstream is
// down. It reads the handshake, then either fakes a status response or sends
// a friendly disconnect, depending on the client's intent.
func handleSleepingClient(conn net.Conn, cfg config) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	hs, err := readHandshake(br)
	if err != nil {
		return fmt.Errorf("read handshake: %w", err)
	}
	switch hs.NextState {
	case nextStateStatus:
		return handleSleepingStatus(br, conn, cfg)
	case nextStateLogin:
		return handleSleepingLogin(br, conn, cfg)
	default:
		return fmt.Errorf("unsupported next state %d", hs.NextState)
	}
}

func handleSleepingStatus(br *bufio.Reader, conn net.Conn, cfg config) error {
	if _, _, err := readPacket(br); err != nil {
		return fmt.Errorf("read status request: %w", err)
	}
	payload, err := buildSleepingStatus(cfg)
	if err != nil {
		return err
	}
	if err := writePacket(conn, pktStatusResp, payload); err != nil {
		return fmt.Errorf("write status response: %w", err)
	}
	// Many clients then send a Ping with a long payload; echo it back as a
	// Pong so the server-list shows a real-looking latency. Ignore errors:
	// the client is allowed to close right after the status response.
	pid, body, err := readPacket(br)
	if err != nil {
		return nil
	}
	if pid == pktPing {
		return writePacket(conn, pktPong, body)
	}
	return nil
}

func handleSleepingLogin(br *bufio.Reader, conn net.Conn, cfg config) error {
	if _, _, err := readPacket(br); err != nil {
		return fmt.Errorf("read login start: %w", err)
	}
	chat, _ := json.Marshal(map[string]any{
		"text":  cfg.DisconnectMsg,
		"color": "yellow",
	})
	return writePacket(conn, pktDisconnect, encodeString(string(chat)))
}

// statusJSON is the structure the client expects in the status response.
type statusJSON struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players struct {
		Max    int   `json:"max"`
		Online int   `json:"online"`
		Sample []any `json:"sample"`
	} `json:"players"`
	Description struct {
		Text string `json:"text"`
	} `json:"description"`
}

func buildSleepingStatus(cfg config) ([]byte, error) {
	var s statusJSON
	s.Version.Name = cfg.VersionName
	s.Version.Protocol = cfg.ProtocolVersion
	s.Players.Max = cfg.MaxPlayers
	s.Players.Online = 0
	s.Players.Sample = []any{}
	s.Description.Text = cfg.SleepingMOTD
	body, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return encodeString(string(body)), nil
}

// probeUpstream performs a full SLP status round-trip against addr and
// returns the reported online-player count. Used by the probe loop.
func probeUpstream(addr string, timeout time.Duration) (int, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	host, port, err := splitHostPort(addr)
	if err != nil {
		return 0, err
	}

	// Handshake (next state = 1, Status)
	hsPayload := appendVarInt(nil, 769) // any plausible protocol version works for SLP
	hsPayload = append(hsPayload, encodeString(host)...)
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], port)
	hsPayload = append(hsPayload, portBuf[:]...)
	hsPayload = appendVarInt(hsPayload, nextStateStatus)
	if err := writePacket(conn, pktHandshake, hsPayload); err != nil {
		return 0, err
	}
	if err := writePacket(conn, pktStatusReq, nil); err != nil {
		return 0, err
	}

	br := bufio.NewReader(conn)
	pid, body, err := readPacket(br)
	if err != nil {
		return 0, err
	}
	if pid != pktStatusResp {
		return 0, fmt.Errorf("unexpected packet id 0x%02x in status reply", pid)
	}
	js, _, err := decodeString(body)
	if err != nil {
		return 0, err
	}
	// Use a minimal struct: real servers return `description` in many
	// shapes (string, {text}, full chat component, array). We only care
	// about the player count, so parse just that and ignore the rest.
	var minimal struct {
		Players struct {
			Online int `json:"online"`
		} `json:"players"`
	}
	if err := json.Unmarshal([]byte(js), &minimal); err != nil {
		return 0, fmt.Errorf("parse status JSON: %w (raw=%.200s)", err, js)
	}
	return minimal.Players.Online, nil
}

// --- Wire format helpers (VarInt, String, Packet) ---------------------------

// writePacket writes "VarInt length | VarInt packetID | payload".
func writePacket(w io.Writer, packetID int32, payload []byte) error {
	idBuf := appendVarInt(nil, packetID)
	body := make([]byte, 0, len(idBuf)+len(payload))
	body = append(body, idBuf...)
	body = append(body, payload...)
	hdr := appendVarInt(nil, int32(len(body)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func readPacket(r *bufio.Reader) (int32, []byte, error) {
	length, err := readVarInt(r)
	if err != nil {
		return 0, nil, err
	}
	if length <= 0 || length > maxPacketSize {
		return 0, nil, fmt.Errorf("invalid packet length %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	pid, n := decodeVarInt(body)
	if n <= 0 {
		return 0, nil, errors.New("invalid packet ID varint")
	}
	return pid, body[n:], nil
}

func readHandshake(r *bufio.Reader) (*handshake, error) {
	pid, body, err := readPacket(r)
	if err != nil {
		return nil, err
	}
	if pid != pktHandshake {
		return nil, fmt.Errorf("expected handshake (0x00), got 0x%02x", pid)
	}
	pos := 0
	pv, n := decodeVarInt(body[pos:])
	if n <= 0 {
		return nil, errors.New("bad protocol-version varint")
	}
	pos += n
	addr, n, err := decodeString(body[pos:])
	if err != nil {
		return nil, err
	}
	pos += n
	if len(body)-pos < 2 {
		return nil, errors.New("missing server-port field")
	}
	port := binary.BigEndian.Uint16(body[pos : pos+2])
	pos += 2
	nextState, n := decodeVarInt(body[pos:])
	if n <= 0 {
		return nil, errors.New("bad next-state varint")
	}
	return &handshake{
		ProtocolVersion: pv,
		ServerAddress:   addr,
		ServerPort:      port,
		NextState:       nextState,
	}, nil
}

func appendVarInt(buf []byte, v int32) []byte {
	uv := uint32(v)
	for {
		b := byte(uv & 0x7F)
		uv >>= 7
		if uv != 0 {
			b |= 0x80
		}
		buf = append(buf, b)
		if uv == 0 {
			return buf
		}
	}
}

func readVarInt(r io.ByteReader) (int32, error) {
	var result uint32
	for i := 0; i < 5; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		result |= uint32(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return int32(result), nil
		}
	}
	return 0, errors.New("VarInt too big")
}

func decodeVarInt(b []byte) (int32, int) {
	var result uint32
	for i := 0; i < 5 && i < len(b); i++ {
		result |= uint32(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			return int32(result), i + 1
		}
	}
	return 0, 0
}

func encodeString(s string) []byte {
	out := appendVarInt(nil, int32(len(s)))
	return append(out, s...)
}

func decodeString(b []byte) (string, int, error) {
	length, n := decodeVarInt(b)
	if n <= 0 {
		return "", 0, errors.New("bad string-length varint")
	}
	if length < 0 || int(length)+n > len(b) {
		return "", 0, errors.New("string out of bounds")
	}
	return string(b[n : n+int(length)]), n + int(length), nil
}

func splitHostPort(addr string) (string, uint16, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	var pn uint16
	if _, err := fmt.Sscanf(p, "%d", &pn); err != nil {
		return "", 0, err
	}
	return h, pn, nil
}
