package devicesync_test

import (
	"context"
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/devicesync"
)

// fakeRunner stands in for brctl. Nothing in these tests runs the real one:
// it would act on the real iCloud Drive.
type fakeRunner struct {
	calls [][]string
	out   []byte
	err   error
	// block makes a call wait for its context, as a request to a provider
	// that is offline does.
	block bool
}

func (r *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.block {
		<-ctx.Done()
		return nil, errors.New("signal: killed")
	}
	return r.out, r.err
}

func TestDownloadRefusesBadTags(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	runner := &fakeRunner{}
	devicesync.SetRunner(store, runner.run, time.Second)
	for _, tags := range [][2]string{{"../escape", deviceTagA}, {spaceTagA, "../escape"}} {
		if err := store.Download(t.Context(), tags[0], tags[1]); err == nil || errors.Is(err, devicesync.ErrDownloadUnsupported) {
			t.Errorf("Download(%q, %q) error = %v", tags[0], tags[1], err)
		}
	}
	if len(runner.calls) != 0 {
		t.Errorf("a bad tag reached the command line: %v", runner.calls)
	}
}

// On macOS 14 and later a File Provider folder keeps a file it moved to the
// cloud under its own name with no data behind it. Such a file is listed as
// dataless, not as evicted: only iCloud Drive can be asked for it by brctl,
// and reading it is what makes every other provider — Dropbox, Google Drive,
// OneDrive — fetch it. A pass that skipped it would never see another
// machine's changes there, so it must stay readable.
func TestFolderStoreListsDatalessFilesAsReadable(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	for _, device := range []string{deviceTagA, deviceTagB} {
		if err := store.Put(t.Context(), spaceTagA, device, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	devicesync.SetDataless(store, func(info fs.FileInfo) bool {
		return info.Name() == deviceTagB+".wsync"
	})
	entries, err := store.List(t.Context(), spaceTagA)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() = %+v, want two entries", entries)
	}
	for _, entry := range entries {
		want := entry.DeviceTag == deviceTagB
		if entry.Dataless != want || entry.Evicted || entry.Modified.IsZero() {
			t.Errorf("entry %+v, want dataless = %v, not evicted, and a modification time", entry, want)
		}
		got, err := store.Get(t.Context(), spaceTagA, entry.DeviceTag)
		if err != nil || string(got) != "x" {
			t.Errorf("Get(%s) = %q, %v; want the file read", entry.DeviceTag, got, err)
		}
	}
}

// A program that outlives its context is killed, and so is the wait for its
// output: a shell killed while its child still holds the pipe must not keep
// the call open until the child is done.
func TestRunCommandStopsWhenItsContextEnds(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"program": {"/bin/sleep", "10"},
		"child":   {"/bin/sh", "-c", "/bin/sleep 10; true"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := devicesync.RunCommand(ctx, args[0], args[1:]...)
			if err == nil {
				t.Fatal("RunCommand() of a killed program succeeded")
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Fatalf("RunCommand() returned %s after its context ended", took)
			}
		})
	}
}

func TestRunCommandReturnsWhatTheProgramPrinted(t *testing.T) {
	t.Parallel()

	out, err := devicesync.RunCommand(t.Context(), "/bin/sh", "-c", "echo to-out; echo to-err >&2; exit 3")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("RunCommand() error = %v, want exit status 3", err)
	}
	if !strings.Contains(string(out), "to-out") || !strings.Contains(string(out), "to-err") {
		t.Errorf("RunCommand() output = %q, want both streams", out)
	}
}
