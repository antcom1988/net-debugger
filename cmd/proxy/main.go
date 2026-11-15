package main

import (
	"context"
	"db-proxy/internal/adapters/inbound"
	"db-proxy/internal/adapters/outbound"
	"db-proxy/internal/core/domain"
	"db-proxy/internal/core/ports"
	"db-proxy/internal/core/services"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	// 1. Read and Parse Configuration File
	configPath := "config.json"
	configFile, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Printf("[FATAL] Failed to read configuration file %s: %v\n", configPath, err)
		os.Exit(1)
	}

	var config domain.AppConfig
	if err := json.Unmarshal(configFile, &config); err != nil {
		fmt.Printf("[FATAL] Failed to parse configuration JSON: %v\n", err)
		os.Exit(1)
	}

	// Resolve log directory to absolute path
	absLogDir, err := filepath.Abs(config.LogDir)
	if err != nil {
		fmt.Printf("[FATAL] Failed to resolve log directory path: %v\n", err)
		os.Exit(1)
	}
	config.LogDir = absLogDir

	// 2. Instantiate Outbound Adapters
	fileLogger, err := outbound.NewFileLogger(config.LogDir)
	if err != nil {
		fmt.Printf("[FATAL] Failed to initialize file logger: %v\n", err)
		os.Exit(1)
	}

	fileLogger.Info("Bootstrap starting...")
	fileLogger.Info("Configuration loaded: log retention is %d days, log dir is %s", config.RetentionDays, config.LogDir)

	fileCleaner := outbound.NewDiskFileCleaner()

	// 3. Instantiate Core Services
	chaosService := services.NewChaosService("chaos_rules.json")
	broadcastHub := services.NewBroadcastHub(fileLogger)
	proxyService := services.NewProxyService(fileLogger, fileLogger, broadcastHub, chaosService)
	retentionService := services.NewRetentionService(config.LogDir, config.RetentionDays, fileCleaner, fileLogger)

	// 4. Instantiate Parser Factories
	// Load ISO8583 GenericPackager XML once at startup. The returned parser is
	// used only as an immutable field-definition template; every TCP socket gets
	// an isolated parser instance below.
	iso8583Template, err := outbound.NewISO8583Parser()
	if err != nil {
		fileLogger.Error("Failed to load ISO8583 packager: %v", err)
		os.Exit(1)
	}
	parserFactories := map[string]func() ports.ProtocolParser{
		"redis":    func() ports.ProtocolParser { return outbound.NewRedisParser() },
		"postgres": func() ports.ProtocolParser { return outbound.NewPostgresParser() },
		"mariadb":  func() ports.ProtocolParser { return outbound.NewMySQLParser() },
		"iso8583":  func() ports.ProtocolParser { return iso8583Template.NewConnectionParser() },
	}

	// 5. Instantiate Inbound Adapters (TCP Server and Web Server)
	server := inbound.NewTCPServer(config, proxyService, parserFactories, fileLogger)
	webServer := inbound.NewWebServer(config, broadcastHub, fileLogger, chaosService, server)

	// 6. Setup Context for signal interception
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 7. Start Inbound Adapters
	if err := server.Start(ctx); err != nil {
		fileLogger.Error("Failed to start proxy server: %v", err)
		os.Exit(1)
	}

	if err := webServer.Start(ctx); err != nil {
		fileLogger.Error("Failed to start web server: %v", err)
		os.Exit(1)
	}

	// 8. Run log retention cleanup in a background ticker
	go func() {
		// Run once shortly after startup
		time.Sleep(2 * time.Second)
		_ = retentionService.CleanupLogs()

		// Run periodic cleanup every 12 hours
		ticker := time.NewTicker(12 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = retentionService.CleanupLogs()
			}
		}
	}()

	fileLogger.Info("Application successfully started. Press Ctrl+C to terminate.")

	// Block until a shutdown signal is received
	sig := <-sigChan
	fileLogger.Info("Received signal %v. Initiating graceful shutdown...", sig)
	cancel()

	// Stop TCP Server and clean up listeners
	if err := server.Stop(); err != nil {
		fileLogger.Error("Error during server shutdown: %v", err)
	}

	// Stop Web Server
	if err := webServer.Stop(); err != nil {
		fileLogger.Error("Error during web server shutdown: %v", err)
	}

	fileLogger.Info("Application stopped.")
}
