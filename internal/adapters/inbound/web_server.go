package inbound

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"db-proxy/internal/core/domain"
	"db-proxy/internal/core/ports"
	"db-proxy/internal/core/services"
)

//go:embed assets/index.html
var staticAssets embed.FS

// WebServer is an inbound adapter that hosts the HTTP dashboard and streams logs via SSE.
type WebServer struct {
	config      domain.AppConfig
	hub         *services.BroadcastHub
	sysLog      ports.SystemLogger
	chaosSvc    *services.ChaosService
	proxyServer ports.ProxyServer
	server      *http.Server
	mu          sync.Mutex
	wg          sync.WaitGroup
}

// NewWebServer instantiates a new WebServer.
func NewWebServer(config domain.AppConfig, hub *services.BroadcastHub, sysLog ports.SystemLogger, chaosSvc *services.ChaosService, proxyServer ports.ProxyServer) *WebServer {
	return &WebServer{
		config:      config,
		hub:         hub,
		sysLog:      sysLog,
		chaosSvc:    chaosSvc,
		proxyServer: proxyServer,
	}
}

// Start launches the HTTP server listener in a background goroutine.
func (ws *WebServer) Start(ctx context.Context) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if !ws.config.Web.Enabled {
		ws.sysLog.Info("[Web] Web server disabled in configuration, skipping")
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", ws.handleIndex)
	mux.HandleFunc("/api/stream", ws.handleStream)
	mux.HandleFunc("/api/chaos-rules", ws.handleChaosRules)
	mux.HandleFunc("/api/services", ws.handleServices)
	mux.HandleFunc("/api/logs", ws.handleLogs)
	mux.HandleFunc("/api/logs/clear", ws.handleClearLogs)

	addr := fmt.Sprintf(":%d", ws.config.Web.Port)
	ws.server = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Create listener to fail early if port is already in use
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on web port %d: %w", ws.config.Web.Port, err)
	}

	ws.sysLog.Info("[Web] Web dashboard listening on http://localhost%s", addr)

	ws.wg.Add(1)
	go func() {
		defer ws.wg.Done()
		if err := ws.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			ws.sysLog.Error("[Web] HTTP Server error: %v", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the HTTP server.
func (ws *WebServer) Stop() error {
	ws.mu.Lock()
	if ws.server == nil {
		ws.mu.Unlock()
		return nil
	}
	server := ws.server
	ws.mu.Unlock()

	ws.sysLog.Info("[Web] Stopping web server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown HTTP server: %w", err)
	}

	ws.wg.Wait()
	ws.sysLog.Info("[Web] Web server stopped.")
	return nil
}

func (ws *WebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	content, err := staticAssets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "Dashboard asset not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(content)
}

func (ws *WebServer) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported by browser/connection", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Create client channel and register it with the BroadcastHub
	clientCh := make(chan domain.LogEntry, 100)
	ws.hub.Register(clientCh)
	defer ws.hub.Unregister(clientCh)

	// Keep-alive/connection confirmation packet
	_, _ = fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			// Client disconnected or connection closed
			return
		case logEntry, open := <-clientCh:
			if !open {
				return
			}

			data, err := json.Marshal(logEntry)
			if err != nil {
				continue
			}

			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (ws *WebServer) handleChaosRules(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch r.Method {
	case http.MethodGet:
		rules := ws.chaosSvc.GetRules()
		if rules == nil {
			rules = []domain.ChaosRule{}
		}
		_ = json.NewEncoder(w).Encode(rules)

	case http.MethodPost:
		var rules []domain.ChaosRule
		if err := json.NewDecoder(r.Body).Decode(&rules); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "Invalid request body: %v"}`, err), http.StatusBadRequest)
			return
		}
		if err := ws.validateChaosRules(rules); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "Invalid chaos rule: %v"}`, err), http.StatusBadRequest)
			return
		}

		if err := ws.chaosSvc.UpdateRules(rules); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "Failed to save rules: %v"}`, err), http.StatusInternalServerError)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "success", "count": len(rules)})

	default:
		http.Error(w, `{"error": "Method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// validateChaosRules prevents protocol-incompatible mutations even when a client
// bypasses the dashboard and calls the API directly.
func (ws *WebServer) validateChaosRules(rules []domain.ChaosRule) error {
	services := ws.config.Services
	if ws.proxyServer != nil {
		services = ws.proxyServer.GetServices()
	}
	for _, rule := range rules {
		if strings.TrimSpace(rule.Service) == "" {
			return errors.New("connection is required")
		}
		connection, ok := services[rule.Service]
		if !ok {
			return fmt.Errorf("connection %q does not exist", rule.Service)
		}
		if !allowedChaosAction(connection.Type, rule.Action) {
			return fmt.Errorf("action %q is not supported for %s connections", rule.Action, connection.Type)
		}
	}
	return nil
}

func allowedChaosAction(connectionType, action string) bool {
	allowed := map[string]map[string]bool{
		"postgres": {"delay": true, "error": true, "close": true, "empty": true, "throttle": true},
		"mariadb":  {"delay": true, "error": true, "close": true, "empty": true, "throttle": true},
		"redis":    {"delay": true, "error": true, "close": true, "empty": true, "throttle": true},
		"iso8583":  {"delay": true, "close": true},
		"api":      {"delay": true, "api_response": true, "api_drop": true, "api_corrupt": true, "api_header": true},
	}
	return allowed[strings.ToLower(connectionType)][action]
}

func (ws *WebServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	entries, err := loadLogEntries(ws.config.LogDir)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to load logs: %v"}`, err), http.StatusInternalServerError)
		return
	}
	_ = json.NewEncoder(w).Encode(entries)
}

func loadLogEntries(logDir string) ([]domain.LogEntry, error) {
	files, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.LogEntry{}, nil
		}
		return nil, fmt.Errorf("read log directory %s: %w", logDir, err)
	}

	var entries []domain.LogEntry
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".log") {
			continue
		}
		service, ok := logServiceName(file.Name())
		if !ok {
			continue
		}
		parsed, err := parseLogFile(filepath.Join(logDir, file.Name()), service)
		if err != nil {
			return nil, err
		}
		entries = append(entries, parsed...)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Timestamp.After(entries[j].Timestamp) })
	return entries, nil
}

func logServiceName(name string) (string, bool) {
	base := strings.TrimSuffix(name, ".log")
	parts := strings.Split(base, "-")
	if len(parts) < 4 {
		return "", false
	}
	if _, err := time.Parse("2006-01-02", strings.Join(parts[len(parts)-3:], "-")); err != nil {
		return "", false
	}
	return strings.Join(parts[:len(parts)-3], "-"), true
}

func parseLogFile(path, service string) ([]domain.LogEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", path, err)
	}
	defer file.Close()

	var entries []domain.LogEntry
	var current *domain.LogEntry
	inResponse := false
	flush := func() {
		if current == nil {
			return
		}
		current.Query = strings.TrimSuffix(current.Query, "\n")
		current.Response = strings.TrimSuffix(current.Response, "\n")
		entries = append(entries, *current)
		current = nil
		inResponse = false
	}

	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if timestamp, clientIP, query, ok := parseLogEntryStart(line); ok {
			flush()
			current = &domain.LogEntry{Timestamp: timestamp, ThirdParty: service, ClientIP: clientIP, Query: query}
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(line, "  Response: ") {
			current.Response = strings.TrimPrefix(line, "  Response: ")
			inResponse = true
			continue
		}
		if inResponse {
			current.Response += "\n" + strings.TrimPrefix(line, "  ")
		} else {
			current.Query += "\n" + line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan log file %s: %w", path, err)
	}
	flush()
	return entries, nil
}

func parseLogEntryStart(line string) (time.Time, string, string, bool) {
	if !strings.HasPrefix(line, "[") {
		return time.Time{}, "", "", false
	}
	firstEnd := strings.Index(line, "] [")
	if firstEnd == -1 {
		return time.Time{}, "", "", false
	}
	timestamp, err := time.Parse(time.RFC3339, line[1:firstEnd])
	if err != nil {
		return time.Time{}, "", "", false
	}
	clientStart := firstEnd + len("] [")
	clientEndOffset := strings.Index(line[clientStart:], "] ")
	if clientEndOffset == -1 {
		return time.Time{}, "", "", false
	}
	clientEnd := clientStart + clientEndOffset
	return timestamp, line[clientStart:clientEnd], line[clientEnd+2:], true
}

func (ws *WebServer) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	deleted, err := clearLogFiles(ws.config.LogDir)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"failed to clear logs: %v"}`, err), http.StatusInternalServerError)
		return
	}
	if ws.sysLog != nil {
		ws.sysLog.Info("[Web] Cleared %d log file(s) from %s", deleted, ws.config.LogDir)
	}
	_ = json.NewEncoder(w).Encode(map[string]int{"deleted": deleted})
}

func isHexColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for _, char := range value[1:] {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') && !(char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func clearLogFiles(logDir string) (int, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read log directory %s: %w", logDir, err)
	}

	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		if err := os.Remove(filepath.Join(logDir, entry.Name())); err != nil {
			return deleted, fmt.Errorf("remove %s: %w", entry.Name(), err)
		}
		deleted++
	}
	return deleted, nil
}

func (ws *WebServer) handleServices(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch r.Method {
	case http.MethodGet:
		services := ws.proxyServer.GetServices()
		if services == nil {
			services = make(map[string]domain.ServiceConfig)
		}
		_ = json.NewEncoder(w).Encode(services)

	case http.MethodPost:
		var req struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			ListenPort  int    `json:"listen_port"`
			BackendAddr string `json:"backend_addr"`
			LabelColor  string `json:"label_color"`
			Enabled     bool   `json:"enabled"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "Invalid request body: %v"}`, err), http.StatusBadRequest)
			return
		}

		if req.Name == "" || req.Type == "" || req.ListenPort <= 0 || req.BackendAddr == "" {
			http.Error(w, `{"error": "All fields (name, type, listen_port, backend_addr) are required"}`, http.StatusBadRequest)
			return
		}
		if req.LabelColor == "" {
			req.LabelColor = "#38bdf8"
		}
		if !isHexColor(req.LabelColor) {
			http.Error(w, `{"error":"label_color must be a #RRGGBB color"}`, http.StatusBadRequest)
			return
		}

		cfg := domain.ServiceConfig{
			Type:        req.Type,
			ListenPort:  req.ListenPort,
			BackendAddr: req.BackendAddr,
			LabelColor:  req.LabelColor,
			Enabled:     req.Enabled,
		}

		if err := ws.proxyServer.UpdateService(req.Name, cfg); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "%v"}`, err), http.StatusInternalServerError)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"})

	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, `{"error": "Query parameter 'name' is required"}`, http.StatusBadRequest)
			return
		}

		if err := ws.proxyServer.RemoveService(name); err != nil {
			http.Error(w, fmt.Sprintf(`{"error": "%v"}`, err), http.StatusInternalServerError)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"})

	default:
		http.Error(w, `{"error": "Method not allowed"}`, http.StatusMethodNotAllowed)
	}
}
