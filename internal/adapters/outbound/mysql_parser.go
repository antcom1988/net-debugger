package outbound

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

type mysqlStmt struct {
	query     string
	numParams int
	types     []uint16 // Cache parameter types across executions
}

// MySQLParser handles client packets and intercepts server prepared statement IDs.
type MySQLParser struct {
	stmtMap        map[uint32]mysqlStmt
	lastPrepareSQL string
}

// NewMySQLParser instantiates a new MySQLParser.
func NewMySQLParser() *MySQLParser {
	return &MySQLParser{
		stmtMap: make(map[uint32]mysqlStmt),
	}
}

// BuildErrorResponse constructs a MySQL protocol-compliant ERR packet.
func (p *MySQLParser) BuildErrorResponse(errMsg string) []byte {
	payload := make([]byte, 1+2+1+5+len(errMsg))
	payload[0] = 0xff // ERR header
	// Error code: 1105 (Unknown error or custom)
	binary.LittleEndian.PutUint16(payload[1:3], 1105)
	payload[3] = '#'
	copy(payload[4:9], "HY000") // SQLState
	copy(payload[9:], errMsg)

	// MySQL packet header: 3 bytes length + 1 byte sequence ID
	length := len(payload)
	res := make([]byte, 4+length)
	res[0] = byte(length & 0xff)
	res[1] = byte((length >> 8) & 0xff)
	res[2] = byte((length >> 16) & 0xff)
	res[3] = 1 // sequence ID (usually 1 for response packet in protocol sequence)
	copy(res[4:], payload)

	return res
}

// ParseServerResponse intercepts server-to-client packets.
// It watches for COM_STMT_PREPARE_OK (0x00 status in sequence 1) to match query texts with statement IDs.
func (p *MySQLParser) ParseServerResponse(data []byte) {
	idx := 0
	for idx < len(data) {
		if idx+4 > len(data) {
			break
		}

		length := int(data[idx]) | int(data[idx+1])<<8 | int(data[idx+2])<<16
		seq := data[idx+3]

		if idx+4+length > len(data) {
			break
		}

		payload := data[idx+4 : idx+4+length]

		// COM_STMT_PREPARE_OK is sent by the server with sequence ID 1.
		// Payload: [1 byte status (0x00)] [4 bytes stmt_id] [2 bytes num_columns] [2 bytes num_params] ...
		if seq == 1 && len(payload) >= 12 && payload[0] == 0x00 && p.lastPrepareSQL != "" {
			stmtID := binary.LittleEndian.Uint32(payload[1:5])
			numParams := binary.LittleEndian.Uint16(payload[7:9])

			p.stmtMap[stmtID] = mysqlStmt{
				query:     p.lastPrepareSQL,
				numParams: int(numParams),
			}
			p.lastPrepareSQL = "" // Reset
		}

		idx += 4 + length
	}
}

// Parse intercepts client-to-server database packets.
// It extracts SQL strings from COM_QUERY and COM_STMT_PREPARE, and decodes actual values from COM_STMT_EXECUTE.
func (p *MySQLParser) Parse(data []byte) []string {
	var queries []string
	idx := 0

	for idx < len(data) {
		if idx+4 > len(data) {
			break
		}

		length := int(data[idx]) | int(data[idx+1])<<8 | int(data[idx+2])<<16
		seq := data[idx+3]

		if idx+4+length > len(data) {
			break
		}

		if length > 0 {
			payload := data[idx+4 : idx+4+length]

			// Client commands start a session command phase with sequence ID 0.
			if seq == 0 && len(payload) > 0 {
				cmd := payload[0]
				switch cmd {
				case 0x03: // COM_QUERY
					query := string(payload[1:])
					if query != "" {
						queries = append(queries, query)
					}

				case 0x16: // COM_STMT_PREPARE
					query := string(payload[1:])
					if query != "" {
						p.lastPrepareSQL = query
					}

				case 0x17: // COM_STMT_EXECUTE
					// Payload layout:
					// [1 byte cmd (0x17)] [4 bytes stmt_id] [1 byte flags] [4 bytes iteration_count (1)]
					if len(payload) >= 10 {
						stmtID := binary.LittleEndian.Uint32(payload[1:5])
						stmt, exists := p.stmtMap[stmtID]
						if exists {
							var params []string
							offset := 10

							if stmt.numParams > 0 {
								// Read Null bitmap: size is (numParams + 7) / 8
								nullBitmapSize := (stmt.numParams + 7) / 8
								if offset+nullBitmapSize > len(payload) {
									break
								}
								nullBitmap := payload[offset : offset+nullBitmapSize]
								offset += nullBitmapSize

								if offset+1 > len(payload) {
									break
								}
								newParamsBound := payload[offset]
								offset++

								// If new parameters are bound, read the new parameter types
								if newParamsBound == 0x01 {
									if offset+stmt.numParams*2 > len(payload) {
										break
									}
									stmt.types = make([]uint16, stmt.numParams)
									for i := 0; i < stmt.numParams; i++ {
										stmt.types[i] = binary.LittleEndian.Uint16(payload[offset : offset+2])
										offset += 2
									}
									p.stmtMap[stmtID] = stmt // Cache types back
								}

								// Decode parameter values
								valid := true
								for i := 0; i < stmt.numParams; i++ {
									// Check null bitmap
									byteIdx := i / 8
									bitIdx := i % 8
									isNull := (nullBitmap[byteIdx] & (1 << bitIdx)) != 0

									if isNull {
										params = append(params, "NULL")
										continue
									}

									typeCode := uint8(0xfc) // Fallback to string type code
									if len(stmt.types) > i {
										typeCode = uint8(stmt.types[i] & 0xff)
									}

									val, ok := decodeMySQLParam(payload, &offset, typeCode)
									if !ok {
										valid = false
										break
									}
									params = append(params, val)
								}

								if valid {
									inlinedSQL := inlineMySQLParams(stmt.query, params)
									queries = append(queries, inlinedSQL)
								}
							} else {
								queries = append(queries, stmt.query)
							}
						}
					}
				}
			}
		}

		idx += 4 + length
	}

	return queries
}

func readLengthEncodedInt(data []byte, offset *int) (uint64, bool) {
	if *offset >= len(data) {
		return 0, false
	}
	b := data[*offset]
	*offset++

	if b < 251 {
		return uint64(b), true
	} else if b == 252 {
		if *offset+2 > len(data) {
			return 0, false
		}
		v := binary.LittleEndian.Uint16(data[*offset : *offset+2])
		*offset += 2
		return uint64(v), true
	} else if b == 253 {
		if *offset+3 > len(data) {
			return 0, false
		}
		v := uint64(data[*offset]) | uint64(data[*offset+1])<<8 | uint64(data[*offset+2])<<16
		*offset += 3
		return v, true
	} else if b == 254 {
		if *offset+8 > len(data) {
			return 0, false
		}
		v := binary.LittleEndian.Uint64(data[*offset : *offset+8])
		*offset += 8
		return v, true
	}
	return 0, false
}

func decodeMySQLParam(payload []byte, offset *int, typeCode uint8) (string, bool) {
	switch typeCode {
	case 0x01: // TINYINT (1 byte)
		if *offset+1 > len(payload) {
			return "", false
		}
		v := payload[*offset]
		*offset++
		return fmt.Sprintf("%d", v), true
	case 0x02: // SHORT / INT16 (2 bytes)
		if *offset+2 > len(payload) {
			return "", false
		}
		v := binary.LittleEndian.Uint16(payload[*offset : *offset+2])
		*offset += 2
		return fmt.Sprintf("%d", int16(v)), true
	case 0x03: // LONG / INT32 (4 bytes)
		if *offset+4 > len(payload) {
			return "", false
		}
		v := binary.LittleEndian.Uint32(payload[*offset : *offset+4])
		*offset += 4
		return fmt.Sprintf("%d", int32(v)), true
	case 0x08: // LONGLONG / INT64 (8 bytes)
		if *offset+8 > len(payload) {
			return "", false
		}
		v := binary.LittleEndian.Uint64(payload[*offset : *offset+8])
		*offset += 8
		return fmt.Sprintf("%d", int64(v)), true
	case 0x04: // FLOAT (4 bytes)
		if *offset+4 > len(payload) {
			return "", false
		}
		v := binary.LittleEndian.Uint32(payload[*offset : *offset+4])
		*offset += 4
		return fmt.Sprintf("%g", math.Float32frombits(v)), true
	case 0x05: // DOUBLE (8 bytes)
		if *offset+8 > len(payload) {
			return "", false
		}
		v := binary.LittleEndian.Uint64(payload[*offset : *offset+8])
		*offset += 8
		return fmt.Sprintf("%g", math.Float64frombits(v)), true
	case 0xfc, 0xfd, 0xfe, 0x0f, 0x00: // Strings / Blobs / Varchar
		strLen, ok := readLengthEncodedInt(payload, offset)
		if !ok {
			return "", false
		}
		if *offset+int(strLen) > len(payload) {
			return "", false
		}
		strVal := string(payload[*offset : *offset+int(strLen)])
		*offset += int(strLen)
		escaped := strings.ReplaceAll(strVal, "'", "''")
		return "'" + escaped + "'", true
	default:
		// Default fallback for time, date types (starts with a length byte)
		length, ok := readLengthEncodedInt(payload, offset)
		if !ok {
			return "", false
		}
		if *offset+int(length) > len(payload) {
			return "", false
		}
		valBytes := payload[*offset : *offset+int(length)]
		*offset += int(length)
		return fmt.Sprintf("0x%x", valBytes), true
	}
}

func inlineMySQLParams(query string, params []string) string {
	for _, param := range params {
		query = strings.Replace(query, "?", param, 1)
	}
	return query
}
