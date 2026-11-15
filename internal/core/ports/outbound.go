package ports

import (
	"db-proxy/internal/core/domain"
)

// QueryLogger is an outbound port implemented by adapters to write query log entries.
type QueryLogger interface {
	LogQuery(entry domain.LogEntry) error
}

// SystemLogger is an outbound port for printing application logs to standard outputs.
type SystemLogger interface {
	Info(msg string, args ...interface{})
	Warn(msg string, args ...interface{})
	Error(msg string, args ...interface{})
}

// FileCleaner is an outbound port implemented by adapters to purge files based on retention settings.
type FileCleaner interface {
	CleanOldLogs(logDir string, retentionDays int) (int, error)
}

// ProtocolParser is an outbound port to extract commands/queries from client payloads.
type ProtocolParser interface {
	Parse(data []byte) []string
	ParseServerResponse(data []byte) // Server-to-client hook for stateful handshake tracing
	BuildErrorResponse(errMsg string) []byte
}

// QueryBroadcaster is an outbound port to broadcast log entries to connected web clients.
type QueryBroadcaster interface {
	Broadcast(entry domain.LogEntry)
}
