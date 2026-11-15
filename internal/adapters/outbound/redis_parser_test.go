package outbound

import (
	"reflect"
	"testing"
)

func TestRedisParserParsesCommandYangTerfragmentasiAntarRead(t *testing.T) {
	parser := NewRedisParser()

	if commands := parser.Parse([]byte("*3\r\n$3\r\nSET\r\n$2\r\nk1\r\n")); len(commands) != 0 {
		t.Fatalf("first fragment commands = %v, want no completed command", commands)
	}

	commands := parser.Parse([]byte("$2\r\nv1\r\n"))
	if expected := []string{"SET k1 v1"}; !reflect.DeepEqual(commands, expected) {
		t.Fatalf("second fragment commands = %v, want %v", commands, expected)
	}
}

func TestParseRedisCommands(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected []string
	}{
		{
			name:     "single SET command",
			input:    []byte("*3\r\n$3\r\nSET\r\n$5\r\nmykey\r\n$8\r\nmyval123\r\n"),
			expected: []string{"SET mykey myval123"},
		},
		{
			name:     "pipelined commands",
			input:    []byte("*3\r\n$3\r\nSET\r\n$2\r\nk1\r\n$2\r\nv1\r\n*2\r\n$3\r\nGET\r\n$2\r\nk1\r\n"),
			expected: []string{"SET k1 v1", "GET k1"},
		},
		{
			name:     "incomplete array",
			input:    []byte("*3\r\n$3\r\nSET\r\n$2\r\nk1\r\n"),
			expected: nil,
		},
		{
			name:     "non-array data ignored",
			input:    []byte("+OK\r\n:123\r\n"),
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseRedisCommands(tt.input)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}
