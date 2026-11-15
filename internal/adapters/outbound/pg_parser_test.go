package outbound

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestParsePostgresQueries(t *testing.T) {
	// Build a simple query ('Q') packet: type + length (4 bytes) + query string (null-terminated)
	queryStr := "SELECT * FROM test_table;"
	payload := append([]byte(queryStr), 0)
	length := 4 + len(payload)
	qMsg := make([]byte, 1+4+len(payload))
	qMsg[0] = 'Q'
	binary.BigEndian.PutUint32(qMsg[1:5], uint32(length))
	copy(qMsg[5:], payload)

	// Build a parse ('P') packet: type + length (4 bytes) + statement name + query string + parameter count
	stmtName := "my_stmt"
	parseQuery := "INSERT INTO test_table(name) VALUES($1);"
	pPayload := append([]byte(stmtName), 0)
	pPayload = append(pPayload, append([]byte(parseQuery), 0)...)
	pPayload = append(pPayload, 0, 0) // Param count = 0
	pLength := 4 + len(pPayload)
	pMsg := make([]byte, 1+4+len(pPayload))
	pMsg[0] = 'P'
	binary.BigEndian.PutUint32(pMsg[1:5], uint32(pLength))
	copy(pMsg[5:], pPayload)

	// Build SSL request message
	sslMsg := make([]byte, 8)
	binary.BigEndian.PutUint32(sslMsg[0:4], 8)
	binary.BigEndian.PutUint32(sslMsg[4:8], 80877103)

	tests := []struct {
		name     string
		input    []byte
		expected []string
	}{
		{
			name:     "simple query message",
			input:    qMsg,
			expected: []string{"SELECT * FROM test_table;"},
		},
		{
			name:     "parse message",
			input:    pMsg,
			expected: nil,
		},
		{
			name:     "ssl request ignored",
			input:    sslMsg,
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewPostgresParser()
			result := parser.Parse(tt.input)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestPostgresParserStateful(t *testing.T) {
	parser := NewPostgresParser()

	// 1. Prepare statement: 'my_stmt' -> SELECT * FROM users WHERE id=$1;
	stmtName := "my_stmt"
	query := "SELECT * FROM users WHERE id=$1;"
	pPayload := append([]byte(stmtName), 0)
	pPayload = append(pPayload, append([]byte(query), 0)...)
	pPayload = append(pPayload, 0, 0) // Param count = 0
	pLength := 4 + len(pPayload)
	pMsg := make([]byte, 1+4+len(pPayload))
	pMsg[0] = 'P'
	binary.BigEndian.PutUint32(pMsg[1:5], uint32(pLength))
	copy(pMsg[5:], pPayload)

	pRes := parser.Parse(pMsg)
	if len(pRes) != 0 {
		t.Fatalf("expected no Prepare log, got %v", pRes)
	}

	// 2. Bind parameter values: portal="", stmt="my_stmt", formats=0, params=1, len=3, val="123"
	bPayload := []byte{0} // portal name "" (null-terminated)
	bPayload = append(bPayload, []byte(stmtName)...)
	bPayload = append(bPayload, 0) // statement name "my_stmt" (null-terminated)

	// formats count = 0 (implies all text)
	bPayload = append(bPayload, 0, 0)

	// params count = 1
	bPayload = append(bPayload, 0, 1)

	// param length = 3
	bPayload = append(bPayload, 0, 0, 0, 3)

	// param value = "123"
	bPayload = append(bPayload, []byte("123")...)

	bLength := 4 + len(bPayload)
	bMsg := make([]byte, 1+4+len(bPayload))
	bMsg[0] = 'B'
	binary.BigEndian.PutUint32(bMsg[1:5], uint32(bLength))
	copy(bMsg[5:], bPayload)

	bRes := parser.Parse(bMsg)
	expectedLog := `SELECT * FROM users WHERE id='123';`
	if len(bRes) != 1 || bRes[0] != expectedLog {
		t.Fatalf("expected log with parameters, got %v", bRes)
	}
}
