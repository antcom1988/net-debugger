package inbound

import (
	"context"
	"db-proxy/internal/core/domain"
	"db-proxy/internal/core/ports"
	"db-proxy/internal/core/services"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
)

// TCPServer is an inbound adapter that implements ports.ProxyServer.
type TCPServer struct {
	config          domain.AppConfig
	proxyService    *services.ProxyService
	parserFactories map[string]func() ports.ProtocolParser
	sysLog          ports.SystemLogger
	listeners       map[string]net.Listener
	apiServers      map[string]*http.Server
	mu              sync.Mutex
	wg              sync.WaitGroup
	ctx             context.Context
	cancel          context.CancelFunc
}

// NewTCPServer instantiates a new multi-port TCP server.
func NewTCPServer(
	config domain.AppConfig,
	proxyService *services.ProxyService,
	parserFactories map[string]func() ports.ProtocolParser,
	sysLog ports.SystemLogger,
) *TCPServer {
	return &TCPServer{
		config:          config,
		proxyService:    proxyService,
		parserFactories: parserFactories,
		sysLog:          sysLog,
		listeners:       make(map[string]net.Listener),
		apiServers:      make(map[string]*http.Server),
	}
}

// Start launches a listener goroutine for each configured and enabled service.
func (s *TCPServer) Start(ctx context.Context) error {
	s.mu.Lock()
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()

	for name, svcCfg := range s.config.Services {
		if !svcCfg.Enabled {
			s.sysLog.Info("[%s] Proxy disabled, skipping", name)
			continue
		}

		dbType := svcCfg.Type
		if dbType == "" {
			dbType = name // fallback for backward compatibility
		}

		factory, ok := s.parserFactories[dbType]
		if dbType != "api" && !ok {
			return fmt.Errorf("no protocol parser factory found for service: %s (type: %s)", name, dbType)
		}

		addr := fmt.Sprintf(":%d", svcCfg.ListenPort)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on port %d for service %s: %w", svcCfg.ListenPort, name, err)
		}

		s.mu.Lock()
		s.listeners[name] = listener
		s.mu.Unlock()

		s.sysLog.Info("[%s] Proxy listening on %s (forwarding to %s)", name, addr, svcCfg.BackendAddr)
		if dbType == "api" {
			handler, err := s.proxyService.NewHTTPHandler(name, svcCfg.BackendAddr)
			if err != nil {
				_ = listener.Close()
				return err
			}
			server := &http.Server{Handler: handler}
			s.mu.Lock()
			s.apiServers[name] = server
			s.mu.Unlock()
			s.wg.Add(1)
			go s.serveAPI(name, listener, server)
			continue
		}

		s.wg.Add(1)
		go s.acceptConnections(name, dbType, listener, svcCfg.BackendAddr, factory)
	}

	return nil
}

func (s *TCPServer) acceptConnections(name, protocolType string, listener net.Listener, backendAddr string, factory func() ports.ProtocolParser) {
	defer s.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.ctx.Done():
				return // Listener stopped intentionally
			default:
				if errors.Is(err, net.ErrClosed) {
					return // Listener was closed dynamically
				}
				s.sysLog.Error("[%s] Accept connection error: %v", name, err)
				continue
			}
		}

		// Instantiate a fresh connection-isolated parser
		parser := factory()

		// Handle connection in a concurrent goroutine
		go s.proxyService.HandleConnection(s.ctx, conn, backendAddr, name, protocolType, parser)
	}
}

func (s *TCPServer) serveAPI(name string, listener net.Listener, server *http.Server) {
	defer s.wg.Done()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		s.sysLog.Error("[%s] API proxy server error: %v", name, err)
	}
}

// Stop closes all active listeners and signals listener loops to terminate.
func (s *TCPServer) Stop() error {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}

	for name, listener := range s.listeners {
		s.sysLog.Info("[%s] Stopping listener...", name)
		_ = listener.Close()
	}
	for name, server := range s.apiServers {
		_ = server.Close()
		delete(s.apiServers, name)
	}
	s.mu.Unlock()

	s.wg.Wait()
	s.sysLog.Info("All proxy listeners stopped.")
	return nil
}

// GetServices returns a copy of current services config.
func (s *TCPServer) GetServices() map[string]domain.ServiceConfig {
	s.mu.Lock()
	defer s.mu.Unlock()

	copied := make(map[string]domain.ServiceConfig)
	for k, v := range s.config.Services {
		copied[k] = v
	}
	return copied
}

// UpdateService registers or updates a proxy service, reloading listeners dynamically.
func (s *TCPServer) UpdateService(name string, cfg domain.ServiceConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.config.Services == nil {
		s.config.Services = make(map[string]domain.ServiceConfig)
	}

	// 1. Stop existing listener if it exists
	if listener, exists := s.listeners[name]; exists {
		s.sysLog.Info("[%s] Stopping listener on port reload...", name)
		_ = listener.Close()
		delete(s.listeners, name)
	}

	// 2. Update config in-memory
	s.config.Services[name] = cfg

	// 3. Start listener if enabled
	if cfg.Enabled {
		dbType := cfg.Type
		if dbType == "" {
			dbType = name
		}
		factory, ok := s.parserFactories[dbType]
		if dbType != "api" && !ok {
			return fmt.Errorf("no parser factory found for db type: %s", dbType)
		}

		addr := fmt.Sprintf(":%d", cfg.ListenPort)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to listen on port %d: %w", cfg.ListenPort, err)
		}

		s.listeners[name] = listener
		s.sysLog.Info("[%s] Proxy listening dynamically on %s (forwarding to %s)", name, addr, cfg.BackendAddr)
		if dbType == "api" {
			handler, err := s.proxyService.NewHTTPHandler(name, cfg.BackendAddr)
			if err != nil {
				_ = listener.Close()
				delete(s.listeners, name)
				return err
			}
			server := &http.Server{Handler: handler}
			s.apiServers[name] = server
			s.wg.Add(1)
			go s.serveAPI(name, listener, server)
			return s.saveConfigToFile()
		}

		s.wg.Add(1)
		go s.acceptConnections(name, dbType, listener, cfg.BackendAddr, factory)
	}

	// 4. Persist config change
	return s.saveConfigToFile()
}

// RemoveService deletes a service config and stops its listener.
func (s *TCPServer) RemoveService(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if listener, exists := s.listeners[name]; exists {
		s.sysLog.Info("[%s] Stopping listener on service removal...", name)
		_ = listener.Close()
		delete(s.listeners, name)
	}

	delete(s.config.Services, name)

	return s.saveConfigToFile()
}

func (s *TCPServer) saveConfigToFile() error {
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile("config.json", data, 0644)
}
