package health

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteProbe_CreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")

	if err := WriteProbe(path); err != nil {
		t.Fatalf("WriteProbe() error: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("probe file does not exist after WriteProbe: %v", err)
	}
}

func TestWriteProbe_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")

	if err := WriteProbe(path); err != nil {
		t.Fatalf("first WriteProbe() error: %v", err)
	}
	if err := WriteProbe(path); err != nil {
		t.Fatalf("second WriteProbe() error: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("probe file does not exist after second WriteProbe: %v", err)
	}
}

func TestRemoveProbe_RemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")

	if err := WriteProbe(path); err != nil {
		t.Fatalf("WriteProbe() error: %v", err)
	}

	if err := RemoveProbe(path); err != nil {
		t.Fatalf("RemoveProbe() error: %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("probe file still exists after RemoveProbe")
	}
}

func TestRemoveProbe_NonexistentFileReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	if err := RemoveProbe(path); err != nil {
		t.Fatalf("RemoveProbe() on nonexistent file returned error: %v", err)
	}
}
