package services

import (
	"db-proxy/internal/core/domain"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedQueryLogger struct {
	mu      sync.Mutex
	entries []domain.LogEntry
}

func (l *recordedQueryLogger) LogQuery(entry domain.LogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
	return nil
}

func (l *recordedQueryLogger) Entries() []domain.LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.LogEntry(nil), l.entries...)
}

type recordedBroadcaster struct {
	mu      sync.Mutex
	entries []domain.LogEntry
}

func (b *recordedBroadcaster) Broadcast(entry domain.LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, entry)
}

func TestHTTPProxyForwardsRequestAndLogsResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/orders" || r.URL.RawQuery != "source=service-a" {
			t.Errorf("URL = %s, want /v1/orders?source=service-a", r.URL.String())
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"item":"coffee"}` {
			t.Errorf("body = %q", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ord-123"}`))
	}))
	defer backend.Close()

	logger := &recordedQueryLogger{}
	broadcaster := &recordedBroadcaster{}
	proxy := NewProxyService(logger, dummyServiceLogger{}, broadcaster, NewChaosService(t.TempDir()+"/chaos.json"))
	handler, err := proxy.NewHTTPHandler("orders_api", backend.URL)
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "http://dapoer.local/v1/orders?source=service-a", strings.NewReader(`{"item":"coffee"}`))
	req.Header.Set("Authorization", "Bearer super-secret")
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", res.Code, http.StatusCreated, res.Body.String())
	}
	if res.Body.String() != `{"id":"ord-123"}` {
		t.Errorf("response body = %q", res.Body.String())
	}

	entries := logger.Entries()
	if len(entries) != 1 {
		t.Fatalf("logged entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.ThirdParty != "orders_api" {
		t.Errorf("third_party = %q, want orders_api", entry.ThirdParty)
	}
	if !strings.Contains(entry.Query, "POST /v1/orders?source=service-a") {
		t.Errorf("query does not contain request line: %q", entry.Query)
	}
	if !strings.Contains(entry.Query, "\nHeader : Authorization: Bearer super-secret; Content-Type: application/json") {
		t.Errorf("query header format = %q", entry.Query)
	}
	if !strings.Contains(entry.Query, "\nBody : {\"item\":\"coffee\"}") {
		t.Errorf("query body format = %q", entry.Query)
	}
	if !strings.Contains(entry.Response, "HTTP 201") || !strings.Contains(entry.Response, `{"id":"ord-123"}`) {
		t.Errorf("response = %q", entry.Response)
	}
	if got := len(broadcaster.entries); got != 1 {
		t.Errorf("broadcasts = %d, want 1", got)
	}
}

type dummyServiceLogger struct{}

func (dummyServiceLogger) Info(string, ...interface{})  {}
func (dummyServiceLogger) Warn(string, ...interface{})  {}
func (dummyServiceLogger) Error(string, ...interface{}) {}

func TestHTTPProxyAcceptsHostPortBackendAsHTTP(t *testing.T) {
	proxy := NewProxyService(&recordedQueryLogger{}, dummyServiceLogger{}, nil, NewChaosService(t.TempDir()+"/chaos.json"))
	if handler, err := proxy.NewHTTPHandler("simulator", "127.0.0.1:18089"); err != nil || handler == nil {
		t.Fatalf("NewHTTPHandler host:port = (%v, %v), want handler without error", handler, err)
	}
	if _, err := proxy.NewHTTPHandler("bad_api", "ftp://127.0.0.1:21"); err == nil {
		t.Fatal("NewHTTPHandler accepted non-HTTP URL")
	}
}

func TestHTTPProxyAppliesChaosDelayForNamedAPIConnection(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	chaos := NewChaosService(t.TempDir() + "/chaos.json")
	if err := chaos.UpdateRules([]domain.ChaosRule{{
		ID: "delay-api", Service: "orders_api", MatchType: "substring", Pattern: "POST /orders",
		Action: "delay", DelayRequestMs: 80, Probability: 1, Enabled: true,
	}}); err != nil {
		t.Fatalf("UpdateRules: %v", err)
	}
	logger := &recordedQueryLogger{}
	proxy := NewProxyService(logger, dummyServiceLogger{}, nil, chaos)
	handler, err := proxy.NewHTTPHandler("orders_api", backend.URL)
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}

	started := time.Now()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodPost, "http://dapoer.local/orders", nil))
	elapsed := time.Since(started)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
	if elapsed < 70*time.Millisecond {
		t.Fatalf("request elapsed %s, want at least 70ms chaos delay", elapsed)
	}
	entries := logger.Entries()
	if len(entries) != 1 || entries[0].ThirdParty != "orders_api" || !strings.HasPrefix(entries[0].Query, "[CHAOS_DELAY_REQUEST: 80ms] POST /orders") {
		t.Fatalf("unexpected API chaos log entries: %#v", entries)
	}
}

func TestHTTPProxyAppliesChaosDelayToAPIResponse(t *testing.T) {
	backendReceived := make(chan time.Time, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendReceived <- time.Now()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	chaos := NewChaosService(t.TempDir() + "/chaos.json")
	if err := chaos.UpdateRules([]domain.ChaosRule{{
		ID: "delay-api-response", Service: "orders_api", MatchType: "substring", Pattern: "GET /orders",
		Action: "delay", DelayResponseMs: 80, Probability: 1, Enabled: true,
	}}); err != nil {
		t.Fatalf("UpdateRules: %v", err)
	}
	proxy := NewProxyService(&recordedQueryLogger{}, dummyServiceLogger{}, nil, chaos)
	handler, err := proxy.NewHTTPHandler("orders_api", backend.URL)
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}

	started := time.Now()
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "http://dapoer.local/orders", nil))
	elapsed := time.Since(started)

	select {
	case receivedAt := <-backendReceived:
		if receivedAt.Sub(started) >= 70*time.Millisecond {
			t.Fatalf("backend received request after %s, want request without response delay", receivedAt.Sub(started))
		}
	default:
		t.Fatal("backend did not receive request")
	}
	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
	if elapsed < 70*time.Millisecond {
		t.Fatalf("response elapsed %s, want at least 70ms response delay", elapsed)
	}
}
