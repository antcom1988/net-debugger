package outbound

import (
	"encoding/binary"
	"strings"
	"testing"
)

func isoFrame(payload string) []byte {
	frame := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(payload)))
	copy(frame[2:], payload)
	return frame
}

func TestISO8583LoadsDefinitionsFromPackager(t *testing.T) {
	parser, err := NewISO8583Parser()
	if err != nil {
		t.Fatalf("NewISO8583Parser() error = %v", err)
	}

	if got := parser.fields[2]; got.Length != 19 || got.VariableDigits != 2 {
		t.Fatalf("DE2 definition = %+v, want LLVAR length 19", got)
	}
	if got := parser.fields[36]; got.Length != 104 || got.VariableDigits != 3 {
		t.Fatalf("DE36 definition = %+v, want LLLVAR length 104", got)
	}
}

func TestISO8583ParsesFragmentedFrame(t *testing.T) {
	parser, err := NewISO8583Parser()
	if err != nil {
		t.Fatal(err)
	}
	// DE3 and DE4 are present in primary bitmap 0x3000000000000000.
	frame := isoFrame("02003000000000000000" + "000000" + "000000001234")

	if got := parser.Parse(frame[:7]); len(got) != 0 {
		t.Fatalf("first fragment logs = %v, want none", got)
	}
	logs := parser.Parse(frame[7:])
	if len(logs) != 1 {
		t.Fatalf("logs = %v, want one summary", logs)
	}
	if !strings.Contains(logs[0], "MTI=0200") || !strings.Contains(logs[0], "DE3=000000") || !strings.Contains(logs[0], "DE4=000000001234") {
		t.Errorf("summary = %q", logs[0])
	}
}

func TestISO8583ResponseSecondaryBitmap(t *testing.T) {
	parser, err := NewISO8583Parser()
	if err != nil {
		t.Fatal(err)
	}
	// Primary DE1 announces a secondary bitmap; secondary DE70 is present.
	frame := isoFrame("081080000000000000000400000000000000" + "301")
	parser.ParseServerResponse(frame[:8])
	parser.ParseServerResponse(frame[8:])

	if got := parser.Parse(isoFrame("02002000000000000000" + "000000")); len(got) != 1 {
		t.Fatalf("parser stopped after buffered response: %v", got)
	}
}

func TestISO8583RedactsSensitiveFields(t *testing.T) {
	parser, err := NewISO8583Parser()
	if err != nil {
		t.Fatal(err)
	}
	// DE2, DE35, DE52 and DE102 are present. Their values must never reach logs.
	payload := "0200" + "C000000020001000" + "0000000004000000" +
		"16" + "4111111111111111" + "37" + "4111111111111111=25122010000012345678" +
		"1234567890ABCDEF" + "06" + "SECRET"
	logs := parser.Parse(isoFrame(payload))
	if len(logs) != 1 {
		t.Fatalf("logs = %v, want one summary", logs)
	}
	for _, secret := range []string{"4111111111111111", "25122010000012345678", "12345678", "SECRET"} {
		if strings.Contains(logs[0], secret) {
			t.Errorf("summary leaked %q: %q", secret, logs[0])
		}
	}
	for _, field := range []string{"DE2=<redacted>", "DE35=<redacted>", "DE52=<redacted>", "DE102=<redacted>"} {
		if !strings.Contains(logs[0], field) {
			t.Errorf("summary missing %q: %q", field, logs[0])
		}
	}
}

func TestISO8583IgnoresMalformedAndIncompleteFrames(t *testing.T) {
	parser, err := NewISO8583Parser()
	if err != nil {
		t.Fatal(err)
	}
	incomplete := []byte{0, 20, '0', '2', '0', '0'}
	if got := parser.Parse(incomplete); len(got) != 0 {
		t.Errorf("incomplete logs = %v, want none", got)
	}
	malformedParser, err := NewISO8583Parser()
	if err != nil {
		t.Fatal(err)
	}
	// Complete but invalid ASCII-hex bitmap must be dropped and not poison later frames.
	if got := malformedParser.Parse(isoFrame("0200ZZZZZZZZZZZZZZZZ")); len(got) != 0 {
		t.Errorf("malformed logs = %v, want none", got)
	}
	if got := malformedParser.Parse(isoFrame("02002000000000000000" + "000000")); len(got) != 1 {
		t.Errorf("valid frame after malformed logs = %v, want one", got)
	}
}

func TestISO8583BuildErrorResponseIsLengthFramed(t *testing.T) {
	parser, err := NewISO8583Parser()
	if err != nil {
		t.Fatal(err)
	}
	response := parser.BuildErrorResponse("blocked")
	if len(response) != 0 && (len(response) < 2 || int(binary.BigEndian.Uint16(response[:2])) != len(response)-2) {
		t.Errorf("invalid length-framed response %v", response)
	}
}
