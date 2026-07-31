package storage_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sxwebdev/walletspace/internal/storage"
)

func TestAtomicWriteUsesPrivatePermissions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "space.json")
	if err := storage.AtomicWrite(path, []byte("first")); err != nil {
		t.Fatalf("AtomicWrite() error = %v", err)
	}
	if err := storage.AtomicWrite(path, []byte("second")); err != nil {
		t.Fatalf("AtomicWrite() replacement error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "second" {
		t.Errorf("contents = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestProcessLockIsExclusiveAndRecoverable(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	first, err := storage.AcquireLock(home)
	if err != nil {
		t.Fatalf("AcquireLock(first) error = %v", err)
	}
	second, err := storage.AcquireLock(home)
	if !errors.Is(err, storage.ErrAlreadyLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("AcquireLock(second) error = %v, want ErrAlreadyLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first) error = %v", err)
	}
	recovered, err := storage.AcquireLock(home)
	if err != nil {
		t.Fatalf("AcquireLock(after release) error = %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatalf("Close(recovered) error = %v", err)
	}
}
