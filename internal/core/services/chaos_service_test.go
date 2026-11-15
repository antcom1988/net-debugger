package services

import (
	"bytes"
	"db-proxy/internal/core/domain"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestChaosServiceEvaluation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "chaos-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	rulePath := filepath.Join(tmpDir, "chaos_rules.json")

	// Initialize ChaosService
	cs := NewChaosService(rulePath)

	rules := []domain.ChaosRule{
		{
			ID:          "1",
			Service:     "postgres",
			MatchType:   "substring",
			Pattern:     "insert into table_a",
			Action:      "error",
			ErrorMsg:    "mock table_a lock error",
			Probability: 1.0,
			Enabled:     true,
		},
		{
			ID:          "2",
			Service:     "", // Match any service
			MatchType:   "regex",
			Pattern:     `(?i)select.*from\s+users`,
			Action:      "delay",
			DelayMs:     500,
			Probability: 1.0,
			Enabled:     true,
		},
		{
			ID:          "3",
			Service:     "redis",
			MatchType:   "exact",
			Pattern:     "ping",
			Action:      "error",
			ErrorMsg:    "mock redis timeout",
			Probability: 1.0,
			Enabled:     false, // Disabled rule
		},
		{
			ID:          "4",
			Service:     "postgres",
			MatchType:   "substring",
			Pattern:     "select 1",
			Action:      "delay",
			DelayMs:     100,
			Probability: 0.0, // 0% probability
			Enabled:     true,
		},
		{
			ID:          "5",
			Service:     "mariadb",
			MatchType:   "substring",
			Pattern:     "delete from table_b",
			Action:      "close",
			Probability: 1.0,
			Enabled:     true,
		},
		{
			ID:          "6",
			Service:     "redis",
			MatchType:   "substring",
			Pattern:     "select * from cache",
			Action:      "empty",
			Probability: 1.0,
			Enabled:     true,
		},
		{
			ID:                 "7",
			Service:            "postgres",
			MatchType:          "substring",
			Pattern:            "select * from large",
			Action:             "throttle",
			ThrottleBytes:      5,
			ThrottleIntervalMs: 20,
			Probability:        1.0,
			Enabled:            true,
		},
	}

	err = cs.UpdateRules(rules)
	if err != nil {
		t.Fatalf("failed to save rules: %v", err)
	}

	// Test case 1: Substring match (Postgres, uppercase/lowercase check)
	rule, matched := cs.Evaluate("postgres", "INSERT INTO table_a (id) VALUES (1);")
	if !matched {
		t.Error("expected match for Postgres table_a insert")
	}
	if rule.Action != "error" || rule.ErrorMsg != "mock table_a lock error" {
		t.Errorf("expected error action with mock message, got %s: %s", rule.Action, rule.ErrorMsg)
	}

	// Test case 2: Service mismatch check
	_, matched = cs.Evaluate("mariadb", "INSERT INTO table_a (id) VALUES (1);")
	if matched {
		t.Error("expected no match because service is mariadb but rule requires postgres")
	}

	// Test case 3: Regex match (Any service, case-insensitive query)
	rule, matched = cs.Evaluate("mariadb", "SELECT name FROM users WHERE id = 5;")
	if !matched {
		t.Error("expected match for regex query")
	}
	if rule.Action != "delay" || rule.DelayMs != 500 {
		t.Errorf("expected delay of 500ms, got %s: %dms", rule.Action, rule.DelayMs)
	}

	// Test case 4: Disabled rule check
	_, matched = cs.Evaluate("redis", "PING")
	if matched {
		t.Error("expected no match because rule 3 is disabled")
	}

	// Test case 5: 0% Probability check
	_, matched = cs.Evaluate("postgres", "SELECT 1;")
	if matched {
		t.Error("expected no match because rule 4 probability is 0.0")
	}

	// Test case 6: Connection close check
	rule, matched = cs.Evaluate("mariadb", "DELETE FROM table_b WHERE id=2;")
	if !matched {
		t.Error("expected match for mariadb connection close")
	}
	if rule.Action != "close" {
		t.Errorf("expected action 'close', got %s", rule.Action)
	}

	// Test case 7: Empty result set check
	rule, matched = cs.Evaluate("redis", "SELECT * FROM cache;")
	if !matched {
		t.Error("expected match for redis empty result set")
	}
	if rule.Action != "empty" {
		t.Errorf("expected action 'empty', got %s", rule.Action)
	}

	// Test case 8: Throttle check
	rule, matched = cs.Evaluate("postgres", "SELECT * FROM large_table;")
	if !matched {
		t.Error("expected match for postgres throttle")
	}
	if rule.Action != "throttle" || rule.ThrottleBytes != 5 || rule.ThrottleIntervalMs != 20 {
		t.Errorf("expected action 'throttle' (5B/20ms), got %s (%dB/%dms)", rule.Action, rule.ThrottleBytes, rule.ThrottleIntervalMs)
	}
}

func TestStripPostgresRows(t *testing.T) {
	// Header packet: 'T' (RowDescription), length = 9 (4 bytes length + 5 bytes payload)
	header := make([]byte, 1+4+5)
	header[0] = 'T'
	binary.BigEndian.PutUint32(header[1:5], 9)
	copy(header[5:], []byte{0, 1, 2, 3, 4})

	// Row packet: 'D' (DataRow), length = 7 (4 bytes length + 3 bytes payload)
	row := make([]byte, 1+4+3)
	row[0] = 'D'
	binary.BigEndian.PutUint32(row[1:5], 7)
	copy(row[5:], []byte{5, 6, 7})

	// Footer packet: 'C' (CommandComplete), length = 6 (4 bytes length + 2 bytes payload)
	footer := make([]byte, 1+4+2)
	footer[0] = 'C'
	binary.BigEndian.PutUint32(footer[1:5], 6)
	copy(footer[5:], []byte{8, 9})

	input := append(append(header, row...), footer...)
	output := stripPostgresRows(input)

	// Output should contain header and footer, but NOT row
	expected := append(header, footer...)
	if !bytes.Equal(output, expected) {
		t.Errorf("Postgres row stripping failed. Expected %x, got %x", expected, output)
	}
}

func TestStripMySQLRows(t *testing.T) {
	// Column def packet: length = 6 (3 bytes length + 1 byte seq + 2 bytes def payload starts with 0x03, 'd', 'e', 'f')
	colDef := []byte{6, 0, 0, 1, 0x03, 'd', 'e', 'f', 8, 9}

	// Row packet: length = 4 (3 bytes length + 1 byte seq + 1 byte payload of other value)
	row := []byte{2, 0, 0, 2, 0x0c, 0x0d}

	// EOF packet: length = 5 (starts with 0xfe)
	eof := []byte{5, 0, 0, 3, 0xfe, 0, 0, 0, 0}

	input := append(append(colDef, row...), eof...)
	output := stripMySQLRows(input)

	// Output should contain colDef and eof, but NOT row
	expected := append(colDef, eof...)
	if !bytes.Equal(output, expected) {
		t.Errorf("MySQL row stripping failed. Expected %x, got %x", expected, output)
	}
}
