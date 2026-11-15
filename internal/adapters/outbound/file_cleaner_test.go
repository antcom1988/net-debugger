package outbound

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskFileCleaner(t *testing.T) {
	// Create a temporary log directory for testing
	tempDir, err := os.MkdirTemp("", "log_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	now := time.Now()
	// Calculate relative dates dynamically to guarantee test reproducibility
	oldDateStr := now.AddDate(0, 0, -10).Format("2006-01-02") // 10 days ago (should be deleted)
	newDateStr := now.AddDate(0, 0, -2).Format("2006-01-02")   // 2 days ago (should be kept)

	oldFile := fmt.Sprintf("redis-%s.log", oldDateStr)
	newFile := fmt.Sprintf("postgres-%s.log", newDateStr)
	invalidFile := "random-file.log"

	// Create test files
	filesToCreate := []string{oldFile, newFile, invalidFile}
	for _, f := range filesToCreate {
		err := os.WriteFile(filepath.Join(tempDir, f), []byte("sample logs"), 0644)
		if err != nil {
			t.Fatalf("failed to write dummy log file: %v", err)
		}
	}

	cleaner := NewDiskFileCleaner()
	deleted, err := cleaner.CleanOldLogs(tempDir, 7) // 7 days retention limit
	if err != nil {
		t.Fatalf("error calling CleanOldLogs: %v", err)
	}

	if deleted != 1 {
		t.Errorf("expected 1 file to be deleted, got %d", deleted)
	}

	// 1. Verify old file is gone
	if _, err := os.Stat(filepath.Join(tempDir, oldFile)); !os.IsNotExist(err) {
		t.Errorf("expected old file %s to be deleted, but it still exists", oldFile)
	}

	// 2. Verify new file is still present
	if _, err := os.Stat(filepath.Join(tempDir, newFile)); os.IsNotExist(err) {
		t.Errorf("expected new file %s to be kept, but it was deleted", newFile)
	}

	// 3. Verify invalid file is still present
	if _, err := os.Stat(filepath.Join(tempDir, invalidFile)); os.IsNotExist(err) {
		t.Errorf("expected file with invalid naming format %s to be kept, but it was deleted", invalidFile)
	}
}
