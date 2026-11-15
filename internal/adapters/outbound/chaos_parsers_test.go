package outbound

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestPostgresBuildErrorResponse(t *testing.T) {
	parser := NewPostgresParser()
	errMsg := "Postgres query blocked"
	res := parser.BuildErrorResponse(errMsg)

	if len(res) < 5 {
		t.Fatalf("packet too short: %d", len(res))
	}
	if res[0] != 'E' {
		t.Errorf("expected header 'E', got %c", res[0])
	}

	length := binary.BigEndian.Uint32(res[1:5])
	if int(length) != len(res)-1 {
		t.Errorf("expected packet length %d, got header value %d", len(res)-1, length)
	}

	// Verify the payload contains our message
	payload := string(res[5:])
	if !strings.Contains(payload, errMsg) {
		t.Errorf("expected payload to contain %q, got: %q", errMsg, payload)
	}
	if !strings.Contains(payload, "ERROR") {
		t.Error("expected severity 'ERROR'")
	}
	if !strings.Contains(payload, "XX000") {
		t.Error("expected SQLSTATE 'XX000'")
	}
}

func TestMySQLBuildErrorResponse(t *testing.T) {
	parser := NewMySQLParser()
	errMsg := "MySQL query blocked"
	res := parser.BuildErrorResponse(errMsg)

	if len(res) < 4 {
		t.Fatalf("packet too short: %d", len(res))
	}

	// First 3 bytes of header: packet payload length (little endian)
	length := int(res[0]) | int(res[1])<<8 | int(res[2])<<16
	if length != len(res)-4 {
		t.Errorf("expected MySQL payload length %d, got header value %d", len(res)-4, length)
	}

	// Sequence number should be 1
	if res[3] != 1 {
		t.Errorf("expected sequence ID 1, got %d", res[3])
	}

	payload := res[4:]
	if payload[0] != 0xff {
		t.Errorf("expected ERR packet header 0xff, got %x", payload[0])
	}

	errCode := binary.LittleEndian.Uint16(payload[1:3])
	if errCode != 1105 {
		t.Errorf("expected error code 1105, got %d", errCode)
	}

	if payload[3] != '#' {
		t.Errorf("expected SQL State marker '#', got %c", payload[3])
	}

	sqlState := string(payload[4:9])
	if sqlState != "HY000" {
		t.Errorf("expected SQL State 'HY000', got %q", sqlState)
	}

	msg := string(payload[9:])
	if msg != errMsg {
		t.Errorf("expected message %q, got %q", errMsg, msg)
	}
}

func TestRedisBuildErrorResponse(t *testing.T) {
	parser := NewRedisParser()
	errMsg := "Redis command blocked"
	res := parser.BuildErrorResponse(errMsg)

	expected := "-ERR " + errMsg + "\r\n"
	if !bytes.Equal(res, []byte(expected)) {
		t.Errorf("expected redis error %q, got %q", expected, string(res))
	}
}
