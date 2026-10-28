package space_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/vault"
)

const (
	bundlePasswordA = "first-space-password"
	bundlePasswordB = "second-space-password"
	// A secp256k1 key with no funds anywhere, used only to put an imported
	// key into a vault.
	bundleImportedKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
)

// twoSpaces makes a source home with two spaces under different passwords:
// one with a derived wallet, one with an imported key.
func twoSpaces(t *testing.T) (*space.Manager, space.CreateResult, space.CreateResult) {
	t.Helper()
	manager := newManager(t, t.TempDir())
	first, err := manager.Create(space.CreateRequest{Name: "first", Password: bundlePasswordA})
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	if _, err := manager.Derive(first.Space.ID, "tron-nile", account.FamilyTron, "derived"); err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	second, err := manager.Create(space.CreateRequest{Name: "second", Password: bundlePasswordB})
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	if _, err := manager.Import(second.Space.ID, "eth-sepolia", account.FamilyEVM, "imported", bundleImportedKey); err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	return manager, first, second
}

func TestBundleMovesSpacesToAnotherMachine(t *testing.T) {
	t.Parallel()

	source, first, second := twoSpaces(t)
	bundle, err := source.ExportBundle([]space.ExportItem{
		{ID: first.Space.ID, Password: bundlePasswordA},
		{ID: second.Space.ID, Password: bundlePasswordB},
	})
	if err != nil {
		t.Fatalf("ExportBundle() error = %v", err)
	}
	if strings.Contains(string(bundle), first.Mnemonic) || strings.Contains(string(bundle), second.Mnemonic) {
		t.Fatal("bundle contains a plaintext mnemonic")
	}
	if strings.Contains(string(bundle), bundleImportedKey) {
		t.Fatal("bundle contains a plaintext private key")
	}

	targetHome := t.TempDir()
	target := newManager(t, targetHome)
	result, err := target.ImportBundle(bundle)
	if err != nil {
		t.Fatalf("ImportBundle() error = %v", err)
	}
	if len(result.Imported) != 2 || len(result.Skipped) != 0 {
		t.Fatalf("ImportBundle() = %+v, want two imported, none skipped", result)
	}
	for _, summary := range result.Imported {
		if !summary.Locked {
			t.Errorf("imported space %s arrived unlocked", summary.ID)
		}
	}

	// The spaces are on disk, not only in this manager: a fresh start sees them.
	restarted := newManager(t, targetHome)
	for _, original := range []struct {
		created  space.CreateResult
		password string
	}{{first, bundlePasswordA}, {second, bundlePasswordB}} {
		id := original.created.Space.ID
		wantAccounts, err := source.Accounts(id)
		if err != nil {
			t.Fatalf("source Accounts(%s) error = %v", id, err)
		}
		gotAccounts, err := restarted.Accounts(id)
		if err != nil {
			t.Fatalf("Accounts(%s) error = %v", id, err)
		}
		if !slices.EqualFunc(gotAccounts, wantAccounts, func(a, b account.Account) bool {
			return a.ID == b.ID && a.Addresses[a.Family] == b.Addresses[b.Family] && a.KeyRef == b.KeyRef
		}) {
			t.Errorf("space %s wallets = %+v, want %+v", id, gotAccounts, wantAccounts)
		}
		// Each space opens with its own password and nothing else.
		if err := restarted.Unlock(id, "not-the-password"); !errors.Is(err, vault.ErrInvalidPassword) {
			t.Fatalf("Unlock(%s, wrong) error = %v, want ErrInvalidPassword", id, err)
		}
		if err := restarted.Unlock(id, original.password); err != nil {
			t.Fatalf("Unlock(%s) error = %v", id, err)
		}
		got, err := restarted.Mnemonic(id, original.password)
		if err != nil {
			t.Fatalf("Mnemonic(%s) error = %v", id, err)
		}
		if got != original.created.Mnemonic {
			t.Errorf("space %s mnemonic changed in transit", id)
		}
	}
	// The imported key came along and still signs for its address.
	accounts, err := restarted.Accounts(second.Space.ID)
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	exported, err := restarted.ExportPrivateKey(second.Space.ID, accounts[0].ID, account.FamilyEVM, bundlePasswordB)
	if err != nil {
		t.Fatalf("ExportPrivateKey() error = %v", err)
	}
	if !strings.Contains(strings.ToLower(exported), bundleImportedKey) {
		t.Errorf("imported key = %q, want %q", exported, bundleImportedKey)
	}
}

func TestExportBundleAsksForEveryPassword(t *testing.T) {
	t.Parallel()

	source, first, second := twoSpaces(t)
	tests := []struct {
		name    string
		items   []space.ExportItem
		wantErr error
	}{
		{
			name: "one space without its password",
			items: []space.ExportItem{
				{ID: first.Space.ID, Password: bundlePasswordA},
				{ID: second.Space.ID},
			},
			wantErr: space.ErrPasswordRequired,
		},
		{
			// One password does not open a space that has another.
			name: "the other space's password",
			items: []space.ExportItem{
				{ID: first.Space.ID, Password: bundlePasswordA},
				{ID: second.Space.ID, Password: bundlePasswordA},
			},
			wantErr: vault.ErrInvalidPassword,
		},
		{
			name:    "unknown space",
			items:   []space.ExportItem{{ID: "spc_missing", Password: bundlePasswordA}},
			wantErr: space.ErrNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := source.ExportBundle(tt.items)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ExportBundle() error = %v, want %v", err, tt.wantErr)
			}
			if data != nil {
				t.Fatal("a failed export still returned a bundle")
			}
		})
	}

	if _, err := source.ExportBundle(nil); err == nil {
		t.Error("ExportBundle(nothing) succeeded")
	}
	if _, err := source.ExportBundle([]space.ExportItem{
		{ID: first.Space.ID, Password: bundlePasswordA},
		{ID: first.Space.ID, Password: bundlePasswordA},
	}); err == nil {
		t.Error("ExportBundle(same space twice) succeeded")
	}
}

// A wrong password in an export is a guess like any other and must cost what
// a wrong unlock costs.
func TestExportBundleGoesThroughTheUnlockThrottle(t *testing.T) {
	t.Parallel()

	source, first, _ := twoSpaces(t)
	var lastErr error
	for range 6 {
		_, lastErr = source.ExportBundle([]space.ExportItem{{ID: first.Space.ID, Password: "a-wrong-password"}})
		if errors.Is(lastErr, space.ErrTooManyAttempts) {
			break
		}
	}
	if !errors.Is(lastErr, space.ErrTooManyAttempts) {
		t.Fatalf("repeated wrong export passwords error = %v, want ErrTooManyAttempts", lastErr)
	}
	if err := source.Unlock(first.Space.ID, bundlePasswordA); !errors.Is(err, space.ErrTooManyAttempts) {
		t.Fatalf("Unlock() after failed exports error = %v, want the shared cooldown", err)
	}
}

func TestImportNeverReplacesASpaceThatIsHere(t *testing.T) {
	t.Parallel()

	source, first, _ := twoSpaces(t)
	bundle, err := source.ExportBundle([]space.ExportItem{{ID: first.Space.ID, Password: bundlePasswordA}})
	if err != nil {
		t.Fatalf("ExportBundle() error = %v", err)
	}
	if _, err := source.Rename(first.Space.ID, "renamed after export"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	result, err := source.ImportBundle(bundle)
	if err != nil {
		t.Fatalf("ImportBundle() error = %v", err)
	}
	if len(result.Imported) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Reason != space.SkipExists {
		t.Fatalf("ImportBundle() = %+v, want the space skipped as existing", result)
	}
	summary, _, err := source.Get(first.Space.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if summary.Name != "renamed after export" {
		t.Errorf("space name = %q: the older copy in the file replaced it", summary.Name)
	}
}

func TestImportAcceptsASingleSpaceBackup(t *testing.T) {
	t.Parallel()

	source, first, _ := twoSpaces(t)
	backup, err := source.Backup(first.Space.ID, bundlePasswordA)
	if err != nil {
		t.Fatalf("Backup() error = %v", err)
	}
	target := newManager(t, t.TempDir())
	result, err := target.ImportBundle(backup)
	if err != nil {
		t.Fatalf("ImportBundle(backup) error = %v", err)
	}
	if len(result.Imported) != 1 || result.Imported[0].ID != first.Space.ID {
		t.Fatalf("ImportBundle(backup) = %+v", result)
	}
	if err := target.Unlock(first.Space.ID, bundlePasswordA); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
}

// spaceFile reads a space's file as it is on disk.
func spaceFile(t *testing.T, home, id string) space.File {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "spaces", id, "space.json"))
	if err != nil {
		t.Fatalf("read space file: %v", err)
	}
	var file space.File
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("decode space file: %v", err)
	}
	return file
}

func encode(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

func TestImportStopsAtTheSpaceQuota(t *testing.T) {
	t.Parallel()

	sourceHome := t.TempDir()
	source := newManager(t, sourceHome)
	created, err := source.Create(space.CreateRequest{Password: bundlePasswordA})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	template := spaceFile(t, sourceHome, created.Space.ID)

	// 64 spaces is the ceiling. The target already has one, so of 64 in the
	// file, 63 fit.
	target := newManager(t, t.TempDir())
	if _, err := target.Create(space.CreateRequest{Password: bundlePasswordA}); err != nil {
		t.Fatalf("Create(target) error = %v", err)
	}
	files := make([]space.File, 64)
	for i := range files {
		files[i] = template
		files[i].ID = fmt.Sprintf("spc_quota%02d", i)
	}
	result, err := target.ImportBundle(encode(t, space.Bundle{
		Format: space.BundleFormat, Version: space.BundleVersion, Spaces: files,
	}))
	if err != nil {
		t.Fatalf("ImportBundle() error = %v", err)
	}
	if len(result.Imported) != 63 || len(result.Skipped) != 1 || result.Skipped[0].Reason != space.SkipQuota {
		t.Fatalf("ImportBundle() imported %d, skipped %+v; want 63 and one over the quota",
			len(result.Imported), result.Skipped)
	}
	if got := len(target.List()); got != 64 {
		t.Errorf("spaces after import = %d, want 64", got)
	}
}

func TestParseBundleRefusesWhatDoesNotHoldTogether(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created, err := manager.Create(space.CreateRequest{Password: bundlePasswordA})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	good := spaceFile(t, home, created.Space.ID)
	bundleOf := func(files ...space.File) []byte {
		return encode(t, space.Bundle{Format: space.BundleFormat, Version: space.BundleVersion, Spaces: files})
	}
	edited := func(edit func(*space.File)) space.File {
		file := good
		file.Accounts = slices.Clone(good.Accounts)
		edit(&file)
		return file
	}

	madeAt := `"` + good.CreatedAt.Format(time.RFC3339Nano) + `"`
	if !strings.Contains(string(bundleOf(good)), madeAt) {
		t.Fatal("the case needs the space's creation time in the file")
	}
	unencodable := []byte(strings.ReplaceAll(string(bundleOf(good)), madeAt, `"2025-01-01T00:00:00+24:00"`))

	if files, err := space.ParseBundle(bundleOf(good)); err != nil || len(files) != 1 {
		t.Fatalf("ParseBundle(good) = %d files, %v", len(files), err)
	}
	tests := []struct {
		name string
		data []byte
	}{
		{name: "not JSON", data: []byte("not json")},
		{name: "empty object as a space", data: []byte(`{}`)},
		{name: "unknown format", data: encode(t, map[string]any{"format": "something-else", "version": 1, "spaces": []space.File{good}})},
		{name: "newer version", data: encode(t, space.Bundle{Format: space.BundleFormat, Version: 2, Spaces: []space.File{good}})},
		{name: "no spaces", data: bundleOf()},
		{name: "same space twice", data: bundleOf(good, good)},
		{name: "invalid id", data: bundleOf(edited(func(f *space.File) { f.ID = "../escape" }))},
		{name: "empty name", data: bundleOf(edited(func(f *space.File) { f.Name = " " }))},
		{name: "unsupported schema", data: bundleOf(edited(func(f *space.File) { f.SchemaVersion = 99 }))},
		{name: "duplicate wallet ids", data: bundleOf(edited(func(f *space.File) {
			f.Accounts = []account.Account{{ID: "acc_one"}, {ID: "acc_one"}}
		}))},
		// The vault cannot be opened here, but a weakened or broken container
		// is refused before it is written anywhere.
		{name: "weakened KDF", data: bundleOf(edited(func(f *space.File) { f.Vault.KDF.MemoryKiB = 8 }))},
		{name: "damaged ciphertext", data: bundleOf(edited(func(f *space.File) { f.Vault.Ciphertext = "!!" }))},
		// It decodes, but the space could never be written back.
		{name: "a time that does not encode again", data: unencodable},
		// Valid JSON, so that only the size can refuse it.
		{name: "too large", data: append(bundleOf(good), bytes.Repeat([]byte(" "), space.MaxBundleBytes)...)},
		// A space id becomes a directory name; a long one failed the write half
		// way through an import. Other long ids could never be synced.
		{name: "space id too long", data: bundleOf(edited(func(f *space.File) { f.ID = "spc_" + strings.Repeat("a", 300) }))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := space.ParseBundle(tt.data); !errors.Is(err, space.ErrInvalidBundle) {
				t.Fatalf("ParseBundle() error = %v, want ErrInvalidBundle", err)
			}
		})
	}

	// A refused file writes nothing.
	target := newManager(t, t.TempDir())
	if _, err := target.ImportBundle(bundleOf(good, good)); !errors.Is(err, space.ErrInvalidBundle) {
		t.Fatalf("ImportBundle(invalid) error = %v", err)
	}
	if got := len(target.List()); got != 0 {
		t.Errorf("an invalid bundle imported %d spaces", got)
	}
}

func TestExportHomeNeedsNoPassword(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	first, err := manager.Create(space.CreateRequest{Name: "first", Password: bundlePasswordA})
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	second, err := manager.Create(space.CreateRequest{Name: "second", Password: bundlePasswordB})
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}

	all, err := space.ExportHome(home, nil)
	if err != nil {
		t.Fatalf("ExportHome(all) error = %v", err)
	}
	files, err := space.ParseBundle(all)
	if err != nil {
		t.Fatalf("ParseBundle(all) error = %v", err)
	}
	ids := make([]string, len(files))
	for i, file := range files {
		ids[i] = file.ID
	}
	if !slices.Equal(ids, []string{first.Space.ID, second.Space.ID}) {
		t.Errorf("exported ids = %v, want both spaces in creation order", ids)
	}

	one, err := space.ExportHome(home, []string{second.Space.ID})
	if err != nil {
		t.Fatalf("ExportHome(one) error = %v", err)
	}
	files, err = space.ParseBundle(one)
	if err != nil || len(files) != 1 || files[0].ID != second.Space.ID {
		t.Fatalf("ExportHome(one) = %+v, %v", files, err)
	}

	if _, err := space.ExportHome(home, []string{"spc_missing"}); !errors.Is(err, space.ErrNotFound) {
		t.Errorf("ExportHome(unknown) error = %v, want ErrNotFound", err)
	}
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "spaces"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := space.ExportHome(empty, nil); err == nil {
		t.Error("ExportHome(empty home) succeeded")
	}
}

// withKeyTombstone is a bundle changed in transit: a key tombstone added to
// one of its spaces, in the plaintext the bundle carries beside the vault.
func withKeyTombstone(t *testing.T, bundle []byte, spaceID, keyRef string) []byte {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(bundle, &decoded); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range decoded["spaces"].([]any) {
		file := item.(map[string]any)
		if file["id"] == spaceID {
			file["deleted_keys"] = []any{map[string]any{"id": keyRef, "deleted_at": time.Now().UTC()}}
			found = true
		}
	}
	if !found {
		t.Fatalf("space %s is not in the bundle", spaceID)
	}
	return encode(t, decoded)
}

// stillHolds fails the test unless the machine's space has the wallet and
// hands out its key.
func stillHolds(t *testing.T, who string, manager *space.Manager, spaceID, accountID string) {
	t.Helper()
	if !slices.Contains(accountIDs(t, manager, spaceID), accountID) {
		t.Fatalf("%s lost the wallet %s", who, accountID)
	}
	key, err := manager.ExportPrivateKey(spaceID, accountID, account.FamilyEVM, syncPassword)
	if err != nil || !strings.Contains(strings.ToLower(key), bundleImportedKey) {
		t.Fatalf("%s lost the key: %q, %v", who, key, err)
	}
}

// A bundle is plaintext beside its vaults. Changed in transit — a key
// tombstone added for a key the space holds — it must not delete that key: not
// on the machine that imports it, and not, through sync, on the machine it
// came from. A key tombstone counts only inside the vault.
func TestAKeyTombstoneAddedToABundleDeletesNothing(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	created, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	imported, err := a.manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, id)
	bundle, err := a.manager.ExportBundle([]space.ExportItem{{ID: id, Password: syncPassword}})
	if err != nil {
		t.Fatal(err)
	}

	b := newMachine(t, folder, "dev_laptopb")
	if _, err := b.manager.ImportBundle(withKeyTombstone(t, bundle, id, imported.Account.KeyRef)); err != nil {
		t.Fatal(err)
	}
	if err := b.manager.Unlock(id, syncPassword); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	stillHolds(t, "B, after the import", b.manager, id, imported.Account.ID)
	b.syncNow(t, id)
	stillHolds(t, "B, after a pass", b.manager, id, imported.Account.ID)
	a.syncNow(t, id)
	stillHolds(t, "A, after reading B", a.manager, id, imported.Account.ID)
}

// The same, for a key the bundle does not hold: imported on the first machine
// after the bundle was made. The machine that imports the bundle has never
// held the key, so a tombstone for it looks like a deletion already done — and,
// honoured, would delete the key on the machine that does hold it.
func TestAKeyTombstonePlantedInABundleForAKeyItLacksDeletesNothing(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := newMachine(t, folder, "dev_laptopa")
	created, err := a.manager.Create(space.CreateRequest{Mnemonic: syncPhrase, Password: syncPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	bundle, err := a.manager.ExportBundle([]space.ExportItem{{ID: id, Password: syncPassword}})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := a.manager.Import(id, "eth-sepolia", account.FamilyEVM, "", bundleImportedKey)
	if err != nil {
		t.Fatal(err)
	}
	a.syncNow(t, id)

	b := newMachine(t, folder, "dev_laptopb")
	if _, err := b.manager.ImportBundle(withKeyTombstone(t, bundle, id, imported.Account.KeyRef)); err != nil {
		t.Fatal(err)
	}
	if err := b.manager.Unlock(id, syncPassword); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	b.syncNow(t, id)
	stillHolds(t, "B, after reading A", b.manager, id, imported.Account.ID)
	a.syncNow(t, id)
	stillHolds(t, "A, after reading B", a.manager, id, imported.Account.ID)
}
