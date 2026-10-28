package space

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/vault"
)

// A name longer than a snapshot carries — set before names were bounded — is
// cut to fit, so the space still syncs. Tombstones are never cut: sending only
// the newest would quietly weaken the deletions the rest record. And the key
// tombstones a snapshot carries are the vault's.
func TestASnapshotCutsAnOldLongNameAndNoTombstone(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tombs := make([]devicesync.Tombstone, devicesync.MaxTombstones+10)
	for i := range tombs {
		tombs[i] = devicesync.Tombstone{ID: fmt.Sprintf("acc_t%d", i), DeletedAt: base.Add(time.Duration(i) * time.Second)}
	}
	file := File{
		Name:            strings.Repeat("я", devicesync.MaxNameRunes+5),
		DeletedAccounts: tombs,
	}
	snap := snapshotOf(file, &Payload{DeletedKeys: []devicesync.Tombstone{{ID: "key_vault", DeletedAt: base}}})
	if got := len([]rune(snap.Name)); got != devicesync.MaxNameRunes {
		t.Errorf("name runes = %d", got)
	}
	if len(snap.DeletedAccounts) != len(tombs) {
		t.Fatalf("tombstones = %d, want all %d", len(snap.DeletedAccounts), len(tombs))
	}
	if len(snap.DeletedKeys) != 1 || snap.DeletedKeys[0].ID != "key_vault" {
		t.Errorf("key tombstones = %+v, want the vault's", snap.DeletedKeys)
	}
}

// Checking what another machine sent derives the seed once per pass, not once
// per wallet: a restore that brings hundreds of wallets must not spend a
// PBKDF2 on each.
//
// Not parallel, and with no sync loop running: it swaps a package variable,
// so passes are run by hand rather than by a background goroutine.
func TestAPassDerivesTheSeedOnce(t *testing.T) {
	const phrase = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	params := vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}
	store, err := devicesync.NewFolderStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	open := func(device string) (*Manager, *syncer, string) {
		manager, err := NewManager(t.TempDir(), 15*time.Minute, params)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(manager.Close)
		created, err := manager.Create(CreateRequest{Mnemonic: phrase, Password: "seed count password"})
		if err != nil {
			t.Fatal(err)
		}
		passes := newSyncer(store, device, slog.Default())
		t.Cleanup(passes.cancel)
		return manager, passes, created.Space.ID
	}
	a, aSync, aID := open("dev_seeda")
	b, bSync, bID := open("dev_seedb")
	// Twenty wallets B has never seen, each of which B checks by deriving it.
	for range 20 {
		if _, err := a.Derive(aID, "tron-nile", account.FamilyTron, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.syncSpace(t.Context(), aSync, aID); err != nil {
		t.Fatal(err)
	}

	original := seedOf
	var calls atomic.Int64
	seedOf = func(mnemonic, passphrase []byte) ([]byte, error) {
		calls.Add(1)
		return original(mnemonic, passphrase)
	}
	t.Cleanup(func() { seedOf = original })
	if err := b.syncSpace(t.Context(), bSync, bID); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("one pass derived the seed %d times, want once", got)
	}
	accounts, err := b.Accounts(bID)
	if err != nil || len(accounts) != 20 {
		t.Fatalf("B has %d wallets, %v; want 20", len(accounts), err)
	}
}

// Every space named before names had times has none. A copy restored by its
// phrase with the name left blank is called "default", also with no time. The
// name the person chose must win however the merge meets the two — not lose
// to "default" because it sorts after "Main".
func TestANameFromBeforeNameTimesBeatsADefault(t *testing.T) {
	t.Parallel()

	made := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	legacy := File{Name: "Main", CreatedAt: made}
	restored := File{Name: defaultName, CreatedAt: made.AddDate(1, 0, 0)}
	now := made.AddDate(2, 0, 0)
	for _, order := range [][]File{{legacy, restored}, {restored, legacy}} {
		snaps := make([]devicesync.Snapshot, len(order))
		for i, file := range order {
			snaps[i] = snapshotOf(file, &Payload{})
		}
		merged, _ := devicesync.Merge(snaps, now)
		if merged.Name != "Main" {
			t.Errorf("merged name = %q, want the name the person chose", merged.Name)
		}
	}

	// A rename made since still wins over the old name.
	renamed := File{Name: "Savings", NameUpdatedAt: made.AddDate(1, 6, 0), CreatedAt: made}
	merged, _ := devicesync.Merge([]devicesync.Snapshot{snapshotOf(legacy, &Payload{}), snapshotOf(renamed, &Payload{})}, now)
	if merged.Name != "Savings" {
		t.Errorf("merged name = %q, want the later rename", merged.Name)
	}
}

const (
	racePhrase      = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	raceKey         = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	raceOldPassword = "old password for the race"
	raceNewPassword = "new password for the race"
)

// raceMachines are two machines holding one phrase, with passes run by hand.
// B has imported a key and written it to the folder; A has not seen it yet.
func raceMachines(t *testing.T) (a *Manager, aSync *syncer, aID, keyRef string) {
	t.Helper()
	store, err := devicesync.NewFolderStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	params := vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}
	open := func(device string) (*Manager, *syncer, string) {
		manager, err := NewManager(t.TempDir(), 15*time.Minute, params)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(manager.Close)
		created, err := manager.Create(CreateRequest{Mnemonic: racePhrase, Password: raceOldPassword})
		if err != nil {
			t.Fatal(err)
		}
		passes := newSyncer(store, device, slog.Default())
		t.Cleanup(passes.cancel)
		return manager, passes, created.Space.ID
	}
	a, aSync, aID = open("dev_racea")
	b, bSync, bID := open("dev_raceb")
	imported, err := b.Import(bID, "eth-sepolia", account.FamilyEVM, "", raceKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.syncSpace(t.Context(), bSync, bID); err != nil {
		t.Fatal(err)
	}
	return a, aSync, aID, imported.Account.KeyRef
}

// pauseFirstSeed holds the first seedOf call after it until release is closed.
// Unlock and ChangePassword derive the seed right after opening the vault, so
// this parks them between reading the vault and writing anything back.
func pauseFirstSeed(t *testing.T) (entered, release chan struct{}) {
	t.Helper()
	original := seedOf
	entered, release = make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	seedOf = func(mnemonic, passphrase []byte) ([]byte, error) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return original(mnemonic, passphrase)
	}
	t.Cleanup(func() { seedOf = original })
	return entered, release
}

// A sync pass may reseal the vault while a password is being checked: it
// takes the manager's lock, not the space's. Unlock and ChangePassword then
// must not install or write back the vault they read before the pass — that
// drops the key the pass adopted while space.json keeps its wallet, and the
// space never unlocks again.
//
// Not parallel: it swaps a package variable.
func TestAPassDuringAPasswordCheckIsNotUndone(t *testing.T) {
	tests := []struct {
		name     string
		run      func(m *Manager, id string) error
		password string
	}{
		{
			name:     "changing the password",
			run:      func(m *Manager, id string) error { return m.ChangePassword(id, raceOldPassword, raceNewPassword) },
			password: raceNewPassword,
		},
		{
			name:     "unlocking a space that is already open",
			run:      func(m *Manager, id string) error { return m.Unlock(id, raceOldPassword) },
			password: raceOldPassword,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, aSync, aID, keyRef := raceMachines(t)
			entered, release := pauseFirstSeed(t)
			done := make(chan error, 1)
			go func() { done <- tt.run(a, aID) }()
			<-entered
			if err := a.syncSpace(t.Context(), aSync, aID); err != nil {
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("error = %v", err)
			}

			a.mu.RLock()
			_, inSession := a.sessions[aID].payload.ImportedKeys[keyRef]
			a.mu.RUnlock()
			if !inSession {
				t.Error("the open session lost the key the pass adopted")
			}
			if err := a.Lock(aID); err != nil {
				t.Fatal(err)
			}
			if err := a.Unlock(aID, tt.password); err != nil {
				t.Fatalf("the space no longer unlocks: %v", err)
			}
		})
	}
}

// countingStore is an empty folder that counts what is asked of it.
type countingStore struct{ lists, puts atomic.Int64 }

func (c *countingStore) Spaces(context.Context) ([]string, error) { return nil, nil }

func (c *countingStore) List(context.Context, string) ([]devicesync.Entry, error) {
	c.lists.Add(1)
	return nil, nil
}

func (c *countingStore) Get(context.Context, string, string) ([]byte, error) {
	return nil, devicesync.ErrNotDownloaded
}

func (c *countingStore) Put(context.Context, string, string, []byte) error {
	c.puts.Add(1)
	return nil
}

// A pass takes its syncer first (SyncNow, the pass after unlock) and syncRun
// after. If SetSync let that syncer go in between, the pass must not touch its
// folder at all — sync there is off, or has moved.
func TestAPassOfALetGoSyncerTouchesNothing(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(t.TempDir(), 15*time.Minute, vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	created, err := manager.Create(CreateRequest{Mnemonic: racePhrase, Password: raceOldPassword})
	if err != nil {
		t.Fatal(err)
	}
	old := &countingStore{}
	manager.SetSync(old, "dev_old", nil)
	taken := manager.currentSyncer()
	manager.SetSync(&countingStore{}, "dev_old", nil)
	before := old.lists.Load()

	if err := manager.syncSpace(context.Background(), taken, created.Space.ID); err == nil {
		t.Error("a pass of a let-go syncer reported success")
	}
	if lists, puts := old.lists.Load()-before, old.puts.Load(); lists != 0 || puts != 0 {
		t.Fatalf("the let-go folder was listed %d and written %d times", lists, puts)
	}
}

// space.json.prev, the copy kept before a merge removes something, holds the
// whole vault as it was. A password changed because the old one leaked must
// not leave that copy opening with the old password.
func TestThePreviousCopyFollowsAPasswordChange(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager, err := NewManager(home, 15*time.Minute, vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	created, err := manager.Create(CreateRequest{Mnemonic: racePhrase, Password: raceOldPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	manager.mu.RLock()
	file := manager.files[id]
	manager.mu.RUnlock()
	// What keepPreviousLocked writes before a merge takes a key away: the
	// space as it was, vault included.
	removing := &Payload{ImportedKeys: map[string]KeyVariants{"key_gone": {{}}}}
	if err := manager.keepPreviousLocked(file, file, removing, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.previousPath(id)); err != nil {
		t.Fatalf("no previous copy to test with: %v", err)
	}

	if err := manager.ChangePassword(id, raceOldPassword, raceNewPassword); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(manager.previousPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var previous File
	if err := json.Unmarshal(data, &previous); err != nil {
		t.Fatal(err)
	}
	var payload Payload
	if err := vault.OpenJSON(raceOldPassword, previous.Vault, aad(id), &payload); err == nil {
		clearPayload(&payload)
		t.Error("the old password still opens the previous copy")
	}
	if err := vault.OpenJSON(raceNewPassword, previous.Vault, aad(id), &payload); err != nil {
		t.Errorf("the new password does not open the previous copy: %v", err)
	}
	clearPayload(&payload)
}

// What a copy of a wallet from another machine may change, and what makes it
// another wallet under the same id. A derived wallet's family and addresses
// may change within its curve — another machine may have bound it, or not yet
// — because they are derived again from the seed before a copy is kept; a
// move to the other curve is another key.
func TestACopyOfAWalletStaysTheSameWallet(t *testing.T) {
	t.Parallel()

	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	imported := account.Account{
		ID: "acc_i", Kind: account.KindImported, Label: "hot", NetworkIDs: []string{"eth-sepolia"},
		Addresses: map[account.Family]string{account.FamilyEVM: "0xE", account.FamilyTron: "TT"},
		KeyRef:    "key_i", Fingerprint: "fp", ImportedAt: &when, CreatedAt: when, UpdatedAt: when,
	}
	derived := account.Account{
		ID: "acc_d", Kind: account.KindDerived, Family: account.FamilyTron, NetworkIDs: []string{"tron-nile"},
		Addresses: map[account.Family]string{account.FamilyTron: "T0"}, Index: new(uint32(0)),
		DerivationProfile: "bip44-v1", CreatedAt: when, UpdatedAt: when,
	}
	legacy := derived
	legacy.Family, legacy.NetworkIDs = "", nil
	legacy.Addresses = map[account.Family]string{account.FamilyTron: "T0", account.FamilyEVM: "0x0"}
	solana := derived
	solana.Family, solana.NetworkIDs = account.FamilySolana, []string{"solana-devnet"}
	solana.Addresses = map[account.Family]string{account.FamilySolana: "S0"}
	edit := func(item account.Account, change func(*account.Account)) account.Account {
		item = item.Clone()
		change(&item)
		return item
	}
	tests := []struct {
		name        string
		here, there account.Account
		same        bool
	}{
		{"an imported wallet renamed, rebound, updated", imported, edit(imported, func(a *account.Account) {
			a.Label, a.NetworkIDs, a.UpdatedAt = "cold", []string{"ethereum-mainnet"}, when.Add(time.Hour)
		}), true},
		{"an imported wallet on another reference", imported, edit(imported, func(a *account.Account) { a.KeyRef = "key_j" }), false},
		{"an imported wallet with another fingerprint", imported, edit(imported, func(a *account.Account) { a.Fingerprint = "fq" }), false},
		{"an imported wallet with another address", imported, edit(imported, func(a *account.Account) {
			a.Addresses[account.FamilyEVM] = "0xF"
		}), false},
		{"an imported wallet with an address less", imported, edit(imported, func(a *account.Account) {
			delete(a.Addresses, account.FamilyTron)
		}), false},
		{"an imported wallet turned derived", imported, edit(imported, func(a *account.Account) { a.Kind = account.KindDerived }), false},
		{"a derived wallet renamed", derived, edit(derived, func(a *account.Account) { a.Label = "x" }), true},
		{"a derived wallet at another index", derived, edit(derived, func(a *account.Account) { *a.Index = 1 }), false},
		{"a derived wallet without its index", derived, edit(derived, func(a *account.Account) { a.Index = nil }), false},
		{"a derived wallet bound to another family", derived, edit(derived, func(a *account.Account) {
			a.Family, a.Addresses = account.FamilyEVM, map[account.Family]string{account.FamilyEVM: "0x0"}
		}), true},
		{"a derived wallet on another profile", derived, edit(derived, func(a *account.Account) { a.DerivationProfile = "other" }), false},
		{"a derived wallet turned imported", derived, edit(derived, func(a *account.Account) { a.Kind = account.KindImported }), false},
		{"a record from before bindings given its family", legacy, edit(legacy, func(a *account.Account) {
			a.Family, a.Addresses = account.FamilyTron, map[account.Family]string{account.FamilyTron: "T0"}
		}), true},
		{"a record from before bindings given the other family", legacy, edit(legacy, func(a *account.Account) {
			a.Family, a.Addresses = account.FamilyEVM, map[account.Family]string{account.FamilyEVM: "0x0"}
		}), true},
		{"a bound wallet seen as it was before bindings", derived, legacy, true},
		{"a record from before bindings at another index", legacy, edit(legacy, func(a *account.Account) { *a.Index = 2 }), false},
		{"a Tron wallet arriving as the Solana wallet at its index", derived, solana, false},
		{"a Solana wallet arriving as the Tron wallet at its index", solana, derived, false},
		{"a record from before bindings arriving as a Solana wallet", legacy, solana, false},
		{"a Solana wallet arriving as a record from before bindings", solana, legacy, false},
		{"a Solana wallet renamed", solana, edit(solana, func(a *account.Account) { a.Label = "y" }), true},
		{"a wallet of a family with no known curve moved to Tron", edit(derived, func(a *account.Account) { a.Family = "btc" }), derived, false},
	}
	for _, tt := range tests {
		if got := sameIdentity(tt.here, tt.there); got != tt.same {
			t.Errorf("%s: sameIdentity() = %v, want %v", tt.name, got, tt.same)
		}
	}
}
