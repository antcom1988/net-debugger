package services

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// parseResponse parses a database response from the buffer.
// It returns the formatted human-readable response, the number of bytes consumed,
// and a boolean indicating if a complete response was successfully parsed.
func parseResponse(dbType string, data []byte) (string, int, bool) {
	if len(data) == 0 {
		return "", 0, false
	}
	switch dbType {
	case "redis":
		return parseRedisResponse(data)
	case "postgres":
		return parsePostgresResponse(data)
	case "mariadb":
		return parseMySQLResponse(data)
	case "iso8583":
		return parseISO8583Response(data)
	default:
		return "", 0, false
	}
}

// parseISO8583Response recognizes one complete 2-byte big-endian length-framed
// ISO 8583 response. The payload is intentionally not logged: ISO response
// fields can contain authorization, account, or private data.
func parseISO8583Response(data []byte) (string, int, bool) {
	if len(data) < 2 {
		return "", 0, false
	}
	frameLength := int(binary.BigEndian.Uint16(data[:2]))
	if frameLength == 0 || len(data) < 2+frameLength {
		return "", 0, false
	}
	return "ISO8583 response received", 2 + frameLength, true
}

// parseRedisResponse parses one complete RESP value.
func parseRedisResponse(data []byte) (string, int, bool) {
	if len(data) == 0 {
		return "", 0, false
	}
	switch data[0] {
	case '+':
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd == -1 {
			return "", 0, false
		}
		return string(data[1:lineEnd]), lineEnd + 2, true
	case '-':
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd == -1 {
			return "", 0, false
		}
		return "Error: " + string(data[1:lineEnd]), lineEnd + 2, true
	case ':':
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd == -1 {
			return "", 0, false
		}
		return string(data[1:lineEnd]), lineEnd + 2, true
	case '$':
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd == -1 {
			return "", 0, false
		}
		lengthStr := string(data[1:lineEnd])
		length, err := strconv.Atoi(lengthStr)
		if err != nil {
			return "", 0, false
		}
		if length == -1 {
			return "nil", lineEnd + 2, true
		}
		valStart := lineEnd + 2
		if valStart+length+2 > len(data) {
			return "", 0, false
		}
		return "\"" + string(data[valStart:valStart+length]) + "\"", valStart + length + 2, true
	case '*':
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd == -1 {
			return "", 0, false
		}
		countStr := string(data[1:lineEnd])
		count, err := strconv.Atoi(countStr)
		if err != nil {
			return "", 0, false
		}
		if count == -1 {
			return "nil", lineEnd + 2, true
		}
		idx := lineEnd + 2
		var items []string
		for i := 0; i < count; i++ {
			if idx >= len(data) {
				return "", 0, false
			}
			itemStr, consumed, ok := parseRedisResponse(data[idx:])
			if !ok {
				return "", 0, false
			}
			items = append(items, itemStr)
			idx += consumed
		}
		return "[" + strings.Join(items, ", ") + "]", idx, true
	case '%':
		lineEnd := bytes.Index(data, []byte("\r\n"))
		if lineEnd == -1 {
			return "", 0, false
		}
		count, err := strconv.Atoi(string(data[1:lineEnd]))
		if err != nil || count < 0 {
			return "", 0, false
		}
		idx := lineEnd + 2
		items := make([]string, 0, count)
		for i := 0; i < count; i++ {
			key, consumed, ok := parseRedisResponse(data[idx:])
			if !ok {
				return "", 0, false
			}
			idx += consumed
			value, consumed, ok := parseRedisResponse(data[idx:])
			if !ok {
				return "", 0, false
			}
			idx += consumed
			items = append(items, key+": "+value)
		}
		return "{" + strings.Join(items, ", ") + "}", idx, true
	default:
		// Not a standard RESP start, skip 1 byte
		return "", 1, false
	}
}

// parsePostgresResponse parses PostgreSQL backend packets up to a 'ReadyForQuery' ('Z') message.
func parsePostgresResponse(data []byte) (string, int, bool) {
	// Handle PostgreSQL SSL negotiation response: a single byte 'S' or 'N'
	if len(data) > 0 && (data[0] == 'S' || data[0] == 'N') {
		if len(data) == 1 || data[1] != 0 {
			return "SSL Response: " + string(data[0]), 1, true
		}
	}

	idx := 0
	var columns []string
	var rows [][]string
	var commandTag string
	var errMsg string
	hasReadyForQuery := false

	for idx < len(data) {
		if idx+5 > len(data) {
			return "", 0, false // Incomplete header
		}
		msgType := data[idx]
		length := int(binary.BigEndian.Uint32(data[idx+1 : idx+5]))
		if idx+1+length > len(data) {
			return "", 0, false // Incomplete packet
		}
		payload := data[idx+5 : idx+1+length]

		switch msgType {
		case 'T': // RowDescription
			if len(payload) >= 2 {
				numCols := int(binary.BigEndian.Uint16(payload[0:2]))
				offset := 2
				for i := 0; i < numCols; i++ {
					zeroIdx := bytes.IndexByte(payload[offset:], 0)
					if zeroIdx == -1 {
						break
					}
					colName := string(payload[offset : offset+zeroIdx])
					columns = append(columns, colName)
					offset += zeroIdx + 1 + 18 // skip OID, attribute, type, size, modifier, format code
				}
			}
		case 'D': // DataRow
			if len(payload) >= 2 {
				numCols := int(binary.BigEndian.Uint16(payload[0:2]))
				offset := 2
				var row []string
				for i := 0; i < numCols; i++ {
					if offset+4 > len(payload) {
						break
					}
					valLen := int(int32(binary.BigEndian.Uint32(payload[offset : offset+4])))
					offset += 4
					if valLen == -1 {
						row = append(row, "NULL")
					} else {
						if offset+valLen > len(payload) {
							break
						}
						val := string(payload[offset : offset+valLen])
						row = append(row, val)
						offset += valLen
					}
				}
				rows = append(rows, row)
			}
		case 'C': // CommandComplete
			commandTag = string(bytes.TrimRight(payload, "\x00"))
		case 'E': // ErrorResponse
			offset := 0
			for offset < len(payload) {
				code := payload[offset]
				if code == 0 {
					break
				}
				offset++
				zeroIdx := bytes.IndexByte(payload[offset:], 0)
				if zeroIdx == -1 {
					break
				}
				val := string(payload[offset : offset+zeroIdx])
				if code == 'M' { // Message field
					errMsg = val
				}
				offset += zeroIdx + 1
			}
		case 'Z': // ReadyForQuery
			hasReadyForQuery = true
			idx += 1 + length
			var result string
			if errMsg != "" {
				result = "Error: " + errMsg
			} else if len(columns) > 0 {
				result = formatTable(columns, rows)
			} else if commandTag != "" {
				result = commandTag
			} else {
				result = "OK"
			}
			return result, idx, true
		}

		idx += 1 + length
	}
	_ = hasReadyForQuery
	return "", 0, false
}

// parseMySQLResponse parses MySQL server packets.
func parseMySQLResponse(data []byte) (string, int, bool) {
	idx := 0
	packetCount := 0

	var columns []string
	var rows [][]string
	var colCount int
	hasColCount := false
	colDefsRead := 0
	firstEOFSeen := false

	for idx < len(data) {
		if idx+4 > len(data) {
			return "", 0, false // Incomplete packet header
		}
		length := int(data[idx]) | int(data[idx+1])<<8 | int(data[idx+2])<<16
		if idx+4+length > len(data) {
			return "", 0, false // Incomplete packet payload
		}
		payload := data[idx+4 : idx+4+length]

		if packetCount == 0 && len(payload) > 0 {
			firstByte := payload[0]
			if firstByte == 0x00 { // OK Packet
				offset := 1
				affectedRows, _ := readLengthEncodedInt(payload, &offset)
				resMsg := fmt.Sprintf("Query OK, %d rows affected", affectedRows)
				return resMsg, idx + 4 + length, true
			} else if firstByte == 0xff { // ERR Packet
				var errStr string
				if len(payload) > 9 {
					errStr = string(payload[9:])
				} else {
					errStr = "Unknown error"
				}
				return "Error: " + errStr, idx + 4 + length, true
			} else {
				// Resultset: column count
				offset := 0
				count, ok := readLengthEncodedInt(payload, &offset)
				if !ok {
					return "", 0, false
				}
				colCount = int(count)
				hasColCount = true
			}
		} else if hasColCount {
			if !firstEOFSeen {
				if len(payload) > 0 && payload[0] == 0xfe && length < 9 {
					firstEOFSeen = true
				} else {
					// Column Definition
					offset := 0
					_ = skipLengthEncodedString(payload, &offset) // catalog
					_ = skipLengthEncodedString(payload, &offset) // db
					_ = skipLengthEncodedString(payload, &offset) // table
					_ = skipLengthEncodedString(payload, &offset) // org_table
					colName, ok := readLengthEncodedString(payload, &offset)
					if ok {
						columns = append(columns, colName)
					}
					colDefsRead++
				}
			} else {
				if len(payload) > 0 && (payload[0] == 0xfe || payload[0] == 0x00) && length < 9 {
					// Final EOF / OK packet
					idx += 4 + length
					var result string
					if len(columns) > 0 {
						result = formatTable(columns, rows)
					} else {
						result = fmt.Sprintf("OK (%d rows)", len(rows))
					}
					return result, idx, true
				} else {
					// Row packet
					var row []string
					offset := 0
					valid := true
					for i := 0; i < colCount; i++ {
						if offset >= len(payload) {
							valid = false
							break
						}
						if payload[offset] == 0xfb {
							row = append(row, "NULL")
							offset++
							continue
						}
						strVal, ok := readLengthEncodedString(payload, &offset)
						if !ok {
							valid = false
							break
						}
						row = append(row, strVal)
					}
					if valid {
						rows = append(rows, row)
					}
				}
			}
		}

		idx += 4 + length
		packetCount++
	}

	return "", 0, false
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

func readLengthEncodedString(data []byte, offset *int) (string, bool) {
	strLen, ok := readLengthEncodedInt(data, offset)
	if !ok {
		return "", false
	}
	if *offset+int(strLen) > len(data) {
		return "", false
	}
	val := string(data[*offset : *offset+int(strLen)])
	*offset += int(strLen)
	return val, true
}

func skipLengthEncodedString(data []byte, offset *int) bool {
	strLen, ok := readLengthEncodedInt(data, offset)
	if !ok {
		return false
	}
	if *offset+int(strLen) > len(data) {
		return false
	}
	*offset += int(strLen)
	return true
}

// formatTable builds a neat text representation of columns and rows.
func formatTable(columns []string, rows [][]string) string {
	maxRows := 10
	printedRows := len(rows)
	if printedRows > maxRows {
		printedRows = maxRows
	}

	widths := make([]int, len(columns))
	for i, col := range columns {
		widths[i] = len(col)
	}
	for _, row := range rows[:printedRows] {
		for i, val := range row {
			if i < len(widths) && len(val) > widths[i] {
				widths[i] = len(val)
			}
		}
	}

	var sb strings.Builder
	for i, col := range columns {
		sb.WriteString(padRight(col, widths[i]))
		if i < len(columns)-1 {
			sb.WriteString(" | ")
		}
	}
	sb.WriteString("\n")

	for i, w := range widths {
		sb.WriteString(strings.Repeat("-", w))
		if i < len(widths)-1 {
			sb.WriteString("-+-")
		}
	}
	sb.WriteString("\n")

	for _, row := range rows[:printedRows] {
		for i, val := range row {
			if i < len(widths) {
				sb.WriteString(padRight(val, widths[i]))
				if i < len(widths)-1 {
					sb.WriteString(" | ")
				}
			}
		}
		sb.WriteString("\n")
	}

	if len(rows) > maxRows {
		sb.WriteString(fmt.Sprintf("(... and %d more rows)\n", len(rows)-maxRows))
	}
	sb.WriteString(fmt.Sprintf("(%d rows)", len(rows)))
	return sb.String()
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}
