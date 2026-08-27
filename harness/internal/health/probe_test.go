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

func TestCheckProbe_MarkerPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")

	if err := WriteProbe(path); err != nil {
		t.Fatalf("WriteProbe() error: %v", err)
	}

	if err := CheckProbe(path); err != nil {
		t.Fatalf("CheckProbe() error: %v", err)
	}
}

func TestCheckProbe_MarkerAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	err := CheckProbe(path)
	if err == nil {
		t.Fatal("CheckProbe() = nil, want an error for a missing marker")
	}
	if !os.IsNotExist(err) {
		t.Errorf("CheckProbe() error = %v, want os.IsNotExist to be true", err)
	}
}

func TestCheckProbe_MarkerUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced when running as root")
	}

	path := filepath.Join(t.TempDir(), "healthy")
	if err := WriteProbe(path); err != nil {
		t.Fatalf("WriteProbe() error: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("os.Chmod() error: %v", err)
	}

	err := CheckProbe(path)
	if err == nil {
		t.Fatal("CheckProbe() = nil, want a permission error for an unreadable marker")
	}
	if os.IsNotExist(err) {
		t.Errorf("CheckProbe() error = %v, want a permission error distinguishable from absent", err)
	}
}
