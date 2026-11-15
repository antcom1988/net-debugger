package inbound

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"db-proxy/internal/core/domain"
)

func TestHandleLogsMembacaHistoryDariFileLog(t *testing.T) {
	logDir := t.TempDir()
	content := "[2026-11-14T22:00:00+07:00] [127.0.0.1:5000] SELECT *\n  FROM users\n  Response: id | name\n    1 | Luna\n"
	if err := os.WriteFile(filepath.Join(logDir, "postgres-2026-11-14.log"), []byte(content), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	ws := NewWebServer(domain.AppConfig{LogDir: logDir}, nil, dummyLogger{}, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	response := httptest.NewRecorder()

	ws.handleLogs(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	var entries []domain.LogEntry
	if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode history response: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entry count = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.ThirdParty != "postgres" || entry.Query != "SELECT *\n  FROM users" || entry.Response != "id | name\n  1 | Luna" {
		t.Fatalf("history entry = %#v", entry)
	}
}

func TestHandleClearLogsMenghapusHanyaFileLog(t *testing.T) {
	logDir := t.TempDir()
	for _, name := range []string{"redis-2026-11-14.log", "postgres-2026-11-14.log", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(logDir, name), []byte("fixture"), 0644); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}

	ws := NewWebServer(domain.AppConfig{LogDir: logDir}, nil, dummyLogger{}, nil, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/logs/clear", nil)
	response := httptest.NewRecorder()

	ws.handleClearLogs(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusOK, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"deleted":2`) {
		t.Fatalf("response = %s, want deleted count", response.Body.String())
	}
	for _, name := range []string{"redis-2026-11-14.log", "postgres-2026-11-14.log"} {
		if _, err := os.Stat(filepath.Join(logDir, name)); !os.IsNotExist(err) {
			t.Fatalf("log file %s still exists, err=%v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(logDir, "keep.txt")); err != nil {
		t.Fatalf("non-log file must be preserved: %v", err)
	}
}

func TestChaosActionCompatibility(t *testing.T) {
	cases := []struct {
		connectionType string
		action         string
		allowed        bool
	}{
		{"postgres", "error", true},
		{"redis", "throttle", true},
		{"iso8583", "delay", true},
		{"iso8583", "error", false},
		{"iso8583", "empty", false},
		{"api", "api_response", true},
		{"api", "error", false},
		{"redis", "api_response", false},
	}
	for _, tc := range cases {
		if got := allowedChaosAction(tc.connectionType, tc.action); got != tc.allowed {
			t.Errorf("allowedChaosAction(%q, %q) = %v, want %v", tc.connectionType, tc.action, got, tc.allowed)
		}
	}
}

func TestHandleClearLogsRejectsMethodSelainPost(t *testing.T) {
	ws := NewWebServer(domain.AppConfig{LogDir: t.TempDir()}, nil, dummyLogger{}, nil, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/logs/clear", nil)
	response := httptest.NewRecorder()

	ws.handleClearLogs(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}
