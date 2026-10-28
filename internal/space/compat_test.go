package space_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/space"
)

// testdata/head-98adcab holds a space file written by the code at commit
// 98adcab, before ed25519 existed: a legacy two-family record (index 2, BIP39
// passphrase), a derived Tron and a derived EVM wallet, and one imported
// secp256k1 key bound to Ethereum and Nile. The expectations below are what
// that same code answered for it — signatures over one digest, exports — so
// this test proves the file opens, verifies and behaves byte for byte as it
// did.
const (
	headSpaceID  = "spc_pm426g4plnykx6uycq2a"
	headPassword = "fixture-vault-password"
	headLegacyID = "acc_sfei6bi3h745r3qfwbeq"
)

func TestSpaceWrittenBeforeEd25519StillWorks(t *testing.T) {
	t.Parallel()

	home, original := headFixtureHome(t)
	target := filepath.Join(home, "spaces", headSpaceID, "space.json")

	manager := newManager(t, home)
	if err := manager.Unlock(headSpaceID, headPassword); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}

	digest := sha256.Sum256([]byte("walletspace head fixture"))
	tests := []struct {
		accountID, networkID string
		family               account.Family
		publicKey, signature string
		exported             string
	}{
		{
			"acc_i6poz5hseqro7jyftraq", "tron-nile", account.FamilyTron,
			"0457095ed6aede044d276e47c484f863d5adbf4d1105aa0a5e3d758e458f7f8efd126dcbcb2cbd7c23125ecaf2737e7df9d9f63ee8b99c9aa795ac627d0b9e2a78",
			"0de100423192d9d22956f6543f87939f2b6cc8c05a40fa32633e3e3245d7d8270443cf951c796e84085f8af86d5210d693a2498f18231b7a8c3e1a7c0fd3c7e900",
			"81d9f1e594fd68e033f5fdeb04fd9ed30b4608cd2f70994fe5e12ef8fa3164b7",
		},
		{
			"acc_4ebvkiulcsvcxwjnlorq", "ethereum-mainnet", account.FamilyEVM,
			"044406ea7b04aeed7f3e25242344c3359a6def5a8e1ac20502a800d2dd108f59ad3761e168aa405592cb9b477ecad58341c017111e10d665fce7c714a1260dac92",
			"40388fcd9d7588ce5870dcfb8c1e5fdf1dd5e4a71af0e8b5479dddbad8d344ef43c8d2103cd1d2b6e74f3fd59374de6c03aae1209cdcfdb6a325e1f52748816401",
			"73cd913846c7f8ebba707d31662c127e7068b08d790c11d26490e27992dcd171",
		},
		{
			"acc_s5kctjpiqlvfa5kqq46a", "ethereum-mainnet", account.FamilyEVM,
			"044e3b81af9c2234cad09d679ce6035ed1392347ce64ce405f5dcd36228a25de6e47fd35c4215d1edf53e6f83de344615ce719bdb0fd878f6ed76f06dd277956de",
			"07f90220705b36ec673b5afde1fcb372c029fa850c954ad33644a47a990ca6c06d56084a050d423942861bfb0f801124063f4cd924520721911ebe3929f5d7df01",
			"4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318",
		},
		{
			"acc_s5kctjpiqlvfa5kqq46a", "tron-nile", account.FamilyTron,
			"044e3b81af9c2234cad09d679ce6035ed1392347ce64ce405f5dcd36228a25de6e47fd35c4215d1edf53e6f83de344615ce719bdb0fd878f6ed76f06dd277956de",
			"07f90220705b36ec673b5afde1fcb372c029fa850c954ad33644a47a990ca6c06d56084a050d423942861bfb0f801124063f4cd924520721911ebe3929f5d7df01",
			"4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318",
		},
	}
	for _, tt := range tests {
		err := manager.WithSigner(t.Context(), headSpaceID, tt.accountID, tt.networkID, tt.family,
			func(signer chain.Signer) error {
				if got := hex.EncodeToString(signer.PublicKey()); got != tt.publicKey {
					t.Errorf("%s/%s public key = %s, want %s", tt.accountID, tt.family, got, tt.publicKey)
				}
				signature, err := signer.SignDigest(t.Context(), digest[:])
				if err != nil {
					return err
				}
				if got := hex.EncodeToString(signature); got != tt.signature {
					t.Errorf("%s/%s signature = %s, want %s", tt.accountID, tt.family, got, tt.signature)
				}
				return nil
			})
		if err != nil {
			t.Fatalf("WithSigner(%s, %s) error = %v", tt.accountID, tt.family, err)
		}
		exported, err := manager.ExportPrivateKey(headSpaceID, tt.accountID, tt.family, headPassword)
		if err != nil || exported != tt.exported {
			t.Errorf("ExportPrivateKey(%s, %s) = %q, %v; want %s", tt.accountID, tt.family, exported, err, tt.exported)
		}
	}
	for family, want := range map[account.Family]string{
		account.FamilyTron: "69e1a5b584114310b7eb5762611766ea34888c62746e23caa52b8ee2d6855496",
		account.FamilyEVM:  "6946e420ab737e06ff47327c1c8570d1753250d891f5c2f0c6d8f9d2d1c8b339",
	} {
		exported, err := manager.ExportPrivateKey(headSpaceID, headLegacyID, family, headPassword)
		if err != nil || exported != want {
			t.Errorf("ExportPrivateKey(legacy, %s) = %q, %v; want %s", family, exported, err, want)
		}
	}

	// Reading, signing and exporting wrote nothing.
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("using the old space rewrote its file")
	}

	// None of the old wallets is, or can become, a Solana wallet.
	for _, accountID := range []string{headLegacyID, "acc_i6poz5hseqro7jyftraq", "acc_4ebvkiulcsvcxwjnlorq", "acc_s5kctjpiqlvfa5kqq46a"} {
		if _, err := manager.BindNetwork(headSpaceID, accountID, "solana-devnet", account.FamilySolana); err == nil {
			t.Errorf("BindNetwork(%s, solana) error = nil", accountID)
		}
	}
	// A Solana wallet is added beside them, and the space still verifies.
	wallet, err := manager.Derive(headSpaceID, "solana-devnet", account.FamilySolana, "")
	if err != nil {
		t.Fatalf("Derive(solana) error = %v", err)
	}
	address, err := account.DerivedAddress(
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
		"walletspace-fixture", account.FamilySolana, 0,
	)
	if err != nil || wallet.Addresses[account.FamilySolana] != address {
		t.Fatalf("Solana wallet = %+v, want %s (%v)", wallet, address, err)
	}
	if err := manager.Lock(headSpaceID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unlock(headSpaceID, headPassword); err != nil {
		t.Fatalf("Unlock() after adding Solana error = %v", err)
	}
	accounts, err := manager.Accounts(headSpaceID)
	if err != nil || len(accounts) != 5 {
		t.Fatalf("Accounts() = %d, %v; want 5", len(accounts), err)
	}
	if _, err := manager.ExportPrivateKey(headSpaceID, wallet.ID, account.FamilySolana, headPassword); err != nil {
		t.Fatalf("ExportPrivateKey(solana) error = %v", err)
	}
	// A fresh process reads the extended file and verifies it too.
	if err := newManager(t, home).Unlock(headSpaceID, headPassword); err != nil {
		t.Fatalf("Unlock() in a new manager error = %v", err)
	}
}

// headFixtureHome copies the HEAD space file into a fresh home and returns the
// home and the file's bytes.
func headFixtureHome(t *testing.T) (string, []byte) {
	t.Helper()
	home := t.TempDir()
	original, err := os.ReadFile(filepath.Join("testdata", "head-98adcab", "spaces", headSpaceID, "space.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, "spaces", headSpaceID, "space.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	return home, original
}

// A pre-binding record carries only its Tron and EVM addresses. Its key is
// offered for those two families and never derived along the Solana path:
// that would be an address the wallet never showed and unlock never checked.
// The record is given bindings on disk (nothing in the vault covers
// network_ids) so that the refusal has to come from the key lookup itself,
// not from the binding check in front of it.
func TestLegacyRecordIsNotASolanaWallet(t *testing.T) {
	t.Parallel()

	home, _ := headFixtureHome(t)
	tamperAccount(t, home, headSpaceID, headLegacyID, func(item map[string]any) {
		item["network_ids"] = []any{"ethereum-mainnet", "solana-devnet", "tron-nile"}
	})
	manager := newManager(t, home)
	if err := manager.Unlock(headSpaceID, headPassword); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}

	if _, err := manager.ExportPrivateKey(headSpaceID, headLegacyID, account.FamilySolana, headPassword); !errors.Is(err, space.ErrNetworkBinding) {
		t.Errorf("ExportPrivateKey(legacy, solana) error = %v, want ErrNetworkBinding", err)
	}
	called := false
	err := manager.WithMessageSigner(t.Context(), headSpaceID, headLegacyID, "solana-devnet", account.FamilySolana,
		func(chain.MessageSigner) error { called = true; return nil })
	if !errors.Is(err, space.ErrNetworkBinding) || called {
		t.Errorf("WithMessageSigner(legacy) error = %v, called = %v; want ErrNetworkBinding before the callback", err, called)
	}

	// Its Tron and EVM keys are exactly what HEAD exported and signed with.
	for family, want := range map[account.Family]string{
		account.FamilyTron: "69e1a5b584114310b7eb5762611766ea34888c62746e23caa52b8ee2d6855496",
		account.FamilyEVM:  "6946e420ab737e06ff47327c1c8570d1753250d891f5c2f0c6d8f9d2d1c8b339",
	} {
		exported, err := manager.ExportPrivateKey(headSpaceID, headLegacyID, family, headPassword)
		if err != nil || exported != want {
			t.Errorf("ExportPrivateKey(legacy, %s) = %q, %v; want %s", family, exported, err, want)
		}
	}
	for networkID, family := range map[string]account.Family{
		"tron-nile": account.FamilyTron, "ethereum-mainnet": account.FamilyEVM,
	} {
		if err := manager.WithSigner(t.Context(), headSpaceID, headLegacyID, networkID, family,
			func(signer chain.Signer) error {
				_, err := signer.SignDigest(t.Context(), make([]byte, 32))
				return err
			}); err != nil {
			t.Errorf("WithSigner(legacy, %s) error = %v", family, err)
		}
	}
}

// The fixture's vault holds one key per reference, as every vault before key
// variants does. Carried to another machine in a backup file and opened
// there, its imported wallet keeps its id, its reference and its key; the
// first change rewrites the vault with variants, and after a restart nothing
// about the key has moved.
func TestAnOldVaultCarriedInABackupKeepsItsKey(t *testing.T) {
	t.Parallel()

	const importedID = "acc_s5kctjpiqlvfa5kqq46a"
	const importedKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"
	_, original := headFixtureHome(t)
	home := t.TempDir()
	manager := newManager(t, home)
	if result, err := manager.ImportBundle(original); err != nil || len(result.Imported) != 1 {
		t.Fatalf("ImportBundle() = %+v, %v", result, err)
	}
	if err := manager.Unlock(headSpaceID, headPassword); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	before, err := manager.Accounts(headSpaceID)
	if err != nil {
		t.Fatal(err)
	}
	if exported, err := manager.ExportPrivateKey(headSpaceID, importedID, account.FamilyEVM, headPassword); err != nil || exported != importedKey {
		t.Fatalf("ExportPrivateKey() = %q, %v", exported, err)
	}
	if _, err := manager.Import(headSpaceID, "eth-sepolia", account.FamilyEVM, "", strings.Repeat("11", 32)); err != nil {
		t.Fatal(err)
	}

	restarted := newManager(t, home)
	if err := restarted.Unlock(headSpaceID, headPassword); err != nil {
		t.Fatalf("Unlock() after the vault was rewritten error = %v", err)
	}
	if exported, err := restarted.ExportPrivateKey(headSpaceID, importedID, account.FamilyEVM, headPassword); err != nil || exported != importedKey {
		t.Fatalf("ExportPrivateKey() after a restart = %q, %v", exported, err)
	}
	after, err := restarted.Accounts(headSpaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range before {
		i := slices.IndexFunc(after, func(a account.Account) bool { return a.ID == item.ID })
		if i < 0 || after[i].KeyRef != item.KeyRef {
			t.Errorf("wallet %s (reference %q) did not come through unchanged", item.ID, item.KeyRef)
		}
	}
	keys, _, err := space.VaultContents(home, headSpaceID, headPassword)
	if err != nil {
		t.Fatal(err)
	}
	if ref := before[slices.IndexFunc(before, func(a account.Account) bool { return a.ID == importedID })].KeyRef; keys[ref] != 1 {
		t.Errorf("vault keys = %v, want one variant under %s", keys, ref)
	}
}

// fixtureMachine is the fixture space on one machine, unlocked, syncing
// through folder.
func fixtureMachine(t *testing.T, folder, deviceID string) *space.Manager {
	t.Helper()
	home, _ := headFixtureHome(t)
	manager := newManager(t, home)
	store, err := devicesync.NewFolderStore(folder)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetSync(store, deviceID, nil)
	if err := manager.Unlock(headSpaceID, headPassword); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	return manager
}

// passes syncs each machine in turn, rounds times, and fails on any wallet
// a pass did not keep.
func passes(t *testing.T, rounds int, machines ...*space.Manager) {
	t.Helper()
	for range rounds {
		for i, manager := range machines {
			status, err := manager.SyncNow(t.Context(), headSpaceID)
			if err != nil {
				t.Fatalf("machine %d: SyncNow() error = %v", i, err)
			}
			if len(status.Dropped) != 0 {
				t.Fatalf("machine %d dropped %v", i, status.Dropped)
			}
		}
	}
}

func legacyRecord(t *testing.T, manager *space.Manager) account.Account {
	t.Helper()
	accounts, err := manager.Accounts(headSpaceID)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(accounts, func(a account.Account) bool { return a.ID == headLegacyID })
	if i < 0 {
		t.Fatal("the record from before bindings is gone")
	}
	return accounts[i]
}

// A record from before network bindings, bound on one machine, reaches the
// other bound — the family set, the other family's address gone — and
// neither machine reports the wallet as dropped.
func TestABindingOfAnOldRecordReachesTheOtherMachine(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := fixtureMachine(t, folder, "dev_fixturea")
	b := fixtureMachine(t, folder, "dev_fixtureb")
	passes(t, 1, a, b)
	if _, err := a.BindNetwork(headSpaceID, headLegacyID, "tron-nile", account.FamilyTron); err != nil {
		t.Fatal(err)
	}
	passes(t, 2, a, b)
	got := legacyRecord(t, b)
	if got.Family != account.FamilyTron || len(got.Addresses) != 1 || !got.BoundTo("tron-nile") {
		t.Fatalf("B's record = %+v, want it bound to Tron", got)
	}
	if want := legacyRecord(t, a); !reflect.DeepEqual(got, want) {
		t.Errorf("the machines disagree:\nA %+v\nB %+v", want, got)
	}
}

// One machine binds an old record while the other, not having seen that yet,
// renames it. The later change wins whole on both, as for any wallet, and
// neither machine keeps reporting the other's copy as dropped.
func TestAnOldRecordBoundHereAndRenamedThereConverges(t *testing.T) {
	t.Parallel()

	folder := t.TempDir()
	a := fixtureMachine(t, folder, "dev_fixturea")
	b := fixtureMachine(t, folder, "dev_fixtureb")
	passes(t, 1, a, b)
	if _, err := a.BindNetwork(headSpaceID, headLegacyID, "tron-nile", account.FamilyTron); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RenameAccount(headSpaceID, headLegacyID, "renamed on B"); err != nil {
		t.Fatal(err)
	}
	passes(t, 3, a, b)
	got, want := legacyRecord(t, a), legacyRecord(t, b)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the machines disagree:\nA %+v\nB %+v", got, want)
	}
	if got.Label != "renamed on B" || got.Family != "" {
		t.Errorf("record = %+v, want B's later rename, still unbound", got)
	}
}
