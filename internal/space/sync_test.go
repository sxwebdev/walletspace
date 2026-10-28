package space_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
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
)

const (
	syncPhrase   = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	syncPassword = "sync test password"
)

// machine is one computer: its own home, its own device id, the shared folder.
type machine struct {
	home    string
	manager *space.Manager
}

func newMachine(t *testing.T, folder, deviceID string) machine {
	t.Helper()
	home := t.TempDir()
	manager := newManager(t, home)
	store, err := devicesync.NewFolderStore(folder)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSync(store, deviceID, nil)
	return machine{home: home, manager: manager}
}

func (m machine) syncNow(t *testing.T, id string) space.SyncStatus {
	t.Helper()
	status, err := m.manager.SyncNow(t.Context(), id)
	if err != nil {
		t.Fatalf("SyncNow() error = %v", err)
	}
	return status
}

func accountIDs(t *testing.T, manager *space.Manager, id string) []string {
	t.Helper()
	accounts, err := manager.Accounts(id)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(accounts))
	for i, item := range accounts {
		out[i] = item.ID
	}
	slices.Sort(out)
	return out
}

// The scenario the feature exists for: a laptop dies, the phrase is typed on
// another one, and the space comes back whole — imported keys included.
func TestARestoredSpaceComesBackWhole(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	created, err := a.manager.Create(space.CreateRequest{Name: "Main", Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	if _, err := a.manager.Derive(id, "tron-nile", account.FamilyTron, "savings"); err != nil {
		t.Fatal(err)
	}
	imported, err := a.manager.Import(id, "eth-sepolia", account.FamilyEVM, "treasury", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	if status := a.syncNow(t, id); status.State != space.SyncOK || status.LastWrite.IsZero() {
		t.Fatalf("A status = %+v, want a written snapshot", status)
	}

	// Laptop A is gone. Laptop B has only the phrase and a new password.
	b := newMachine(t, folder, "dev_laptopb")
	restored, err := b.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: "a different password"})
	if err != nil {
		t.Fatal(err)
	}
	bid := restored.Space.ID
	if got, want := accountIDs(t, b.manager, bid), accountIDs(t, a.manager, id); !slices.Equal(got, want) {
		t.Fatalf("restored wallets = %v, want %v", got, want)
	}
	if len(restored.Accounts) != 2 {
		t.Errorf("Create() returned %d wallets: the first sync was not waited for", len(restored.Accounts))
	}
	summary, _, _ := b.manager.Get(bid)
	if summary.Name != "Main" {
		t.Errorf("restored name = %q: the default name overrode the one set on purpose", summary.Name)
	}
	key, err := b.manager.ExportPrivateKey(bid, imported.Account.ID, account.FamilyEVM, "a different password")
	if err != nil {
		t.Fatalf("the imported key did not come back: %v", err)
	}
	if !strings.Contains(strings.ToLower(key), bundleImportedKey) {
		t.Errorf("restored key = %q", key)
	}
	// And it is in B's own vault now, not only in memory: a restart keeps it.
	restarted := newManager(t, b.home)
	if err := restarted.Unlock(bid, "a different password"); err != nil {
		t.Fatalf("Unlock(after restart) error = %v", err)
	}
	if _, err := restarted.ExportPrivateKey(bid, imported.Account.ID, account.FamilyEVM, "a different password"); err != nil {
		t.Fatalf("the restored key is not in the vault on disk: %v", err)
	}
}

func TestChangesTravelBothWays(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	b := newMachine(t, folder, "dev_laptopb")
	ca, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := b.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}

	first, err := a.manager.Derive(ca.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, ca.Space.ID)
	b.syncNow(t, cb.Space.ID)
	if _, err := b.manager.RenameAccount(cb.Space.ID, first.ID, "named on B"); err != nil {
		t.Fatalf("the wallet from A is not on B: %v", err)
	}
	if _, err := b.manager.Rename(cb.Space.ID, "Renamed on B"); err != nil {
		t.Fatal(err)
	}
	// B sees A's wallet, so it does not hand out the same index again.
	second, err := b.manager.Derive(cb.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	if *second.Index == *first.Index {
		t.Fatalf("B derived index %d again", *second.Index)
	}
	b.syncNow(t, cb.Space.ID)
	a.syncNow(t, ca.Space.ID)

	accounts, err := a.manager.Accounts(ca.Space.ID)
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for _, item := range accounts {
		labels[item.ID] = item.Label
	}
	if labels[first.ID] != "named on B" {
		t.Errorf("A sees label %q, want the rename made on B", labels[first.ID])
	}
	if _, ok := labels[second.ID]; !ok {
		t.Error("A does not see the wallet B derived")
	}
	if summary, _, _ := a.manager.Get(ca.Space.ID); summary.Name != "Renamed on B" {
		t.Errorf("A space name = %q", summary.Name)
	}
}

func TestADeletionReachesTheOtherMachine(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	b := newMachine(t, folder, "dev_laptopb")
	ca, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := a.manager.Import(ca.Space.ID, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, ca.Space.ID)
	cb, err := b.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	if got := accountIDs(t, b.manager, cb.Space.ID); !slices.Equal(got, []string{imported.Account.ID}) {
		t.Fatalf("B wallets = %v before the deletion", got)
	}

	if err := a.manager.DeleteAccount(ca.Space.ID, imported.Account.ID, syncPassword); err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, ca.Space.ID)
	b.syncNow(t, cb.Space.ID)
	if got := accountIDs(t, b.manager, cb.Space.ID); len(got) != 0 {
		t.Fatalf("B still has %v after A deleted it", got)
	}
	if _, err := b.manager.ExportPrivateKey(cb.Space.ID, imported.Account.ID, account.FamilyEVM, syncPassword); err == nil {
		t.Fatal("B still holds the deleted key")
	}
	// B keeps the key's tombstone in its own vault, so that B's file carries
	// the deletion on when A's is gone.
	keys, deleted, err := space.VaultContents(b.home, cb.Space.ID, syncPassword)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 || !slices.ContainsFunc(deleted, func(tomb devicesync.Tombstone) bool { return tomb.ID == imported.Account.KeyRef }) {
		t.Fatalf("B's vault keys %v, key tombstones %v; want the key gone and its tombstone kept", keys, deleted)
	}
	// The removal was not B's own act, so B kept a copy of the space as it
	// was before it.
	prev, err := os.ReadFile(filepath.Join(b.home, "spaces", cb.Space.ID, "space.json.prev"))
	if err != nil {
		t.Fatalf("no space.json.prev before a merge removed a wallet: %v", err)
	}
	if !bytes.Contains(prev, []byte(imported.Account.ID)) {
		t.Error("space.json.prev does not hold the removed wallet")
	}
}

// Something that can write to the folder and holds the seed — or a bug on
// another machine — sends a wallet the vault does not vouch for. It is
// dropped; the rest of the space is untouched.
func TestAWalletTheVaultDoesNotVouchForIsDropped(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	ca, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	good, err := a.manager.Derive(ca.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, ca.Space.ID)

	seed, err := account.Seed(syncPhrase, "")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := devicesync.DeriveKeys(seed)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	forged := devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: now,
		Accounts: []account.Account{
			{
				ID: "acc_forgedderived", Kind: account.KindDerived, Family: account.FamilyTron,
				NetworkIDs: []string{"tron-nile"}, Index: new(uint32(7)), DerivationProfile: "bip44-v1",
				Addresses: map[account.Family]string{account.FamilyTron: "TAttackerAddressxxxxxxxxxxxxxxxxx"},
				CreatedAt: now, UpdatedAt: now,
			},
			{
				ID: "acc_forgedimported", Kind: account.KindImported, NetworkIDs: []string{"eth-sepolia"},
				Addresses: map[account.Family]string{account.FamilyEVM: "0x000000000000000000000000000000000000dEaD"},
				KeyRef:    "key_forged", CreatedAt: now, UpdatedAt: now,
			},
		},
		ImportedKeys: map[string][]devicesync.Key{
			"key_forged": {{Curve: "secp256k1", PrivateKey: bytes.Repeat([]byte{1}, 32), ImportedAt: now}},
		},
	}
	forgedTag := keys.DeviceTag("dev_attacker", "spc_x")
	blob, err := devicesync.Seal(keys, keys.SpaceTag(), forgedTag, forged)
	if err != nil {
		t.Fatal(err)
	}
	store, err := devicesync.NewFolderStore(folder)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(t.Context(), keys.SpaceTag(), forgedTag, blob); err != nil {
		t.Fatal(err)
	}
	// And a file that is not a snapshot at all.
	if err := store.Put(t.Context(), keys.SpaceTag(), strings.Repeat("e", 32), []byte("garbage")); err != nil {
		t.Fatal(err)
	}

	status := a.syncNow(t, ca.Space.ID)
	slices.Sort(status.Dropped)
	if !slices.Equal(status.Dropped, []string{"acc_forgedderived", "acc_forgedimported"}) {
		t.Errorf("Dropped = %v, want both forged wallets", status.Dropped)
	}
	if status.Unreadable != 1 {
		t.Errorf("Unreadable = %d, want the garbage file", status.Unreadable)
	}
	if got := accountIDs(t, a.manager, ca.Space.ID); !slices.Equal(got, []string{good.ID}) {
		t.Fatalf("wallets = %v, want only the genuine one", got)
	}
	// Still opens after a restart: nothing unverified reached the disk.
	restarted := newManager(t, a.home)
	if err := restarted.Unlock(ca.Space.ID, syncPassword); err != nil {
		t.Fatalf("Unlock(after a forged snapshot) error = %v", err)
	}
}

func TestNothingLeavesALockedSpaceOrOneWithoutAPhrase(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	ca, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.manager.Lock(ca.Space.ID); err != nil {
		t.Fatal(err)
	}
	if status := a.syncNow(t, ca.Space.ID); status.State != space.SyncLocked {
		t.Fatalf("locked status = %+v", status)
	}

	home := t.TempDir()
	if err := space.WriteSpaceWithoutPhrase(home, phraselessID, syncPassword, fastKDF); err != nil {
		t.Fatal(err)
	}
	manager := newManager(t, home)
	store, err := devicesync.NewFolderStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSync(store, "dev_laptopc", nil)
	if err := manager.Unlock(phraselessID, syncPassword); err != nil {
		t.Fatal(err)
	}
	status, err := manager.SyncNow(t.Context(), phraselessID)
	if err != nil || status.State != space.SyncNoPhrase {
		t.Fatalf("phraseless status = %+v, %v", status, err)
	}
	if spaces, _ := store.Spaces(t.Context()); len(spaces) != 0 {
		t.Fatalf("a space without a phrase wrote %v", spaces)
	}
	// Once it has a phrase, it syncs.
	if _, err := manager.AddRecoveryPhrase(phraselessID, syncPassword, ""); err != nil {
		t.Fatal(err)
	}
	if status, err := manager.SyncNow(t.Context(), phraselessID); err != nil || status.State != space.SyncOK {
		t.Fatalf("status after adding a phrase = %+v, %v", status, err)
	}
}

// What is in the folder opens with the phrase and with nothing else: no
// plaintext, and no key on this machine's disk that would open it.
func TestTheFolderHoldsOnlyCiphertext(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	ca, err := a.manager.Create(space.CreateRequest{Name: "Visible name", Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := a.manager.Derive(ca.Space.ID, "tron-nile", account.FamilyTron, "visible label")
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, ca.Space.ID)

	secrets := [][]byte{
		[]byte("Visible name"), []byte("visible label"), []byte(wallet.Addresses[account.FamilyTron]),
		[]byte(wallet.ID), []byte(ca.Space.ID), []byte("dev_laptopa"),
	}
	files := 0
	err = filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range secrets {
			if bytes.Contains(data, secret) || strings.Contains(path, string(secret)) {
				t.Errorf("%s carries %q in the clear", path, secret)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Errorf("the folder holds %d files, want one", files)
	}
	// Nothing on the local disk opens the folder: the keys live in memory.
	// The outbox is the folder's file, sealed the same way.
	entries, err := os.ReadDir(filepath.Join(a.home, "spaces", ca.Space.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "space.json", "sync.json":
		case "outbox.wsync":
			data, err := os.ReadFile(filepath.Join(a.home, "spaces", ca.Space.ID, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range append(secrets, []byte(syncPhrase), []byte("abandon")) {
				if bytes.Contains(data, secret) {
					t.Errorf("the outbox carries %q in the clear", secret)
				}
			}
		default:
			t.Errorf("unexpected file %s next to the space", entry.Name())
		}
	}
	cache, err := os.ReadFile(filepath.Join(a.home, "spaces", ca.Space.ID, "sync.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Only public tags, the folder, times and revisions: no key, no phrase,
	// nothing else.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(cache, &fields); err != nil {
		t.Fatal(err)
	}
	public := []string{"space_tag", "last_write", "device_id", "device_tag", "outbox_revision", "folder", "written_revision", "written_sha256"}
	for name := range fields {
		if !slices.Contains(public, name) {
			t.Errorf("sync.json has %q: %s", name, cache)
		}
	}
}

func TestAnUnchangedSpaceIsNotWrittenAgain(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	ca, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	first := a.syncNow(t, ca.Space.ID)
	second := a.syncNow(t, ca.Space.ID)
	if !second.LastWrite.Equal(first.LastWrite) {
		t.Errorf("an unchanged space was written again: %v → %v", first.LastWrite, second.LastWrite)
	}
	if _, err := a.manager.Rename(ca.Space.ID, "changed"); err != nil {
		t.Fatal(err)
	}
	if third := a.syncNow(t, ca.Space.ID); !third.LastWrite.After(first.LastWrite) && third.LastWrite.Equal(first.LastWrite) {
		t.Errorf("a changed space was not written: %v", third.LastWrite)
	}
}

func TestOneMachineMayHoldTheSamePhraseTwice(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	one, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	two, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := a.manager.Derive(one.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, one.Space.ID)
	a.syncNow(t, two.Space.ID)
	if got := accountIDs(t, a.manager, two.Space.ID); !slices.Equal(got, []string{wallet.ID}) {
		t.Fatalf("the second space with the phrase has %v", got)
	}
	store, err := devicesync.NewFolderStore(folder)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := store.Spaces(t.Context())
	if err != nil || len(tags) != 1 {
		t.Fatalf("folder spaces = %v, %v; want one", tags, err)
	}
	entries, err := store.List(t.Context(), tags[0])
	if err != nil || len(entries) != 2 {
		t.Fatalf("files = %v, %v; want one per local space", entries, err)
	}
}

func TestTheOverviewCountsSpacesThatAreNotHere(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	ca, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, ca.Space.ID)
	other, err := a.manager.Create(space.CreateRequest{Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, other.Space.ID)

	b := newMachine(t, folder, "dev_laptopb")
	cb, err := b.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	overview := b.manager.SyncOverview(t.Context())
	if !overview.Enabled || overview.Unknown != 1 {
		t.Fatalf("overview = %+v, want one space not on B", overview)
	}
	if overview.Spaces[cb.Space.ID].State != space.SyncOK {
		t.Errorf("B space state = %+v", overview.Spaces[cb.Space.ID])
	}

	b.manager.SetSync(nil, "", nil)
	if overview := b.manager.SyncOverview(t.Context()); overview.Enabled || overview.Spaces[cb.Space.ID].State != space.SyncOff {
		t.Errorf("overview with sync off = %+v", overview)
	}
}

// Edits keep coming while passes run; nothing races and the machines agree
// at the end.
func TestSyncAlongsideEdits(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	b := newMachine(t, folder, "dev_laptopb")
	ca, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	cb, err := b.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 5 {
			if _, err := a.manager.Derive(ca.Space.ID, "tron-nile", account.FamilyTron, ""); err != nil {
				t.Errorf("A Derive(%d) error = %v", i, err)
			}
		}
	})
	wg.Go(func() {
		for range 5 {
			if _, err := a.manager.SyncNow(t.Context(), ca.Space.ID); err != nil {
				t.Errorf("A SyncNow() error = %v", err)
			}
		}
	})
	wg.Go(func() {
		for range 5 {
			if _, err := b.manager.SyncNow(t.Context(), cb.Space.ID); err != nil {
				t.Errorf("B SyncNow() error = %v", err)
			}
		}
	})
	wg.Wait()
	a.syncNow(t, ca.Space.ID)
	b.syncNow(t, cb.Space.ID)
	if got, want := accountIDs(t, b.manager, cb.Space.ID), accountIDs(t, a.manager, ca.Space.ID); !slices.Equal(got, want) || len(want) != 5 {
		t.Fatalf("B = %v, A = %v; want the same five", got, want)
	}
}

func TestSyncNowWithoutSync(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created, err := manager.Create(space.CreateRequest{Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.SyncNow(t.Context(), created.Space.ID)
	if err != nil || status.State != space.SyncOff {
		t.Fatalf("SyncNow(off) = %+v, %v", status, err)
	}
	store, err := devicesync.NewFolderStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSync(store, "dev_x", nil)
	if _, err := manager.SyncNow(t.Context(), "spc_missing"); !errors.Is(err, space.ErrNotFound) {
		t.Errorf("SyncNow(unknown) error = %v", err)
	}
}

// A space that is here but has not synced here yet cannot be told apart, from
// the folder, from one that is not here. It must not be counted as missing:
// that would send the person to restore a space they already have.
func TestASpaceHereButNotYetSyncedIsNotCountedMissing(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	first, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, first.Space.ID)
	second, err := a.manager.Create(space.CreateRequest{Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, second.Space.ID)
	bundle, err := a.manager.ExportBundle([]space.ExportItem{{ID: first.Space.ID, Password: syncPassword}})
	if err != nil {
		t.Fatal(err)
	}

	// B has the first space, locked and never synced, and not the second.
	b := newMachine(t, folder, "dev_laptopb")
	if _, err := b.manager.ImportBundle(bundle); err != nil {
		t.Fatal(err)
	}
	overview := b.manager.SyncOverview(t.Context())
	if overview.Unknown != 1 || overview.Unidentified != 1 {
		t.Fatalf("overview = unknown %d, unidentified %d; want 1 and 1", overview.Unknown, overview.Unidentified)
	}
	// Once it has synced, it is recognised.
	if err := b.manager.Unlock(first.Space.ID, syncPassword); err != nil {
		t.Fatal(err)
	}
	b.syncNow(t, first.Space.ID)
	overview = b.manager.SyncOverview(t.Context())
	if overview.Unknown != 1 || overview.Unidentified != 0 {
		t.Fatalf("after syncing = unknown %d, unidentified %d; want 1 and 0", overview.Unknown, overview.Unidentified)
	}
}

// A space made without a phrase and moved to a second machine gets its phrase
// on one machine; the other is given that phrase, and the two copies meet in
// one folder. A new phrase on each would leave them syncing past each other.
func TestASpaceWithoutAPhraseGetsTheSamePhraseEverywhere(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	if err := space.WriteSpaceWithoutPhrase(a.home, phraselessID, syncPassword, fastKDF); err != nil {
		t.Fatal(err)
	}
	a = machine{home: a.home, manager: newManager(t, a.home)}
	store, err := devicesync.NewFolderStore(folder)
	if err != nil {
		t.Fatal(err)
	}
	a.manager.SetSync(store, "dev_laptopa", nil)
	bundle, err := a.manager.ExportBundle([]space.ExportItem{{ID: phraselessID, Password: syncPassword}})
	if err != nil {
		t.Fatal(err)
	}
	b := newMachine(t, folder, "dev_laptopb")
	if _, err := b.manager.ImportBundle(bundle); err != nil {
		t.Fatal(err)
	}

	if err := a.manager.Unlock(phraselessID, syncPassword); err != nil {
		t.Fatal(err)
	}
	phrase, err := a.manager.AddRecoveryPhrase(phraselessID, syncPassword, "")
	if err != nil {
		t.Fatal(err)
	}
	imported, err := a.manager.Import(phraselessID, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, phraselessID)

	if err := b.manager.Unlock(phraselessID, syncPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := b.manager.AddRecoveryPhrase(phraselessID, syncPassword, "not a phrase"); !errors.Is(err, account.ErrInvalidMnemonic) {
		t.Fatalf("AddRecoveryPhrase(invalid) error = %v", err)
	}
	given, err := b.manager.AddRecoveryPhrase(phraselessID, syncPassword, "  "+phrase+"  ")
	if err != nil {
		t.Fatalf("AddRecoveryPhrase(the other machine's phrase) error = %v", err)
	}
	if given != phrase {
		t.Fatalf("B was given %q, want A's phrase", given)
	}
	b.syncNow(t, phraselessID)
	if _, err := b.manager.ExportPrivateKey(phraselessID, imported.Account.ID, account.FamilyEVM, syncPassword); err != nil {
		t.Fatalf("B did not receive the key imported on A: %v", err)
	}
}
