package httpapi

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/vault"
)

// A sync folder on a mount that stopped answering — a network volume gone
// away, a provider stuck — must not keep the wallet from starting. The look
// at the folder is given up on, and sync is reported as not running, the
// way a folder that is not there is.
//
// Not parallel: it replaces the package's folder check and its bound.
func TestAFolderThatDoesNotAnswerAtStartIsReportedNotWaitedFor(t *testing.T) {
	release := make(chan struct{})
	originalCheck, originalTimeout := checkFolder, folderCheckTimeout
	checkFolder = func(string) error {
		<-release
		return nil
	}
	folderCheckTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		close(release)
		checkFolder, folderCheckTimeout = originalCheck, originalTimeout
	})

	home := t.TempDir()
	spaces, err := space.NewManager(home, 15*time.Minute, vault.Params{Time: 1, MemoryKiB: 8 * 1024, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(spaces.Close)
	started := make(chan struct{})
	go func() {
		defer close(started)
		StartSync(spaces, home, config.SyncSettings{Enabled: true, Folder: filepath.Join(t.TempDir(), "Walletspace")},
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("start waited for a sync folder that does not answer")
	}
	overview := spaces.SyncOverview(t.Context())
	if !overview.Enabled || !strings.Contains(overview.Error, "did not answer") {
		t.Fatalf("overview = %+v, want sync reported on and not running", overview)
	}
}

// A folder that is away at start — a volume not mounted yet — does not stop
// changes from being prepared: they are sealed into the outbox as they are
// made, unlocked or locked. When the folder comes back it is opened without a
// restart, and what was prepared is sent without an unlock.
//
// Not parallel: it replaces the package's folder check and retry interval.
func TestSyncOverAFolderAwayAtStartPreparesChangesAndResumesWhenItIsBack(t *testing.T) {
	var back atomic.Bool
	originalCheck, originalRetry := checkFolder, folderRetryInterval
	checkFolder = func(folder string) error {
		if !back.Load() {
			return errors.New("the volume is not mounted")
		}
		return prepareFolder(folder)
	}
	folderRetryInterval = 50 * time.Millisecond
	t.Cleanup(func() { checkFolder, folderRetryInterval = originalCheck, originalRetry })

	const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	const password = "away folder password"
	home := t.TempDir()
	spaces, err := space.NewManager(home, 15*time.Minute, vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(spaces.Close)
	created, err := spaces.Create(space.CreateRequest{Mnemonic: phrase, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	if err := spaces.Lock(id); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(t.TempDir(), "Walletspace")
	StartSync(spaces, home, config.SyncSettings{Enabled: true, Folder: folder}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if overview := spaces.SyncOverview(t.Context()); !overview.Enabled || !strings.Contains(overview.Error, "not mounted") {
		t.Fatalf("overview = %+v, want sync on and not running", overview)
	}

	if err := spaces.Unlock(id, password); err != nil {
		t.Fatal(err)
	}
	if _, err := spaces.Import(id, "eth-sepolia", account.FamilyEVM, "", strings.Repeat("22", 32)); err != nil {
		t.Fatal(err)
	}
	if err := spaces.Lock(id); err != nil {
		t.Fatal(err)
	}
	status := spaces.SyncOverview(t.Context()).Spaces[id]
	if status.OutboxRevision != status.LocalRevision || status.Delivery != space.DeliveryUnsent || status.NeedsUnlock {
		t.Fatalf("status = %+v, want the change sealed and unsent", status)
	}
	outbox, err := os.ReadFile(filepath.Join(home, "spaces", id, "outbox.wsync"))
	if err != nil {
		t.Fatal(err)
	}

	back.Store(true)
	seed, err := account.Seed(phrase, "")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := devicesync.DeriveKeys(seed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(keys.Clear)
	deviceID, err := devicesync.DeviceID(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, "v1", keys.SpaceTag(), keys.DeviceTag(deviceID, id)+".wsync")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && bytes.Equal(data, outbox) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the change prepared while the folder was away never reached it")
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for spaces.SyncOverview(t.Context()).Spaces[id].Delivery != space.DeliveryUploadUnknown {
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v, want written", spaces.SyncOverview(t.Context()).Spaces[id])
		}
		time.Sleep(10 * time.Millisecond)
	}
	if overview := spaces.SyncOverview(t.Context()); overview.Error != "" {
		t.Fatalf("overview error = %q after the folder came back", overview.Error)
	}
}

// awaySync starts sync over a folder whose check is answered by check, with
// the check's bound and the retry interval scaled down, and returns the
// manager.
//
// Not for parallel tests: it replaces the package's folder check, bound and
// retry interval.
func awaySync(t *testing.T, check func(string) error, timeout time.Duration) (*space.Manager, string) {
	t.Helper()
	originalCheck, originalTimeout, originalRetry := checkFolder, folderCheckTimeout, folderRetryInterval
	checkFolder, folderCheckTimeout, folderRetryInterval = check, timeout, 10*time.Millisecond
	t.Cleanup(func() {
		checkFolder, folderCheckTimeout, folderRetryInterval = originalCheck, originalTimeout, originalRetry
	})
	home := t.TempDir()
	spaces, err := space.NewManager(home, 15*time.Minute, vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(spaces.Close)
	folder := filepath.Join(t.TempDir(), "Walletspace")
	StartSync(spaces, home, config.SyncSettings{Enabled: true, Folder: folder}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return spaces, folder
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5 s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A mount that stays hung is asked about again on every retry, and every
// check of it would hang a thread for good: thousands of retries later the
// process would die at its thread limit. However often it is retried, one
// check of the folder is in flight; once it returns, the next retry opens the
// folder.
func TestAHungFolderIsCheckedOneAtATime(t *testing.T) {
	release := make(chan struct{})
	var calls, inFlight, most atomic.Int64
	spaces, _ := awaySync(t, func(folder string) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			if m := most.Load(); n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		if calls.Add(1) == 1 {
			<-release
			return errors.New("the volume answered at last")
		}
		return prepareFolder(folder)
	}, 20*time.Millisecond)
	// Dozens of retries while the first check is stuck.
	time.Sleep(400 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("a hung folder was checked %d times at once", n)
	}
	if overview := spaces.SyncOverview(t.Context()); !overview.Enabled || overview.Error == "" {
		t.Fatalf("overview = %+v, want sync on and not running", overview)
	}
	close(release)
	waitUntil(t, "the folder opened", func() bool { return spaces.SyncOverview(t.Context()).Error == "" })
	if n := most.Load(); n != 1 {
		t.Fatalf("%d checks of the folder ran at once", n)
	}
}

// Sync turned off while its folder is away stays off when the folder comes
// back: the retry belongs to the setting that started it. A retry that was
// under way when sync was turned off does not install its folder, and no
// retry follows it.
func TestTurningSyncOffWhileItsFolderIsAwayStopsTheRetry(t *testing.T) {
	t.Run("a retry under way", func(t *testing.T) {
		var back, holdNext atomic.Bool
		var calls atomic.Int64
		entered, release := make(chan struct{}, 1), make(chan struct{})
		spaces, _ := awaySync(t, func(folder string) error {
			calls.Add(1)
			if holdNext.CompareAndSwap(true, false) {
				entered <- struct{}{}
				<-release
			}
			if !back.Load() {
				return errors.New("the volume is not mounted")
			}
			return prepareFolder(folder)
		}, 5*time.Second)
		waitUntil(t, "a retry", func() bool { return calls.Load() >= 2 })
		holdNext.Store(true)
		<-entered
		spaces.SetSync(nil, "", nil)
		back.Store(true)
		close(release)
		time.Sleep(200 * time.Millisecond)
		if overview := spaces.SyncOverview(t.Context()); overview.Enabled {
			t.Fatalf("overview = %+v, want sync off", overview)
		}
	})

	t.Run("no retry after", func(t *testing.T) {
		var back atomic.Bool
		var calls atomic.Int64
		spaces, _ := awaySync(t, func(folder string) error {
			calls.Add(1)
			if !back.Load() {
				return errors.New("the volume is not mounted")
			}
			return prepareFolder(folder)
		}, 20*time.Millisecond)
		waitUntil(t, "a retry", func() bool { return calls.Load() >= 2 })
		spaces.SetSync(nil, "", nil)
		settled := calls.Load()
		time.Sleep(200 * time.Millisecond)
		// A retry already under way when sync was turned off may finish.
		if n := calls.Load(); n > settled+1 {
			t.Fatalf("the folder was checked %d more times after sync was turned off", n-settled)
		}
		back.Store(true)
		time.Sleep(100 * time.Millisecond)
		if overview := spaces.SyncOverview(t.Context()); overview.Enabled {
			t.Fatalf("overview = %+v, want sync off", overview)
		}
	})
}
