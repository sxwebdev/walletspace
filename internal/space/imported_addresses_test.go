package space_test

import (
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/vault"
)

// headImportedID is the imported secp256k1 key in testdata/head-98adcab, bound
// to Ethereum and Nile.
const headImportedID = "acc_s5kctjpiqlvfa5kqq46a"

// The receive addresses of an imported secp256k1 key are part of what unlock
// checks, as they already are for an ed25519 key: someone who can edit
// space.json but not the vault must not be able to swap the address the
// wallet shows. The file HEAD wrote opens untouched; every edit to the
// imported key's addresses is refused.
func TestUnlockChecksImportedSecp256k1Addresses(t *testing.T) {
	t.Parallel()

	const (
		headEVM  = "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23"
		headTron = "TE2H9hWjzYdwzDFRJfx9BFhr4MmjH1CHaz"
		// The legacy wallet's addresses in the same file: real addresses of
		// another key in the same space.
		otherEVM  = "0x3E50D2D5F93be1ED37B4Ecc2B4f2bb47f93b6D35"
		otherTron = "TYeYduiMkzzjRqpL56sVM2h7fxGkprCV2P"
	)

	t.Run("as HEAD wrote it", func(t *testing.T) {
		t.Parallel()

		home, _ := headFixtureHome(t)
		manager := newManager(t, home)
		if err := manager.Unlock(headSpaceID, headPassword); err != nil {
			t.Fatalf("Unlock() error = %v", err)
		}
		accounts, err := manager.Accounts(headSpaceID)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range accounts {
			if item.ID == headImportedID && (len(item.Addresses) != 2 ||
				item.Addresses[account.FamilyEVM] != headEVM || item.Addresses[account.FamilyTron] != headTron) {
				t.Fatalf("imported wallet addresses = %v", item.Addresses)
			}
		}
	})

	for name, addresses := range map[string]map[string]any{
		"evm swapped":      {"evm": otherEVM, "tron": headTron},
		"tron swapped":     {"evm": headEVM, "tron": otherTron},
		"both swapped":     {"evm": otherEVM, "tron": otherTron},
		"evm dropped":      {"tron": headTron},
		"tron dropped":     {"evm": headEVM},
		"none":             {},
		"solana added":     {"evm": headEVM, "tron": headTron, "solana": "4zvwRjXUKGfvwnParsHAS3HuSVzV5cA4McphgmoCtajS"},
		"unknown family":   {"evm": headEVM, "tron": headTron, "btc": "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"},
		"evm without case": {"evm": "0x2c7536e3605d9c16a7a3d7b1898e529396a65c23", "tron": headTron},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			home, _ := headFixtureHome(t)
			tamperAccount(t, home, headSpaceID, headImportedID, func(item map[string]any) {
				item["addresses"] = addresses
			})
			manager := newManager(t, home)
			err := manager.Unlock(headSpaceID, headPassword)
			if err == nil {
				t.Fatal("Unlock() accepted a swapped imported address")
			}
			if errors.Is(err, vault.ErrInvalidPassword) {
				t.Fatalf("Unlock() error = %v, want a verification error, not a wrong password", err)
			}
			err = manager.WithSigner(t.Context(), headSpaceID, headImportedID, "ethereum-mainnet", account.FamilyEVM,
				func(chain.Signer) error { return nil })
			if !errors.Is(err, space.ErrLocked) {
				t.Fatalf("WithSigner() after a refused unlock error = %v, want ErrLocked", err)
			}
		})
	}
}

// The same holds for a key imported by the current code: it opens again as
// written, and an address edited on disk afterwards is refused.
func TestUnlockChecksFreshImportAddresses(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created := createWithNileWallet(t, manager, "test-vault-password")
	id := created.Space.ID
	imported, err := manager.Import(id, "ethereum-mainnet", account.FamilyEVM, "",
		"0000000000000000000000000000000000000000000000000000000000000003")
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := newManager(t, home).Unlock(id, "test-vault-password"); err != nil {
		t.Fatalf("Unlock() of an untouched import error = %v", err)
	}

	tamperAccount(t, home, id, imported.Account.ID, func(item map[string]any) {
		addresses := item["addresses"].(map[string]any)
		addresses["evm"] = "0x3E50D2D5F93be1ED37B4Ecc2B4f2bb47f93b6D35"
	})
	if err := newManager(t, home).Unlock(id, "test-vault-password"); err == nil {
		t.Fatal("Unlock() accepted an imported key with a swapped EVM address")
	}
}
