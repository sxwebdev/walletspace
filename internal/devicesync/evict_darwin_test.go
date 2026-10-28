package devicesync_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/devicesync"
)

// iCloud's tool is asked for the file by its real path, which is what the
// placeholder stands for, and by its full path so PATH cannot swap it.
func TestDownloadAsksBrctlForTheRealFile(t *testing.T) {
	t.Parallel()

	store, root := newStore(t)
	dir := filepath.Join(root, "v1", spaceTagA)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "."+deviceTagB+".wsync.icloud"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{out: []byte("download complete")}
	devicesync.SetRunner(store, runner.run, time.Second)
	if err := store.Download(t.Context(), spaceTagA, deviceTagB); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	want := []string{"/usr/bin/brctl", "download", filepath.Join(dir, deviceTagB+".wsync")}
	if len(runner.calls) != 1 || !slices.Equal(runner.calls[0], want) {
		t.Fatalf("ran %q, want %q", runner.calls, want)
	}
}

// A refused request says what brctl said — a folder outside iCloud Drive,
// iCloud signed out — so the status can show it.
func TestDownloadReportsARefusedRequest(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	runner := &fakeRunner{out: []byte("  Unable to start downloads: not in iCloud Drive\n"), err: errors.New("exit status 1")}
	devicesync.SetRunner(store, runner.run, time.Second)
	err := store.Download(t.Context(), spaceTagA, deviceTagA)
	if err == nil || errors.Is(err, devicesync.ErrDownloadUnsupported) {
		t.Fatalf("Download() error = %v, want a failed request", err)
	}
	if !strings.Contains(err.Error(), "Unable to start downloads: not in iCloud Drive") {
		t.Errorf("Download() error = %q, want what brctl printed", err)
	}
}

// A Mac without brctl cannot be asked, any more than another system can: the
// caller tells the person to keep the folder downloaded instead.
func TestDownloadWithoutBrctlIsUnsupported(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	runner := &fakeRunner{err: &fs.PathError{Op: "fork/exec", Path: "/usr/bin/brctl", Err: syscall.ENOENT}}
	devicesync.SetRunner(store, runner.run, time.Second)
	if err := store.Download(t.Context(), spaceTagA, deviceTagA); !errors.Is(err, devicesync.ErrDownloadUnsupported) {
		t.Fatalf("Download() error = %v, want ErrDownloadUnsupported", err)
	}
}

// brctl may wait for the download to finish, and an offline provider never
// finishes it. The request is given up on after a while, as a failure of the
// request — not as the caller's context ending, which would read as the pass
// being cut short.
func TestDownloadGivesUpOnARequestThatDoesNotFinish(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	runner := &fakeRunner{block: true}
	devicesync.SetRunner(store, runner.run, 50*time.Millisecond)
	returned := make(chan error, 1)
	go func() { returned <- store.Download(t.Context(), spaceTagA, deviceTagA) }()
	select {
	case err := <-returned:
		if err == nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, devicesync.ErrDownloadUnsupported) {
			t.Fatalf("Download() error = %v, want a request that did not finish", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Download() kept waiting on a request that does not finish")
	}
}

// Whoever asked going away is theirs to report: the error is their context's.
func TestDownloadStopsWithItsCaller(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	runner := &fakeRunner{}
	devicesync.SetRunner(store, runner.run, time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Download(ctx, spaceTagA, deviceTagA); !errors.Is(err, context.Canceled) {
		t.Fatalf("Download(cancelled) error = %v, want context.Canceled", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a cancelled Download ran %q", runner.calls)
	}

	blocked := &fakeRunner{block: true}
	devicesync.SetRunner(store, blocked.run, time.Minute)
	ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := store.Download(ctx, spaceTagA, deviceTagA); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Download() whose caller gave up error = %v, want the caller's deadline", err)
	}
}

type statInfo struct {
	fs.FileInfo
	sys any
}

func (i statInfo) Sys() any { return i.sys }

// A real dataless file cannot be made here: only the provider makes one. The
// flag is what tells it, and no other flag does.
func TestIsDatalessReadsTheFlag(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		sys  any
		want bool
	}{
		{"dataless", &syscall.Stat_t{Flags: 0x40000000}, true},
		{"dataless and hidden", &syscall.Stat_t{Flags: 0x40000000 | 0x8000}, true},
		{"hidden", &syscall.Stat_t{Flags: 0x8000}, false},
		{"compressed", &syscall.Stat_t{Flags: 0x20}, false},
		{"no stat", nil, false},
	} {
		if got := devicesync.IsDataless(statInfo{sys: tc.sys}); got != tc.want {
			t.Errorf("%s: IsDataless() = %v, want %v", tc.name, got, tc.want)
		}
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
		t.Error("IsDataless() of a plain local file = true")
	}
}
