package devicesync_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/devicesync"
)

var (
	spaceTagA  = strings.Repeat("a", 32)
	deviceTagA = strings.Repeat("1", 32)
	deviceTagB = strings.Repeat("2", 32)
)

// hang is how long a test waits before it calls an operation hung. It only
// catches what never returns, so it is far beyond any margin a loaded
// machine needs; no test relies on anything finishing within it.
const hang = 30 * time.Second

func newStore(t *testing.T) (*devicesync.FolderStore, string) {
	t.Helper()
	root := t.TempDir()
	store, err := devicesync.NewFolderStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return store, root
}

func TestFolderStoreRoundTrip(t *testing.T) {
	t.Parallel()

	store, root := newStore(t)
	if got, err := store.Spaces(t.Context()); err != nil || len(got) != 0 {
		t.Fatalf("Spaces(empty) = %v, %v", got, err)
	}
	if got, err := store.List(t.Context(), spaceTagA); err != nil || len(got) != 0 {
		t.Fatalf("List(unknown space) = %v, %v", got, err)
	}
	if _, err := store.Get(t.Context(), spaceTagA, deviceTagA); !errors.Is(err, devicesync.ErrNotDownloaded) {
		t.Fatalf("Get(missing) error = %v, want ErrNotDownloaded", err)
	}

	if err := store.Put(t.Context(), spaceTagA, deviceTagA, []byte("one")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := store.Put(t.Context(), spaceTagA, deviceTagA, []byte("two")); err != nil {
		t.Fatalf("Put(again) error = %v", err)
	}
	got, err := store.Get(t.Context(), spaceTagA, deviceTagA)
	if err != nil || string(got) != "two" {
		t.Fatalf("Get() = %q, %v; want the last write", got, err)
	}
	path := filepath.Join(root, "v1", spaceTagA, deviceTagA+".wsync")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the file is not where the layout says: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
	if spaces, err := store.Spaces(t.Context()); err != nil || !slices.Equal(spaces, []string{spaceTagA}) {
		t.Errorf("Spaces() = %v, %v", spaces, err)
	}
	if store.Root() != root {
		t.Errorf("Root() = %q", store.Root())
	}
}

// The folder is shared with whatever else lives in iCloud Drive or Dropbox,
// and with the provider's own temporary files. Only well-named files count.
func TestFolderStoreIgnoresWhatIsNotItsOwn(t *testing.T) {
	t.Parallel()

	store, root := newStore(t)
	if err := store.Put(t.Context(), spaceTagA, deviceTagA, []byte("mine")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "v1", spaceTagA)
	for _, name := range []string{
		"notes.txt",
		deviceTagB + ".wsync.tmp",
		"." + deviceTagA + ".wsync.12345.tmp",
		strings.ToUpper(spaceTagA) + ".wsync",
		deviceTagB + " (conflicted copy).wsync",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, strings.Repeat("3", 32)+".wsync"), 0o700); err != nil {
		t.Fatal(err)
	}
	// iCloud's placeholder for a file it has not downloaded yet.
	if err := os.WriteFile(filepath.Join(dir, "."+deviceTagB+".wsync.icloud"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "v1", "not-a-tag"), 0o700); err != nil {
		t.Fatal(err)
	}

	entries, err := store.List(t.Context(), spaceTagA)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("List() = %+v, want this device's file and one placeholder", entries)
	}
	for _, entry := range entries {
		switch entry.DeviceTag {
		case deviceTagA:
			if entry.Evicted || entry.Modified.IsZero() {
				t.Errorf("own file entry = %+v", entry)
			}
		case deviceTagB:
			if !entry.Evicted {
				t.Errorf("placeholder entry = %+v, want evicted", entry)
			}
		default:
			t.Errorf("unexpected entry %+v", entry)
		}
	}
	if spaces, err := store.Spaces(t.Context()); err != nil || !slices.Equal(spaces, []string{spaceTagA}) {
		t.Errorf("Spaces() = %v, %v; want only the tag directory", spaces, err)
	}
}

func TestFolderStoreRefusesBadNames(t *testing.T) {
	t.Parallel()

	if _, err := devicesync.NewFolderStore("relative/path"); err == nil {
		t.Error("NewFolderStore(relative) succeeded")
	}
	store, _ := newStore(t)
	for _, tags := range [][2]string{{"../escape", deviceTagA}, {spaceTagA, "../escape"}, {"", ""}} {
		if err := store.Put(t.Context(), tags[0], tags[1], []byte("x")); err == nil {
			t.Errorf("Put(%q, %q) succeeded", tags[0], tags[1])
		}
		if _, err := store.Get(t.Context(), tags[0], tags[1]); err == nil {
			t.Errorf("Get(%q, %q) succeeded", tags[0], tags[1])
		}
	}
	if _, err := store.List(t.Context(), "../escape"); err == nil {
		t.Error("List(bad tag) succeeded")
	}
}

func TestFolderStoreRespectsACancelledContext(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	ctx, cancel := contextWithCancel(t)
	cancel()
	if err := store.Put(ctx, spaceTagA, deviceTagA, []byte("x")); err == nil {
		t.Error("Put(cancelled) succeeded")
	}
	if _, err := store.Get(ctx, spaceTagA, deviceTagA); err == nil {
		t.Error("Get(cancelled) succeeded")
	}
}

// A read can block in the kernel: a file the provider has not downloaded is
// fetched on open, and a volume that stopped answering never returns. The
// store must still give up when its context ends — that is what lets sync be
// turned off, and the wallet quit, while the folder is stuck. A FIFO with no
// writer blocks its reader the same way.
func TestFolderStoreGivesUpOnAReadThatBlocks(t *testing.T) {
	t.Parallel()

	store, root := newStore(t)
	dir := filepath.Join(root, "v1", spaceTagA)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stuck := filepath.Join(dir, deviceTagA+".wsync")
	if err := syscall.Mkfifo(stuck, 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	t.Cleanup(func() {
		// Release the abandoned reader: a writer that opens and closes the
		// FIFO ends its read.
		if writer, err := os.OpenFile(stuck, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			writer.Close()
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := store.Get(ctx, spaceTagA, deviceTagA)
		returned <- err
	}()
	select {
	case err := <-returned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Get() error = %v, want the deadline", err)
		}
	case <-time.After(hang):
		t.Fatal("Get() kept waiting on a blocked read after its context ended")
	}
}

// The folder belongs to whatever copies it. When it goes away under a running
// wallet — Dropbox unlinked, a volume unmounted — a write must fail where the
// person sees it, not recreate the path as a plain local folder that nothing
// copies while every pass reports the space written.
func TestFolderStoreDoesNotRecreateAFolderThatWentAway(t *testing.T) {
	t.Parallel()

	provider := filepath.Join(t.TempDir(), "Dropbox")
	root := filepath.Join(provider, "Walletspace")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := devicesync.NewFolderStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), spaceTagA, deviceTagA, []byte("x")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := os.RemoveAll(provider); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), spaceTagA, deviceTagA, []byte("y")); err == nil {
		t.Error("Put() into a folder that went away succeeded")
	}
	if _, err := os.Stat(provider); !os.IsNotExist(err) {
		t.Fatalf("the folder that went away was made again: %v", err)
	}
}

// Each call given up on keeps its thread until the folder answers it. A folder
// that never does must not cost one more thread per pass: once a few calls
// are stuck, the store stops asking and says so. The stuck reads here are
// given up on only once each is known to be in the folder, so all of them
// hold a slot however slowly the machine runs; a real blocking read is
// TestFolderStoreGivesUpOnAReadThatBlocks.
func TestFolderStoreStopsAskingAFolderThatDoesNotAnswer(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	entered := make(chan struct{}, 8)
	unstick := make(chan struct{})
	t.Cleanup(func() { close(unstick) })
	var reads atomic.Int32
	devicesync.SetReadFile(store, func(string) ([]byte, error) {
		reads.Add(1)
		entered <- struct{}{}
		<-unstick
		return nil, nil
	})
	devicesync.SetSlotWait(store, 50*time.Millisecond)

	for range 4 {
		ctx, cancel := context.WithCancel(t.Context())
		returned := make(chan error, 1)
		go func() {
			_, err := store.Get(ctx, spaceTagA, deviceTagA)
			returned <- err
		}()
		select {
		case <-entered:
		case <-time.After(hang):
			t.Fatal("a read never reached the folder")
		}
		cancel()
		select {
		case err := <-returned:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Get() error = %v, want context.Canceled", err)
			}
		case <-time.After(hang):
			t.Fatal("Get() kept waiting on a stuck read after its caller went away")
		}
	}
	returned := make(chan error, 1)
	go func() {
		_, err := store.Get(t.Context(), spaceTagA, deviceTagA)
		returned <- err
	}()
	select {
	case err := <-returned:
		if !errors.Is(err, devicesync.ErrNotAnswering) {
			t.Fatalf("Get() error = %v, want ErrNotAnswering", err)
		}
	case <-time.After(hang):
		t.Fatal("the store kept asking a folder whose earlier calls are stuck")
	}
	if n := reads.Load(); n != 4 {
		t.Fatalf("the folder was asked %d times, want only the 4 stuck reads", n)
	}
}

// stuckWriter is a write that does not return until released, as a write
// into a folder that stopped answering does not. Its first call is the stuck
// one; any call made while it is stuck is counted, and should not happen.
type stuckWriter struct {
	entered chan struct{}
	release chan struct{}
	started atomic.Bool
	others  atomic.Int32
}

func newStuckWriter(t *testing.T) *stuckWriter {
	t.Helper()
	w := &stuckWriter{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(w.free)
	return w
}

func (w *stuckWriter) write(string, []byte) error {
	if w.started.CompareAndSwap(false, true) {
		close(w.entered)
		<-w.release
		return nil
	}
	w.others.Add(1)
	return nil
}

func (w *stuckWriter) free() {
	select {
	case <-w.release:
	default:
		close(w.release)
	}
}

// abandonWrite starts a write through store that does not return, and gives
// up waiting for it.
func abandonWrite(t *testing.T, store *devicesync.FolderStore, w *stuckWriter) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- store.Put(ctx, spaceTagA, deviceTagA, []byte("old")) }()
	select {
	case <-w.entered:
	case <-time.After(hang):
		t.Fatal("the stuck write never started")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Put(stuck) error = %v, want context.Canceled", err)
		}
	case <-time.After(hang):
		t.Fatal("Put() kept waiting on a stuck write after its caller went away")
	}
}

// putEventually retries a write that is refused while an abandoned one is
// still in flight, until that one has landed and freed the folder.
func putEventually(t *testing.T, store *devicesync.FolderStore, blob string) {
	t.Helper()
	deadline := time.Now().Add(hang)
	for {
		err := store.Put(t.Context(), spaceTagA, deviceTagA, []byte(blob))
		if err == nil {
			return
		}
		if !errors.Is(err, devicesync.ErrNotAnswering) || time.Now().After(deadline) {
			t.Fatalf("Put() after the stuck write landed error = %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A write given up on may still land. Were a second write let in beside it,
// the older content could land last and replace the newer: one write at a
// time is what keeps the order.
func TestFolderStoreKeepsOneWriteInFlight(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	w := newStuckWriter(t)
	devicesync.SetWriteFile(store, w.write)
	abandonWrite(t, store, w)
	devicesync.SetSlotWait(store, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), hang)
	defer cancel()
	if err := store.Put(ctx, spaceTagA, deviceTagA, []byte("new")); !errors.Is(err, devicesync.ErrNotAnswering) {
		t.Fatalf("Put() beside a stuck write error = %v, want ErrNotAnswering", err)
	}
	if n := w.others.Load(); n != 0 {
		t.Fatalf("%d writes ran beside the stuck one", n)
	}

	w.free()
	putEventually(t, store, "new")
}

// The wallet makes a new store each time the sync settings are saved. A write
// abandoned through the old store must keep out writes through the new one
// for the same folder — however its path is spelled — and only for it.
func TestFolderStoreKeepsOneWriteInFlightPerFolder(t *testing.T) {
	t.Parallel()

	first, root := newStore(t)
	w := newStuckWriter(t)
	devicesync.SetWriteFile(first, w.write)
	abandonWrite(t, first, w)

	second, err := devicesync.NewFolderStore(root + string(filepath.Separator) + ".")
	if err != nil {
		t.Fatal(err)
	}
	devicesync.SetSlotWait(second, 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), hang)
	defer cancel()
	if err := second.Put(ctx, spaceTagA, deviceTagA, []byte("new")); !errors.Is(err, devicesync.ErrNotAnswering) {
		t.Fatalf("Put() through a new store beside a stuck write error = %v, want ErrNotAnswering", err)
	}
	if _, err := second.Get(t.Context(), spaceTagA, deviceTagA); !errors.Is(err, devicesync.ErrNotDownloaded) {
		t.Fatalf("a write ran beside the stuck one: Get() error = %v", err)
	}

	other, _ := newStore(t)
	if err := other.Put(t.Context(), spaceTagA, deviceTagA, []byte("elsewhere")); err != nil {
		t.Fatalf("Put() into another folder error = %v; a stuck folder held it up", err)
	}

	w.free()
	putEventually(t, second, "new")
	if got, err := second.Get(t.Context(), spaceTagA, deviceTagA); err != nil || string(got) != "new" {
		t.Fatalf("Get() = %q, %v; want the newer write", got, err)
	}
}

// One write at a time must not mean a refused write: a call that has
// returned has given its slot back, so the next one is let in. The stores
// here do not wait for a busy slot at all, so that a slot still held after
// its call returned shows as a refusal. Many stores write at once only to
// make the scheduler switch away at the worst moment; each store writes one
// call after another.
func TestFolderStoreLetsTheNextWriteIn(t *testing.T) {
	t.Parallel()

	var refused atomic.Int64
	var writers sync.WaitGroup
	for range 16 {
		store, _ := newStore(t)
		devicesync.SetWriteFile(store, func(string, []byte) error { return nil })
		devicesync.SetSlotWait(store, 0)
		writers.Go(func() {
			for range 5000 {
				err := store.Put(t.Context(), spaceTagA, deviceTagA, []byte("x"))
				if errors.Is(err, devicesync.ErrNotAnswering) {
					refused.Add(1)
				} else if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	writers.Wait()
	if n := refused.Load(); n != 0 {
		t.Fatalf("%d writes made after the previous one returned were refused", n)
	}
}

// gatedWriter is a healthy but slow write: its first call holds until
// opened, and every call is recorded in the order it ran.
type gatedWriter struct {
	entered chan struct{}
	open    chan struct{}
	started atomic.Bool
	mu      sync.Mutex
	order   []string
}

func newGatedWriter(t *testing.T) *gatedWriter {
	t.Helper()
	g := &gatedWriter{entered: make(chan struct{}), open: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-g.open:
		default:
			close(g.open)
		}
	})
	return g
}

func (g *gatedWriter) write(path string, blob []byte) error {
	if g.started.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.open
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.order = append(g.order, filepath.Base(path))
	return nil
}

// Every space of the installation writes into one folder, so a write often
// finds another under way: the 30-second pass over one space while an
// unlock writes another. A healthy folder is not a stuck one; the second
// write waits its turn and lands after the first, through the same store or
// through another one for the same folder.
func TestFolderStoreRunsBusyWritesOneAfterAnother(t *testing.T) {
	t.Parallel()

	for name, sameStore := range map[string]bool{"same store": true, "two stores": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			first, root := newStore(t)
			g := newGatedWriter(t)
			devicesync.SetWriteFile(first, g.write)
			second := first
			if !sameStore {
				var err error
				if second, err = devicesync.NewFolderStore(root); err != nil {
					t.Fatal(err)
				}
				devicesync.SetWriteFile(second, g.write)
			}

			results := make(chan error, 2)
			go func() { results <- first.Put(t.Context(), spaceTagA, deviceTagA, []byte("a")) }()
			select {
			case <-g.entered:
			case <-time.After(hang):
				t.Fatal("the first write never started")
			}
			go func() { results <- second.Put(t.Context(), spaceTagA, deviceTagB, []byte("b")) }()
			select {
			case err := <-results:
				t.Fatalf("the second write returned %v while the first was under way; want it to wait", err)
			case <-time.After(100 * time.Millisecond):
			}
			close(g.open)
			for range 2 {
				select {
				case err := <-results:
					if err != nil {
						t.Fatalf("Put() error = %v", err)
					}
				case <-time.After(hang):
					t.Fatal("a write never returned")
				}
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			want := []string{deviceTagA + ".wsync", deviceTagB + ".wsync"}
			if !slices.Equal(g.order, want) {
				t.Fatalf("writes ran in the order %q, want %q", g.order, want)
			}
		})
	}
}

// Reads wait their turn the same way: with every read slot held by a read
// that is slow but will end — a provider fetching a file on open — the next
// read waits and then runs, instead of reporting the folder as not
// answering.
func TestFolderStoreRunsBusyReadsOneAfterAnother(t *testing.T) {
	t.Parallel()

	store, _ := newStore(t)
	if err := store.Put(t.Context(), spaceTagA, deviceTagB, []byte("b")); err != nil {
		t.Fatal(err)
	}
	inSlow := make(chan struct{}, 4)
	open := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-open:
		default:
			close(open)
		}
	})
	devicesync.SetReadFile(store, func(path string) ([]byte, error) {
		if filepath.Base(path) == deviceTagA+".wsync" {
			inSlow <- struct{}{}
			<-open
			return []byte("a"), nil
		}
		return os.ReadFile(path)
	})

	slowReads := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := store.Get(t.Context(), spaceTagA, deviceTagA)
			slowReads <- err
		}()
	}
	for range 4 {
		select {
		case <-inSlow:
		case <-time.After(hang):
			t.Fatal("the slow reads never started")
		}
	}
	type read struct {
		blob []byte
		err  error
	}
	next := make(chan read, 1)
	go func() {
		blob, err := store.Get(t.Context(), spaceTagA, deviceTagB)
		next <- read{blob, err}
	}()
	select {
	case r := <-next:
		t.Fatalf("Get() returned %q, %v while every slot was busy; want it to wait", r.blob, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(open)
	select {
	case r := <-next:
		if r.err != nil || string(r.blob) != "b" {
			t.Fatalf("Get() behind busy reads = %q, %v; want it to wait and read", r.blob, r.err)
		}
	case <-time.After(hang):
		t.Fatal("Get() behind busy reads never returned")
	}
	for range 4 {
		if err := <-slowReads; err != nil {
			t.Fatalf("slow Get() error = %v", err)
		}
	}
}

// Waiting for a busy slot is bounded. A write stuck in the folder makes the
// next one wait out the bound and hear that the folder is not answering —
// not wait for good — and a caller who goes away while waiting gets its own
// error at once.
func TestFolderStoreGivesUpWaitingForAStuckWrite(t *testing.T) {
	t.Parallel()

	first, root := newStore(t)
	w := newStuckWriter(t)
	devicesync.SetWriteFile(first, w.write)
	abandonWrite(t, first, w)
	second, err := devicesync.NewFolderStore(root)
	if err != nil {
		t.Fatal(err)
	}

	const bound = 100 * time.Millisecond
	devicesync.SetSlotWait(second, bound)
	start := time.Now()
	returned := make(chan error, 1)
	go func() { returned <- second.Put(t.Context(), spaceTagA, deviceTagB, []byte("b")) }()
	select {
	case err := <-returned:
		if !errors.Is(err, devicesync.ErrNotAnswering) {
			t.Fatalf("Put() behind a stuck write error = %v, want ErrNotAnswering", err)
		}
		if took := time.Since(start); took < bound {
			t.Fatalf("Put() gave up after %s, before the %s bound", took, bound)
		}
	case <-time.After(hang):
		t.Fatal("Put() kept waiting behind a stuck write past its bound")
	}

	devicesync.SetSlotWait(second, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { returned <- second.Put(ctx, spaceTagA, deviceTagB, []byte("b")) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Put() whose caller went away error = %v, want context.Canceled", err)
		}
	case <-time.After(hang):
		t.Fatal("Put() kept waiting for a slot after its caller went away")
	}
	if _, err := second.Get(t.Context(), spaceTagA, deviceTagB); !errors.Is(err, devicesync.ErrNotDownloaded) {
		t.Fatalf("a write ran beside the stuck one: Get() error = %v", err)
	}
}

func TestFolderStoreReportsAnUnreadableFolder(t *testing.T) {
	t.Parallel()

	store, root := newStore(t)
	// A file where the version directory should be.
	if err := os.WriteFile(filepath.Join(root, "v1"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Spaces(t.Context()); err == nil {
		t.Error("Spaces() over a broken folder succeeded")
	}
	if _, err := store.List(t.Context(), spaceTagA); err == nil {
		t.Error("List() over a broken folder succeeded")
	}
	if err := store.Put(t.Context(), spaceTagA, deviceTagA, []byte("x")); err == nil {
		t.Error("Put() over a broken folder succeeded")
	}
	if _, err := store.Get(t.Context(), spaceTagA, deviceTagA); err == nil || errors.Is(err, devicesync.ErrNotDownloaded) {
		t.Errorf("Get() over a broken folder error = %v", err)
	}
}

func TestDeviceIDIsMadeOnceAndKept(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	first, err := devicesync.DeviceID(home)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "dev_") || len(first) < 20 {
		t.Fatalf("DeviceID() = %q", first)
	}
	again, err := devicesync.DeviceID(home)
	if err != nil || again != first {
		t.Fatalf("DeviceID(again) = %q, %v; want %q", again, err, first)
	}
	other, err := devicesync.DeviceID(t.TempDir())
	if err != nil || other == first {
		t.Fatalf("two homes share the device id %q", other)
	}

	for name, content := range map[string]string{
		"not JSON":   "not json",
		"invalid id": `{"device_id":"../x"}`,
	} {
		broken := t.TempDir()
		if err := os.WriteFile(filepath.Join(broken, "device.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := devicesync.DeviceID(broken); err == nil {
			t.Errorf("%s: DeviceID() succeeded", name)
		}
	}
	unreadable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unreadable, "device.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := devicesync.DeviceID(unreadable); err == nil {
		t.Error("DeviceID() with a directory in place of the file succeeded")
	}
}

func TestSuggestedFolderIsInICloudDrive(t *testing.T) {
	t.Parallel()

	got := devicesync.SuggestedFolder()
	if got != "" && !strings.HasSuffix(got, filepath.Join("com~apple~CloudDocs", "Walletspace")) {
		t.Errorf("SuggestedFolder() = %q", got)
	}
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithCancel(t.Context())
}
