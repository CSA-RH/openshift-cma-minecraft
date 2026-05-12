package main

import (
	"bufio"
	"bytes"
	"testing"
)

func TestVarIntRoundTrip(t *testing.T) {
	cases := []int32{0, 1, 127, 128, 255, 2097151, 2147483647, -1}
	for _, v := range cases {
		buf := appendVarInt(nil, v)
		got, err := readVarInt(bufio.NewReader(bytes.NewReader(buf)))
		if err != nil {
			t.Fatalf("readVarInt(%d): %v", v, err)
		}
		if got != v {
			t.Fatalf("VarInt round-trip: in=%d out=%d", v, got)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, s := range []string{"", "hello", "§eMOTD test", "a longer string with spaces"} {
		buf := encodeString(s)
		out, n, err := decodeString(buf)
		if err != nil {
			t.Fatalf("decodeString(%q): %v", s, err)
		}
		if n != len(buf) {
			t.Fatalf("decodeString(%q): consumed %d of %d bytes", s, n, len(buf))
		}
		if out != s {
			t.Fatalf("string round-trip: in=%q out=%q", s, out)
		}
	}
}

func TestPacketRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := encodeString("hello")
	if err := writePacket(&buf, 0x42, payload); err != nil {
		t.Fatalf("writePacket: %v", err)
	}
	pid, body, err := readPacket(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("readPacket: %v", err)
	}
	if pid != 0x42 {
		t.Fatalf("packet id: got 0x%02x, want 0x42", pid)
	}
	got, _, err := decodeString(body)
	if err != nil {
		t.Fatalf("decodeString: %v", err)
	}
	if got != "hello" {
		t.Fatalf("payload: got %q, want %q", got, "hello")
	}
}

func TestBuildSleepingStatus(t *testing.T) {
	cfg := config{
		ProtocolVersion: 769,
		VersionName:     "Sleeping",
		MaxPlayers:      20,
		SleepingMOTD:    "test motd",
	}
	out, err := buildSleepingStatus(cfg)
	if err != nil {
		t.Fatalf("buildSleepingStatus: %v", err)
	}
	js, _, err := decodeString(out)
	if err != nil {
		t.Fatalf("decodeString: %v", err)
	}
	if !bytes.Contains([]byte(js), []byte(`"name":"Sleeping"`)) {
		t.Fatalf("status JSON missing version name; got %s", js)
	}
	if !bytes.Contains([]byte(js), []byte(`"text":"test motd"`)) {
		t.Fatalf("status JSON missing MOTD; got %s", js)
	}
}
