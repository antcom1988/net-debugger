package outbound

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
)

// PostgresParser statefully handles query extraction and parameter decoding per connection.
type PostgresParser struct {
	stmtMap map[string]string
}

// NewPostgresParser creates a new PostgresParser instance.
func NewPostgresParser() *PostgresParser {
	return &PostgresParser{
		stmtMap: make(map[string]string),
	}
}

// ParseServerResponse is a no-op for PostgreSQL since name mapping is client-side driven.
func (p *PostgresParser) ParseServerResponse(data []byte) {}

// BuildErrorResponse constructs a PostgreSQL protocol-compliant ErrorResponse ('E') packet.
func (p *PostgresParser) BuildErrorResponse(errMsg string) []byte {
	var buf bytes.Buffer
	// Severity field
	buf.WriteByte('S')
	buf.WriteString("ERROR")
	buf.WriteByte(0)
	// SQLState code field (XX000 - internal error)
	buf.WriteByte('C')
	buf.WriteString("XX000")
	buf.WriteByte(0)
	// Message field
	buf.WriteByte('M')
	buf.WriteString(errMsg)
	buf.WriteByte(0)
	// Terminating byte
	buf.WriteByte(0)

	payload := buf.Bytes()
	length := uint32(4 + len(payload))

	res := make([]byte, 1+4+len(payload))
	res[0] = 'E'
	binary.BigEndian.PutUint32(res[1:5], length)
	copy(res[5:], payload)
	return res
}

// Parse processes PostgreSQL client-to-server protocol v3.0 packets.
// It extracts simple queries ('Q'), prepares statements ('P'), and binds statement parameter values ('B').
func (p *PostgresParser) Parse(data []byte) []string {
	var queries []string
	idx := 0

	// Check if this is a startup message or SSL request which doesn't follow normal type-length format.
	if len(data) >= 8 {
		length := binary.BigEndian.Uint32(data[0:4])
		if length <= 1024 {
			proto := binary.BigEndian.Uint32(data[4:8])
			if proto == 80877103 || proto == 196608 {
				return nil
			}
		}
	}

	for idx < len(data) {
		if idx+5 > len(data) {
			break // Incomplete header
		}

		msgType := data[idx]
		length := int(binary.BigEndian.Uint32(data[idx+1 : idx+5]))

		if idx+1+length > len(data) || length < 4 {
			break // Incomplete packet or corrupt length
		}

		payload := data[idx+5 : idx+1+length]

		switch msgType {
		case 'Q': // Simple Query
			query := string(bytes.TrimRight(payload, "\x00"))
			if query != "" {
				queries = append(queries, query)
			}

		case 'P': // Parse (Prepared statement definition)
			// Payload: statement name (string), query string (string), param types count (int16)...
			parts := bytes.Split(payload, []byte{0})
			if len(parts) > 1 {
				stmtName := string(parts[0])
				query := string(parts[1])
				if query != "" {
					p.stmtMap[stmtName] = query
				}
			}

		case 'B': // Bind (Parameter binding)
			// Payload:
			// - portal name (string)
			// - statement name (string)
			// - format codes count (int16)
			// - format codes (int16 array)
			// - parameter values count (int16)
			// - parameter values: length (int32) + value
			parts := bytes.Split(payload, []byte{0})
			if len(parts) < 2 {
				break
			}
			portalName := string(parts[0])
			stmtName := string(parts[1])

			// Skip names
			offset := len(portalName) + 1 + len(stmtName) + 1
			if offset+2 > len(payload) {
				break
			}

			// Read format codes
			numFormats := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
			offset += 2

			var formats []uint16
			if offset+numFormats*2 <= len(payload) {
				for i := 0; i < numFormats; i++ {
					formats = append(formats, binary.BigEndian.Uint16(payload[offset:offset+2]))
					offset += 2
				}
			}

			if offset+2 > len(payload) {
				break
			}

			// Read parameter count
			numParams := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
			offset += 2

			var params []string
			valid := true
			for i := 0; i < numParams; i++ {
				if offset+4 > len(payload) {
					valid = false
					break
				}
				valLen := int(int32(binary.BigEndian.Uint32(payload[offset : offset+4])))
				offset += 4

				if valLen == -1 {
					params = append(params, "NULL")
					continue
				}

				if offset+valLen > len(payload) {
					valid = false
					break
				}

				valBytes := payload[offset : offset+valLen]
				offset += valLen

				// Determine text vs binary encoding format: 0 = text, 1 = binary
				isBinary := false
				if len(formats) == 1 {
					isBinary = formats[0] == 1
				} else if len(formats) > i {
					isBinary = formats[i] == 1
				}

				params = append(params, decodePostgresParam(valBytes, isBinary))
			}

			if valid {
				// Resolve target SQL query from stmtMap cache
				sql, exists := p.stmtMap[stmtName]
				if !exists {
					sql, exists = p.stmtMap[""] // Try unnamed statement fallback
				}

				if exists {
					inlinedSQL := inlinePostgresParams(sql, params)
					queries = append(queries, inlinedSQL)
				}
			}
		}

		idx += 1 + length
	}

	return queries
}

func decodePostgresParam(val []byte, isBinary bool) string {
	if !isBinary {
		escaped := strings.ReplaceAll(string(val), "'", "''")
		return "'" + escaped + "'"
	}
	// Fallback decoding for binary parameter types
	switch len(val) {
	case 1:
		return fmt.Sprintf("%d", val[0])
	case 2:
		return fmt.Sprintf("%d", int16(binary.BigEndian.Uint16(val)))
	case 4:
		return fmt.Sprintf("%d", int32(binary.BigEndian.Uint32(val)))
	case 8:
		return fmt.Sprintf("%d", int64(binary.BigEndian.Uint64(val)))
	default:
		return fmt.Sprintf("0x%x", val)
	}
}

func inlinePostgresParams(query string, params []string) string {
	// Replace placeholders in reverse order (e.g. $10, $9... down to $1) to avoid prefix mismatch errors
	for i := len(params) - 1; i >= 0; i-- {
		placeholder := fmt.Sprintf("$%d", i+1)
		query = strings.ReplaceAll(query, placeholder, params[i])
	}
	return query
}
