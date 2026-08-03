//go:build unix

package storage_test

import (
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/storage"
)

func TestProcessLock(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	first, err := storage.AcquireLock(home)
	if err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := storage.AcquireLock(home)
	if !errors.Is(err, storage.ErrAlreadyLocked) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second AcquireLock() error = %v", err)
	}
}
