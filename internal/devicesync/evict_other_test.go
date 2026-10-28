//go:build !darwin

package devicesync_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/devicesync"
)

// Only iCloud Drive on macOS can be asked for a file. Elsewhere the store
// says so without running anything.
func TestDownloadIsUnsupportedHere(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	runner := &fakeRunner{}
	devicesync.SetRunner(store, runner.run, time.Second)
	if err := store.Download(t.Context(), spaceTagA, deviceTagA); !errors.Is(err, devicesync.ErrDownloadUnsupported) {
		t.Fatalf("Download() error = %v, want ErrDownloadUnsupported", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("Download() ran %q", runner.calls)
	}

	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if devicesync.IsDataless(info) {
		t.Error("IsDataless() = true where no file is kept without its data")
	}
}
