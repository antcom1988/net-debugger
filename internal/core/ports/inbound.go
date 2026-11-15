package ports

import (
	"context"
	"db-proxy/internal/core/domain"
)

// ProxyServer is an inbound port driven by the entry point to run the TCP proxy server.
type ProxyServer interface {
	Start(ctx context.Context) error
	Stop() error
	GetServices() map[string]domain.ServiceConfig
	UpdateService(name string, cfg domain.ServiceConfig) error
	RemoveService(name string) error
}

// RetentionService is an inbound port driven by the scheduler/cron or entry point to clean up old logs.
type RetentionService interface {
	CleanupLogs() error
}

// WebServer is an inbound port driven by the entry point to start/stop the web dashboard.
type WebServer interface {
	Start(ctx context.Context) error
	Stop() error
}
