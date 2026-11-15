package outbound

import (
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ISO8583Parser incrementally extracts safe summaries from two-byte
// big-endian length-framed ASCII GenericPackager messages. One parser is used
// per TCP connection, so it can safely retain incomplete client/server frames.
type ISO8583Parser struct {
	fields        map[int]ISO8583FieldDefinition
	clientPending []byte
	serverPending []byte
}

// ISO8583FieldDefinition is loaded from the GenericPackager XML rather than
// maintained as a partial, divergent field table in code.
type ISO8583FieldDefinition struct {
	Length         int
	Name           string
	Class          string
	VariableDigits int
}

type isoPackagerXML struct {
	Fields []isoFieldXML `xml:"isofield"`
}

type isoFieldXML struct {
	ID     int    `xml:"id,attr"`
	Length int    `xml:"length,attr"`
	Name   string `xml:"name,attr"`
	Class  string `xml:"class,attr"`
}

// NewISO8583Parser loads packager/iso8583/iso_spec.xml. The lookup walks up
// from the process working directory so it also works under go test package
// directories while keeping the configured path relative to the repository.
func NewISO8583Parser() (*ISO8583Parser, error) {
	definitions, err := loadISO8583Definitions(findISO8583Spec())
	if err != nil {
		return nil, err
	}
	return &ISO8583Parser{fields: definitions}, nil
}

// NewConnectionParser creates an isolated parser for one TCP connection while
// reusing the already validated XML field definitions. Pending bytes must never
// be shared across client sockets.
func (p *ISO8583Parser) NewConnectionParser() *ISO8583Parser {
	return &ISO8583Parser{fields: p.fields}
}

func findISO8583Spec() string {
	wd, err := os.Getwd()
	if err != nil {
		return filepath.Join("packager", "iso8583", "iso_spec.xml")
	}
	for {
		candidate := filepath.Join(wd, "packager", "iso8583", "iso_spec.xml")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return filepath.Join("packager", "iso8583", "iso_spec.xml")
		}
		wd = parent
	}
}

func loadISO8583Definitions(path string) (map[int]ISO8583FieldDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ISO8583 packager %q: %w", path, err)
	}
	var packager isoPackagerXML
	if err := xml.Unmarshal(data, &packager); err != nil {
		return nil, fmt.Errorf("parse ISO8583 packager %q: %w", path, err)
	}
	definitions := make(map[int]ISO8583FieldDefinition, len(packager.Fields))
	for _, field := range packager.Fields {
		if field.ID < 0 || field.ID > 128 || field.Length < 0 || field.Class == "" {
			return nil, fmt.Errorf("invalid ISO8583 field definition %d", field.ID)
		}
		variableDigits := 0
		switch {
		case strings.Contains(field.Class, "_LLL"):
			variableDigits = 3
		case strings.Contains(field.Class, "_LL"):
			variableDigits = 2
		}
		definitions[field.ID] = ISO8583FieldDefinition{Length: field.Length, Name: field.Name, Class: field.Class, VariableDigits: variableDigits}
	}
	if definitions[0].Length != 4 || definitions[1].Length == 0 {
		return nil, fmt.Errorf("ISO8583 packager missing MTI or bitmap definition")
	}
	return definitions, nil
}

// Parse buffers client TCP bytes and returns one redacted summary for every
// complete, structurally valid ISO8583 frame.
func (p *ISO8583Parser) Parse(data []byte) []string {
	p.clientPending = append(p.clientPending, data...)
	return p.consumeFrames(&p.clientPending)
}

// ParseServerResponse consumes server TCP frames to preserve framing state.
// It intentionally emits no server logs because ProtocolParser exposes only
// client request summaries.
func (p *ISO8583Parser) ParseServerResponse(data []byte) {
	p.serverPending = append(p.serverPending, data...)
	p.consumeFrames(&p.serverPending)
}

func (p *ISO8583Parser) consumeFrames(pending *[]byte) []string {
	var summaries []string
	for len(*pending) >= 2 {
		length := int(binary.BigEndian.Uint16((*pending)[:2]))
		if len(*pending) < 2+length {
			break
		}
		payload := (*pending)[2 : 2+length]
		*pending = (*pending)[2+length:]
		if summary, ok := p.parsePayload(payload); ok {
			summaries = append(summaries, summary)
		}
	}
	return summaries
}

func (p *ISO8583Parser) parsePayload(payload []byte) (string, bool) {
	if len(payload) < 20 { // MTI (4) + ASCII-hex primary bitmap (16)
		return "", false
	}
	mti := string(payload[:4])
	if !isASCIIDigits(mti) {
		return "", false
	}
	primary, ok := decodeBitmap(payload[4:20])
	if !ok {
		return "", false
	}
	offset := 20
	bits := primary
	if primary[0] { // DE1: secondary bitmap indicator, not an independent data field.
		if len(payload) < offset+16 {
			return "", false
		}
		secondary, valid := decodeBitmap(payload[offset : offset+16])
		if !valid {
			return "", false
		}
		offset += 16
		bits = append(bits, secondary...)
	}

	parts := []string{"ISO8583", "MTI=" + mti}
	for index, present := range bits {
		fieldID := index + 1
		if !present || fieldID == 1 {
			continue
		}
		definition, exists := p.fields[fieldID]
		if !exists {
			return "", false
		}
		value, consumed, valid := readISO8583Field(payload[offset:], definition)
		if !valid {
			return "", false
		}
		offset += consumed
		if sensitiveISO8583Field(fieldID) {
			parts = append(parts, fmt.Sprintf("DE%d=<redacted>", fieldID))
		} else {
			parts = append(parts, fmt.Sprintf("DE%d=%s", fieldID, value))
		}
	}
	if offset != len(payload) {
		return "", false
	}
	return strings.Join(parts, " "), true
}

func decodeBitmap(data []byte) ([]bool, bool) {
	if len(data) != 16 {
		return nil, false
	}
	bits := make([]bool, 64)
	for i, character := range data {
		var nibble byte
		switch {
		case character >= '0' && character <= '9':
			nibble = character - '0'
		case character >= 'A' && character <= 'F':
			nibble = character - 'A' + 10
		case character >= 'a' && character <= 'f':
			nibble = character - 'a' + 10
		default:
			return nil, false
		}
		for bit := 0; bit < 4; bit++ {
			bits[i*4+bit] = nibble&(1<<uint(3-bit)) != 0
		}
	}
	return bits, true
}

func readISO8583Field(data []byte, definition ISO8583FieldDefinition) (string, int, bool) {
	length := definition.Length
	prefix := 0
	if definition.VariableDigits > 0 {
		if len(data) < definition.VariableDigits || !isASCIIDigits(string(data[:definition.VariableDigits])) {
			return "", 0, false
		}
		parsed, err := strconv.Atoi(string(data[:definition.VariableDigits]))
		if err != nil || parsed > definition.Length {
			return "", 0, false
		}
		length, prefix = parsed, definition.VariableDigits
	}
	// IFA_BINARY stores each byte as two ASCII hexadecimal characters.
	if strings.Contains(definition.Class, "BINARY") {
		length *= 2
	}
	if len(data) < prefix+length {
		return "", 0, false
	}
	value := string(data[prefix : prefix+length])
	return value, prefix + length, true
}

func isASCIIDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func sensitiveISO8583Field(fieldID int) bool {
	if fieldID >= 102 {
		return true
	}
	switch fieldID {
	case 2, 35, 36, 45, 52, 53:
		return true
	}
	return false
}

// BuildErrorResponse returns no bytes: ISO8583 error responses require request
// context (at least MTI/bitmap and usually DE11/41) that this proxy interface
// does not provide. An empty output is safer than emitting a malformed frame.
func (p *ISO8583Parser) BuildErrorResponse(errMsg string) []byte { return nil }
