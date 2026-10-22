package space

// What the vault stores for an imported key, and how unlock reads it, cannot
// be seen from outside: the payload is encrypted. These tests look inside.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/account"
)

// RFC 8032 §7.1 test 2, as a Solana keypair.
const internalRFCKeypair = "2Y4QjyJVZf9tTmTPP1SY9ACpFYTo7brW9iCQ8SunQht5yQ2r1U9KsVv5aMsCGnzj3NR8KG9P3NY7FKBiYbbTJ2no"

func TestImportedEd25519KeyIsStoredAsItsSeed(t *testing.T) {
	t.Parallel()

	manager := newGrantManager(t, t.TempDir())
	created, err := manager.Create(CreateRequest{Password: "test-vault-password", ImportedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ed, err := manager.Import(created.Space.ID, "solana-devnet", account.FamilySolana, "", internalRFCKeypair)
	if err != nil {
		t.Fatal(err)
	}
	secp, err := manager.Import(created.Space.ID, "tron-nile", account.FamilyTron, "",
		"0x4C0883A69102937D6231471B5DBB6204FE5129617082792AE468D01A3F362318")
	if err != nil {
		t.Fatal(err)
	}

	manager.mu.RLock()
	defer manager.mu.RUnlock()
	keys := manager.sessions[created.Space.ID].payload.ImportedKeys
	entry := keys[ed.Account.KeyRef]
	if entry.Curve != "ed25519" ||
		hex.EncodeToString(entry.PrivateKey) != "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb" {
		t.Errorf("ed25519 entry = %s %x, want the RFC 8032 seed", entry.Curve, entry.PrivateKey)
	}
	// The secp256k1 entry is the 32-byte scalar under the old curve name.
	entry = keys[secp.Account.KeyRef]
	if entry.Curve != "secp256k1" ||
		hex.EncodeToString(entry.PrivateKey) != "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318" {
		t.Errorf("secp256k1 entry = %s %x", entry.Curve, entry.PrivateKey)
	}
}

func TestVerifyImportedKey(t *testing.T) {
	t.Parallel()

	seed, _ := hex.DecodeString("4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb")
	key := ed25519.NewKeyFromSeed(seed)
	fingerprint, err := account.Ed25519Fingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	addresses, err := account.ImportedEd25519Addresses(key)
	if err != nil {
		t.Fatal(err)
	}
	scalar, _ := hex.DecodeString("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	edItem := account.Account{Kind: account.KindImported, Fingerprint: fingerprint, Addresses: addresses}
	secpItem := account.Account{
		Kind: account.KindImported, Fingerprint: "f49c63af9f6e657abc52a941bb7e812c",
		// The secp256k1 check has never looked at addresses; it still does not.
		Addresses: map[account.Family]string{account.FamilyEVM: "anything"},
	}
	withAddresses := func(item account.Account, addresses map[account.Family]string) account.Account {
		item.Addresses = addresses
		return item
	}
	withFingerprint := func(item account.Account, fingerprint string) account.Account {
		item.Fingerprint = fingerprint
		return item
	}

	tests := []struct {
		name    string
		item    account.Account
		entry   KeyEntry
		wantErr string
	}{
		{"ed25519", edItem, KeyEntry{Curve: "ed25519", PrivateKey: seed}, ""},
		{"secp256k1", secpItem, KeyEntry{Curve: "secp256k1", PrivateKey: scalar}, ""},
		{"secp256k1 without a curve", secpItem, KeyEntry{PrivateKey: scalar}, ""},
		{"unknown curve", secpItem, KeyEntry{Curve: "sr25519", PrivateKey: scalar}, "unknown curve"},
		{"ed25519 short seed", edItem, KeyEntry{Curve: "ed25519", PrivateKey: seed[:31]}, "invalid"},
		{"ed25519 whole keypair stored", edItem, KeyEntry{Curve: "ed25519", PrivateKey: key}, "invalid"},
		{"ed25519 other key", withFingerprint(edItem, "00"), KeyEntry{Curve: "ed25519", PrivateKey: seed}, "fingerprint"},
		{"ed25519 no address", withAddresses(edItem, nil), KeyEntry{Curve: "ed25519", PrivateKey: seed}, "address"},
		{"ed25519 wrong address", withAddresses(edItem, map[account.Family]string{
			account.FamilySolana: "586Z7H2vpX9qNhN2T4e9Utugie3ogjbxzGaMtM3E6HR6",
		}), KeyEntry{Curve: "ed25519", PrivateKey: seed}, "address"},
		{"secp256k1 key under ed25519", secpItem, KeyEntry{Curve: "ed25519", PrivateKey: scalar}, "fingerprint"},
		{"ed25519 seed under secp256k1", edItem, KeyEntry{Curve: "secp256k1", PrivateKey: seed}, "fingerprint"},
		{"secp256k1 other key", withFingerprint(secpItem, "00"), KeyEntry{Curve: "secp256k1", PrivateKey: scalar}, "fingerprint"},
		{"secp256k1 out of range", secpItem, KeyEntry{Curve: "secp256k1", PrivateKey: bytes.Repeat([]byte{0xff}, 32)}, "invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := verifyImportedKey(tt.item, tt.entry)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyImportedKey() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("verifyImportedKey() error = %v, want one about %q", err, tt.wantErr)
			}
		})
	}
}

// A key read for signing or export is only ever offered on its own curve,
// and a damaged entry is refused rather than used.
func TestPrivateKeyForImportedEntries(t *testing.T) {
	t.Parallel()

	seed, _ := hex.DecodeString("4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb")
	scalar, _ := hex.DecodeString("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	payload := &Payload{ImportedKeys: map[string]KeyEntry{
		"ed":      {Curve: "ed25519", PrivateKey: seed},
		"secp":    {Curve: "secp256k1", PrivateKey: scalar},
		"legacy":  {PrivateKey: scalar},
		"short":   {Curve: "ed25519", PrivateKey: seed[:16]},
		"zero":    {Curve: "secp256k1", PrivateKey: make([]byte, 32)},
		"unknown": {Curve: "sr25519", PrivateKey: seed},
	}}
	tests := []struct {
		keyRef  string
		family  account.Family
		wantOK  bool
		wantErr string
	}{
		{"ed", account.FamilySolana, true, ""},
		{"ed", account.FamilyTron, false, "not available"},
		{"secp", account.FamilyEVM, true, ""},
		{"secp", account.FamilySolana, false, "not available"},
		{"legacy", account.FamilyTron, true, ""},
		{"short", account.FamilySolana, false, "invalid"},
		{"zero", account.FamilyEVM, false, "invalid"},
		{"unknown", account.FamilySolana, false, "unknown curve"},
		{"missing", account.FamilySolana, false, "missing"},
		{"ed", account.Family("btc"), false, "unsupported"},
	}
	for _, tt := range tests {
		item := account.Account{Kind: account.KindImported, KeyRef: tt.keyRef}
		key, err := privateKeyFor(item, payload, tt.family)
		if tt.wantOK {
			if err != nil {
				t.Errorf("privateKeyFor(%s, %s) error = %v", tt.keyRef, tt.family, err)
				continue
			}
			exported, err := key.export()
			if err != nil || exported == "" {
				t.Errorf("export(%s) = %q, %v", tt.keyRef, exported, err)
			}
			key.clear()
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("privateKeyFor(%s, %s) error = %v, want one about %q", tt.keyRef, tt.family, err, tt.wantErr)
		}
	}
	// The vault's own bytes are never handed out: clearing a resolved key
	// leaves the entry intact.
	key, err := privateKeyFor(account.Account{Kind: account.KindImported, KeyRef: "ed"}, payload, account.FamilySolana)
	if err != nil {
		t.Fatal(err)
	}
	key.clear()
	if hex.EncodeToString(payload.ImportedKeys["ed"].PrivateKey) != "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb" {
		t.Fatal("clearing a signing key wiped the vault entry")
	}
	if _, err := (signingKey{}).export(); err == nil {
		t.Error("export(empty) error = nil")
	}
	unknownKind := account.Account{Kind: "custodial"}
	if _, err := privateKeyFor(unknownKind, payload, account.FamilySolana); err == nil {
		t.Error("privateKeyFor(unknown kind) error = nil")
	}
	// A pre-binding derived record is a key for the families it has
	// addresses for, and only those.
	mnemonicPayload := &Payload{Mnemonic: []byte("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about")}
	index := uint32(0)
	legacy := account.Account{Kind: account.KindDerived, Index: &index, Addresses: map[account.Family]string{
		account.FamilyTron: "TUEZSdKsoDHQMeZwihtdoBiN46zxhGWYdH",
		account.FamilyEVM:  "0x9858EfFD232B4033E47d90003D41EC34EcaEda94",
	}}
	for family, wantOK := range map[account.Family]bool{
		account.FamilyTron: true, account.FamilyEVM: true, account.FamilySolana: false,
	} {
		key, err := privateKeyFor(legacy, mnemonicPayload, family)
		if wantOK != (err == nil) {
			t.Errorf("privateKeyFor(legacy, %s) error = %v, want ok=%v", family, err, wantOK)
		}
		key.clear()
	}
	tronOnly := legacy
	tronOnly.Addresses = map[account.Family]string{account.FamilyTron: "TUEZSdKsoDHQMeZwihtdoBiN46zxhGWYdH"}
	if _, err := privateKeyFor(tronOnly, mnemonicPayload, account.FamilyEVM); err == nil {
		t.Error("privateKeyFor(legacy without an EVM address, evm) error = nil")
	}
	derivedNoIndex := account.Account{Kind: account.KindDerived, Family: account.FamilySolana}
	if _, err := privateKeyFor(derivedNoIndex, payload, account.FamilySolana); err == nil {
		t.Error("privateKeyFor(derived without index) error = nil")
	}
}
