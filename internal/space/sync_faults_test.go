package space_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/space"
)

// faultyStore is a store in memory whose operations can be made to fail.
type faultyStore struct {
	mu       sync.Mutex
	files    map[string][]byte
	evicted  map[string]bool
	dataless map[string]bool
	listErr  error
	getErr   error
	putErr   error
	puts     int
}

func newFaultyStore() *faultyStore {
	return &faultyStore{files: map[string][]byte{}, evicted: map[string]bool{}, dataless: map[string]bool{}}
}

func (f *faultyStore) Spaces(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	seen := map[string]bool{}
	for key := range f.files {
		seen[strings.Split(key, "/")[0]] = true
	}
	var out []string
	for tag := range seen {
		out = append(out, tag)
	}
	return out, nil
}

func (f *faultyStore) List(_ context.Context, spaceTag string) ([]devicesync.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []devicesync.Entry
	for key := range f.files {
		if tag, device, ok := strings.Cut(key, "/"); ok && tag == spaceTag {
			out = append(out, devicesync.Entry{DeviceTag: device, Evicted: f.evicted[key], Dataless: f.dataless[key]})
		}
	}
	return out, nil
}

func (f *faultyStore) Get(_ context.Context, spaceTag, deviceTag string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	data, ok := f.files[spaceTag+"/"+deviceTag]
	if !ok {
		return nil, devicesync.ErrNotDownloaded
	}
	return data, nil
}

func (f *faultyStore) Put(_ context.Context, spaceTag, deviceTag string, blob []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.puts++
	f.files[spaceTag+"/"+deviceTag] = blob
	return nil
}

func (f *faultyStore) set(fn func(*faultyStore)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func syncKeysFor(t *testing.T) *devicesync.Keys {
	t.Helper()
	seed, err := account.Seed(syncPhrase, "")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := devicesync.DeriveKeys(seed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(keys.Clear)
	return keys
}

func faultyMachine(t *testing.T) (*space.Manager, *faultyStore, string) {
	t.Helper()
	manager := newManager(t, t.TempDir())
	store := newFaultyStore()
	manager.SetSync(store, "dev_faulty", nil)
	created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, created.Space.ID
}

func TestAFolderThatFailsIsReportedNotFatal(t *testing.T) {
	t.Parallel()

	manager, store, id := faultyMachine(t)
	broken := errors.New("folder went away")

	store.set(func(f *faultyStore) { f.listErr = broken })
	status, err := manager.SyncNow(t.Context(), id)
	if !errors.Is(err, broken) || status.State != space.SyncError || !strings.Contains(status.Error, "folder went away") {
		t.Fatalf("list failure: status %+v, error %v", status, err)
	}
	if overview := manager.SyncOverview(t.Context()); !strings.Contains(overview.Error, "folder went away") {
		t.Errorf("overview error = %q", overview.Error)
	}

	store.set(func(f *faultyStore) { f.listErr = nil; f.putErr = broken })
	if _, err := manager.Derive(id, "tron-nile", account.FamilyTron, ""); err != nil {
		t.Fatalf("a failing folder blocked a local change: %v", err)
	}
	status, err = manager.SyncNow(t.Context(), id)
	if !errors.Is(err, broken) || status.State != space.SyncError {
		t.Fatalf("write failure: status %+v, error %v", status, err)
	}

	// Once the folder is back, the pass that failed to write writes.
	store.set(func(f *faultyStore) { f.putErr = nil })
	if status, err := manager.SyncNow(t.Context(), id); err != nil || status.State != space.SyncOK || status.LastWrite.IsZero() {
		t.Fatalf("after recovery: status %+v, error %v", status, err)
	}
}

func TestFilesNotHereYetAreCountedNotFailed(t *testing.T) {
	t.Parallel()

	manager, store, id := faultyMachine(t)
	keys := syncKeysFor(t)
	tag := keys.SpaceTag()
	store.set(func(f *faultyStore) {
		f.files[tag+"/"+strings.Repeat("e", 32)] = nil
		f.evicted[tag+"/"+strings.Repeat("e", 32)] = true
	})
	status, err := manager.SyncNow(t.Context(), id)
	if err != nil || status.Pending != 1 || status.State != space.SyncOK {
		t.Fatalf("evicted file: status %+v, error %v", status, err)
	}

	// Listed, but gone by the time it is read.
	store.set(func(f *faultyStore) {
		delete(f.evicted, tag+"/"+strings.Repeat("e", 32))
		f.getErr = devicesync.ErrNotDownloaded
	})
	status, err = manager.SyncNow(t.Context(), id)
	if err != nil || status.Pending == 0 {
		t.Fatalf("not downloaded: status %+v, error %v", status, err)
	}
	store.set(func(f *faultyStore) { f.getErr = errors.New("read failed") })
	status, err = manager.SyncNow(t.Context(), id)
	if err != nil || status.Unreadable == 0 {
		t.Fatalf("read failure: status %+v, error %v", status, err)
	}
}

// A key tombstone with no wallet tombstone beside it — another machine's
// data that does not hold together — still takes the key, and the wallet
// that can no longer sign goes with it rather than staying unverifiable.
func TestAWalletLosesItsDeletedKey(t *testing.T) {
	t.Parallel()

	// No background pass: the one asked for below is the one that meets the
	// tombstone, and reports the wallet it drops.
	manager, store, _, id := quietFaultyMachine(t)
	imported, err := manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	keys := syncKeysFor(t)
	now := time.Now().UTC()
	tomb := devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: now,
		DeletedKeys: []devicesync.Tombstone{{ID: imported.Account.KeyRef, DeletedAt: now}},
	}
	other := keys.DeviceTag("dev_other", "spc_other")
	blob, err := devicesync.Seal(keys, keys.SpaceTag(), other, tomb)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), keys.SpaceTag(), other, blob); err != nil {
		t.Fatal(err)
	}
	status, err := manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.Dropped, []string{imported.Account.ID}) {
		t.Errorf("Dropped = %v", status.Dropped)
	}
	if got := accountIDs(t, manager, id); len(got) != 0 {
		t.Fatalf("wallets = %v, want the key-less wallet gone", got)
	}
	if _, err := manager.ExportPrivateKey(id, imported.Account.ID, account.FamilyEVM, syncPassword); err == nil {
		t.Fatal("the tombstoned key is still in the vault")
	}
}

func TestAMergeOverTheWalletCeilingIsRefused(t *testing.T) {
	t.Parallel()

	manager, store, id := faultyMachine(t)
	keys := syncKeysFor(t)
	now := time.Now().UTC()
	snap := devicesync.Snapshot{Version: devicesync.SnapshotVersion, WrittenAt: now}
	// Genuine wallets, so verification keeps them all: one machine's worth
	// is within the ceiling, two machines' worth is not.
	for i := range devicesync.MaxAccounts {
		address, err := account.DerivedAddress(syncPhrase, "", account.FamilyTron, uint32(i))
		if err != nil {
			t.Fatal(err)
		}
		snap.Accounts = append(snap.Accounts, account.Account{
			ID: fmt.Sprintf("acc_remote%04d", i), Kind: account.KindDerived, Family: account.FamilyTron,
			NetworkIDs: []string{"tron-nile"}, Index: new(uint32(i)), DerivationProfile: "bip44-v1",
			Addresses: map[account.Family]string{account.FamilyTron: address}, CreatedAt: now, UpdatedAt: now,
		})
	}
	other := keys.DeviceTag("dev_other", "spc_other")
	blob, err := devicesync.Seal(keys, keys.SpaceTag(), other, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), keys.SpaceTag(), other, blob); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Derive(id, "tron", account.FamilyTron, "local"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.SyncNow(t.Context(), id)
	if !errors.Is(err, space.ErrQuotaExceeded) || status.State != space.SyncError {
		t.Fatalf("status %+v, error %v; want the quota refusal", status, err)
	}
	if got := accountIDs(t, manager, id); len(got) != 1 {
		t.Fatalf("a refused merge changed the space: %d wallets", len(got))
	}
}

func TestReplacingTheStoreStartsOver(t *testing.T) {
	t.Parallel()

	manager, first, id := faultyMachine(t)
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	second := newFaultyStore()
	manager.SetSync(second, "dev_faulty", nil)
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	// The new folder gets this machine's file even though nothing changed:
	// what was written to the old one says nothing about the new one.
	second.mu.Lock()
	puts := second.puts
	second.mu.Unlock()
	if puts == 0 {
		t.Fatal("the new store was never written")
	}
	_ = first
}

// putRemote seals a snapshot as another machine would and drops it in the
// store.
func putRemote(t *testing.T, store devicesync.Store, keys *devicesync.Keys, device string, snap devicesync.Snapshot) {
	t.Helper()
	tag := keys.DeviceTag(device, "spc_"+strings.TrimPrefix(device, "dev_"))
	blob, err := devicesync.Seal(keys, keys.SpaceTag(), tag, snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), keys.SpaceTag(), tag, blob); err != nil {
		t.Fatal(err)
	}
}

// importedRecord is the wallet record an import of hexKey under ref makes,
// with the key itself as a snapshot variant.
func importedRecord(t *testing.T, id, ref, hexKey string, at time.Time) (account.Account, devicesync.Key) {
	t.Helper()
	key, raw, err := account.ParsePrivateKey(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := account.ImportedAddresses(key)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := account.Fingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	return account.Account{
		ID: id, Kind: account.KindImported, NetworkIDs: []string{"eth-sepolia"},
		Addresses: addresses, KeyRef: ref, Fingerprint: fingerprint,
		ImportedAt: new(at), CreatedAt: at, UpdatedAt: at,
	}, devicesync.Key{
		Curve: devicesync.CurveSecp256k1, PrivateKey: raw, ImportedAt: at,
	}
}

// signerKey is the public key the space signs with for a wallet.
func signerKey(t *testing.T, manager *space.Manager, id, accountID string) []byte {
	t.Helper()
	var public []byte
	if err := manager.WithSigner(t.Context(), id, accountID, "eth-sepolia", account.FamilyEVM, func(signer chain.Signer) error {
		public = signer.PublicKey()
		return nil
	}); err != nil {
		t.Fatalf("WithSigner(%s) error = %v", accountID, err)
	}
	return public
}

// Another machine — a bug, or someone holding the phrase who can write to the
// folder — offers a different key under a key reference this machine has,
// with a wallet built on it. Keeping only one key per reference would lose a
// secret (the first sync build kept "its own", and on the other machine that
// was the other key); taking the other machine's would leave this wallet on
// a key that does not make its address. Both keys stay as variants of the
// reference, the reference is reported, and each wallet signs with its own.
func TestTwoKeysUnderOneReferenceAreBothKept(t *testing.T) {
	t.Parallel()

	const otherKey = "1111111111111111111111111111111111111111111111111111111111111111"
	manager, store, id := faultyMachine(t)
	mine, err := manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	ref := mine.Account.KeyRef
	keys := syncKeysFor(t)
	now := time.Now().UTC()
	theirs, variant := importedRecord(t, "acc_otherkey", ref, otherKey, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	putRemote(t, store, keys, "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: now,
		Accounts:     []account.Account{theirs},
		ImportedKeys: map[string][]devicesync.Key{ref: {variant}},
	})

	status, err := manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.KeyConflicts, []string{ref}) {
		t.Errorf("KeyConflicts = %v, want %s", status.KeyConflicts, ref)
	}
	if len(status.Dropped) != 0 {
		t.Errorf("Dropped = %v; both wallets have their key", status.Dropped)
	}
	want := []string{"acc_otherkey", mine.Account.ID}
	slices.Sort(want)
	if got := accountIDs(t, manager, id); !slices.Equal(got, want) {
		t.Fatalf("wallets = %v, want both", got)
	}
	// One of the two keys sorts first under the reference; each wallet still
	// gets its own, for export and for signing.
	for accountID, want := range map[string]string{mine.Account.ID: bundleImportedKey, "acc_otherkey": otherKey} {
		got, err := manager.ExportPrivateKey(id, accountID, account.FamilyEVM, syncPassword)
		if err != nil || !strings.Contains(strings.ToLower(got), want) {
			t.Fatalf("ExportPrivateKey(%s) = %q, %v; want its own key", accountID, got, err)
		}
		key, _, err := account.ParsePrivateKey(want)
		if err != nil {
			t.Fatal(err)
		}
		if public := signerKey(t, manager, id, accountID); !bytes.Equal(public, crypto.FromECDSAPub(&key.PublicKey)) {
			t.Errorf("%s signs with %x, not its own key", accountID, public)
		}
	}
	// And both keys are in the vault on disk: the space opens, both wallets
	// verified against their own variant.
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unlock(id, syncPassword); err != nil {
		t.Fatalf("Unlock() after a key conflict error = %v", err)
	}
	if got := accountIDs(t, manager, id); len(got) != 2 {
		t.Fatalf("wallets after unlock = %v", got)
	}
	// Both keys come back from the vault — after an unlock and after a
	// password change, which writes the vault anew — and each wallet still
	// gets its own.
	exportsOwn := func(password string) {
		t.Helper()
		for accountID, want := range map[string]string{mine.Account.ID: bundleImportedKey, "acc_otherkey": otherKey} {
			got, err := manager.ExportPrivateKey(id, accountID, account.FamilyEVM, password)
			if err != nil || !strings.Contains(strings.ToLower(got), want) {
				t.Fatalf("ExportPrivateKey(%s) = %q, %v; want its own key", accountID, got, err)
			}
		}
	}
	exportsOwn(syncPassword)
	if err := manager.ChangePassword(id, syncPassword, "a changed sync password"); err != nil {
		t.Fatal(err)
	}
	exportsOwn("a changed sync password")
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unlock(id, "a changed sync password"); err != nil {
		t.Fatalf("Unlock() after the password change error = %v", err)
	}
	exportsOwn("a changed sync password")
}

// A wallet whose reference holds keys, none of them its own, is not kept: a
// fingerprint, a reference or a curve alone does not make a key a wallet's,
// and with several keys under the reference none is taken by default.
func TestAWalletWithNoKeyOfItsOwnIsDropped(t *testing.T) {
	t.Parallel()

	manager, store, id := faultyMachine(t)
	mine, err := manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	keys := syncKeysFor(t)
	now := time.Now().UTC()
	// Its own key's fingerprint and the other key's addresses, under this
	// machine's reference.
	stranger, _ := importedRecord(t, "acc_stranger", mine.Account.KeyRef, strings.Repeat("22", 32), now)
	stranger.Fingerprint = mine.Account.Fingerprint
	_, another := importedRecord(t, "acc_unused", mine.Account.KeyRef, strings.Repeat("33", 32), now)
	putRemote(t, store, keys, "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: now, Accounts: []account.Account{stranger},
		ImportedKeys: map[string][]devicesync.Key{mine.Account.KeyRef: {another}},
	})
	status, err := manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.Dropped, []string{"acc_stranger"}) {
		t.Errorf("Dropped = %v, want the wallet without a key of its own", status.Dropped)
	}
	if got := accountIDs(t, manager, id); !slices.Equal(got, []string{mine.Account.ID}) {
		t.Fatalf("wallets = %v", got)
	}
}

// This machine's own file, evicted by the provider, cannot be read, so it
// cannot be merged — and it may hold what the space does not (see
// TestAFileFromBeforeABackupWasRestoredIsMergedNotOverwritten). It is pending
// like any file not downloaded, and it is not written over: a change made
// meanwhile waits until the file is back and merged.
func TestAnEvictedOwnFileIsNotRewritten(t *testing.T) {
	t.Parallel()

	manager, store, _, id := quietFaultyMachine(t)
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	key := syncKeysFor(t).SpaceTag() + "/" + syncKeysFor(t).DeviceTag("dev_faulty", id)
	store.set(func(f *faultyStore) { f.evicted[key] = true })
	store.mu.Lock()
	before := store.puts
	store.mu.Unlock()
	for range 3 {
		status, err := manager.SyncNow(t.Context(), id)
		if err != nil || status.Pending != 1 || status.Delivery != space.DeliveryUploadUnknown {
			t.Fatalf("status %+v, error %v; want the file pending and the space written", status, err)
		}
	}
	if _, err := manager.Rename(id, "changed"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.SyncNow(t.Context(), id)
	if err != nil || status.Delivery != space.DeliveryUnsent || !status.MergeFirst {
		t.Fatalf("status %+v, error %v; want the change held back", status, err)
	}
	store.mu.Lock()
	after := store.puts
	store.mu.Unlock()
	if after != before {
		t.Fatalf("an evicted own file was written over %d time(s)", after-before)
	}

	// Downloaded again: read, merged, and then replaced.
	store.set(func(f *faultyStore) { delete(f.evicted, key) })
	if status, err := manager.SyncNow(t.Context(), id); err != nil || status.Delivery != space.DeliveryUploadUnknown {
		t.Fatalf("status %+v, error %v; want the change written", status, err)
	}
	store.mu.Lock()
	blob := store.files[key]
	store.mu.Unlock()
	if snap := openOwn(t, blob, "dev_faulty", id); snap.Name != "changed" {
		t.Fatalf("the folder holds %q, want the change", snap.Name)
	}
}

// hangingStore is a folder whose listing does not answer until the pass gives
// up on it — iCloud fetching a file that is not downloaded yet.
type hangingStore struct {
	*faultyStore
	entered chan struct{}
}

func (h hangingStore) List(ctx context.Context, _ string) ([]devicesync.Entry, error) {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// Turning sync off — or quitting — must not wait for a stuck folder, and a
// pass that was in flight must not go on to write into a folder that was let
// go. Both kinds of pass count: the loop's, and one asked for by hand.
func TestTurningSyncOffStopsAPassInFlight(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		start func(t *testing.T, manager *space.Manager, id string) <-chan error
	}{
		{
			name: "the loop's pass",
			// SetSync marks every open space for a pass of its own.
			start: func(*testing.T, *space.Manager, string) <-chan error { return nil },
		},
		{
			name: "a pass asked for by hand",
			start: func(t *testing.T, manager *space.Manager, id string) <-chan error {
				result := make(chan error, 1)
				go func() {
					_, err := manager.SyncNow(t.Context(), id)
					result <- err
				}()
				return result
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager := newManager(t, t.TempDir())
			created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
			if err != nil {
				t.Fatal(err)
			}
			store := hangingStore{faultyStore: newFaultyStore(), entered: make(chan struct{}, 1)}
			manager.SetSync(store, "dev_hanging", nil)
			byHand := tt.start(t, manager, created.Space.ID)
			select {
			case <-store.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("no pass reached the folder")
			}

			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				manager.SetSync(nil, "", nil)
			}()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("turning sync off waited for the stuck pass")
			}
			if byHand != nil {
				select {
				case err := <-byHand:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("the pass asked for by hand ended with %v, want it cancelled", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the pass asked for by hand kept running after sync was turned off")
				}
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.puts != 0 {
				t.Fatalf("%d files were written into a folder that was let go", store.puts)
			}
		})
	}
}

// Another machine sends a newer copy of a wallet this machine has, and the
// copy does not check out — a key that disagrees, a record from a client this
// one does not understand. The newer copy is refused; the wallet is not: the
// copy here was checked at unlock and nothing deleted it.
func TestAWalletIsNotLostToABadNewerCopy(t *testing.T) {
	t.Parallel()

	manager, store, id := faultyMachine(t)
	mine, err := manager.Derive(id, "tron-nile", account.FamilyTron, "mine")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	newer := mine.Clone()
	newer.Label = "renamed elsewhere"
	newer.Addresses = map[account.Family]string{account.FamilyTron: "TNotTheAddressOfThisIndexxxxxxxxx"}
	newer.UpdatedAt = mine.UpdatedAt.Add(time.Minute)
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: newer.UpdatedAt,
		Accounts: []account.Account{newer},
	})

	status, err := manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.Dropped, []string{mine.ID}) {
		t.Errorf("Dropped = %v, want the bad copy of %s reported", status.Dropped, mine.ID)
	}
	accounts, err := manager.Accounts(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].ID != mine.ID ||
		accounts[0].Label != "mine" || accounts[0].Addresses[account.FamilyTron] != mine.Addresses[account.FamilyTron] {
		t.Fatalf("wallets = %+v, want %s kept as it was here", accounts, mine.ID)
	}
}

// Another machine — a client of its own, or anyone holding the phrase and the
// folder — names the space with spaces only. The wallet refuses a blank name
// in space.json at start, so keeping it would stop every space on this
// machine from opening.
func TestABlankNameFromAnotherMachineIsNotKept(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	store := newFaultyStore()
	manager.SetSync(store, "dev_blank", nil)
	created, err := manager.Create(space.CreateRequest{Name: "Main", Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(time.Minute)
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: later, Name: "   ", NameUpdatedAt: later,
	})
	if _, err := manager.SyncNow(t.Context(), created.Space.ID); err != nil {
		t.Fatal(err)
	}
	summary, _, err := manager.Get(created.Space.ID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Name != "Main" {
		t.Errorf("name = %q, want the name kept", summary.Name)
	}
	if _, err := space.NewManager(home, 15*time.Minute, fastKDF); err != nil {
		t.Fatalf("the wallet no longer starts: %v", err)
	}
}

// When this machine last wrote its file is what tells the person the space has
// reached the folder. One failed pass must not erase it: the file is still
// there, and the next pass, finding nothing new, does not write it again.
func TestTheLastWriteSurvivesAFailedPass(t *testing.T) {
	t.Parallel()

	manager, store, id := faultyMachine(t)
	first, err := manager.SyncNow(t.Context(), id)
	if err != nil || first.LastWrite.IsZero() {
		t.Fatalf("status %+v, error %v; want a write", first, err)
	}

	store.set(func(f *faultyStore) { f.listErr = errors.New("folder went away") })
	failed, _ := manager.SyncNow(t.Context(), id)
	store.set(func(f *faultyStore) { f.listErr = nil })
	recovered, err := manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for name, status := range map[string]space.SyncStatus{"failed": failed, "recovered": recovered} {
		if !status.LastWrite.Equal(first.LastWrite) {
			t.Errorf("%s pass: LastWrite = %v, want %v", name, status.LastWrite, first.LastWrite)
		}
	}
}

// A pass asked for by a request that went away — the person closed the page —
// did not fail. Its status must not replace what the last pass found.
func TestAPassCutShortByItsCallerIsNotAFailure(t *testing.T) {
	t.Parallel()

	manager, _, id := faultyMachine(t)
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := manager.SyncNow(gone, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("SyncNow(cancelled) error = %v", err)
	}
	if status := manager.SyncOverview(t.Context()).Spaces[id]; status.State != space.SyncOK || status.Error != "" {
		t.Fatalf("status after a cancelled pass = %+v, want the last pass's", status)
	}
}

// A derived wallet is checked by deriving its family's address again. A copy
// from another machine that also carries an address for another chain must
// not be kept: the UI shows that address as where to receive on that chain,
// and nothing derives it.
func TestADerivedWalletWithAForeignAddressIsDropped(t *testing.T) {
	t.Parallel()

	manager, store, id := faultyMachine(t)
	address, err := account.DerivedAddress(syncPhrase, "", account.FamilyTron, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: now,
		Accounts: []account.Account{{
			ID: "acc_foreign", Kind: account.KindDerived, Family: account.FamilyTron,
			NetworkIDs: []string{"tron-nile", "eth-sepolia"}, Index: new(uint32(0)), DerivationProfile: "bip44-v1",
			Addresses: map[account.Family]string{
				account.FamilyTron: address,
				account.FamilyEVM:  "0x000000000000000000000000000000000000dEaD",
			},
			CreatedAt: now, UpdatedAt: now,
		}},
	})
	status, err := manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.Dropped, []string{"acc_foreign"}) {
		t.Errorf("Dropped = %v, want the wallet with a foreign address", status.Dropped)
	}
	if got := accountIDs(t, manager, id); len(got) != 0 {
		t.Fatalf("wallets = %v, want none kept", got)
	}
}

// Two wallets on one key reference — only a snapshot Walletspace did not write
// makes that. Deleting one of them must not take the key the other still
// signs with, or the space would no longer unlock; so the key stays, and no
// key tombstone goes out to delete it elsewhere. Deleting the last one takes
// the key, as deleting an imported wallet always does.
func TestDeletingOneOfTwoWalletsOnAKeyKeepsTheKey(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	store := newFaultyStore()
	manager.SetSync(store, "dev_faulty", nil)
	created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	mine, err := manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	twin, _ := importedRecord(t, "acc_twin", mine.Account.KeyRef, bundleImportedKey, time.Now().UTC())
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: time.Now().UTC(), Accounts: []account.Account{twin},
	})
	if status, err := manager.SyncNow(t.Context(), id); err != nil || len(status.Dropped) != 0 {
		t.Fatalf("status %+v, error %v; want the twin kept", status, err)
	}

	if err := manager.DeleteAccount(id, mine.Account.ID, syncPassword); err != nil {
		t.Fatal(err)
	}
	keys, deleted, err := space.VaultContents(home, id, syncPassword)
	if err != nil {
		t.Fatal(err)
	}
	if keys[mine.Account.KeyRef] != 1 || len(deleted) != 0 {
		t.Fatalf("vault keys %v, key tombstones %v; want the key kept and no tombstone", keys, tombIDs(deleted))
	}
	stillHolds(t, "the space", manager, id, "acc_twin")
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unlock(id, syncPassword); err != nil {
		t.Fatalf("Unlock() after deleting one of two wallets on a key error = %v", err)
	}

	if err := manager.DeleteAccount(id, "acc_twin", syncPassword); err != nil {
		t.Fatal(err)
	}
	keys, deleted, err = space.VaultContents(home, id, syncPassword)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 || !slices.Equal(tombIDs(deleted), []string{mine.Account.KeyRef}) {
		t.Fatalf("vault keys %v, key tombstones %v; want the key gone with the last wallet", keys, tombIDs(deleted))
	}
}

func tombIDs(items []devicesync.Tombstone) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ID
	}
	return out
}

// manyTombstones are n wallet tombstones with ids no wallet has.
func manyTombstones(n int, at time.Time) []devicesync.Tombstone {
	out := make([]devicesync.Tombstone, n)
	for i := range out {
		out[i] = devicesync.Tombstone{ID: fmt.Sprintf("acc_old%06d", i), DeletedAt: at}
	}
	return out
}

// Tombstones are never cut to fit a snapshot: sending only some would quietly
// weaken the deletions the rest record. A space holding more than a snapshot
// carries fails its pass, says why, and is left as it is.
func TestASpaceWithMoreDeletionsThanASnapshotCarriesFailsItsPass(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	first := newManager(t, home)
	created, err := first.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	first.Close()
	file := spaceFile(t, home, id)
	file.DeletedAccounts = manyTombstones(devicesync.MaxTombstones+1, time.Now().UTC())
	path := filepath.Join(home, "spaces", id, "space.json")
	if err := os.WriteFile(path, encode(t, file), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	manager := newManager(t, home)
	store := newFaultyStore()
	manager.SetSync(store, "dev_faulty", nil)
	if err := manager.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	status, err := manager.SyncNow(t.Context(), id)
	if !errors.Is(err, devicesync.ErrInvalidSnapshot) || status.State != space.SyncError || !strings.Contains(status.Error, "deletions") {
		t.Fatalf("status %+v, error %v; want the pass refused over its deletions", status, err)
	}
	store.mu.Lock()
	puts := store.puts
	store.mu.Unlock()
	if puts != 0 {
		t.Errorf("a snapshot with part of the deletions was written %d times", puts)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the refused pass changed space.json (%v)", err)
	}
}

// The same bound reached by a merge: this machine's deletions and another's
// together are more than a snapshot carries. The pass fails before it keeps
// anything — not the other machine's deletions, not the wallet they remove —
// rather than save a space it then cannot write to the folder.
func TestAMergePastTheDeletionBoundChangesNothing(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	store := newFaultyStore()
	manager.SetSync(store, "dev_faulty", nil)
	created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	wallet, err := manager.Derive(id, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	local, err := manager.Derive(id, "tron-nile", account.FamilyTron, "deleted here")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteAccount(id, local.ID, syncPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "spaces", id, "space.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	remote := manyTombstones(devicesync.MaxTombstones-1, now)
	remote = append(remote, devicesync.Tombstone{ID: wallet.ID, DeletedAt: now})
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: now, DeletedAccounts: remote,
	})
	status, err := manager.SyncNow(t.Context(), id)
	if !errors.Is(err, devicesync.ErrInvalidSnapshot) || status.State != space.SyncError {
		t.Fatalf("status %+v, error %v; want the merge refused", status, err)
	}
	if got := accountIDs(t, manager, id); !slices.Equal(got, []string{wallet.ID}) {
		t.Errorf("wallets = %v, want the refused merge to have removed nothing", got)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the refused merge changed space.json (%v)", err)
	}
}

// faultyMachineAt is faultyMachine with its home, for tests that look at the
// vault on disk.
func faultyMachineAt(t *testing.T) (*space.Manager, *faultyStore, string, string) {
	t.Helper()
	home := t.TempDir()
	manager := newManager(t, home)
	store := newFaultyStore()
	manager.SetSync(store, "dev_faulty", nil)
	created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, home, created.Space.ID
}

// quietFaultyMachine is faultyMachineAt with no sync loop: passes and sends
// happen only when the test asks for them.
func quietFaultyMachine(t *testing.T) (*space.Manager, *faultyStore, string, string) {
	t.Helper()
	home := t.TempDir()
	manager := newManager(t, home)
	store := newFaultyStore()
	space.SetSyncQuiet(manager, store, "dev_faulty")
	created, err := manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	return manager, store, home, created.Space.ID
}

// A newer copy of a wallet that is here, from a snapshot this wallet did not
// write, keeps the wallet's id and key reference but describes another key —
// and brings that key along as a second variant, so that it checks out. Kept,
// it would carry the wallet off: the wallet would sign with the other key, and
// its own key would be left under the reference with no wallet. A wallet here
// stays what it is; the copy is reported as dropped.
func TestACopyOfAWalletCannotMoveItToAnotherKey(t *testing.T) {
	t.Parallel()

	const otherKey = "1111111111111111111111111111111111111111111111111111111111111111"
	manager, store, home, id := faultyMachineAt(t)
	mine, err := manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	moved, variant := importedRecord(t, mine.Account.ID, mine.Account.KeyRef, otherKey, time.Now().UTC().Add(time.Minute))
	moved.CreatedAt = mine.Account.CreatedAt
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: time.Now().UTC(), Accounts: []account.Account{moved},
		ImportedKeys: map[string][]devicesync.Key{mine.Account.KeyRef: {variant}},
	})
	status, err := manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.Dropped, []string{mine.Account.ID}) {
		t.Errorf("Dropped = %v, want the copy that moves the wallet", status.Dropped)
	}
	stillHolds(t, "the space", manager, id, mine.Account.ID)
	key, _, err := account.ParsePrivateKey(bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	if public := signerKey(t, manager, id, mine.Account.ID); !bytes.Equal(public, crypto.FromECDSAPub(&key.PublicKey)) {
		t.Errorf("the wallet signs with %x, not its own key", public)
	}
	accounts, err := manager.Accounts(id)
	if err != nil || len(accounts) != 1 || accounts[0].Fingerprint != mine.Account.Fingerprint {
		t.Fatalf("wallets = %+v, %v; want the wallet as it was", accounts, err)
	}

	// Deleting the wallet is what it always is: its reference goes, every
	// key under it with it.
	if err := manager.DeleteAccount(id, mine.Account.ID, syncPassword); err != nil {
		t.Fatal(err)
	}
	keys, deleted, err := space.VaultContents(home, id, syncPassword)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 || !slices.Equal(tombIDs(deleted), []string{mine.Account.KeyRef}) {
		t.Errorf("vault keys %v, key tombstones %v after the delete", keys, tombIDs(deleted))
	}
}

// A pass that adopts a wallet and its key and then fails to write the space
// leaves the space as it was, in the session as on disk: the session must
// keep matching the vault, or the next write would store what this one could
// not, or lose what it had.
func TestAPassWhoseWriteFailsChangesNothing(t *testing.T) {
	t.Parallel()

	const otherKey = "1111111111111111111111111111111111111111111111111111111111111111"
	// No background pass: one running while the space's directory is
	// swapped for a file below would make it again.
	manager, store, home, id := quietFaultyMachine(t)
	mine, err := manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	remote, variant := importedRecord(t, "acc_remote", "key_remote", otherKey, time.Now().UTC())
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: time.Now().UTC(), Accounts: []account.Account{remote},
		ImportedKeys: map[string][]devicesync.Key{"key_remote": {variant}},
	})
	spacePath := filepath.Join(home, "spaces", id)
	if err := os.RemoveAll(spacePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spacePath, []byte("blocks the space's directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SyncNow(t.Context(), id); err == nil {
		t.Fatal("SyncNow() error = nil; the case needs the space write to fail")
	}
	// No pass from here on gets as far as merging.
	store.set(func(f *faultyStore) { f.listErr = errors.New("folder went away") })

	if got := accountIDs(t, manager, id); !slices.Equal(got, []string{mine.Account.ID}) {
		t.Errorf("wallets = %v after a failed pass, want the space as it was", got)
	}
	stillHolds(t, "the session", manager, id, mine.Account.ID)

	// The next write stores the session's vault: it holds this machine's key
	// and nothing the failed pass took in.
	if err := os.Remove(spacePath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(spacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Import(id, "eth-sepolia", account.FamilyEVM, "", strings.Repeat("22", 32)); err != nil {
		t.Fatal(err)
	}
	keys, _, err := space.VaultContents(home, id, syncPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, adopted := keys["key_remote"]; adopted || keys[mine.Account.KeyRef] != 1 || len(keys) != 2 {
		t.Errorf("vault keys = %v, want this machine's two and nothing from the failed pass", keys)
	}
}

// A key reference no wallet uses keeps every variant: the merge removes keys
// only by tombstone, and so does the space — through an unlock, a password
// change, later passes, and into what it writes to the folder.
func TestAKeyNoWalletUsesKeepsEveryVariant(t *testing.T) {
	t.Parallel()

	manager, store, home, id := faultyMachineAt(t)
	_, first := importedRecord(t, "acc_x", "key_unused", strings.Repeat("11", 32), time.Now().UTC())
	_, second := importedRecord(t, "acc_y", "key_unused", strings.Repeat("22", 32), time.Now().UTC())
	keys := syncKeysFor(t)
	putRemote(t, store, keys, "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: time.Now().UTC(),
		ImportedKeys: map[string][]devicesync.Key{"key_unused": {first, second}},
	})
	held := func(password, when string) {
		t.Helper()
		got, _, err := space.VaultContents(home, id, password)
		if err != nil {
			t.Fatal(err)
		}
		if got["key_unused"] != 2 {
			t.Fatalf("%s: vault keys = %v, want both variants of the unused reference", when, got)
		}
	}
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	held(syncPassword, "after the pass that brought it")
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unlock(id, syncPassword); err != nil {
		t.Fatal(err)
	}
	if err := manager.ChangePassword(id, syncPassword, "a changed sync password"); err != nil {
		t.Fatal(err)
	}
	held("a changed sync password", "after a password change")
	// A change here makes the next pass write the space and this machine's
	// file again, from the vault as it now is.
	if _, err := manager.Rename(id, "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	held("a changed sync password", "after another pass")

	own := keys.DeviceTag("dev_faulty", id)
	store.mu.Lock()
	blob := store.files[keys.SpaceTag()+"/"+own]
	store.mu.Unlock()
	snap, err := devicesync.Open(keys, keys.SpaceTag(), own, blob)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Clear()
	if len(snap.ImportedKeys["key_unused"]) != 2 {
		t.Errorf("this machine's file carries %d variants of the unused reference, want 2", len(snap.ImportedKeys["key_unused"]))
	}
}

// derivedCopy is a remote copy of a derived wallet here, changed by edit and
// stamped later, so that it wins the merge if it is kept.
func derivedCopy(t *testing.T, store *faultyStore, item account.Account, edit func(*account.Account)) {
	t.Helper()
	copied := item.Clone()
	edit(&copied)
	copied.UpdatedAt = time.Now().UTC().Add(time.Minute)
	putRemote(t, store, syncKeysFor(t), "dev_other", devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: time.Now().UTC(), Accounts: []account.Account{copied},
	})
}

func derivedAddress(t *testing.T, family account.Family, index uint32) string {
	t.Helper()
	address, err := account.DerivedAddress(syncPhrase, "", family, index)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

// A remote copy of a derived wallet that is genuine seed derivation but on
// the other curve — the Tron wallet at index 0 arriving as the Solana wallet
// at index 0 — is another key. No machine binds a wallet across curves; kept,
// the copy would make the wallet's Tron address vanish. The wallet stays as
// it is and the copy is reported.
func TestACopyOfADerivedWalletCannotMoveItToTheOtherCurve(t *testing.T) {
	t.Parallel()

	manager, store, id := faultyMachine(t)
	wallet, err := manager.Derive(id, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	derivedCopy(t, store, wallet, func(a *account.Account) {
		a.Family, a.NetworkIDs = account.FamilySolana, []string{"solana-devnet"}
		a.Addresses = map[account.Family]string{account.FamilySolana: derivedAddress(t, account.FamilySolana, *wallet.Index)}
	})
	status, err := manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.Dropped, []string{wallet.ID}) {
		t.Errorf("Dropped = %v, want the copy on the other curve", status.Dropped)
	}
	accounts, err := manager.Accounts(id)
	if err != nil || len(accounts) != 1 || accounts[0].Family != account.FamilyTron ||
		!maps.Equal(accounts[0].Addresses, wallet.Addresses) {
		t.Fatalf("wallets = %+v, %v; want the Tron wallet as it was", accounts, err)
	}
}

// A bound Tron wallet arriving in the form from before bindings — no family,
// a Tron and an EVM address — is kept only if both addresses are the ones its
// index derives: that check is all that stands between such a copy and a
// receive address of someone else's choosing.
func TestAnOldFormCopyOfABoundWalletNeedsBothAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		evm  func(t *testing.T, index uint32) string
		tron func(t *testing.T, index uint32) string
	}{
		{
			name: "a wrong EVM address",
			evm:  func(*testing.T, uint32) string { return "0x000000000000000000000000000000000000dEaD" },
			tron: func(t *testing.T, index uint32) string { return derivedAddress(t, account.FamilyTron, index) },
		},
		{
			name: "a wrong Tron address",
			evm:  func(t *testing.T, index uint32) string { return derivedAddress(t, account.FamilyEVM, index) },
			tron: func(t *testing.T, index uint32) string { return derivedAddress(t, account.FamilyTron, index+1) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			manager, store, id := faultyMachine(t)
			wallet, err := manager.Derive(id, "tron-nile", account.FamilyTron, "")
			if err != nil {
				t.Fatal(err)
			}
			derivedCopy(t, store, wallet, func(a *account.Account) {
				a.Family = ""
				a.Addresses = map[account.Family]string{
					account.FamilyTron: tt.tron(t, *wallet.Index), account.FamilyEVM: tt.evm(t, *wallet.Index),
				}
			})
			status, err := manager.SyncNow(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(status.Dropped, []string{wallet.ID}) {
				t.Errorf("Dropped = %v, want the copy with %s", status.Dropped, tt.name)
			}
			accounts, err := manager.Accounts(id)
			if err != nil || len(accounts) != 1 || accounts[0].Family != account.FamilyTron ||
				!maps.Equal(accounts[0].Addresses, wallet.Addresses) {
				t.Fatalf("wallets = %+v, %v; want the bound wallet as it was", accounts, err)
			}
		})
	}

	// With both addresses its own, the copy is the same wallet in the old
	// form, and the later copy wins as any other would.
	manager, store, id := faultyMachine(t)
	wallet, err := manager.Derive(id, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	derivedCopy(t, store, wallet, func(a *account.Account) {
		a.Family = ""
		a.Addresses = map[account.Family]string{
			account.FamilyTron: derivedAddress(t, account.FamilyTron, *wallet.Index),
			account.FamilyEVM:  derivedAddress(t, account.FamilyEVM, *wallet.Index),
		}
	})
	if status, err := manager.SyncNow(t.Context(), id); err != nil || len(status.Dropped) != 0 {
		t.Fatalf("status %+v, error %v; want the genuine copy kept", status, err)
	}
}
