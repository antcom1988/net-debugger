package services

import (
	"db-proxy/internal/core/ports"
)

// RetentionService manages log file rotation and retention.
type RetentionService struct {
	logDir        string
	retentionDays int
	cleaner       ports.FileCleaner
	sysLog        ports.SystemLogger
}

// NewRetentionService creates a new instance of RetentionService.
func NewRetentionService(logDir string, retentionDays int, cleaner ports.FileCleaner, sysLog ports.SystemLogger) *RetentionService {
	return &RetentionService{
		logDir:        logDir,
		retentionDays: retentionDays,
		cleaner:       cleaner,
		sysLog:        sysLog,
	}
}

// CleanupLogs executes log deletion for files exceeding the retention day limit.
func (rs *RetentionService) CleanupLogs() error {
	rs.sysLog.Info("Starting log retention cleanup (retention: %d days, dir: %s)...", rs.retentionDays, rs.logDir)
	deleted, err := rs.cleaner.CleanOldLogs(rs.logDir, rs.retentionDays)
	if err != nil {
		rs.sysLog.Error("Log retention cleanup failed: %v", err)
		return err
	}
	rs.sysLog.Info("Log retention cleanup finished. Removed %d log file(s).", deleted)
	return nil
}
