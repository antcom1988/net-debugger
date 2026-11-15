package outbound

import (
	"bytes"
	"strconv"
	"strings"
)

// ParseRedisCommands parses standard RESP commands from a byte buffer.
// It searches for RESP arrays representing client command invocations.
func ParseRedisCommands(data []byte) []string {
	var commands []string
	idx := 0

	for idx < len(data) {
		// Redis client commands are always RESP Arrays (starting with '*')
		if data[idx] != '*' {
			idx++
			continue
		}

		// Read array length
		lineEnd := bytes.Index(data[idx:], []byte("\r\n"))
		if lineEnd == -1 {
			break // Incomplete packet
		}

		arrLenStr := string(data[idx+1 : idx+lineEnd])
		arrLen, err := strconv.Atoi(arrLenStr)
		if err != nil || arrLen <= 0 {
			idx += lineEnd + 2
			continue
		}

		currIdx := idx + lineEnd + 2
		var args []string
		valid := true

		for i := 0; i < arrLen; i++ {
			if currIdx >= len(data) {
				valid = false
				break
			}
			if data[currIdx] != '$' {
				valid = false
				break
			}

			argLineEnd := bytes.Index(data[currIdx:], []byte("\r\n"))
			if argLineEnd == -1 {
				valid = false
				break
			}

			argLenStr := string(data[currIdx+1 : currIdx+argLineEnd])
			argLen, err := strconv.Atoi(argLenStr)
			if err != nil || argLen < 0 {
				valid = false
				break
			}

			valStart := currIdx + argLineEnd + 2
			if valStart+argLen+2 > len(data) {
				valid = false
				break
			}

			// Read bulk string value
			argVal := string(data[valStart : valStart+argLen])
			args = append(args, argVal)
			currIdx = valStart + argLen + 2
		}

		if valid && len(args) > 0 {
			commands = append(commands, strings.Join(args, " "))
			idx = currIdx
		} else {
			// If parsing fails for this array, move forward past the current line
			idx += lineEnd + 2
		}
	}

	return commands
}

// RedisParser implements the ports.ProtocolParser interface. A parser belongs to
// exactly one client connection, so pending RESP bytes are safe to retain here.
type RedisParser struct {
	pending []byte
}

// NewRedisParser creates a new RedisParser.
func NewRedisParser() *RedisParser {
	return &RedisParser{}
}

// Parse extracts complete commands from a RESP byte stream. TCP reads are not
// message boundaries, so incomplete frames remain pending until a later read.
func (p *RedisParser) Parse(data []byte) []string {
	p.pending = append(p.pending, data...)

	var commands []string
	for len(p.pending) > 0 {
		if p.pending[0] != '*' {
			next := bytes.IndexByte(p.pending, '*')
			if next <= 0 {
				p.pending = nil
				break
			}
			p.pending = p.pending[next:]
		}

		command, consumed, complete := parseRedisCommand(p.pending)
		if !complete {
			break
		}
		p.pending = p.pending[consumed:]
		if command != "" {
			commands = append(commands, command)
		}
	}
	return commands
}

func parseRedisCommand(data []byte) (command string, consumed int, complete bool) {
	if len(data) == 0 || data[0] != '*' {
		return "", 0, false
	}

	lineEnd := bytes.Index(data, []byte("\r\n"))
	if lineEnd == -1 {
		return "", 0, false
	}
	arrLen, err := strconv.Atoi(string(data[1:lineEnd]))
	if err != nil || arrLen <= 0 {
		return "", lineEnd + 2, true
	}

	idx := lineEnd + 2
	args := make([]string, 0, arrLen)
	for i := 0; i < arrLen; i++ {
		if idx >= len(data) || data[idx] != '$' {
			return "", 0, false
		}
		argLineEndOffset := bytes.Index(data[idx:], []byte("\r\n"))
		if argLineEndOffset == -1 {
			return "", 0, false
		}
		argLineEnd := idx + argLineEndOffset
		argLen, err := strconv.Atoi(string(data[idx+1 : argLineEnd]))
		if err != nil || argLen < 0 {
			return "", argLineEnd + 2, true
		}
		valueStart := argLineEnd + 2
		frameEnd := valueStart + argLen + 2
		if frameEnd > len(data) {
			return "", 0, false
		}
		if !bytes.Equal(data[valueStart+argLen:frameEnd], []byte("\r\n")) {
			return "", frameEnd, true
		}
		args = append(args, string(data[valueStart:valueStart+argLen]))
		idx = frameEnd
	}
	return strings.Join(args, " "), idx, true
}

// ParseServerResponse is a no-op for Redis since it operates state-freely.
func (p *RedisParser) ParseServerResponse(data []byte) {}

// BuildErrorResponse constructs a Redis RESP protocol-compliant error string.
func (p *RedisParser) BuildErrorResponse(errMsg string) []byte {
	return []byte("-ERR " + errMsg + "\r\n")
}
