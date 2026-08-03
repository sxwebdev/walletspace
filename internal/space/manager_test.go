package space_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
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

func createWithNileWallet(t *testing.T, manager *space.Manager, password string) space.CreateResult {
	t.Helper()
	result, err := manager.Create(space.CreateRequest{Password: password})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	wallet, err := manager.Derive(result.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatalf("Derive(Tron Nile) error = %v", err)
	}
	result.Accounts = []account.Account{wallet}
	return result
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
	if len(result.Accounts) != 0 || result.Space.AccountCount != 0 {
		t.Fatalf("new space created wallets: result=%+v summary=%+v", result.Accounts, result.Space)
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
	tronAccount, err := manager.Derive(result.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatalf("Derive(tron) error = %v", err)
	}
	accountID := tronAccount.ID
	tronKey, err := manager.ExportPrivateKey(result.Space.ID, accountID, account.FamilyTron)
	if err != nil {
		t.Fatalf("ExportPrivateKey(tron) error = %v", err)
	}
	if _, err := manager.ExportPrivateKey(result.Space.ID, accountID, account.FamilyEVM); !errors.Is(err, space.ErrNetworkBinding) {
		t.Fatalf("ExportPrivateKey(evm) error = %v, want ErrNetworkBinding", err)
	}
	evmAccount, err := manager.Derive(result.Space.ID, "ethereum-mainnet", account.FamilyEVM, "EVM")
	if err != nil {
		t.Fatalf("Derive(evm) error = %v", err)
	}
	evmKey, err := manager.ExportPrivateKey(result.Space.ID, evmAccount.ID, account.FamilyEVM)
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

func TestDerivationStartsAtZeroPerNetworkAndReusesCompatibleWallet(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created := createWithNileWallet(t, manager, "password")
	nileZero := created.Accounts[0]
	nileOne, err := manager.Derive(created.Space.ID, "tron-nile", account.FamilyTron, "Nile 1")
	if err != nil {
		t.Fatalf("Derive(Nile) error = %v", err)
	}
	if nileOne.Index == nil || *nileOne.Index != 1 {
		t.Fatalf("Nile index = %v, want 1", nileOne.Index)
	}

	mainnetZero, err := manager.Derive(
		created.Space.ID, "tron-mainnet", account.FamilyTron, "Mainnet 0",
	)
	if err != nil {
		t.Fatalf("Derive(Tron Mainnet) error = %v", err)
	}
	if mainnetZero.ID != nileZero.ID || !mainnetZero.BoundTo("tron-mainnet") {
		t.Fatalf("Tron index 0 was duplicated instead of bound: %+v", mainnetZero)
	}

	evmZero, err := manager.Derive(
		created.Space.ID, "ethereum-mainnet", account.FamilyEVM, "Ethereum 0",
	)
	if err != nil {
		t.Fatalf("Derive(Ethereum) error = %v", err)
	}
	if evmZero.Index == nil || *evmZero.Index != 0 || evmZero.ID == nileZero.ID {
		t.Fatalf("Ethereum wallet = %+v", evmZero)
	}
	bscZero, err := manager.Derive(
		created.Space.ID, "bsc-mainnet", account.FamilyEVM, "BSC 0",
	)
	if err != nil {
		t.Fatalf("Derive(BSC) error = %v", err)
	}
	if bscZero.ID != evmZero.ID || !bscZero.BoundTo("bsc-mainnet") {
		t.Fatalf("EVM index 0 was duplicated instead of bound: %+v", bscZero)
	}
}

func TestDerivationUsesLowestFreeIndexAfterOutOfOrderBinding(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created, err := manager.Create(space.CreateRequest{Password: "password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	ethereum := make([]account.Account, 3)
	for index := range ethereum {
		ethereum[index], err = manager.Derive(
			created.Space.ID, "ethereum-mainnet", account.FamilyEVM, "",
		)
		if err != nil {
			t.Fatalf("Derive(Ethereum %d) error = %v", index, err)
		}
	}
	if _, err := manager.BindNetwork(
		created.Space.ID, ethereum[2].ID, "bsc-mainnet", account.FamilyEVM,
	); err != nil {
		t.Fatalf("BindNetwork(BSC index 2) error = %v", err)
	}

	bscZero, err := manager.Derive(created.Space.ID, "bsc-mainnet", account.FamilyEVM, "")
	if err != nil {
		t.Fatalf("Derive(BSC 0) error = %v", err)
	}
	if bscZero.ID != ethereum[0].ID || bscZero.Index == nil || *bscZero.Index != 0 {
		t.Fatalf("first free BSC wallet = %+v, want Ethereum index 0", bscZero)
	}
	bscOne, err := manager.Derive(created.Space.ID, "bsc-mainnet", account.FamilyEVM, "")
	if err != nil {
		t.Fatalf("Derive(BSC 1) error = %v", err)
	}
	if bscOne.ID != ethereum[1].ID || bscOne.Index == nil || *bscOne.Index != 1 {
		t.Fatalf("second free BSC wallet = %+v, want Ethereum index 1", bscOne)
	}
}

func TestSignerRequiresAnExplicitNetworkBinding(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created := createWithNileWallet(t, manager, "password")
	accountID := created.Accounts[0].ID
	called := 0
	callback := func(signer chain.Signer) error {
		called++
		if signer.Family() != chain.FamilyTron {
			t.Errorf("signer family = %q", signer.Family())
		}
		return nil
	}
	if err := manager.WithSigner(
		t.Context(), created.Space.ID, accountID, "tron-mainnet", account.FamilyTron, callback,
	); !errors.Is(err, space.ErrNetworkBinding) {
		t.Fatalf("WithSigner(unbound) error = %v, want ErrNetworkBinding", err)
	}
	if called != 0 {
		t.Fatalf("callback calls after rejected binding = %d", called)
	}
	if err := manager.WithSigner(
		t.Context(), created.Space.ID, accountID, "tron-nile", account.FamilyTron, callback,
	); err != nil {
		t.Fatalf("WithSigner(bound) error = %v", err)
	}
	if called != 1 {
		t.Fatalf("callback calls = %d, want 1", called)
	}
}

func TestFailedBindingDoesNotMutateInMemoryState(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created := createWithNileWallet(t, manager, "password")
	accountID := created.Accounts[0].ID
	spacePath := filepath.Join(home, "spaces", created.Space.ID)
	if err := os.RemoveAll(spacePath); err != nil {
		t.Fatalf("RemoveAll(space) error = %v", err)
	}
	if err := os.WriteFile(spacePath, []byte("blocks directory recreation"), 0o600); err != nil {
		t.Fatalf("WriteFile(space path) error = %v", err)
	}

	if _, err := manager.BindNetwork(
		created.Space.ID, accountID, "tron-mainnet", account.FamilyTron,
	); err == nil {
		t.Fatal("BindNetwork() error = nil")
	}
	accounts, err := manager.Accounts(created.Space.ID)
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	if accounts[0].BoundTo("tron-mainnet") {
		t.Fatal("failed binding leaked into in-memory state")
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
	imported, err := manager.Import(
		result.Space.ID, "ethereum-mainnet", account.FamilyEVM, "Treasury", privateKey,
	)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if imported.Account.Kind != account.KindImported {
		t.Errorf("kind = %q", imported.Account.Kind)
	}
	if _, err := manager.Import(
		result.Space.ID, "ethereum-mainnet", account.FamilyEVM, "Duplicate", privateKey,
	); !errors.Is(err, space.ErrDuplicateKey) {
		t.Fatalf("duplicate Import() error = %v", err)
	}
	bound, err := manager.Import(
		result.Space.ID, "bsc-mainnet", account.FamilyEVM, "Duplicate", privateKey,
	)
	if err != nil {
		t.Fatalf("Import(BSC binding) error = %v", err)
	}
	if bound.Account.ID != imported.Account.ID || !bound.Account.BoundTo("bsc-mainnet") {
		t.Fatalf("imported wallet was duplicated: %+v", bound.Account)
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
	if _, err := manager.ExportPrivateKey(
		result.Space.ID, imported.Account.ID, account.Family("unknown"),
	); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Fatalf("ExportPrivateKey(unknown) error = %v, want ErrUnsupportedFamily", err)
	}
}

func TestChangePasswordPreservesAddresses(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	result := createWithNileWallet(t, manager, "old-password")
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

func TestLegacyWalletRequiresExplicitNetworkAssignment(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	mnemonic := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	addresses, err := account.DerivedAddresses(mnemonic, "", 0)
	if err != nil {
		t.Fatalf("DerivedAddresses() error = %v", err)
	}
	created, err := manager.ImportLegacy(space.CreateRequest{
		Name: "legacy", Password: "password", Mnemonic: mnemonic,
	}, []space.LegacyAccount{{
		Index: 0, Label: "Nile", TronAddress: addresses[account.FamilyTron],
	}})
	if err != nil {
		t.Fatalf("ImportLegacy() error = %v", err)
	}
	legacy := created.Accounts[0]
	if legacy.BoundTo("tron-nile") {
		t.Fatal("legacy wallet was assigned without user input")
	}
	assigned, err := manager.BindNetwork(
		created.Space.ID, legacy.ID, "tron-nile", account.FamilyTron,
	)
	if err != nil {
		t.Fatalf("BindNetwork() error = %v", err)
	}
	if assigned.Family != account.FamilyTron || !assigned.BoundTo("tron-nile") ||
		assigned.Addresses[account.FamilyEVM] != "" {
		t.Fatalf("assigned legacy wallet = %+v", assigned)
	}
	if _, err := manager.BindNetwork(
		created.Space.ID, legacy.ID, "ethereum-mainnet", account.FamilyEVM,
	); !errors.Is(err, space.ErrNetworkBinding) {
		t.Fatalf("BindNetwork(wrong family) error = %v, want ErrNetworkBinding", err)
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
