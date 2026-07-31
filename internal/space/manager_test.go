package space_test

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
)

var fastKDF = vault.Params{Time: 1, MemoryKiB: 8 * 1024, Parallelism: 1}

func newManager(t *testing.T, home string) *space.Manager {
	t.Helper()
	manager, err := space.NewManager(home, 15*time.Minute, fastKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func TestCreateDefaultIsAtomicAndUnlocked(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	result, err := manager.Create(space.CreateRequest{Password: "password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if result.Space.Name != "default" || result.Space.Locked {
		t.Errorf("space = %+v", result.Space)
	}
	if !result.MnemonicGenerated || result.Mnemonic == "" {
		t.Error("generated mnemonic was not returned")
	}
	if len(result.Accounts) != 1 || result.Accounts[0].Kind != account.KindDerived {
		t.Fatalf("accounts = %+v", result.Accounts)
	}
	if result.Accounts[0].Addresses[account.FamilyTron] == "" ||
		result.Accounts[0].Addresses[account.FamilyEVM] == "" {
		t.Errorf("addresses = %+v", result.Accounts[0].Addresses)
	}
}

func TestNewPasswordsRequireEightCharacters(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	if _, err := manager.Create(space.CreateRequest{Password: "short"}); err == nil {
		t.Error("Create() accepted a short password")
	}

	result, err := manager.Create(space.CreateRequest{Password: "password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := manager.ChangePassword(result.Space.ID, "password", "short"); err == nil {
		t.Error("ChangePassword() accepted a short password")
	}
}

func TestLockUnlockAndFamilyExport(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	result, err := manager.Create(space.CreateRequest{
		Name: "Trading", Password: "old-password",
		Mnemonic: "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	accountID := result.Accounts[0].ID
	tronKey, err := manager.ExportPrivateKey(result.Space.ID, accountID, account.FamilyTron)
	if err != nil {
		t.Fatalf("ExportPrivateKey(tron) error = %v", err)
	}
	evmKey, err := manager.ExportPrivateKey(result.Space.ID, accountID, account.FamilyEVM)
	if err != nil {
		t.Fatalf("ExportPrivateKey(evm) error = %v", err)
	}
	if tronKey == evmKey {
		t.Error("derived family keys are equal")
	}
	if err := manager.Lock(result.Space.ID); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if _, err := manager.ExportPrivateKey(result.Space.ID, accountID, account.FamilyTron); !errors.Is(err, space.ErrLocked) {
		t.Fatalf("locked ExportPrivateKey() error = %v, want ErrLocked", err)
	}
	if err := manager.Unlock(result.Space.ID, "wrong"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("Unlock(wrong) error = %v", err)
	}
	if err := manager.Unlock(result.Space.ID, "old-password"); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
}

func TestImportedKeyPersistsAndDeduplicates(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	result, err := manager.Create(space.CreateRequest{Password: "password", ImportedOnly: true})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	privateKey := "0000000000000000000000000000000000000000000000000000000000000001"
	imported, err := manager.Import(result.Space.ID, "Treasury", privateKey)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if imported.Account.Kind != account.KindImported {
		t.Errorf("kind = %q", imported.Account.Kind)
	}
	if _, err := manager.Import(result.Space.ID, "Duplicate", privateKey); !errors.Is(err, space.ErrDuplicateKey) {
		t.Fatalf("duplicate Import() error = %v", err)
	}
	if err := manager.Lock(result.Space.ID); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.Unlock(result.Space.ID, "password"); err != nil {
		t.Fatalf("Unlock() after import error = %v", err)
	}
	exported, err := manager.ExportPrivateKey(result.Space.ID, imported.Account.ID, account.FamilyEVM)
	if err != nil {
		t.Fatalf("ExportPrivateKey() error = %v", err)
	}
	if exported != privateKey {
		t.Errorf("exported key = %q", exported)
	}
}

func TestChangePasswordPreservesAddresses(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	result, err := manager.Create(space.CreateRequest{Password: "old-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	before := result.Accounts[0].Addresses
	if err := manager.ChangePassword(result.Space.ID, "old-password", "new-password"); err != nil {
		t.Fatalf("ChangePassword() error = %v", err)
	}
	if err := manager.Lock(result.Space.ID); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.Unlock(result.Space.ID, "old-password"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("old password error = %v", err)
	}
	if err := manager.Unlock(result.Space.ID, "new-password"); err != nil {
		t.Fatalf("new password error = %v", err)
	}
	_, accounts, err := manager.Get(result.Space.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	for family, want := range before {
		if got := accounts[0].Addresses[family]; got != want {
			t.Errorf("%s address = %q, want %q", family, got, want)
		}
	}
}

func TestCreateFirstSpaceIsRaceSafe(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	const workers = 8
	var wait sync.WaitGroup
	results := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := manager.Create(space.CreateRequest{
				Password: "password", ExpectEmpty: true,
			})
			results <- err
		}()
	}
	wait.Wait()
	close(results)

	var created, rejected int
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, space.ErrFirstSpaceExists):
			rejected++
		default:
			t.Fatalf("Create() error = %v", err)
		}
	}
	if created != 1 || rejected != workers-1 {
		t.Fatalf("created = %d, rejected = %d", created, rejected)
	}
	if got := len(manager.List()); got != 1 {
		t.Fatalf("spaces = %d, want 1", got)
	}
}

func TestImportLegacyVerifiesEverythingBeforePublishing(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	mnemonic := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	addresses, err := account.DerivedAddresses(mnemonic, "", 0)
	if err != nil {
		t.Fatalf("DerivedAddresses() error = %v", err)
	}
	_, err = manager.ImportLegacy(space.CreateRequest{
		Name: "legacy", Password: "password", Mnemonic: mnemonic,
	}, []space.LegacyAccount{
		{Index: 0, Label: "valid", TronAddress: addresses[account.FamilyTron]},
		{Index: 1, Label: "invalid", TronAddress: "TInvalidAddress"},
	})
	if err == nil {
		t.Fatal("ImportLegacy() error = nil")
	}
	if got := len(manager.List()); got != 0 {
		t.Fatalf("spaces after failed import = %d, want 0", got)
	}
}

func TestEncryptedBackupCanBeRestoredAndUnlocked(t *testing.T) {
	t.Parallel()

	source := newManager(t, t.TempDir())
	created, err := source.Create(space.CreateRequest{
		Name: "backup", Password: "correct-password",
		Mnemonic: "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	backup, err := source.Backup(created.Space.ID)
	if err != nil {
		t.Fatalf("Backup() error = %v", err)
	}
	if strings.Contains(string(backup), created.Mnemonic) {
		t.Fatal("backup contains plaintext mnemonic")
	}

	restoreHome := t.TempDir()
	restorePath := filepath.Join(restoreHome, "spaces", created.Space.ID, "space.json")
	if err := storage.AtomicWrite(restorePath, backup); err != nil {
		t.Fatalf("AtomicWrite(backup) error = %v", err)
	}
	restored := newManager(t, restoreHome)
	if err := restored.Unlock(created.Space.ID, "wrong-password"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("Unlock(wrong) error = %v", err)
	}
	if err := restored.Unlock(created.Space.ID, "correct-password"); err != nil {
		t.Fatalf("Unlock(correct) error = %v", err)
	}
	mnemonic, err := restored.Mnemonic(created.Space.ID)
	if err != nil {
		t.Fatalf("Mnemonic() error = %v", err)
	}
	if mnemonic != created.Mnemonic {
		t.Errorf("restored mnemonic = %q, want %q", mnemonic, created.Mnemonic)
	}
}
