package services

import (
	"encoding/binary"
	"testing"
)

func TestParseISO8583ResponseConsumesACompleteLengthFramedMessage(t *testing.T) {
	payload := []byte("0210" + "0000000000000000")
	frame := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(payload)))
	copy(frame[2:], payload)

	response, consumed, ok := parseResponse("iso8583", frame)
	if !ok || consumed != len(frame) {
		t.Fatalf("ISO8583 response parsed as (%q, %d, %t), want a complete response consuming %d bytes", response, consumed, ok, len(frame))
	}
	if response != "ISO8583 response received" {
		t.Fatalf("ISO8583 response = %q", response)
	}
}

func TestParseISO8583ResponseWaitsForFullFrame(t *testing.T) {
	incomplete := []byte{0, 20, '0', '2', '1', '0'}
	if response, consumed, ok := parseResponse("iso8583", incomplete); ok || consumed != 0 || response != "" {
		t.Fatalf("incomplete ISO8583 response parsed as (%q, %d, %t), want no response", response, consumed, ok)
	}
}

func TestParseRedisResponseRESP3Map(t *testing.T) {
	input := []byte("%2\r\n$6\r\nserver\r\n$5\r\nredis\r\n$7\r\nversion\r\n$5\r\n7.2.0\r\n")

	response, consumed, ok := parseRedisResponse(input)
	if !ok || consumed != len(input) {
		t.Fatalf("RESP3 map parsed as (%q, %d, %t), want a complete response consuming %d bytes", response, consumed, ok, len(input))
	}
	if response != `{"server": "redis", "version": "7.2.0"}` {
		t.Fatalf("RESP3 map response = %q", response)
	}
}

func TestParseRedisResponseRESP3MapDenganNilaiNested(t *testing.T) {
	input := []byte("%1\r\n$7\r\nmodules\r\n*1\r\n%1\r\n$4\r\nname\r\n$10\r\ntimeseries\r\n")

	response, consumed, ok := parseRedisResponse(input)
	if !ok || consumed != len(input) {
		t.Fatalf("nested RESP3 map parsed as (%q, %d, %t), want complete response consuming %d bytes", response, consumed, ok, len(input))
	}
	if response != `{"modules": [{"name": "timeseries"}]}` {
		t.Fatalf("nested RESP3 map response = %q", response)
	}
}

func TestParseRedisResponse(t *testing.T) {
	tests := []struct {
		input    []byte
		expected string
		consumed int
		ok       bool
	}{
		{[]byte("+OK\r\n"), "OK", 5, true},
		{[]byte("-ERR some error\r\n"), "Error: ERR some error", 17, true},
		{[]byte(":1000\r\n"), "1000", 7, true},
		{[]byte("$5\r\nhello\r\n"), "\"hello\"", 11, true},
		{[]byte("$-1\r\n"), "nil", 5, true},
		{[]byte("*2\r\n$3\r\nfoo\r\n$3\r\nbar\r\n"), "[\"foo\", \"bar\"]", 22, true},
		{[]byte("*0\r\n"), "[]", 4, true},
	}

	for _, tc := range tests {
		res, consumed, ok := parseRedisResponse(tc.input)
		if ok != tc.ok || consumed != tc.consumed || (ok && res != tc.expected) {
			t.Errorf("redis: input %q, expected (%q, %d, %t), got (%q, %d, %t)", tc.input, tc.expected, tc.consumed, tc.ok, res, consumed, ok)
		}
	}
}

func TestParsePostgresResponse(t *testing.T) {
	// 1. SSL Response
	res, consumed, ok := parsePostgresResponse([]byte{'S'})
	if !ok || consumed != 1 || res != "SSL Response: S" {
		t.Errorf("expected SSL response, got (%q, %d, %t)", res, consumed, ok)
	}

	// 2. SSL Response followed by AuthOk
	buf := []byte{'S', 'R', 0, 0, 0, 8, 0, 0, 0, 0}
	res, consumed, ok = parsePostgresResponse(buf)
	if !ok || consumed != 1 || res != "SSL Response: S" {
		t.Errorf("expected SSL response to consume 1 byte, got (%q, %d, %t)", res, consumed, ok)
	}

	// 3. Simple OK response (ReadyForQuery)
	zPacket := make([]byte, 6)
	zPacket[0] = 'Z'
	binary.BigEndian.PutUint32(zPacket[1:5], 5)
	zPacket[5] = 'I' // Idle

	res, consumed, ok = parsePostgresResponse(zPacket)
	if !ok || consumed != 6 || res != "OK" {
		t.Errorf("expected OK for Z packet, got (%q, %d, %t)", res, consumed, ok)
	}

	// 4. CommandComplete followed by ReadyForQuery
	cPacket := []byte{'C', 0, 0, 0, 14}
	cPacket = append(cPacket, []byte("SELECT 42\x00")...)
	fullBuf := append(cPacket, zPacket...)
	res, consumed, ok = parsePostgresResponse(fullBuf)
	if !ok || consumed != len(fullBuf) || res != "SELECT 42" {
		t.Errorf("expected SELECT 42, got (%q, %d, %t)", res, consumed, ok)
	}
}
