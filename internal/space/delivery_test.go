package space_test

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/space"
	"golang.org/x/crypto/chacha20poly1305"
)

// waitFor polls until ok holds, for at most five seconds.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5 s for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ownTag is the tag of this machine's file in the test phrase's space.
func ownTag(t *testing.T, deviceID, spaceID string) string {
	t.Helper()
	return syncKeysFor(t).DeviceTag(deviceID, spaceID)
}

// folderFile reads a machine's file from a folder, or nil when it is not
// there.
func folderFile(t *testing.T, folder, deviceID, spaceID string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(folder, "v1", syncKeysFor(t).SpaceTag(), ownTag(t, deviceID, spaceID)+".wsync"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func outboxOf(t *testing.T, home, id string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "spaces", id, "outbox.wsync"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// openOwn opens a machine's file with the phrase's keys.
func openOwn(t *testing.T, blob []byte, deviceID, spaceID string) devicesync.Snapshot {
	t.Helper()
	keys := syncKeysFor(t)
	snap, err := devicesync.Open(keys, keys.SpaceTag(), ownTag(t, deviceID, spaceID), blob)
	if err != nil {
		t.Fatalf("open the machine's file: %v", err)
	}
	t.Cleanup(snap.Clear)
	return snap
}

func statusOf(t *testing.T, manager *space.Manager, id string) space.SyncStatus {
	t.Helper()
	return manager.SyncOverview(t.Context()).Spaces[id]
}

// The case delivery exists for: a wallet added, and the space locked at
// once — before the debounced send, before any pass. Locking takes the keys;
// the change was sealed with the save, and leaves while the space is locked.
func TestAChangeMadeJustBeforeLockingReachesTheFolder(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	created, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	wallet, err := a.manager.Derive(id, "tron-nile", account.FamilyTron, "made before locking")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	// The status says written only once the write is recorded, after the
	// file is in place: wait for that, then look at the file.
	waitFor(t, "the change reported written", func() bool {
		status := statusOf(t, a.manager, id)
		return status.Delivery == space.DeliveryUploadUnknown && status.WrittenRevision == status.LocalRevision
	})
	if status := statusOf(t, a.manager, id); status.State != space.SyncLocked || status.NeedsUnlock {
		t.Fatalf("status = %+v, want the locked space's change written", status)
	}
	blob := folderFile(t, folder, "dev_laptopa", id)
	if blob == nil || !slices.ContainsFunc(openOwn(t, blob, "dev_laptopa", id).Accounts,
		func(item account.Account) bool { return item.ID == wallet.ID }) {
		t.Fatal("the folder does not hold the wallet made before locking")
	}
}

// The wallet stops between saving a change and sealing it into the outbox.
// The space is locked when it starts again: the status says the change is
// not sent and needs an unlock — only then are the keys there to seal it —
// and the unlock sends it without any further edit.
func TestAChangeLeftOutOfTheOutboxIsPreparedAtTheNextUnlock(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	created, err := a.manager.Create(space.CreateRequest{Name: "before", Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	a.syncNow(t, id)
	// With sync off the change is saved and nothing is sealed: on disk, the
	// state a crash between the two leaves.
	a.manager.SetSync(nil, "", nil)
	if _, err := a.manager.Rename(id, "after"); err != nil {
		t.Fatal(err)
	}
	a.manager.Close()

	restarted := newManager(t, a.home)
	store, err := devicesync.NewFolderStore(folder)
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetSync(store, "dev_laptopa", nil)
	status := statusOf(t, restarted, id)
	if status.State != space.SyncLocked || status.Delivery != space.DeliveryUnsent || !status.NeedsUnlock ||
		status.OutboxRevision+1 != status.LocalRevision {
		t.Fatalf("status after the restart = %+v, want unsent and needing an unlock", status)
	}
	local := status.LocalRevision

	if err := restarted.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the change in the folder", func() bool {
		blob := folderFile(t, folder, "dev_laptopa", id)
		return blob != nil && openOwn(t, blob, "dev_laptopa", id).Name == "after"
	})
	status = statusOf(t, restarted, id)
	if status.Delivery != space.DeliveryUploadUnknown || status.NeedsUnlock || status.LocalRevision != local {
		t.Fatalf("status after the unlock = %+v, want the change written at revision %d", status, local)
	}
}

// A send that fails leaves the outbox where it is, says so, and is sent by a
// later attempt — with the space locked by then.
func TestAFailedSendStaysQueuedAndIsSentLater(t *testing.T) {
	t.Parallel()

	manager, store, home, id := faultyMachineAt(t)
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	broken := errors.New("the folder is full")
	store.set(func(f *faultyStore) { f.putErr = broken })
	if _, err := manager.Rename(id, "not sent yet"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.SyncNow(t.Context(), id)
	if !errors.Is(err, broken) || status.Delivery != space.DeliveryUnsent || status.State != space.SyncError ||
		!strings.Contains(status.SendError, "the folder is full") || status.WrittenRevision == status.LocalRevision {
		t.Fatalf("status %+v, error %v; want the change reported unsent", status, err)
	}
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if status := statusOf(t, manager, id); status.Delivery != space.DeliveryUnsent || status.NeedsUnlock {
		t.Fatalf("locked status %+v, want unsent and ready to send without an unlock", status)
	}

	store.set(func(f *faultyStore) { f.putErr = nil })
	status, err = manager.SyncNow(t.Context(), id)
	if err != nil || status.Delivery != space.DeliveryUploadUnknown || status.SendError != "" {
		t.Fatalf("status %+v, error %v; want the queued change written", status, err)
	}
	store.mu.Lock()
	sent := store.files[syncKeysFor(t).SpaceTag()+"/"+ownTag(t, "dev_faulty", id)]
	store.mu.Unlock()
	if outbox := outboxOf(t, home, id); outbox == nil || !bytes.Equal(sent, outbox) {
		t.Fatal("the folder does not hold the outbox")
	}
}

// At start, every outbox the folder does not hold is sent — a locked
// space's included, with no key. And one the folder holds already is not
// sent again on every start.
func TestALockedSpaceSendsItsOutboxAtStart(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	first := newManager(t, home)
	store := newFaultyStore()
	store.set(func(f *faultyStore) { f.putErr = errors.New("offline") })
	first.SetSync(store, "dev_faulty", nil)
	created, err := first.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	if err := first.Lock(id); err != nil {
		t.Fatal(err)
	}
	first.Close()
	outbox := outboxOf(t, home, id)
	if outbox == nil {
		t.Fatal("no outbox was prepared")
	}
	key := syncKeysFor(t).SpaceTag() + "/" + ownTag(t, "dev_faulty", id)
	store.set(func(f *faultyStore) { f.putErr = nil })

	second := newManager(t, home)
	second.SetSync(store, "dev_faulty", nil)
	waitFor(t, "the outbox in the folder", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return bytes.Equal(store.files[key], outbox)
	})
	if status := statusOf(t, second, id); status.State != space.SyncLocked || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status = %+v, want the locked space's outbox written", status)
	}
	second.Close()

	store.mu.Lock()
	puts := store.puts
	store.mu.Unlock()
	third := newManager(t, home)
	third.SetSync(store, "dev_faulty", nil)
	// Runs after, or alongside, the start's own send; neither writes.
	if _, err := third.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.puts != puts {
		t.Fatalf("a start rewrote a file the folder holds %d time(s)", store.puts-puts)
	}
}

// An outbox sealed under another device id — the installation was copied,
// and the copy got an id of its own — is not this machine's to send: it
// would write the other machine's file. It is dropped, and the next unlock
// seals the space under this machine's own tag.
func TestAnOutboxOfAnotherDeviceIsDiscarded(t *testing.T) {
	t.Parallel()

	for _, lockedAtStart := range []bool{true, false} {
		name := "unlocked at start"
		if lockedAtStart {
			name = "locked at start"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			first := newManager(t, home)
			store := newFaultyStore()
			first.SetSync(store, "dev_before", nil)
			created, err := first.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
			if err != nil {
				t.Fatal(err)
			}
			id := created.Space.ID
			if _, err := first.SyncNow(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			first.Close()
			tag := syncKeysFor(t).SpaceTag()
			before, after := tag+"/"+ownTag(t, "dev_before", id), tag+"/"+ownTag(t, "dev_after", id)
			store.mu.Lock()
			original := store.files[before]
			store.mu.Unlock()

			second := newManager(t, home)
			if !lockedAtStart {
				if err := second.Unlock(id, syncPassword); err != nil {
					t.Fatal(err)
				}
			}
			second.SetSync(store, "dev_after", nil)
			if lockedAtStart {
				waitFor(t, "the outbox dropped", func() bool { return outboxOf(t, home, id) == nil })
				if status := statusOf(t, second, id); status.Delivery != space.DeliveryUnsent || !status.NeedsUnlock {
					t.Fatalf("status = %+v, want unsent and needing an unlock", status)
				}
				if err := second.Unlock(id, syncPassword); err != nil {
					t.Fatal(err)
				}
			}
			waitFor(t, "this machine's own file", func() bool {
				store.mu.Lock()
				defer store.mu.Unlock()
				return store.files[after] != nil
			})
			if _, deviceTag, ok := devicesync.EnvelopeTags(outboxOf(t, home, id)); !ok || deviceTag != ownTag(t, "dev_after", id) {
				t.Fatalf("the outbox is sealed for %q, want this machine's tag", deviceTag)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if !bytes.Equal(store.files[before], original) {
				t.Fatal("the other device's file was written")
			}
		})
	}
}

// What the folder lost is sent again, though nothing changed here: the
// folder is compared with the outbox, not with a memory of having written
// it. A deleted file is sent again at once, open or locked — there is
// nothing in the folder to lose. A file rolled back to an older version is
// another version of this machine's file, and is merged before it is
// replaced: by the pass of an open space, or after the next unlock.
func TestAFileTheFolderLostIsSentAgain(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	created, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	a.syncNow(t, id)
	path := filepath.Join(folder, "v1", syncKeysFor(t).SpaceTag(), ownTag(t, "dev_laptopa", id)+".wsync")
	older, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.manager.Rename(id, "newer"); err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, id)

	check := func(what string) {
		t.Helper()
		status := a.syncNow(t, id)
		if got := folderFile(t, folder, "dev_laptopa", id); !bytes.Equal(got, outboxOf(t, a.home, id)) {
			t.Fatalf("%s: the folder does not hold the outbox", what)
		}
		if status.Delivery != space.DeliveryUploadUnknown {
			t.Fatalf("%s: status = %+v", what, status)
		}
	}
	if err := os.WriteFile(path, older, 0o600); err != nil {
		t.Fatal(err)
	}
	check("rolled back")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check("deleted")

	if err := a.manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	check("deleted while locked")

	if err := os.WriteFile(path, older, 0o600); err != nil {
		t.Fatal(err)
	}
	status := a.syncNow(t, id)
	if got := folderFile(t, folder, "dev_laptopa", id); !bytes.Equal(got, older) {
		t.Fatal("rolled back while locked: the file was replaced before it was merged")
	}
	if status.Delivery != space.DeliveryUnsent || !status.NeedsUnlock || !status.MergeFirst {
		t.Fatalf("rolled back while locked: status = %+v, want unsent until an unlock merges it", status)
	}
	if err := a.manager.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	check("rolled back, after the unlock")
}

// The revision counts changes: every save of one, a merge's included, and
// nothing else — not a pass that changes nothing, not a new password. A file
// from before it reads as revision 0.
func TestTheRevisionCountsChangesOnly(t *testing.T) {
	t.Parallel()

	manager, store, home, id := faultyMachineAt(t)
	revision := func() uint64 {
		t.Helper()
		onDisk := spaceFile(t, home, id).LocalRevision
		if reported := statusOf(t, manager, id).LocalRevision; reported != onDisk {
			t.Fatalf("status reports revision %d, the file holds %d", reported, onDisk)
		}
		return onDisk
	}
	if got := revision(); got != 1 {
		t.Fatalf("revision after creating = %d, want 1", got)
	}
	if _, err := manager.Rename(id, "renamed"); err != nil {
		t.Fatal(err)
	}
	if got := revision(); got != 2 {
		t.Fatalf("revision after a rename = %d, want 2", got)
	}
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := manager.ChangePassword(id, syncPassword, syncPassword+" changed"); err != nil {
		t.Fatal(err)
	}
	if got := revision(); got != 2 {
		t.Fatalf("revision after a pass and a password change = %d, want 2", got)
	}
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: time.Now().UTC(),
		Name: "from the other machine", NameUpdatedAt: time.Now().UTC().Add(time.Second),
	})
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := revision(); got != 3 {
		t.Fatalf("revision after a merge brought a change = %d, want 3", got)
	}
	if status := statusOf(t, manager, id); status.OutboxRevision != 3 || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status = %+v, want the merged change in the outbox and the folder", status)
	}

	// The same file without the field: a space saved before revisions.
	manager.Close()
	path := filepath.Join(home, "spaces", id, "space.json")
	var fields map[string]json.RawMessage
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "local_revision")
	if err := os.WriteFile(path, encode(t, fields), 0o600); err != nil {
		t.Fatal(err)
	}
	old := newManager(t, home)
	if _, err := old.Rename(id, "after the upgrade"); err != nil {
		t.Fatal(err)
	}
	if got := spaceFile(t, home, id).LocalRevision; got != 1 {
		t.Fatalf("revision of an old file after one change = %d, want 1", got)
	}
}

// downloadStore is a faultyStore whose provider can be asked to download
// files.
type downloadStore struct {
	*faultyStore
	dmu sync.Mutex
	// answer is what Download returns; block, when set, holds it first.
	answer  error
	block   chan struct{}
	asked   []string
	failGet map[string]bool
}

func (d *downloadStore) Download(ctx context.Context, _, deviceTag string) error {
	d.dmu.Lock()
	d.asked = append(d.asked, deviceTag)
	answer, block := d.answer, d.block
	d.dmu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return answer
}

func (d *downloadStore) Get(ctx context.Context, spaceTag, deviceTag string) ([]byte, error) {
	d.dmu.Lock()
	fail := d.failGet[deviceTag]
	d.dmu.Unlock()
	if fail {
		return nil, errors.New("the provider could not fetch the file")
	}
	return d.faultyStore.Get(ctx, spaceTag, deviceTag)
}

func (d *downloadStore) askedFor(tag string) int {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	n := 0
	for _, asked := range d.asked {
		if asked == tag {
			n++
		}
	}
	return n
}

func (d *downloadStore) setAnswer(err error) {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	d.answer = err
}

// downloadMachine is a space syncing through a downloadStore that holds one
// other machine's file, not downloaded.
func downloadMachine(t *testing.T, answer error) (*space.Manager, *downloadStore, string, string) {
	t.Helper()
	manager := newManager(t, t.TempDir())
	store := &downloadStore{faultyStore: newFaultyStore(), answer: answer, failGet: map[string]bool{}}
	manager.SetSync(store, "dev_faulty", nil)
	created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	tag := syncKeysFor(t).SpaceTag()
	other := strings.Repeat("e", 32)
	store.set(func(f *faultyStore) {
		f.files[tag+"/"+other] = nil
		f.evicted[tag+"/"+other] = true
	})
	return manager, store, id, other
}

// A file another machine wrote that is not downloaded is asked for, and
// shown as downloading — or, where the provider cannot be asked or keeps
// refusing, as missing, for the person to keep the folder downloaded. A
// request never fails a pass. This machine's own evicted file is pending and
// asked for like any other: it is merged before it is replaced.
func TestFilesNotDownloadedAreAskedForAndReported(t *testing.T) {
	t.Parallel()

	t.Run("asked for", func(t *testing.T) {
		t.Parallel()
		manager, store, id, other := downloadMachine(t, nil)
		status, err := manager.SyncNow(t.Context(), id)
		if err != nil || status.State != space.SyncOK || status.Pending != 1 || status.Missing != 0 {
			t.Fatalf("status %+v, error %v; want one file downloading", status, err)
		}
		waitFor(t, "a download request", func() bool { return store.askedFor(other) > 0 })
	})

	t.Run("this machine's own file", func(t *testing.T) {
		t.Parallel()
		manager, store, id, _ := downloadMachine(t, nil)
		mine := ownTag(t, "dev_faulty", id)
		store.set(func(f *faultyStore) { f.evicted[syncKeysFor(t).SpaceTag()+"/"+mine] = true })
		status, err := manager.SyncNow(t.Context(), id)
		if err != nil || status.Pending != 2 {
			t.Fatalf("status %+v, error %v; want this machine's file pending too", status, err)
		}
		waitFor(t, "a download request for this machine's file", func() bool { return store.askedFor(mine) > 0 })
	})

	t.Run("the provider cannot be asked", func(t *testing.T) {
		t.Parallel()
		manager, store, id, other := downloadMachine(t, devicesync.ErrDownloadUnsupported)
		if _, err := manager.SyncNow(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the file reported missing", func() bool { return statusOf(t, manager, id).Missing == 1 })
		asked := store.askedFor(other)
		status, err := manager.SyncNow(t.Context(), id)
		if err != nil || status.Missing != 1 || status.Pending != 1 {
			t.Fatalf("status %+v, error %v; want the file missing", status, err)
		}
		time.Sleep(50 * time.Millisecond)
		if store.askedFor(other) != asked {
			t.Fatal("a provider that cannot be asked was asked again")
		}
	})

	t.Run("no way to ask", func(t *testing.T) {
		t.Parallel()
		manager, store, id := faultyMachine(t)
		tag := syncKeysFor(t).SpaceTag()
		store.set(func(f *faultyStore) {
			f.files[tag+"/"+strings.Repeat("e", 32)] = nil
			f.evicted[tag+"/"+strings.Repeat("e", 32)] = true
		})
		status, err := manager.SyncNow(t.Context(), id)
		if err != nil || status.Pending != 1 || status.Missing != 1 {
			t.Fatalf("status %+v, error %v; want the file missing", status, err)
		}
	})

	t.Run("requests that keep failing", func(t *testing.T) {
		t.Parallel()
		manager, store, id, other := downloadMachine(t, errors.New("brctl: no such item"))
		for attempt := 1; attempt <= 3; attempt++ {
			waitFor(t, "another request", func() bool {
				if store.askedFor(other) >= attempt {
					return true
				}
				status, err := manager.SyncNow(t.Context(), id)
				if err != nil || status.State != space.SyncOK {
					t.Fatalf("status %+v, error %v; a failed request failed the pass", status, err)
				}
				if attempt < 3 && status.Missing != 0 {
					t.Fatalf("missing after %d failed request(s): %+v", attempt-1, status)
				}
				return false
			})
		}
		waitFor(t, "the file reported missing", func() bool { return statusOf(t, manager, id).Missing == 1 })
		// A request taken resets the count.
		store.setAnswer(nil)
		waitFor(t, "the file downloading again", func() bool {
			if _, err := manager.SyncNow(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			return statusOf(t, manager, id).Missing == 0
		})
	})

	t.Run("dataless files", func(t *testing.T) {
		t.Parallel()
		manager, store, id := faultyMachine(t)
		keys := syncKeysFor(t)
		putRemote(t, store, keys, "dev_other", devicesync.Snapshot{Version: devicesync.SnapshotVersion, WrittenAt: time.Now().UTC()})
		remote := keys.DeviceTag("dev_other", "spc_other")
		downloads := &downloadStore{faultyStore: store, failGet: map[string]bool{}}
		manager.SetSync(downloads, "dev_faulty", nil)
		store.set(func(f *faultyStore) { f.dataless[keys.SpaceTag()+"/"+remote] = true })
		// Read: reading is what fetches it.
		status, err := manager.SyncNow(t.Context(), id)
		if err != nil || status.Devices != 2 || status.Pending != 0 {
			t.Fatalf("status %+v, error %v; want the dataless file read", status, err)
		}
		// Not fetched: pending, and asked for.
		downloads.dmu.Lock()
		downloads.failGet[remote] = true
		downloads.dmu.Unlock()
		status, err = manager.SyncNow(t.Context(), id)
		if err != nil || status.Pending != 1 || status.Unreadable != 0 {
			t.Fatalf("status %+v, error %v; want the file pending", status, err)
		}
		waitFor(t, "a download request", func() bool { return downloads.askedFor(remote) > 0 })
	})
}

// A request may take seconds; the unlock that found the file does not wait
// for it.
func TestADownloadRequestDoesNotHoldUpAnUnlock(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	store := &downloadStore{faultyStore: newFaultyStore(), block: release, failGet: map[string]bool{}}
	tag := syncKeysFor(t).SpaceTag()
	other := strings.Repeat("e", 32)
	store.set(func(f *faultyStore) {
		f.files[tag+"/"+other] = nil
		f.evicted[tag+"/"+other] = true
	})
	manager.SetSync(store, "dev_faulty", nil)
	started := time.Now()
	if _, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("the restore took %v: it waited for a download request", elapsed)
	}
	waitFor(t, "a download request", func() bool { return store.askedFor(other) > 0 })
}

// A space's tag is kept in its sync.json at every unlock and creation, sync
// on or off: a locked space is then still recognised — by the overview, and
// when another space is given a phrase that this one already has.
func TestASpaceRemembersItsTagWithSyncOff(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	path := filepath.Join(home, "spaces", id, "sync.json")
	tagOnDisk := func() string {
		t.Helper()
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		var cache struct {
			SpaceTag string `json:"space_tag"`
		}
		if err := json.Unmarshal(data, &cache); err != nil {
			t.Fatal(err)
		}
		return cache.SpaceTag
	}
	want := syncKeysFor(t).SpaceTag()
	if got := tagOnDisk(); got != want {
		t.Fatalf("tag after creating = %q, want %q", got, want)
	}
	manager.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	restarted := newManager(t, home)
	if err := restarted.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Lock(id); err != nil {
		t.Fatal(err)
	}
	if got := tagOnDisk(); got != want {
		t.Fatalf("tag after an unlock with sync off = %q, want %q", got, want)
	}
}

// Quitting sends what is not in the folder yet, if the folder answers at
// once — a change made a moment before quitting, before the debounced send —
// and does not wait for a folder that does not answer.
func TestQuittingSendsWhatIsLeftButDoesNotWaitForTheFolder(t *testing.T) {
	t.Parallel()

	t.Run("the folder answers", func(t *testing.T) {
		t.Parallel()
		manager, store, home, id := faultyMachineAt(t)
		if _, err := manager.SyncNow(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Rename(id, "just before quitting"); err != nil {
			t.Fatal(err)
		}
		manager.Close()
		store.mu.Lock()
		sent := store.files[syncKeysFor(t).SpaceTag()+"/"+ownTag(t, "dev_faulty", id)]
		store.mu.Unlock()
		if !bytes.Equal(sent, outboxOf(t, home, id)) {
			t.Fatal("the last change did not leave when the wallet quit")
		}
	})

	t.Run("the folder does not answer", func(t *testing.T) {
		t.Parallel()
		manager := newManager(t, t.TempDir())
		if _, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword}); err != nil {
			t.Fatal(err)
		}
		store := hangingStore{faultyStore: newFaultyStore(), entered: make(chan struct{}, 1)}
		manager.SetSync(store, "dev_hanging", nil)
		<-store.entered
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			manager.Close()
		}()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("quitting waited for a folder that does not answer")
		}
	})
}

// A folder found not to hold what was recorded as written — replaced by
// something else — is not reported as holding the change, and what it holds
// instead is not written over while the space is locked.
func TestAFileTheFolderLostIsNotReportedWrittenWhenItCannotBeSentAgain(t *testing.T) {
	t.Parallel()

	manager, store, _, id := faultyMachineAt(t)
	if status, err := manager.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status %+v, error %v; want written", status, err)
	}
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	key := syncKeysFor(t).SpaceTag() + "/" + ownTag(t, "dev_faulty", id)
	store.set(func(f *faultyStore) { f.files[key] = []byte("an older version the provider restored") })
	store.mu.Lock()
	puts := store.puts
	store.mu.Unlock()
	if status, err := manager.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUnsent ||
		status.WrittenRevision != 0 || !status.NeedsUnlock {
		t.Fatalf("status %+v, error %v; want unsent", status, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.puts != puts {
		t.Fatal("the folder's file was written over while the space was locked")
	}
}

// A space with no recovery phrase has nothing to deliver, and no unlock
// changes that: locked, it says it has no phrase, not that it waits for an
// unlock — also after a restart, before it is opened again. Given a phrase,
// it delivers like any other.
func TestASpaceWithoutAPhraseHasNoDelivery(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	if err := space.WriteSpaceWithoutPhrase(home, phraselessID, syncPassword, fastKDF); err != nil {
		t.Fatal(err)
	}
	manager := newManager(t, home)
	manager.SetSync(newFaultyStore(), "dev_faulty", nil)
	if err := manager.Unlock(phraselessID, syncPassword); err != nil {
		t.Fatal(err)
	}
	none := func(what string, status space.SyncStatus) {
		t.Helper()
		if status.State != space.SyncNoPhrase || status.Delivery != "" || status.NeedsUnlock {
			t.Fatalf("%s: status = %+v, want no phrase and no delivery", what, status)
		}
	}
	none("open", statusOf(t, manager, phraselessID))
	if err := manager.Lock(phraselessID); err != nil {
		t.Fatal(err)
	}
	none("locked", statusOf(t, manager, phraselessID))
	manager.Close()

	restarted := newManager(t, home)
	store := newFaultyStore()
	restarted.SetSync(store, "dev_faulty", nil)
	none("locked after a restart", statusOf(t, restarted, phraselessID))

	if err := restarted.Unlock(phraselessID, syncPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.AddRecoveryPhrase(phraselessID, syncPassword, ""); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Lock(phraselessID); err != nil {
		t.Fatal(err)
	}
	if status := statusOf(t, restarted, phraselessID); status.State == space.SyncNoPhrase || status.Delivery == "" {
		t.Fatalf("status after a phrase was added = %+v, want a delivery state", status)
	}
}

// What an unlock or a restore waits for is reported: whether the first pass
// finished, and the files it is still missing — so that a restore waiting
// for files is not shown as complete.
func TestTheFirstSyncIsReported(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	if created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword}); err != nil ||
		created.Sync != nil {
		t.Fatalf("Create() with sync off = %+v, %v; want no sync summary", created.Sync, err)
	}

	manager, store, id := faultyMachine(t)
	tag := syncKeysFor(t).SpaceTag()
	store.set(func(f *faultyStore) {
		f.files[tag+"/"+strings.Repeat("e", 32)] = nil
		f.evicted[tag+"/"+strings.Repeat("e", 32)] = true
	})
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	first, err := manager.UnlockAndSync(id, syncPassword)
	if err != nil || first == nil || !first.Finished || first.Pending != 1 || first.Missing != 1 {
		t.Fatalf("UnlockAndSync() = %+v, %v; want finished with one file missing", first, err)
	}

	restored := newManager(t, t.TempDir())
	restored.SetSync(store, "dev_restored", nil)
	created, err := restored.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil || created.Sync == nil || !created.Sync.Finished || created.Sync.Pending != 1 {
		t.Fatalf("Create() = %+v, %v; want the restore's pending file reported", created.Sync, err)
	}

	store.set(func(f *faultyStore) { f.listErr = errors.New("folder went away") })
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if first, err := manager.UnlockAndSync(id, syncPassword); err != nil || first == nil || first.Finished {
		t.Fatalf("UnlockAndSync() = %+v, %v; want a pass that failed reported unfinished", first, err)
	}
}

// A new device's first seal starts its delivery record over. Kept, the old
// device's record of a write to this folder would show the new device's
// change as written while its own file has never been sent.
func TestANewDeviceDoesNotInheritTheOldDevicesWrite(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	first := newManager(t, home)
	store := newFaultyStore()
	first.SetSync(store, "dev_before", nil)
	created, err := first.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	if status, err := first.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status %+v, error %v; want written", status, err)
	}
	first.Close()

	second := newManager(t, home)
	if err := second.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	// The folder does not answer: the new device seals, and sends nothing.
	store.set(func(f *faultyStore) { f.listErr = errors.New("folder went away") })
	second.SetSync(store, "dev_after", nil)
	if _, err := second.SyncNow(t.Context(), id); err == nil {
		t.Fatal("SyncNow() error = nil, want the folder's")
	}
	if status := statusOf(t, second, id); status.Delivery != space.DeliveryUnsent || status.WrittenRevision != 0 {
		t.Fatalf("status = %+v, want the new device's file unsent", status)
	}
}

// rootedStore is a faultyStore with a folder path of its own.
type rootedStore struct {
	*faultyStore
	root string
}

func (r rootedStore) Root() string { return r.root }

// This machine's file listed but not downloaded in a new folder — a copy of
// the old one, say — is not taken for written there on the strength of the
// old folder's record, and is not written over before it can be merged.
func TestAnEvictedFileInANewFolderIsNotTakenForWritten(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	manager.SetSync(rootedStore{newFaultyStore(), "/old"}, "dev_faulty", nil)
	created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	if status, err := manager.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status %+v, error %v; want written", status, err)
	}
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	moved := rootedStore{newFaultyStore(), "/new"}
	key := syncKeysFor(t).SpaceTag() + "/" + ownTag(t, "dev_faulty", id)
	moved.set(func(f *faultyStore) {
		f.files[key] = nil
		f.evicted[key] = true
	})
	space.SetSyncQuiet(manager, moved, "dev_faulty")
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if status := statusOf(t, manager, id); status.Delivery != space.DeliveryUnsent || !status.NeedsUnlock {
		t.Fatalf("status = %+v, want unsent until the file can be merged", status)
	}
	moved.mu.Lock()
	defer moved.mu.Unlock()
	if moved.puts != 0 || moved.files[key] != nil {
		t.Fatal("an evicted file was written over")
	}
	_ = home
}

// A phrase given to a space while sync is off is remembered too: turned on
// later, with the space locked, sync shows the space's delivery — not "no
// phrase" from before the phrase was added.
func TestAPhraseAddedWithSyncOffEndsNoPhrase(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	if err := space.WriteSpaceWithoutPhrase(home, phraselessID, syncPassword, fastKDF); err != nil {
		t.Fatal(err)
	}
	manager := newManager(t, home)
	if err := manager.Unlock(phraselessID, syncPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AddRecoveryPhrase(phraselessID, syncPassword, ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.Lock(phraselessID); err != nil {
		t.Fatal(err)
	}
	space.SetSyncQuiet(manager, newFaultyStore(), "dev_faulty")
	if status := statusOf(t, manager, phraselessID); status.State == space.SyncNoPhrase || status.Delivery != space.DeliveryUnsent ||
		!status.NeedsUnlock {
		t.Fatalf("status = %+v, want unsent and needing an unlock", status)
	}
}

// The data folder is restored from last night's backup on the same machine:
// same device id, so the same file in the folder — and that file was written
// today, after the backup, with a key imported today. The outbox from the
// backup holds last night. It must not replace today's file: locked, the
// file is left as it is; the unlock merges it, and the key comes back into
// the space and stays in the folder.
func TestAFileFromBeforeABackupWasRestoredIsMergedNotOverwritten(t *testing.T) {
	t.Parallel()

	folder, home, backup := t.TempDir(), filepath.Join(t.TempDir(), "home"), filepath.Join(t.TempDir(), "backup")
	open := func() *space.Manager {
		t.Helper()
		manager := newManager(t, home)
		store, err := devicesync.NewFolderStore(folder)
		if err != nil {
			t.Fatal(err)
		}
		manager.SetSync(store, "dev_laptopa", nil)
		return manager
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	night := open()
	created, err := night.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	if _, err := night.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	night.Close()
	if err := os.CopyFS(backup, os.DirFS(home)); err != nil {
		t.Fatal(err)
	}

	day := open()
	if err := day.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	imported, err := day.Import(id, "eth-sepolia", account.FamilyEVM, "today's key", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := day.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status %+v, error %v; want today's key written", status, err)
	}
	day.Close()
	today := folderFile(t, folder, "dev_laptopa", id)
	holdsKey := func(blob []byte) bool {
		return blob != nil && slices.ContainsFunc(openOwn(t, blob, "dev_laptopa", id).Accounts,
			func(item account.Account) bool { return item.ID == imported.Account.ID })
	}
	if !holdsKey(today) {
		t.Fatal("the case needs today's key in the folder")
	}

	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(home, os.DirFS(backup)); err != nil {
		t.Fatal(err)
	}
	restored := open()
	// Runs after, or alongside, the start's own send: neither may replace
	// the file.
	status, err := restored.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(folderFile(t, folder, "dev_laptopa", id), today) {
		t.Fatal("locked, the restored outbox replaced today's file")
	}
	if status.Delivery != space.DeliveryUnsent || !status.NeedsUnlock || !status.MergeFirst {
		t.Fatalf("locked status = %+v, want unsent until an unlock merges the folder's file", status)
	}

	if err := restored.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	if status, err := restored.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status %+v, error %v; want written after the merge", status, err)
	}
	if got := accountIDs(t, restored, id); !slices.Contains(got, imported.Account.ID) {
		t.Fatalf("wallets = %v, want today's key merged back", got)
	}
	if !holdsKey(folderFile(t, folder, "dev_laptopa", id)) {
		t.Fatal("today's key is gone from the folder")
	}
}

// An open space reads its own file like any other — a dataless one too: the
// read fetches it — and adopts what it holds. One it cannot read is not
// written over.
func TestAnOpenSpaceMergesItsOwnFileBeforeReplacingIt(t *testing.T) {
	t.Parallel()

	manager, store, _, id := quietFaultyMachine(t)
	keys := syncKeysFor(t)
	key := keys.SpaceTag() + "/" + ownTag(t, "dev_faulty", id)
	at := time.Now().UTC()
	record, variant := importedRecord(t, "acc_fromthefolder", "key_fromthefolder", bundleImportedKey, at)
	blob, err := devicesync.Seal(keys, keys.SpaceTag(), ownTag(t, "dev_faulty", id), devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: at, Accounts: []account.Account{record},
		ImportedKeys: map[string][]devicesync.Key{"key_fromthefolder": {variant}},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("unreadable", func(t *testing.T) {
		downloads := &downloadStore{faultyStore: store, failGet: map[string]bool{ownTag(t, "dev_faulty", id): true}}
		space.SetSyncQuiet(manager, downloads, "dev_faulty")
		store.set(func(f *faultyStore) {
			f.files[key] = blob
			f.dataless[key] = true
		})
		store.mu.Lock()
		puts := store.puts
		store.mu.Unlock()
		if _, err := manager.Rename(id, "changed here"); err != nil {
			t.Fatal(err)
		}
		status, err := manager.SyncNow(t.Context(), id)
		if err != nil || status.Pending != 1 || !status.MergeFirst || status.Delivery != space.DeliveryUnsent {
			t.Fatalf("status %+v, error %v; want the file pending and kept", status, err)
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if store.puts != puts || !bytes.Equal(store.files[key], blob) {
			t.Fatal("a file that could not be read was written over")
		}
	})

	t.Run("dataless", func(t *testing.T) {
		space.SetSyncQuiet(manager, store, "dev_faulty")
		if status, err := manager.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUploadUnknown {
			t.Fatalf("status %+v, error %v; want merged and written", status, err)
		}
		if got := accountIDs(t, manager, id); !slices.Contains(got, record.ID) {
			t.Fatalf("wallets = %v, want the wallet from this machine's file", got)
		}
		store.mu.Lock()
		written := store.files[key]
		store.mu.Unlock()
		snap := openOwn(t, written, "dev_faulty", id)
		if snap.Name != "changed here" || !slices.ContainsFunc(snap.Accounts, func(a account.Account) bool { return a.ID == record.ID }) {
			t.Fatalf("the folder holds %q and %d wallets, want both the change and the wallet", snap.Name, len(snap.Accounts))
		}
	})
}

// Locked, a dataless own file is read — the read fetches it — to tell
// whether it is this machine's last write. It is, so a change made before
// locking still replaces it while the space is locked.
func TestALockedSpaceReadsItsDatalessFileBeforeReplacingIt(t *testing.T) {
	t.Parallel()

	manager, store, home, id := quietFaultyMachine(t)
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	key := syncKeysFor(t).SpaceTag() + "/" + ownTag(t, "dev_faulty", id)
	store.set(func(f *faultyStore) { f.dataless[key] = true })
	if _, err := manager.Rename(id, "made before locking"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status %+v, error %v; want the change written", status, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !bytes.Equal(store.files[key], outboxOf(t, home, id)) {
		t.Fatal("the folder does not hold the change")
	}
}

// sealUnchecked seals plaintext as this build would seal a file for device in
// the test phrase's space, without checking it first: a file a later
// release could write, which this build authenticates and does not read. The
// keys are derived here as devicesync derives them; the check at the end
// shows they are the same.
func sealUnchecked(t *testing.T, deviceTag string, plaintext []byte) []byte {
	t.Helper()
	seed, err := account.Seed(syncPhrase, "")
	if err != nil {
		t.Fatal(err)
	}
	root, err := hkdf.Key(sha256.New, seed, nil, "walletspace/sync/v1/root", 32)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := hkdf.Key(sha256.New, root, nil, "enc", 32)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := chacha20poly1305.NewX(enc)
	if err != nil {
		t.Fatal(err)
	}
	keys := syncKeysFor(t)
	head := append(append([]byte("WSYN"), devicesync.EnvelopeVersion), keys.SpaceTag()+deviceTag...)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	blob := aead.Seal(append(slices.Clone(head), nonce...), nonce, plaintext, head)
	if _, err := devicesync.Open(keys, keys.SpaceTag(), deviceTag, blob); !errors.Is(err, devicesync.ErrInvalidSnapshot) {
		t.Fatalf("Open(unchecked file) error = %v, want it authenticated and refused", err)
	}
	return blob
}

// This machine's file written by a newer Walletspace — a snapshot version
// this build does not read, after the data folder was restored with an older
// app — holds what this build cannot merge: a key imported on the newer
// release. It is never written over, open or locked, and the status says to
// update Walletspace rather than to unlock.
func TestAFileFromANewerWalletspaceIsNeverOverwritten(t *testing.T) {
	t.Parallel()

	manager, store, _, id := quietFaultyMachine(t)
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	record, variant := importedRecord(t, "acc_fromlater", "key_fromlater", bundleImportedKey, time.Now().UTC())
	plaintext, err := json.Marshal(devicesync.Snapshot{
		Version: devicesync.SnapshotVersion + 1, WrittenAt: time.Now().UTC(), Accounts: []account.Account{record},
		ImportedKeys: map[string][]devicesync.Key{"key_fromlater": {variant}},
	})
	if err != nil {
		t.Fatal(err)
	}
	newer := sealUnchecked(t, ownTag(t, "dev_faulty", id), plaintext)
	key := syncKeysFor(t).SpaceTag() + "/" + ownTag(t, "dev_faulty", id)
	store.set(func(f *faultyStore) { f.files[key] = newer })
	kept := func(what string, reason string, needsUnlock bool) {
		t.Helper()
		status, err := manager.SyncNow(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		held := store.files[key]
		store.mu.Unlock()
		if !bytes.Equal(held, newer) {
			t.Fatalf("%s: the newer Walletspace's file was written over", what)
		}
		if status.Delivery != space.DeliveryUnsent || status.MergeFirstReason != reason || status.NeedsUnlock != needsUnlock {
			t.Fatalf("%s: status = %+v, want unsent, %s", what, status, reason)
		}
	}

	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	kept("locked", space.MergeFirstNotMerged, true)
	if err := manager.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	kept("unlocked", space.MergeFirstNewerVersion, false)
	if _, err := manager.Rename(id, "changed here"); err != nil {
		t.Fatal(err)
	}
	kept("after a change", space.MergeFirstNewerVersion, false)
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	kept("locked again", space.MergeFirstNewerVersion, false)
}

// This machine's file damaged in the folder — it does not authenticate —
// holds nothing any build can recover. Locked, it is left as it is like any
// file that is not this machine's last write; the unlock finds it damaged
// and replaces it.
func TestADamagedOwnFileIsReplacedAfterAnUnlock(t *testing.T) {
	t.Parallel()

	manager, store, home, id := quietFaultyMachine(t)
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	key := syncKeysFor(t).SpaceTag() + "/" + ownTag(t, "dev_faulty", id)
	store.mu.Lock()
	damaged := slices.Clone(store.files[key])
	damaged[len(damaged)-1] ^= 0xff
	store.files[key] = damaged
	store.mu.Unlock()

	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.SyncNow(t.Context(), id); err != nil || !status.MergeFirst || !status.NeedsUnlock {
		t.Fatalf("locked: status %+v, error %v; want the file kept until an unlock", status, err)
	}
	if err := manager.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	status, err := manager.SyncNow(t.Context(), id)
	if err != nil || status.Delivery != space.DeliveryUploadUnknown || status.MergeFirst {
		t.Fatalf("unlocked: status %+v, error %v; want the damaged file replaced", status, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !bytes.Equal(store.files[key], outboxOf(t, home, id)) {
		t.Fatal("the damaged file was not replaced")
	}
}
