package space_test

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/decred/base58"
	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/space"
)

const abandonMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

// Vectors computed outside this repository; see the account package tests for
// their sources. Passphrase "TREZOR", m/44'/501'/i'/0'.
const (
	trezorSolana0        = "7zSmbu6gKkb6HB7UDPtHYjwCWuBHU1D4TpNZFm4sndQe"
	trezorSolana0Keypair = "5yTLvhbem7H3sbPnCM6nY4PHLUkjucF224WLqY1qx58AeNGa88fsQqBhq1LwoRESPvMm6fRFURFYkGt398dXZiGi"
	trezorSolana1        = "8CW93AFYFKj2rKZRq3YjLesb4XbgtAiq8k2FgfruVZL5"
)

// RFC 8032 §7.1 test 2 as a Solana keypair (tweetnacl 1.0.3 + bs58 6.0.0).
const (
	rfcKeypair   = "2Y4QjyJVZf9tTmTPP1SY9ACpFYTo7brW9iCQ8SunQht5yQ2r1U9KsVv5aMsCGnzj3NR8KG9P3NY7FKBiYbbTJ2no"
	rfcAddress   = "586Z7H2vpX9qNhN2T4e9Utugie3ogjbxzGaMtM3E6HR5"
	rfcMessage   = "72"
	rfcSignature = "92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da" +
		"085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00"
	// The same key as solana-keygen's id.json.
	rfcCLIJSON = "[76,205,8,155,40,255,150,218,157,182,195,70,236,17,78,15,91,138,49,159,53,171,166,36,218,140,246,237,79,184,166,251," +
		"61,64,23,195,232,67,137,90,146,183,10,167,77,27,126,188,156,152,44,207,46,196,150,140,192,205,85,241,42,244,102,12]"
)

func TestSolanaDeriveExportAndUnlock(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created, err := manager.Create(space.CreateRequest{
		Password: "test-vault-password", Mnemonic: abandonMnemonic, BIP39Passphrase: "TREZOR",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID

	first, err := manager.Derive(id, "solana-devnet", account.FamilySolana, "Devnet 0")
	if err != nil {
		t.Fatalf("Derive(solana-devnet) error = %v", err)
	}
	if first.Family != account.FamilySolana || *first.Index != 0 || len(first.Addresses) != 1 ||
		first.Addresses[account.FamilySolana] != trezorSolana0 {
		t.Fatalf("first Solana wallet = %+v, want index 0 at %s", first, trezorSolana0)
	}
	second, err := manager.Derive(id, "solana-devnet", account.FamilySolana, "Devnet 1")
	if err != nil {
		t.Fatalf("Derive(solana-devnet) error = %v", err)
	}
	if *second.Index != 1 || second.Addresses[account.FamilySolana] != trezorSolana1 {
		t.Fatalf("second Solana wallet = %+v, want index 1 at %s", second, trezorSolana1)
	}
	// Index 0 on the other cluster is the same key: one wallet, two bindings.
	mainnet, err := manager.Derive(id, "solana-mainnet", account.FamilySolana, "")
	if err != nil {
		t.Fatalf("Derive(solana-mainnet) error = %v", err)
	}
	if mainnet.ID != first.ID || !mainnet.BoundTo("solana-mainnet") || !mainnet.BoundTo("solana-devnet") {
		t.Fatalf("mainnet wallet = %+v, want %s bound to both clusters", mainnet, first.ID)
	}

	exported, err := manager.ExportPrivateKey(id, first.ID, account.FamilySolana, "test-vault-password")
	if err != nil {
		t.Fatalf("ExportPrivateKey(solana) error = %v", err)
	}
	if exported != trezorSolana0Keypair {
		t.Errorf("exported keypair = %s, want %s", exported, trezorSolana0Keypair)
	}
	for _, family := range []account.Family{account.FamilyTron, account.FamilyEVM} {
		if _, err := manager.ExportPrivateKey(id, first.ID, family, "test-vault-password"); !errors.Is(err, space.ErrNetworkBinding) {
			t.Errorf("ExportPrivateKey(solana wallet as %s) error = %v, want ErrNetworkBinding", family, err)
		}
	}
	if _, err := manager.BindNetwork(id, first.ID, "tron-nile", account.FamilyTron); !errors.Is(err, space.ErrNetworkBinding) {
		t.Errorf("BindNetwork(solana wallet to tron) error = %v, want ErrNetworkBinding", err)
	}

	// Unlock re-derives every Solana address and accepts them.
	if err := manager.Lock(id); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.Unlock(id, "test-vault-password"); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}

	// A stored Solana address that the vault does not produce is refused.
	tamperAccount(t, home, id, first.ID, func(item map[string]any) {
		item["addresses"] = map[string]any{"solana": rfcAddress}
	})
	reopened := newManager(t, home)
	if err := reopened.Unlock(id, "test-vault-password"); err == nil {
		t.Fatal("Unlock() accepted a Solana address the vault does not derive")
	}
}

func TestSolanaMessageSigner(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password", Mnemonic: abandonMnemonic})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID
	wallet, err := manager.Derive(id, "solana-devnet", account.FamilySolana, "")
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	message := []byte("walletspace solana signing vector")
	// tweetnacl 1.0.3 signing message with the key of HAgk14…, the
	// mnemonic's index 0 without passphrase.
	const want = "28507cd25ae28f72ed11bb7fc407809de0206b158362d7dd4374ca147f38d12b" +
		"df494e735e2ccf144c51da2ce896d82fb84646f8da7ad765f867523eeea38f03"

	var escaped chain.MessageSigner
	err = manager.WithMessageSigner(t.Context(), id, wallet.ID, "solana-devnet", account.FamilySolana,
		func(signer chain.MessageSigner) error {
			escaped = signer
			if signer.Family() != chain.FamilySolana {
				t.Errorf("Family() = %s", signer.Family())
			}
			public := signer.PublicKey()
			if got := base58.Encode(public); got != wallet.Addresses[account.FamilySolana] {
				t.Errorf("PublicKey() = %s, want the wallet address %s", got, wallet.Addresses[account.FamilySolana])
			}
			signature, err := signer.SignMessage(t.Context(), message)
			if err != nil {
				return err
			}
			if !ed25519.Verify(public, message, signature) {
				t.Error("signature does not verify under the wallet's public key")
			}
			if got := hex.EncodeToString(signature); got != want {
				t.Errorf("signature = %s, want %s", got, want)
			}
			if _, err := signer.SignMessage(t.Context(), nil); err == nil {
				t.Error("SignMessage(empty) error = nil")
			}
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := signer.SignMessage(cancelled, message); !errors.Is(err, context.Canceled) {
				t.Errorf("SignMessage(cancelled) error = %v, want context.Canceled", err)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("WithMessageSigner() error = %v", err)
	}
	// A signer kept past its callback holds no key any more.
	if _, err := escaped.SignMessage(t.Context(), message); err == nil {
		t.Error("escaped signer still signs")
	}
	if escaped.PublicKey() != nil {
		t.Error("escaped signer still reports a public key")
	}

	if err := manager.WithMessageSigner(t.Context(), id, wallet.ID, "solana-mainnet", account.FamilySolana,
		func(chain.MessageSigner) error { return nil }); !errors.Is(err, space.ErrNetworkBinding) {
		t.Errorf("WithMessageSigner(unbound cluster) error = %v, want ErrNetworkBinding", err)
	}
	if err := manager.WithMessageSigner(t.Context(), id, wallet.ID, "solana-devnet", account.FamilySolana, nil); err == nil {
		t.Error("WithMessageSigner(nil) error = nil")
	}
}

// Each curve hands out only its own kind of signer, and asking for the
// other kind is an explicit refusal before any key is touched.
func TestSignerKindFollowsTheCurve(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created := createWithNileWallet(t, manager, "test-vault-password")
	id, tronID := created.Space.ID, created.Accounts[0].ID
	solana, err := manager.Derive(id, "solana-devnet", account.FamilySolana, "")
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	called := 0
	digestFn := func(chain.Signer) error { called++; return nil }
	messageFn := func(chain.MessageSigner) error { called++; return nil }

	if err := manager.WithSigner(t.Context(), id, solana.ID, "solana-devnet", account.FamilySolana, digestFn); !errors.Is(err, chain.ErrSignerMismatch) {
		t.Errorf("WithSigner(solana) error = %v, want ErrSignerMismatch", err)
	}
	for _, family := range []account.Family{account.FamilyTron, account.FamilyEVM} {
		if err := manager.WithMessageSigner(t.Context(), id, tronID, "tron-nile", family, messageFn); !errors.Is(err, chain.ErrSignerMismatch) {
			t.Errorf("WithMessageSigner(%s) error = %v, want ErrSignerMismatch", family, err)
		}
	}
	if err := manager.WithSigner(t.Context(), id, tronID, "tron-nile", account.Family("btc"), digestFn); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Errorf("WithSigner(btc) error = %v, want ErrUnsupportedFamily", err)
	}
	if err := manager.WithMessageSigner(t.Context(), id, solana.ID, "solana-devnet", account.Family("btc"), messageFn); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Errorf("WithMessageSigner(btc) error = %v, want ErrUnsupportedFamily", err)
	}
	// A Solana network named with the Tron family reaches the Tron wallet's
	// binding check, not its key.
	if err := manager.WithMessageSigner(t.Context(), id, tronID, "tron-nile", account.FamilySolana, messageFn); !errors.Is(err, space.ErrNetworkBinding) {
		t.Errorf("WithMessageSigner(tron wallet as solana) error = %v, want ErrNetworkBinding", err)
	}
	if called != 0 {
		t.Fatalf("a refused request reached the callback %d times", called)
	}

	// The secp256k1 path is unchanged: a 65-byte recoverable signature over
	// exactly 32 bytes.
	err = manager.WithSigner(t.Context(), id, tronID, "tron-nile", account.FamilyTron, func(signer chain.Signer) error {
		called++
		if _, err := signer.SignDigest(t.Context(), make([]byte, 31)); err == nil {
			t.Error("SignDigest(31 bytes) error = nil")
		}
		signature, err := signer.SignDigest(t.Context(), make([]byte, 32))
		if err != nil || len(signature) != 65 {
			t.Errorf("SignDigest() = %x, %v; want 65 bytes", signature, err)
		}
		return nil
	})
	if err != nil || called != 1 {
		t.Fatalf("WithSigner(tron) error = %v, calls = %d", err, called)
	}
}

func TestSolanaImportFormats(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager := newManager(t, home)
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password", ImportedOnly: true})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID

	imported, err := manager.Import(id, "solana-devnet", account.FamilySolana, "Phantom", rfcKeypair)
	if err != nil {
		t.Fatalf("Import(base58) error = %v", err)
	}
	if imported.Account.Kind != account.KindImported || imported.Account.Fingerprint != "39f713d0a644253f04529421b9f51b9b" ||
		len(imported.Account.Addresses) != 1 || imported.Account.Addresses[account.FamilySolana] != rfcAddress {
		t.Fatalf("imported = %+v", imported.Account)
	}
	// The solana-keygen form of the same key is the same wallet.
	if _, err := manager.Import(id, "solana-devnet", account.FamilySolana, "", rfcCLIJSON); !errors.Is(err, space.ErrDuplicateKey) {
		t.Fatalf("Import(json duplicate) error = %v, want ErrDuplicateKey", err)
	}
	bound, err := manager.Import(id, "solana-mainnet", account.FamilySolana, "", rfcCLIJSON)
	if err != nil || bound.Account.ID != imported.Account.ID || !bound.Account.BoundTo("solana-mainnet") {
		t.Fatalf("Import(json on mainnet) = %+v, %v; want a binding of %s", bound.Account, err, imported.Account.ID)
	}

	sign := func() string {
		t.Helper()
		var signature []byte
		err := manager.WithMessageSigner(t.Context(), id, imported.Account.ID, "solana-mainnet", account.FamilySolana,
			func(signer chain.MessageSigner) error {
				message, _ := hex.DecodeString(rfcMessage)
				var err error
				signature, err = signer.SignMessage(t.Context(), message)
				return err
			})
		if err != nil {
			t.Fatalf("WithMessageSigner() error = %v", err)
		}
		return hex.EncodeToString(signature)
	}
	if got := sign(); got != rfcSignature {
		t.Errorf("signature = %s, want RFC 8032 %s", got, rfcSignature)
	}

	// The vault keeps it across a lock, and unlock verifies it.
	if err := manager.Lock(id); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.Unlock(id, "test-vault-password"); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	exported, err := manager.ExportPrivateKey(id, imported.Account.ID, account.FamilySolana, "test-vault-password")
	if err != nil || exported != rfcKeypair {
		t.Fatalf("ExportPrivateKey() = %q, %v; want %s", exported, err, rfcKeypair)
	}
	if got := sign(); got != rfcSignature {
		t.Errorf("signature after unlock = %s, want %s", got, rfcSignature)
	}

	// An ed25519 key is a Solana wallet only.
	if _, err := manager.BindNetwork(id, imported.Account.ID, "ethereum-mainnet", account.FamilyEVM); !errors.Is(err, space.ErrNetworkBinding) {
		t.Errorf("BindNetwork(ed25519 to evm) error = %v, want ErrNetworkBinding", err)
	}
	if _, err := manager.ExportPrivateKey(id, imported.Account.ID, account.FamilyEVM, "test-vault-password"); !errors.Is(err, space.ErrNetworkBinding) {
		t.Errorf("ExportPrivateKey(ed25519 as evm) error = %v, want ErrNetworkBinding", err)
	}

	// And a secp256k1 key is never a Solana wallet.
	secp, err := manager.Import(id, "ethereum-mainnet", account.FamilyEVM, "",
		"0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("Import(secp256k1) error = %v", err)
	}
	if _, err := manager.BindNetwork(id, secp.Account.ID, "solana-devnet", account.FamilySolana); !errors.Is(err, space.ErrNetworkBinding) {
		t.Errorf("BindNetwork(secp256k1 to solana) error = %v, want ErrNetworkBinding", err)
	}
	if _, err := manager.ExportPrivateKey(id, secp.Account.ID, account.FamilySolana, "test-vault-password"); !errors.Is(err, space.ErrNetworkBinding) {
		t.Errorf("ExportPrivateKey(secp256k1 as solana) error = %v, want ErrNetworkBinding", err)
	}
	evmExport, err := manager.ExportPrivateKey(id, secp.Account.ID, account.FamilyEVM, "test-vault-password")
	if err != nil || evmExport != "0000000000000000000000000000000000000000000000000000000000000001" {
		t.Errorf("ExportPrivateKey(secp256k1) = %q, %v", evmExport, err)
	}

	// Each family reads its own format only, and nothing half-valid lands.
	key, err := account.ParseEd25519PrivateKey(rfcKeypair)
	if err != nil {
		t.Fatal(err)
	}
	other := ed25519.NewKeyFromSeed(make([]byte, 32))
	mismatched := base58.Encode(append(append([]byte(nil), key[:32]...), other[32:]...))
	for name, tt := range map[string]struct {
		network string
		family  account.Family
		key     string
	}{
		"hex key on solana":       {"solana-devnet", account.FamilySolana, "0000000000000000000000000000000000000000000000000000000000000002"},
		"keypair on evm":          {"ethereum-mainnet", account.FamilyEVM, rfcKeypair},
		"keypair on tron":         {"tron-nile", account.FamilyTron, rfcKeypair},
		"mismatched halves":       {"solana-devnet", account.FamilySolana, mismatched},
		"32-byte seed alone":      {"solana-devnet", account.FamilySolana, base58.Encode(key[:32])},
		"json with 63 numbers":    {"solana-devnet", account.FamilySolana, "[1,2,3]"},
		"keypair with extra byte": {"solana-devnet", account.FamilySolana, base58.Encode(append(append([]byte(nil), key...), 0))},
	} {
		if _, err := manager.Import(id, tt.network, tt.family, name, tt.key); !errors.Is(err, account.ErrInvalidPrivateKey) {
			t.Errorf("Import(%s) error = %v, want ErrInvalidPrivateKey", name, err)
		}
	}
	if _, err := manager.Import(id, "solana-devnet", account.Family("btc"), "", rfcKeypair); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Errorf("Import(btc) error = %v, want ErrUnsupportedFamily", err)
	}
	accounts, err := manager.Accounts(id)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("Accounts() = %d, %v; want the two good imports only", len(accounts), err)
	}

	// The stored address is part of what unlock checks for an ed25519 key.
	if err := manager.Lock(id); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	tamperAccount(t, home, id, imported.Account.ID, func(item map[string]any) {
		item["addresses"] = map[string]any{"solana": rfcAddress, "tron": "TMVQGm1qAQYVdetCeGRRkTWYYrLXuHK2HC"}
	})
	if err := newManager(t, home).Unlock(id, "test-vault-password"); err == nil {
		t.Fatal("Unlock() accepted an ed25519 wallet with a Tron address")
	}
}

// A space without any Solana wallet gains one through Derive, with no
// migration of what is already there.
func TestSolanaWalletJoinsAnExistingSpace(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created := createWithNileWallet(t, manager, "test-vault-password")
	id, tronID := created.Space.ID, created.Accounts[0].ID
	if _, err := manager.BindNetwork(id, tronID, "solana-devnet", account.FamilySolana); !errors.Is(err, space.ErrNetworkBinding) {
		t.Fatalf("BindNetwork(tron wallet to solana) error = %v, want ErrNetworkBinding", err)
	}
	wallet, err := manager.Derive(id, "solana-devnet", account.FamilySolana, "")
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}
	// Indexes are per network, so the Solana wallet starts at 0 too.
	if *wallet.Index != 0 || wallet.ID == tronID {
		t.Fatalf("Solana wallet = %+v", wallet)
	}
	accounts, err := manager.Accounts(id)
	if err != nil || len(accounts) != 2 {
		t.Fatalf("Accounts() = %+v, %v", accounts, err)
	}
	for _, item := range accounts {
		if item.ID == tronID && len(item.Addresses) != 1 {
			t.Errorf("Tron wallet changed: %+v", item)
		}
	}
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Unlock(id, "test-vault-password"); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
}

// tamperAccount rewrites one account record in the space file on disk.
func tamperAccount(t *testing.T, home, spaceID, accountID string, edit func(map[string]any)) {
	t.Helper()
	path := filepath.Join(home, "spaces", spaceID, "space.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file map[string]any
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range file["accounts"].([]any) {
		item := raw.(map[string]any)
		if item["id"] == accountID {
			edit(item)
			found = true
		}
	}
	if !found {
		t.Fatalf("account %s is not in %s", accountID, path)
	}
	data, err = json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every entry point refuses a family it does not know before touching the
// vault, and the Solana signer obeys the same lock and lookup rules as the
// digest signer.
func TestSolanaEntryPointRefusals(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created, err := manager.Create(space.CreateRequest{Password: "test-vault-password", Mnemonic: abandonMnemonic})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	wallet, err := manager.Derive(id, "solana-devnet", account.FamilySolana, "")
	if err != nil {
		t.Fatal(err)
	}
	unknown := account.Family("btc")
	if _, err := manager.Derive(id, "solana-devnet", unknown, ""); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Errorf("Derive(btc) error = %v", err)
	}
	if _, err := manager.BindNetwork(id, wallet.ID, "solana-devnet", unknown); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Errorf("BindNetwork(btc) error = %v", err)
	}
	if _, err := manager.ExportPrivateKey(id, wallet.ID, unknown, "test-vault-password"); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Errorf("ExportPrivateKey(btc) error = %v", err)
	}
	if _, err := manager.Derive(id, "", account.FamilySolana, ""); err == nil {
		t.Error("Derive(no network) error = nil")
	}
	if err := manager.WithSigner(t.Context(), id, wallet.ID, "solana-devnet", account.FamilySolana, nil); err == nil {
		t.Error("WithSigner(nil) error = nil")
	}
	noop := func(chain.MessageSigner) error { return nil }
	if err := manager.WithMessageSigner(t.Context(), id, "acc_missing", "solana-devnet", account.FamilySolana, noop); !errors.Is(err, space.ErrAccountNotFound) {
		t.Errorf("WithMessageSigner(unknown account) error = %v, want ErrAccountNotFound", err)
	}
	callbackErr := errors.New("callback failed")
	if err := manager.WithMessageSigner(t.Context(), id, wallet.ID, "solana-devnet", account.FamilySolana,
		func(chain.MessageSigner) error { return callbackErr }); !errors.Is(err, callbackErr) {
		t.Errorf("WithMessageSigner() error = %v, want the callback's", err)
	}
	if err := manager.Lock(id); err != nil {
		t.Fatal(err)
	}
	if err := manager.WithMessageSigner(t.Context(), id, wallet.ID, "solana-devnet", account.FamilySolana, noop); !errors.Is(err, space.ErrLocked) {
		t.Errorf("WithMessageSigner(locked) error = %v, want ErrLocked", err)
	}
	if _, err := manager.Derive(id, "solana-devnet", account.FamilySolana, ""); !errors.Is(err, space.ErrLocked) {
		t.Errorf("Derive(locked) error = %v, want ErrLocked", err)
	}
}
