package outbound

import (
	"reflect"
	"testing"
)

func TestParseMySQLQueries(t *testing.T) {
	// Build COM_QUERY packet: length (3 bytes) + seq (1 byte) + cmd (0x03) + SQL
	queryStr := "SELECT * FROM test_table"
	qLength := 1 + len(queryStr)
	qPacket := make([]byte, 4+qLength)
	qPacket[0] = byte(qLength & 0xFF)
	qPacket[1] = byte((qLength >> 8) & 0xFF)
	qPacket[2] = byte((qLength >> 16) & 0xFF)
	qPacket[3] = 0    // SeqID = 0 (Client command start)
	qPacket[4] = 0x03 // COM_QUERY
	copy(qPacket[5:], queryStr)

	// Build COM_STMT_PREPARE packet: length (3 bytes) + seq (1 byte) + cmd (0x16) + SQL
	prepStr := "INSERT INTO test_table(name) VALUES(?)"
	pLength := 1 + len(prepStr)
	pPacket := make([]byte, 4+pLength)
	pPacket[0] = byte(pLength & 0xFF)
	pPacket[1] = byte((pLength >> 8) & 0xFF)
	pPacket[2] = byte((pLength >> 16) & 0xFF)
	pPacket[3] = 0    // SeqID = 0
	pPacket[4] = 0x16 // COM_STMT_PREPARE
	copy(pPacket[5:], prepStr)

	// Build a packet with sequence ID 1 (e.g. login/handshake payload) - should be ignored
	hPacket := make([]byte, 8)
	hPacket[0] = 4
	hPacket[1] = 0
	hPacket[2] = 0
	hPacket[3] = 1    // SeqID = 1
	hPacket[4] = 0x85 // Dummy command byte

	tests := []struct {
		name     string
		input    []byte
		expected []string
	}{
		{
			name:     "COM_QUERY command",
			input:    qPacket,
			expected: []string{"SELECT * FROM test_table"},
		},
		{
			name:     "COM_STMT_PREPARE command",
			input:    pPacket,
			expected: nil,
		},
		{
			name:     "handshake packet ignored",
			input:    hPacket,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewMySQLParser()
			result := parser.Parse(tt.input)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestMySQLParserStateful(t *testing.T) {
	parser := NewMySQLParser()

	// 1. Client prepares query: "SELECT * FROM users WHERE name = ? AND age = ?"
	queryStr := "SELECT * FROM users WHERE name = ? AND age = ?"
	prepLength := 1 + len(queryStr)
	prepPacket := make([]byte, 4+prepLength)
	prepPacket[0] = byte(prepLength & 0xFF)
	prepPacket[1] = byte((prepLength >> 8) & 0xFF)
	prepPacket[2] = byte((prepLength >> 16) & 0xFF)
	prepPacket[3] = 0    // SeqID = 0
	prepPacket[4] = 0x16 // COM_STMT_PREPARE
	copy(prepPacket[5:], queryStr)

	res := parser.Parse(prepPacket)
	if len(res) != 0 {
		t.Fatalf("expected no prepare log, got %v", res)
	}

	// 2. Server replies with COM_STMT_PREPARE_OK: ID=1, numColumns=1, numParams=2
	// Payload: [0x00 status] [0x01,0x00,0x00,0x00 ID] [0x01,0x00 cols] [0x02,0x00 params] [0x00 filler] [0x00,0x00 warning]
	okPayload := []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00}
	okPacket := make([]byte, 4+len(okPayload))
	okPacket[0] = byte(len(okPayload) & 0xFF)
	okPacket[1] = byte((len(okPayload) >> 8) & 0xFF)
	okPacket[2] = byte((len(okPayload) >> 16) & 0xFF)
	okPacket[3] = 1 // SeqID = 1
	copy(okPacket[4:], okPayload)

	parser.ParseServerResponse(okPacket)

	// 3. Client executes statement: stmtID=1, flags=0, iteration=1, nullBitmap=0x00, newParams=0x01
	// types: [0xfc, 0x00] (varchar), [0x03, 0x00] (long / int32)
	// values: [0x04, 'J', 'o', 'h', 'n'] ("John"), [25, 0, 0, 0] (25)
	execPayload := []byte{
		0x17,                   // COM_STMT_EXECUTE
		0x01, 0x00, 0x00, 0x00, // statement ID = 1
		0x00,                   // flags = 0
		0x01, 0x00, 0x00, 0x00, // iteration count = 1
		0x00,                   // null bitmap (no parameters are null)
		0x01,                   // new parameters bound flag = 1
		0xfc, 0x00,             // Type 1: BLOB/string (0xfc)
		0x03, 0x00,             // Type 2: LONG/int32 (0x03)
		0x04, 'J', 'o', 'h', 'n', // Value 1: length 4 + "John"
		25, 0, 0, 0, // Value 2: int32 25
	}
	execPacket := make([]byte, 4+len(execPayload))
	execPacket[0] = byte(len(execPayload) & 0xFF)
	execPacket[1] = byte((len(execPayload) >> 8) & 0xFF)
	execPacket[2] = byte((len(execPayload) >> 16) & 0xFF)
	execPacket[3] = 0 // SeqID = 0
	copy(execPacket[4:], execPayload)

	execRes := parser.Parse(execPacket)
	expectedLog := `SELECT * FROM users WHERE name = 'John' AND age = 25`
	if len(execRes) != 1 || execRes[0] != expectedLog {
		t.Fatalf("expected execute log with parameters, got %v", execRes)
	}
}
