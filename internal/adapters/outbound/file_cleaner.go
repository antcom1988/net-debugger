package outbound

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DiskFileCleaner is an outbound adapter that handles log file deletion.
type DiskFileCleaner struct{}

// NewDiskFileCleaner instantiates a new DiskFileCleaner.
func NewDiskFileCleaner() *DiskFileCleaner {
	return &DiskFileCleaner{}
}

// CleanOldLogs scans the given directory and removes log files matching <service>-YYYY-MM-DD.log
// if the parsed date is strictly before (time.Now() - retentionDays).
func (dfc *DiskFileCleaner) CleanOldLogs(logDir string, retentionDays int) (int, error) {
	if retentionDays <= 0 {
		return 0, nil // Infinite retention / disabled
	}

	// Calculate cutoff date: midnight of retentionDays ago
	now := time.Now()
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -retentionDays)

	files, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // Directory does not exist yet
		}
		return 0, fmt.Errorf("failed to read log directory %s: %w", logDir, err)
	}

	deletedCount := 0
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		name := file.Name()
		if !strings.HasSuffix(name, ".log") {
			continue
		}

		// Example: redis-2026-06-20.log
		base := strings.TrimSuffix(name, ".log")
		parts := strings.Split(base, "-")
		if len(parts) < 4 {
			// Filename must have at least service-YYYY-MM-DD (4 elements separated by dash)
			continue
		}

		// Extract last 3 parts as YYYY-MM-DD
		dateStr := strings.Join(parts[len(parts)-3:], "-")
		fileDate, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			// Filename doesn't end with a valid YYYY-MM-DD date, ignore
			continue
		}

		// Check if file is older than cutoff date
		if fileDate.Before(cutoff) {
			filePath := filepath.Join(logDir, name)
			if err := os.Remove(filePath); err != nil {
				// Attempt to clean other files even if one fails
				continue
			}
			deletedCount++
		}
	}

	return deletedCount, nil
}
