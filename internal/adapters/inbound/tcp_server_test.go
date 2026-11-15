package inbound

import (
	"context"
	"db-proxy/internal/core/domain"
	"db-proxy/internal/core/ports"
	"db-proxy/internal/core/services"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type dummyLogger struct{}

func (d dummyLogger) Info(msg string, args ...interface{})  {}
func (d dummyLogger) Warn(msg string, args ...interface{})  {}
func (d dummyLogger) Error(msg string, args ...interface{}) {}

type dummyQueryLogger struct{}

func (d dummyQueryLogger) LogQuery(entry domain.LogEntry) error { return nil }

type dummyQueryBroadcaster struct{}

func (d dummyQueryBroadcaster) Broadcast(entry domain.LogEntry) {}

type dummyParser struct{}

func (d *dummyParser) Parse(data []byte) []string { return []string{string(data)} }
func (d *dummyParser) ParseServerResponse(data []byte) {}
func (d *dummyParser) BuildErrorResponse(errMsg string) []byte { return []byte(errMsg) }

func TestTCPServerLifecycle(t *testing.T) {
	// Clean up config file if it exists, and clean up at the end
	_ = os.Remove("config.json")
	defer func() {
		_ = os.Remove("config.json")
	}()

	sysLog := dummyLogger{}
	qLogger := dummyQueryLogger{}
	qBroadcaster := dummyQueryBroadcaster{}

	// Setup chaos service with dummy temp path
	tmpDir, err := os.MkdirTemp("", "chaos-tcp-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	chaosSvc := services.NewChaosService(filepath.Join(tmpDir, "chaos_rules.json"))

	proxySvc := services.NewProxyService(qLogger, sysLog, qBroadcaster, chaosSvc)

	parserFactories := map[string]func() ports.ProtocolParser{
		"redis": func() ports.ProtocolParser {
			return &dummyParser{}
		},
	}

	config := domain.AppConfig{
		LogDir:        tmpDir,
		RetentionDays: 7,
		Services:      make(map[string]domain.ServiceConfig),
	}

	server := NewTCPServer(config, proxySvc, parserFactories, sysLog)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := server.Start(ctx); err != nil {
		t.Fatalf("failed to start TCP server: %v", err)
	}
	defer func() {
		_ = server.Stop()
	}()

	// Test 1: GetServices initially empty
	svcs := server.GetServices()
	if len(svcs) != 0 {
		t.Errorf("expected 0 services, got %d", len(svcs))
	}

	// Test 2: UpdateService (Add a service)
	testPort := 28350
	svcName := "redis_test"
	svcCfg := domain.ServiceConfig{
		Type:        "redis",
		ListenPort:  testPort,
		BackendAddr: "localhost:6379",
		Enabled:     true,
	}

	err = server.UpdateService(svcName, svcCfg)
	if err != nil {
		t.Fatalf("failed to update service: %v", err)
	}

	// Verify GetServices returns the added service
	svcs = server.GetServices()
	if len(svcs) != 1 {
		t.Errorf("expected 1 service, got %d", len(svcs))
	}
	if svcs[svcName].ListenPort != testPort {
		t.Errorf("expected service port %d, got %d", testPort, svcs[svcName].ListenPort)
	}

	// Verify config.json is saved and contains the updated service
	configFile, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("config.json was not created/readable: %v", err)
	}
	var loadedConfig domain.AppConfig
	if err := json.Unmarshal(configFile, &loadedConfig); err != nil {
		t.Fatalf("failed to parse config.json: %v", err)
	}
	if _, ok := loadedConfig.Services[svcName]; !ok {
		t.Errorf("config.json does not contain service %s", svcName)
	}

	// Verify the port is open and accepting TCP connections
	addr := fmt.Sprintf("127.0.0.1:%d", testPort)
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		t.Errorf("failed to connect to service port %d: %v", testPort, err)
	} else {
		conn.Close()
	}

	// Test 3: Disable service
	svcCfg.Enabled = false
	err = server.UpdateService(svcName, svcCfg)
	if err != nil {
		t.Fatalf("failed to update service to disabled: %v", err)
	}

	// Verify GetServices is disabled
	svcs = server.GetServices()
	if !svcs[svcName].Enabled {
		// Verify the port is closed (dial should fail or time out)
		conn, err = net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Errorf("expected connection to port %d to fail after service was disabled, but it succeeded", testPort)
		}
	} else {
		t.Errorf("service was not updated in config")
	}

	// Test 4: RemoveService
	err = server.RemoveService(svcName)
	if err != nil {
		t.Fatalf("failed to remove service: %v", err)
	}

	// Verify GetServices is empty
	svcs = server.GetServices()
	if len(svcs) != 0 {
		t.Errorf("expected 0 services after removal, got %d", len(svcs))
	}

	// Verify config.json has it removed
	configFile, err = os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("config.json readable error: %v", err)
	}
	loadedConfig = domain.AppConfig{}
	if err := json.Unmarshal(configFile, &loadedConfig); err != nil {
		t.Fatalf("failed to parse config.json: %v", err)
	}
	if _, ok := loadedConfig.Services[svcName]; ok {
		t.Errorf("config.json still contains service %s after removal", svcName)
	}
}
