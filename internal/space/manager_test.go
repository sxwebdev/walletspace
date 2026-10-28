package space_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// The cheapest parameters the vault will accept. Anything lower is refused, so
// this is also a check that the floor is not accidentally raised past what a
// test can afford.
var fastKDF = vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}

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
	result, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
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

// The vault password is the only thing between a copy of the encrypted backup
// and the seed inside it, and an offline attacker gets unlimited attempts.
func TestNewPasswordsAreScoredNotJustMeasured(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	weak := []struct {
		name     string
		password string
	}{
		{name: "empty", password: ""},
		{name: "short", password: "short"},
		{name: "one under the minimum", password: "12345678901"},
		{name: "top of every wordlist", password: "password123"},
		{name: "the xkcd example everyone uses", password: "correcthorsebatterystaple"},
		{name: "one character repeated", password: "aaaaaaaaaaaaaaaa"},
	}
	for _, tt := range weak {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := manager.Create(space.CreateRequest{Password: tt.password}); err == nil {
				t.Errorf("Create() accepted %q", tt.password)
			}
		})
	}

	result, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := manager.ChangePassword(result.Space.ID, "test-vault-password", "short"); err == nil {
		t.Error("ChangePassword() accepted a short password")
	}
}

func TestLockUnlockAndFamilyExport(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	result, err := manager.Create(space.CreateRequest{
		Name: "Trading", Password: "old-vault-password",
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
	tronKey, err := manager.ExportPrivateKey(result.Space.ID, accountID, account.FamilyTron, "old-vault-password")
	if err != nil {
		t.Fatalf("ExportPrivateKey(tron) error = %v", err)
	}
	if _, err := manager.ExportPrivateKey(result.Space.ID, accountID, account.FamilyEVM, "old-vault-password"); !errors.Is(err, space.ErrNetworkBinding) {
		t.Fatalf("ExportPrivateKey(evm) error = %v, want ErrNetworkBinding", err)
	}
	evmAccount, err := manager.Derive(result.Space.ID, "ethereum-mainnet", account.FamilyEVM, "EVM")
	if err != nil {
		t.Fatalf("Derive(evm) error = %v", err)
	}
	evmKey, err := manager.ExportPrivateKey(result.Space.ID, evmAccount.ID, account.FamilyEVM, "old-vault-password")
	if err != nil {
		t.Fatalf("ExportPrivateKey(evm) error = %v", err)
	}
	if tronKey == evmKey {
		t.Error("derived family keys are equal")
	}
	if err := manager.Lock(result.Space.ID); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if _, err := manager.ExportPrivateKey(result.Space.ID, accountID, account.FamilyTron, "old-vault-password"); !errors.Is(err, space.ErrLocked) {
		t.Fatalf("locked ExportPrivateKey() error = %v, want ErrLocked", err)
	}
	if err := manager.Unlock(result.Space.ID, "wrong"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("Unlock(wrong) error = %v", err)
	}
	if err := manager.Unlock(result.Space.ID, "old-vault-password"); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
}

func TestDerivationStartsAtZeroPerNetworkAndReusesCompatibleWallet(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created := createWithNileWallet(t, manager, "test-vault-password")
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
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
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
	created := createWithNileWallet(t, manager, "test-vault-password")
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
	created := createWithNileWallet(t, manager, "test-vault-password")
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

func TestDeriveNetworksTakesAnIndexFreeOnEveryChosenNetwork(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	spaceID := created.Space.ID
	derive := func(networkIDs ...string) account.Account {
		t.Helper()
		item, err := manager.DeriveNetworks(spaceID, networkIDs, account.FamilyEVM, "")
		if err != nil {
			t.Fatalf("DeriveNetworks(%v) error = %v", networkIDs, err)
		}
		return item
	}
	// Index 0 on Ethereum and BSC, index 1 on BSC alone.
	evmZero := derive("ethereum-mainnet", "bsc-mainnet")
	bscOne := derive("bsc-mainnet")
	if *evmZero.Index != 0 || *bscOne.Index != 1 ||
		!slices.Equal(evmZero.NetworkIDs, []string{"bsc-mainnet", "ethereum-mainnet"}) {
		t.Fatalf("setup wallets = %+v, %+v", evmZero, bscOne)
	}

	// Base is free everywhere, BSC is taken at 0 and 1: the wallet is new at 2.
	fresh := derive("base-mainnet", "bsc-mainnet", "base-mainnet")
	if fresh.Index == nil || *fresh.Index != 2 ||
		!slices.Equal(fresh.NetworkIDs, []string{"base-mainnet", "bsc-mainnet"}) {
		t.Fatalf("fresh wallet = %+v, want index 2 on Base and BSC once each", fresh)
	}
	want, err := account.DerivedAddress(mnemonic(t, manager, spaceID), "", account.FamilyEVM, 2)
	if err != nil {
		t.Fatalf("DerivedAddress() error = %v", err)
	}
	if fresh.Addresses[account.FamilyEVM] != want {
		t.Fatalf("fresh address = %s, want index 2 address %s", fresh.Addresses[account.FamilyEVM], want)
	}

	// Ethereum uses 0, Base uses 2: index 1 is free on both, and the BSC
	// wallet at index 1 is that same key, so it gains the two networks.
	reused := derive("ethereum-mainnet", "base-mainnet")
	if reused.ID != bscOne.ID ||
		!slices.Equal(reused.NetworkIDs, []string{"base-mainnet", "bsc-mainnet", "ethereum-mainnet"}) {
		t.Fatalf("index 1 wallet = %+v, want %s on Base, BSC and Ethereum", reused, bscOne.ID)
	}
	accounts, err := manager.Accounts(spaceID)
	if err != nil || len(accounts) != 3 {
		t.Fatalf("accounts = %d (%v), want 3: no key stored twice", len(accounts), err)
	}

	for _, networkIDs := range [][]string{nil, {"ethereum-mainnet", ""}} {
		if _, err := manager.DeriveNetworks(spaceID, networkIDs, account.FamilyEVM, ""); err == nil {
			t.Errorf("DeriveNetworks(%q) error = nil", networkIDs)
		}
	}
}

func mnemonic(t *testing.T, manager *space.Manager, spaceID string) string {
	t.Helper()
	value, err := manager.Mnemonic(spaceID, "test-vault-password")
	if err != nil {
		t.Fatalf("Mnemonic() error = %v", err)
	}
	return value
}

func TestImportNetworksBindsTheKeyOnceAcrossNetworks(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	result, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	spaceID := result.Space.ID
	privateKey := "0000000000000000000000000000000000000000000000000000000000000002"
	// secp256k1 serves EVM and Tron alike: one import reaches both.
	imported, err := manager.ImportNetworks(
		spaceID, []string{"ethereum-mainnet", "tron-nile"}, account.FamilyEVM, "Ops", privateKey,
	)
	if err != nil {
		t.Fatalf("ImportNetworks() error = %v", err)
	}
	if !slices.Equal(imported.Account.NetworkIDs, []string{"ethereum-mainnet", "tron-nile"}) ||
		imported.Account.Addresses[account.FamilyEVM] == "" || imported.Account.Addresses[account.FamilyTron] == "" {
		t.Fatalf("imported wallet = %+v", imported.Account)
	}
	if _, err := manager.ImportNetworks(
		spaceID, []string{"tron-nile", "ethereum-mainnet"}, account.FamilyEVM, "", privateKey,
	); !errors.Is(err, space.ErrDuplicateKey) {
		t.Fatalf("ImportNetworks(same networks) error = %v, want ErrDuplicateKey", err)
	}
	// One new network among known ones binds the new one to the same wallet.
	widened, err := manager.ImportNetworks(
		spaceID, []string{"ethereum-mainnet", "bsc-mainnet"}, account.FamilyEVM, "", privateKey,
	)
	if err != nil {
		t.Fatalf("ImportNetworks(add BSC) error = %v", err)
	}
	if widened.Account.ID != imported.Account.ID ||
		!slices.Equal(widened.Account.NetworkIDs, []string{"bsc-mainnet", "ethereum-mainnet", "tron-nile"}) {
		t.Fatalf("widened wallet = %+v", widened.Account)
	}
}

func TestImportedKeyPersistsAndDeduplicates(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	result, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
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
	if err := manager.Unlock(result.Space.ID, "test-vault-password"); err != nil {
		t.Fatalf("Unlock() after import error = %v", err)
	}
	exported, err := manager.ExportPrivateKey(result.Space.ID, imported.Account.ID, account.FamilyEVM, "test-vault-password")
	if err != nil {
		t.Fatalf("ExportPrivateKey() error = %v", err)
	}
	if exported != privateKey {
		t.Errorf("exported key = %q", exported)
	}
	if _, err := manager.ExportPrivateKey(
		result.Space.ID, imported.Account.ID, account.Family("unknown"), "test-vault-password",
	); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Fatalf("ExportPrivateKey(unknown) error = %v, want ErrUnsupportedFamily", err)
	}
}

func TestChangePasswordPreservesAddresses(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	result := createWithNileWallet(t, manager, "old-vault-password")
	before := result.Accounts[0].Addresses
	if err := manager.ChangePassword(result.Space.ID, "old-vault-password", "new-password"); err != nil {
		t.Fatalf("ChangePassword() error = %v", err)
	}
	if err := manager.Lock(result.Space.ID); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.Unlock(result.Space.ID, "old-vault-password"); !errors.Is(err, vault.ErrInvalidPassword) {
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
		wait.Go(func() {
			_, err := manager.Create(space.CreateRequest{
				Password: "test-vault-password", ExpectEmpty: true,
			})
			results <- err
		})
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
		Name: "legacy", Password: "test-vault-password", Mnemonic: mnemonic,
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
		Name: "legacy", Password: "test-vault-password", Mnemonic: mnemonic,
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
	// The file is the whole vault, so it is a step-up like the exports are.
	if _, err := source.Backup(created.Space.ID, ""); !errors.Is(err, space.ErrPasswordRequired) {
		t.Fatalf("Backup(no password) error = %v, want ErrPasswordRequired", err)
	}
	if _, err := source.Backup(created.Space.ID, "wrong-password"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("Backup(wrong password) error = %v, want ErrInvalidPassword", err)
	}
	backup, err := source.Backup(created.Space.ID, "correct-password")
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
	mnemonic, err := restored.Mnemonic(created.Space.ID, "correct-password")
	if err != nil {
		t.Fatalf("Mnemonic() error = %v", err)
	}
	if mnemonic != created.Mnemonic {
		t.Errorf("restored mnemonic = %q, want %q", mnemonic, created.Mnemonic)
	}
}

// A password that no longer opens the vault must not still be authorising
// transfers. The session used to be refreshed in place, so the window a
// replaced password had bought outlived it.
func TestChangingThePasswordClosesTheSpendingWindow(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created, err := manager.Create(space.CreateRequest{Name: "spend", Password: "first-vault-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID
	if _, err := manager.ConfirmSend(t.Context(), id, "first-vault-password"); err != nil {
		t.Fatalf("ConfirmSend() error = %v", err)
	}
	if err := manager.RequireSendConfirmation(id); err != nil {
		t.Fatalf("RequireSendConfirmation() after confirming = %v, want nil", err)
	}

	if err := manager.ChangePassword(id, "first-vault-password", "second-vault-password"); err != nil {
		t.Fatalf("ChangePassword() error = %v", err)
	}
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, space.ErrSendConfirmationRequired) {
		t.Errorf("RequireSendConfirmation() after the password changed = %v, want the window closed", err)
	}

	// The space itself stays open, and its decrypted payload with it: the
	// session is replaced, not dropped, so nobody is locked out of a space by
	// changing its password.
	summary, _, err := manager.Get(id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if summary.Locked {
		t.Fatal("changing the password locked the space")
	}
	if _, err := manager.Derive(id, "tron-nile", account.FamilyTron, ""); err != nil {
		t.Fatalf("Derive() after the password changed = %v", err)
	}
	// And the new password buys a window, which the old one no longer can.
	if _, err := manager.ConfirmSend(t.Context(), id, "first-vault-password"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("ConfirmSend(replaced password) error = %v, want ErrInvalidPassword", err)
	}
	if _, err := manager.ConfirmSend(t.Context(), id, "second-vault-password"); err != nil {
		t.Fatalf("ConfirmSend(new password) error = %v", err)
	}
	if err := manager.RequireSendConfirmation(id); err != nil {
		t.Errorf("RequireSendConfirmation() after re-confirming = %v, want nil", err)
	}

	// Changing a locked space's password has no session to replace, and must
	// not produce one: the key it derives to check the old password is not an
	// unlocking.
	if err := manager.Lock(id); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.ChangePassword(id, "second-vault-password", "third-vault-password"); err != nil {
		t.Fatalf("ChangePassword(locked) error = %v", err)
	}
	summary, _, err = manager.Get(id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !summary.Locked {
		t.Error("changing a locked space's password unlocked it")
	}
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, space.ErrLocked) {
		t.Errorf("RequireSendConfirmation() on the locked space = %v, want ErrLocked", err)
	}
}

func TestUnbindNetworkKeepsTheWalletOnItsOtherNetworks(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created := createWithNileWallet(t, manager, "test-vault-password")
	wallet := created.Accounts[0]
	if _, err := manager.BindNetwork(
		created.Space.ID, wallet.ID, "tron-mainnet", account.FamilyTron,
	); err != nil {
		t.Fatalf("BindNetwork() error = %v", err)
	}

	updated, err := manager.UnbindNetwork(created.Space.ID, wallet.ID, "tron-mainnet")
	if err != nil {
		t.Fatalf("UnbindNetwork() error = %v", err)
	}
	if updated.BoundTo("tron-mainnet") || !updated.BoundTo("tron-nile") ||
		updated.Addresses[account.FamilyTron] != wallet.Addresses[account.FamilyTron] {
		t.Fatalf("unbound wallet = %+v", updated)
	}
	// The record on disk lost the network too, not only the one in memory.
	_, persisted, err := newManager(t, home).Get(created.Space.ID)
	if err != nil || len(persisted) != 1 || persisted[0].BoundTo("tron-mainnet") {
		t.Fatalf("persisted accounts = %+v, err = %v", persisted, err)
	}
	// Nothing signs for a network the wallet was taken off.
	if err := manager.WithSigner(
		t.Context(), created.Space.ID, wallet.ID, "tron-mainnet", account.FamilyTron,
		func(chain.Signer) error { return nil },
	); !errors.Is(err, space.ErrNetworkBinding) {
		t.Fatalf("WithSigner(unbound) error = %v, want ErrNetworkBinding", err)
	}

	for _, tt := range []struct {
		name      string
		accountID string
		networkID string
		want      error
	}{
		{"network already off", wallet.ID, "tron-mainnet", space.ErrNetworkBinding},
		{"last network", wallet.ID, "tron-nile", space.ErrLastNetwork},
		{"unknown wallet", "acc_missing", "tron-nile", space.ErrAccountNotFound},
	} {
		if _, err := manager.UnbindNetwork(created.Space.ID, tt.accountID, tt.networkID); !errors.Is(err, tt.want) {
			t.Errorf("%s: UnbindNetwork() error = %v, want %v", tt.name, err, tt.want)
		}
	}
	accounts, err := manager.Accounts(created.Space.ID)
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	if !accounts[0].BoundTo("tron-nile") {
		t.Fatalf("refused unbinding changed the wallet: %+v", accounts[0])
	}
}

func TestDeleteDerivedAccountCanBeDerivedAgain(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created := createWithNileWallet(t, manager, "test-vault-password")
	wallet := created.Accounts[0]

	if err := manager.DeleteAccount(created.Space.ID, wallet.ID, "test-vault-password"); err != nil {
		t.Fatalf("DeleteAccount() error = %v", err)
	}
	accounts, err := manager.Accounts(created.Space.ID)
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts after delete = %+v, want none", accounts)
	}
	// The record is gone from disk, not only from memory.
	if _, persisted, err := newManager(t, home).Get(created.Space.ID); err != nil || len(persisted) != 0 {
		t.Fatalf("persisted accounts = %+v, err = %v; want none", persisted, err)
	}

	// Nothing is lost: the lowest free index on that network is the same one.
	again, err := manager.Derive(created.Space.ID, "tron-nile", account.FamilyTron, "")
	if err != nil {
		t.Fatalf("Derive() after delete error = %v", err)
	}
	if again.Addresses[account.FamilyTron] != wallet.Addresses[account.FamilyTron] || *again.Index != *wallet.Index {
		t.Fatalf("re-derived wallet = %+v, want the deleted %+v back", again, wallet)
	}
}

func TestDeleteImportedAccountRemovesItsKey(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	result, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	privateKey := "0000000000000000000000000000000000000000000000000000000000000001"
	imported, err := manager.Import(result.Space.ID, "ethereum-mainnet", account.FamilyEVM, "Hot", privateKey)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}

	if err := manager.DeleteAccount(result.Space.ID, imported.Account.ID, "test-vault-password"); err != nil {
		t.Fatalf("DeleteAccount() error = %v", err)
	}
	if _, err := manager.ExportPrivateKey(
		result.Space.ID, imported.Account.ID, account.FamilyEVM, "test-vault-password",
	); !errors.Is(err, space.ErrAccountNotFound) {
		t.Fatalf("ExportPrivateKey(deleted) error = %v, want ErrAccountNotFound", err)
	}

	// The resealed vault still opens and verifies against the remaining
	// records: a key left behind, or a record without its key, would fail here.
	if err := manager.Lock(result.Space.ID); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.Unlock(result.Space.ID, "test-vault-password"); err != nil {
		t.Fatalf("Unlock() after delete error = %v", err)
	}
	// The key is no longer known to the vault, so importing it again is a new
	// wallet rather than a duplicate.
	again, err := manager.Import(result.Space.ID, "ethereum-mainnet", account.FamilyEVM, "Hot", privateKey)
	if err != nil {
		t.Fatalf("Import() after delete error = %v", err)
	}
	if again.Account.ID == imported.Account.ID {
		t.Fatal("re-import returned the deleted account")
	}
}

func TestDeleteAccountRefusals(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created := createWithNileWallet(t, manager, "test-vault-password")
	spaceID, accountID := created.Space.ID, created.Accounts[0].ID

	if err := manager.DeleteAccount(spaceID, accountID, ""); !errors.Is(err, space.ErrPasswordRequired) {
		t.Errorf("DeleteAccount(no password) error = %v, want ErrPasswordRequired", err)
	}
	if err := manager.DeleteAccount(spaceID, accountID, "not-the-password"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Errorf("DeleteAccount(wrong password) error = %v, want ErrInvalidPassword", err)
	}
	if err := manager.DeleteAccount(spaceID, "acc_missing", "test-vault-password"); !errors.Is(err, space.ErrAccountNotFound) {
		t.Errorf("DeleteAccount(unknown) error = %v, want ErrAccountNotFound", err)
	}
	if err := manager.Lock(spaceID); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.DeleteAccount(spaceID, accountID, "test-vault-password"); !errors.Is(err, space.ErrLocked) {
		t.Errorf("DeleteAccount(locked) error = %v, want ErrLocked", err)
	}

	accounts, err := manager.Accounts(spaceID)
	if err != nil {
		t.Fatalf("Accounts() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("a refused delete removed the wallet: %+v", accounts)
	}
}

func TestFailedDeleteKeepsTheKeyInTheSession(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	result, err := manager.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	privateKey := "0000000000000000000000000000000000000000000000000000000000000001"
	imported, err := manager.Import(result.Space.ID, "ethereum-mainnet", account.FamilyEVM, "", privateKey)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	spacePath := filepath.Join(home, "spaces", result.Space.ID)
	if err := os.RemoveAll(spacePath); err != nil {
		t.Fatalf("RemoveAll(space) error = %v", err)
	}
	if err := os.WriteFile(spacePath, []byte("blocks directory recreation"), 0o600); err != nil {
		t.Fatalf("WriteFile(space path) error = %v", err)
	}

	if err := manager.DeleteAccount(result.Space.ID, imported.Account.ID, "test-vault-password"); err == nil {
		t.Fatal("DeleteAccount() error = nil, want the write failure")
	}
	exported, err := manager.ExportPrivateKey(
		result.Space.ID, imported.Account.ID, account.FamilyEVM, "test-vault-password",
	)
	if err != nil || exported != privateKey {
		t.Fatalf("after a failed delete ExportPrivateKey() = %q, %v; want the key still there", exported, err)
	}

	// Nor does the session keep the key tombstone of the delete that failed:
	// the next write of the vault would carry it, and sync would delete the
	// key everywhere.
	if err := os.Remove(spacePath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(spacePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Import(result.Space.ID, "ethereum-mainnet", account.FamilyEVM, "", bundleImportedKey); err != nil {
		t.Fatal(err)
	}
	keys, deleted, err := space.VaultContents(home, result.Space.ID, "test-vault-password")
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 || keys[imported.Account.KeyRef] != 1 {
		t.Fatalf("vault keys %v, key tombstones %v after a failed delete; want the key and no tombstone", keys, deleted)
	}
}
