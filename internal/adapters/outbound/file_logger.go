package outbound

import (
	"db-proxy/internal/core/domain"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileLogger adapter writes query logs to file and system logs to the standard output.
type FileLogger struct {
	logDir string
	mu     sync.Mutex
}

// NewFileLogger instantiates a new FileLogger and ensures the log directory exists.
func NewFileLogger(logDir string) (*FileLogger, error) {
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, err
	}
	return &FileLogger{logDir: logDir}, nil
}

// LogQuery writes an entry to a daily log file, named like <thirdparty>-YYYY-MM-DD.log.
func (fl *FileLogger) LogQuery(entry domain.LogEntry) error {
	fl.mu.Lock()
	defer fl.mu.Unlock()

	dateStr := entry.Timestamp.Format("2006-01-02")
	fileName := fmt.Sprintf("%s-%s.log", entry.ThirdParty, dateStr)
	filePath := filepath.Join(fl.logDir, fileName)

	// Format: [2006-01-02T15:04:05Z07:00] [127.0.0.1:12345] SELECT * FROM test_table
	var logLine string
	if entry.Response != "" {
		logLine = fmt.Sprintf("[%s] [%s] %s\n  Response: %s\n", entry.Timestamp.Format(time.RFC3339), entry.ClientIP, entry.Query, strings.ReplaceAll(entry.Response, "\n", "\n  "))
	} else {
		logLine = fmt.Sprintf("[%s] [%s] %s\n", entry.Timestamp.Format(time.RFC3339), entry.ClientIP, entry.Query)
	}

	// Open file in append mode. Create it if it doesn't exist.
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", filePath, err)
	}
	defer f.Close()

	if _, err := f.WriteString(logLine); err != nil {
		return fmt.Errorf("failed to write log string: %w", err)
	}

	return nil
}

// Info logs messages with an INFO prefix to standard output.
func (fl *FileLogger) Info(msg string, args ...interface{}) {
	log.Printf("[INFO] "+msg, args...)
}

// Warn logs messages with a WARN prefix to standard output.
func (fl *FileLogger) Warn(msg string, args ...interface{}) {
	log.Printf("[WARN] "+msg, args...)
}

// Error logs messages with an ERROR prefix to standard output.
func (fl *FileLogger) Error(msg string, args ...interface{}) {
	log.Printf("[ERROR] "+msg, args...)
}
