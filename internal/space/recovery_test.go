package space_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/vault"
)

const phraselessID = "spc_phraseless"

func TestASpaceWithoutAPhraseGetsOne(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	const password = "an older space password"
	if err := space.WriteSpaceWithoutPhrase(home, phraselessID, password, fastKDF); err != nil {
		t.Fatalf("WriteSpaceWithoutPhrase() error = %v", err)
	}
	manager := newManager(t, home)

	// Locked: nothing to add a phrase to yet, and nothing is said about one.
	if _, err := manager.AddRecoveryPhrase(phraselessID, password, ""); !errors.Is(err, space.ErrLocked) {
		t.Fatalf("AddRecoveryPhrase(locked) error = %v, want ErrLocked", err)
	}
	if err := manager.Unlock(phraselessID, password); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	summary, _, err := manager.Get(phraselessID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.HasPhrase {
		t.Fatal("a space without a phrase reports one")
	}
	imported, err := manager.Import(phraselessID, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}

	// It is a step-up like revealing a phrase.
	if _, err := manager.AddRecoveryPhrase(phraselessID, "", ""); !errors.Is(err, space.ErrPasswordRequired) {
		t.Fatalf("AddRecoveryPhrase(no password) error = %v", err)
	}
	if _, err := manager.AddRecoveryPhrase(phraselessID, "not the password", ""); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("AddRecoveryPhrase(wrong password) error = %v", err)
	}
	phrase, err := manager.AddRecoveryPhrase(phraselessID, password, "")
	if err != nil {
		t.Fatalf("AddRecoveryPhrase() error = %v", err)
	}
	if err := account.ValidateMnemonic(phrase); err != nil {
		t.Fatalf("the added phrase is not a valid BIP39 phrase: %v", err)
	}
	if _, err := manager.AddRecoveryPhrase(phraselessID, password, ""); !errors.Is(err, space.ErrHasSeed) {
		t.Fatalf("AddRecoveryPhrase(again) error = %v, want ErrHasSeed", err)
	}

	// It is in the vault: after a restart the space opens, reveals it, still
	// holds its imported key, and can now derive.
	restarted := newManager(t, home)
	if err := restarted.Unlock(phraselessID, password); err != nil {
		t.Fatalf("Unlock(after restart) error = %v", err)
	}
	if summary, _, _ := restarted.Get(phraselessID); !summary.HasPhrase {
		t.Error("the space does not report its new phrase")
	}
	if got, err := restarted.Mnemonic(phraselessID, password); err != nil || got != phrase {
		t.Fatalf("Mnemonic() = %q, %v; want the added phrase", got, err)
	}
	if _, err := restarted.ExportPrivateKey(phraselessID, imported.Account.ID, account.FamilyEVM, password); err != nil {
		t.Fatalf("the imported key is gone: %v", err)
	}
	if _, err := restarted.Derive(phraselessID, "tron-nile", account.FamilyTron, ""); err != nil {
		t.Fatalf("Derive() after adding a phrase error = %v", err)
	}
}

func TestEveryNewSpaceHasAPhrase(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	if !created.MnemonicGenerated || !created.Space.HasPhrase {
		t.Fatalf("a new space has no phrase: %+v", created.Space)
	}
	if _, err := manager.AddRecoveryPhrase(created.Space.ID, "test-vault-password", ""); !errors.Is(err, space.ErrHasSeed) {
		t.Fatalf("AddRecoveryPhrase(has one) error = %v, want ErrHasSeed", err)
	}
	if err := manager.Lock(created.Space.ID); err != nil {
		t.Fatal(err)
	}
	if summary, _, _ := manager.Get(created.Space.ID); summary.HasPhrase {
		t.Error("a locked space claims to know about its phrase")
	}
}

func TestRenamesAreStampedForSync(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created, err := manager.Create(space.CreateRequest{Name: "first", Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	before := spaceFile(t, home, created.Space.ID).NameUpdatedAt
	if before.IsZero() {
		t.Fatal("a space named at creation has no name time")
	}
	unnamed, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	// The default name has no time, so a name set elsewhere wins over it.
	if got := spaceFile(t, home, unnamed.Space.ID).NameUpdatedAt; !got.IsZero() {
		t.Errorf("the default name has a time %v", got)
	}
	if _, err := manager.Rename(created.Space.ID, "second"); err != nil {
		t.Fatal(err)
	}
	after := spaceFile(t, home, created.Space.ID)
	if after.NameUpdatedAt.Before(before) {
		t.Errorf("rename moved the name time back: %v → %v", before, after.NameUpdatedAt)
	}
	if !after.NameUpdatedAt.Equal(after.UpdatedAt) {
		t.Errorf("name time %v, update time %v: a rename sets both", after.NameUpdatedAt, after.UpdatedAt)
	}

	// Other changes move UpdatedAt but not the name time.
	if _, err := manager.Derive(created.Space.ID, "tron-nile", account.FamilyTron, ""); err != nil {
		t.Fatal(err)
	}
	if got := spaceFile(t, home, created.Space.ID).NameUpdatedAt; !got.Equal(after.NameUpdatedAt) {
		t.Errorf("deriving a wallet moved the name time to %v", got)
	}
}

func tombstoneIDs(items []devicesync.Tombstone) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ID
	}
	return out
}

func TestDeletionsLeaveTombstones(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	derived, err := manager.Derive(created.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	imported, err := manager.Import(created.Space.ID, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{derived.ID, imported.Account.ID} {
		if err := manager.DeleteAccount(created.Space.ID, id, "test-vault-password"); err != nil {
			t.Fatalf("DeleteAccount(%s) error = %v", id, err)
		}
	}
	file := spaceFile(t, home, created.Space.ID)
	if got := tombstoneIDs(file.DeletedAccounts); !slices.Equal(got, []string{derived.ID, imported.Account.ID}) {
		t.Errorf("wallet tombstones = %v", got)
	}
	// A derived wallet has no key of its own; an imported one does. Its
	// tombstone is in the vault, under the password: in the plaintext file
	// anyone who can write it could add one, and a key tombstone deletes the
	// key on every machine.
	if raw, err := os.ReadFile(filepath.Join(home, "spaces", created.Space.ID, "space.json")); err != nil || bytes.Contains(raw, []byte("deleted_keys")) {
		t.Errorf("space.json carries key tombstones in plaintext (%v)", err)
	}
	keys, deletedKeys, err := space.VaultContents(home, created.Space.ID, "test-vault-password")
	if err != nil {
		t.Fatal(err)
	}
	if got := tombstoneIDs(deletedKeys); !slices.Equal(got, []string{imported.Account.KeyRef}) {
		t.Errorf("vault key tombstones = %v, want only the imported wallet's key", got)
	}
	if len(keys) != 0 {
		t.Errorf("vault keys = %v, want the deleted one gone", keys)
	}
	for _, tomb := range append(file.DeletedAccounts, deletedKeys...) {
		if tomb.DeletedAt.IsZero() {
			t.Errorf("tombstone %s has no time", tomb.ID)
		}
	}

	// They survive a restart, and the space still opens.
	restarted := newManager(t, home)
	if err := restarted.Unlock(created.Space.ID, "test-vault-password"); err != nil {
		t.Fatalf("Unlock(with tombstones) error = %v", err)
	}
}

func TestAFailedDeleteLeavesNoTombstone(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	derived, err := manager.Derive(created.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.DeleteAccount(created.Space.ID, derived.ID, "wrong password!"); err == nil {
		t.Fatal("DeleteAccount(wrong password) succeeded")
	}
	if got := spaceFile(t, home, created.Space.ID).DeletedAccounts; len(got) != 0 {
		t.Fatalf("a refused delete left tombstones %v", tombstoneIDs(got))
	}
}

func TestATombstoneWithABadIDIsRefused(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	good := spaceFile(t, home, created.Space.ID)
	for name, edit := range map[string]func(*space.File){
		"wallet": func(f *space.File) { f.DeletedAccounts = []devicesync.Tombstone{{ID: "../acc"}} },
	} {
		file := good
		edit(&file)
		if _, err := space.ParseBundle(encode(t, file)); !errors.Is(err, space.ErrInvalidBundle) {
			t.Errorf("%s tombstone with a bad id: ParseBundle() error = %v", name, err)
		}
	}
}

// A name a snapshot cannot carry is refused where it is set, so it can never
// stop the space from syncing.
func TestANameTooLongToSyncIsRefused(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	long := strings.Repeat("n", devicesync.MaxNameRunes+1)
	if _, err := manager.Create(space.CreateRequest{Name: long, Password: "test-vault-password"}); err == nil {
		t.Fatal("Create(long name) succeeded")
	}
	created, err := manager.Create(space.CreateRequest{Name: strings.Repeat("n", devicesync.MaxNameRunes), Password: "test-vault-password"})
	if err != nil {
		t.Fatalf("Create(name at the limit) error = %v", err)
	}
	if _, err := manager.Rename(created.Space.ID, long); err == nil {
		t.Fatal("Rename(long name) succeeded")
	}
}

// A space from before every space had a phrase, holding an imported key, still
// opens after a restart and still hands its key out. Such spaces are on disks
// today, and nothing else covers opening one since the imported-only mode
// went away.
func TestAnOlderSpaceWithoutAPhraseStillOpensWithItsKey(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	const password = "an older space password"
	if err := space.WriteSpaceWithoutPhrase(home, phraselessID, password, fastKDF); err != nil {
		t.Fatal(err)
	}
	manager := newManager(t, home)
	if err := manager.Unlock(phraselessID, password); err != nil {
		t.Fatal(err)
	}
	imported, err := manager.Import(phraselessID, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newManager(t, home)
	if err := restarted.Unlock(phraselessID, password); err != nil {
		t.Fatalf("an older space with an imported key does not reopen: %v", err)
	}
	if _, err := restarted.ExportPrivateKey(phraselessID, imported.Account.ID, account.FamilyEVM, password); err != nil {
		t.Fatalf("ExportPrivateKey() error = %v", err)
	}
}

// An unreleased build kept key tombstones in plaintext space.json. They are
// not read at all: anything that can write the file could have put one there,
// and a key tombstone deletes a key on every machine the space syncs to.
// Neither one for a key the vault holds nor one for a key it does not — a key
// other machines may hold — deletes anything here or reaches the folder, and
// the list goes at the next write of the file.
func TestAPlaintextKeyTombstoneInSpaceJSONDeletesNothing(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	first := newManager(t, home)
	created, err := first.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	held, err := first.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	path := filepath.Join(home, "spaces", id, "space.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	file["deleted_keys"] = []any{
		map[string]any{"id": held.Account.KeyRef, "deleted_at": now},
		map[string]any{"id": "key_heldelsewhere", "deleted_at": now},
	}
	if err := os.WriteFile(path, encode(t, file), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := newManager(t, home)
	store := newFaultyStore()
	manager.SetSync(store, "dev_faulty", nil)
	if err := manager.Unlock(id, syncPassword); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	if _, err := manager.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	stillHolds(t, "the space", manager, id, held.Account.ID)

	keys := syncKeysFor(t)
	store.mu.Lock()
	written := maps.Clone(store.files)
	store.mu.Unlock()
	if len(written) == 0 {
		t.Fatal("nothing was written to the folder")
	}
	for name, blob := range written {
		spaceTag, deviceTag, _ := strings.Cut(name, "/")
		snap, err := devicesync.Open(keys, spaceTag, deviceTag, blob)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.DeletedKeys) != 0 {
			t.Errorf("the folder got key tombstones %v from plaintext space.json", tombstoneIDs(snap.DeletedKeys))
		}
		if len(snap.ImportedKeys[held.Account.KeyRef]) != 1 {
			t.Errorf("the folder lacks the held key: %d variants", len(snap.ImportedKeys[held.Account.KeyRef]))
		}
		snap.Clear()
	}
	if _, deleted, err := space.VaultContents(home, id, syncPassword); err != nil || len(deleted) != 0 {
		t.Errorf("vault key tombstones = %v, %v; want none", deleted, err)
	}

	if _, err := manager.Rename(id, "renamed"); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(path); err != nil || bytes.Contains(raw, []byte("deleted_keys")) {
		t.Errorf("the plaintext list survived a write of space.json (%v)", err)
	}
}

// A phrase that is already another space's here would make the two one space
// for good: both would sync through its folder and merge.
func TestAnotherLocalSpacesPhraseIsRefused(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	const password = "an older space password"
	if err := space.WriteSpaceWithoutPhrase(home, phraselessID, password, fastKDF); err != nil {
		t.Fatal(err)
	}
	manager := newManager(t, home)
	other, err := manager.Create(space.CreateRequest{Name: "other", Password: "the other space's password"})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Unlock(phraselessID, password); err != nil {
		t.Fatal(err)
	}
	refused := func(phrase string) {
		t.Helper()
		if _, err := manager.AddRecoveryPhrase(phraselessID, password, phrase); !errors.Is(err, space.ErrPhraseOfAnotherSpace) {
			t.Fatalf("AddRecoveryPhrase(another space's phrase) error = %v, want ErrPhraseOfAnotherSpace", err)
		}
		if summary, _, err := manager.Get(phraselessID); err != nil || summary.HasPhrase {
			t.Fatalf("a refused phrase was kept: %+v, %v", summary, err)
		}
	}

	// The other space is open: its keys say which folder it syncs through.
	refused(other.Mnemonic)

	// The password is checked first: without it, nothing is said about
	// which phrases other spaces here have.
	if _, err := manager.AddRecoveryPhrase(phraselessID, "not the password", other.Mnemonic); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("AddRecoveryPhrase(another space's phrase, wrong password) error = %v, want ErrInvalidPassword", err)
	}
	if summary, _, err := manager.Get(phraselessID); err != nil || summary.HasPhrase {
		t.Fatalf("a phrase given with a wrong password was kept: %+v, %v", summary, err)
	}

	// Locked, it is known by the tag its last pass left beside it.
	store, err := devicesync.NewFolderStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSync(store, "dev_recoverytest", nil)
	if _, err := manager.SyncNow(t.Context(), other.Space.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Lock(other.Space.ID); err != nil {
		t.Fatal(err)
	}
	refused("  " + other.Mnemonic + "  ")

	// Any other phrase is this space's to take.
	fresh, err := account.GenerateMnemonic()
	if err != nil {
		t.Fatal(err)
	}
	given, err := manager.AddRecoveryPhrase(phraselessID, password, fresh)
	if err != nil || given != fresh {
		t.Fatalf("AddRecoveryPhrase(a phrase of its own) = %q, %v", given, err)
	}
	if summary, _, err := manager.Get(phraselessID); err != nil || !summary.HasPhrase {
		t.Fatalf("the space's own phrase was not kept: %+v, %v", summary, err)
	}
}
