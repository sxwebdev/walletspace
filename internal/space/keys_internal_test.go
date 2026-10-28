package space

// What the vault stores for an imported key, and how unlock reads it, cannot
// be seen from outside: the payload is encrypted. These tests look inside.

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
)

// RFC 8032 §7.1 test 2, as a Solana keypair.
const internalRFCKeypair = "2Y4QjyJVZf9tTmTPP1SY9ACpFYTo7brW9iCQ8SunQht5yQ2r1U9KsVv5aMsCGnzj3NR8KG9P3NY7FKBiYbbTJ2no"

func TestImportedEd25519KeyIsStoredAsItsSeed(t *testing.T) {
	t.Parallel()

	manager := newGrantManager(t, t.TempDir())
	created, err := manager.Create(CreateRequest{Password: "test-vault-password"})
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
	if len(keys[ed.Account.KeyRef]) != 1 || len(keys[secp.Account.KeyRef]) != 1 {
		t.Fatalf("keys = %d and %d variants, want one each", len(keys[ed.Account.KeyRef]), len(keys[secp.Account.KeyRef]))
	}
	entry := keys[ed.Account.KeyRef][0]
	if entry.Curve != "ed25519" ||
		hex.EncodeToString(entry.PrivateKey) != "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb" {
		t.Errorf("ed25519 entry = %s %x, want the RFC 8032 seed", entry.Curve, entry.PrivateKey)
	}
	// The secp256k1 entry is the 32-byte scalar under the old curve name.
	entry = keys[secp.Account.KeyRef][0]
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
		Addresses: map[account.Family]string{
			account.FamilyEVM:  "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23",
			account.FamilyTron: "TE2H9hWjzYdwzDFRJfx9BFhr4MmjH1CHaz",
		},
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
		{"secp256k1 no address", withAddresses(secpItem, nil), KeyEntry{Curve: "secp256k1", PrivateKey: scalar}, "address"},
		{"secp256k1 evm only", withAddresses(secpItem, map[account.Family]string{
			account.FamilyEVM: "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23",
		}), KeyEntry{Curve: "secp256k1", PrivateKey: scalar}, "address"},
		{"secp256k1 wrong address", withAddresses(secpItem, map[account.Family]string{
			account.FamilyEVM:  "0x3E50D2D5F93be1ED37B4Ecc2B4f2bb47f93b6D35",
			account.FamilyTron: "TE2H9hWjzYdwzDFRJfx9BFhr4MmjH1CHaz",
		}), KeyEntry{Curve: "secp256k1", PrivateKey: scalar}, "address"},
		{"secp256k1 with a solana address", withAddresses(secpItem, map[account.Family]string{
			account.FamilyEVM:    "0x2c7536E3605D9C16a7a3D7b1898e529396a65c23",
			account.FamilyTron:   "TE2H9hWjzYdwzDFRJfx9BFhr4MmjH1CHaz",
			account.FamilySolana: "4zvwRjXUKGfvwnParsHAS3HuSVzV5cA4McphgmoCtajS",
		}), KeyEntry{Curve: "secp256k1", PrivateKey: scalar}, "address"},
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

// importedItem is the wallet record an import of entry makes.
func importedItem(t *testing.T, ref string, entry KeyEntry) account.Account {
	t.Helper()
	item := account.Account{Kind: account.KindImported, KeyRef: ref}
	curve, err := entryCurve(entry)
	if err != nil {
		t.Fatal(err)
	}
	if curve == account.CurveEd25519 {
		key := ed25519.NewKeyFromSeed(entry.PrivateKey)
		item.Fingerprint, _ = account.Ed25519Fingerprint(key)
		item.Addresses, _ = account.ImportedEd25519Addresses(key)
		return item
	}
	key, _, err := account.ParsePrivateKey(hex.EncodeToString(entry.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	item.Fingerprint, _ = account.Fingerprint(key)
	item.Addresses, _ = account.ImportedAddresses(key)
	return item
}

// A key read for signing or export is only ever offered on its own curve,
// and a damaged entry is refused rather than used.
func TestPrivateKeyForImportedEntries(t *testing.T) {
	t.Parallel()

	seed, _ := hex.DecodeString("4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb")
	scalar, _ := hex.DecodeString("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	payload := &Payload{ImportedKeys: map[string]KeyVariants{
		"ed":      {{Curve: "ed25519", PrivateKey: seed}},
		"secp":    {{Curve: "secp256k1", PrivateKey: scalar}},
		"legacy":  {{PrivateKey: scalar}},
		"short":   {{Curve: "ed25519", PrivateKey: seed[:16]}},
		"zero":    {{Curve: "secp256k1", PrivateKey: make([]byte, 32)}},
		"unknown": {{Curve: "sr25519", PrivateKey: seed}},
		"two":     {{Curve: "ed25519", PrivateKey: seed}, {Curve: "secp256k1", PrivateKey: scalar}},
	}}
	edItem := importedItem(t, "ed", payload.ImportedKeys["ed"][0])
	secpItem := importedItem(t, "secp", payload.ImportedKeys["secp"][0])
	tests := []struct {
		item    account.Account
		family  account.Family
		wantOK  bool
		wantErr string
	}{
		{edItem, account.FamilySolana, true, ""},
		{edItem, account.FamilyTron, false, "not available"},
		{secpItem, account.FamilyEVM, true, ""},
		{secpItem, account.FamilySolana, false, "not available"},
		{importedItem(t, "legacy", payload.ImportedKeys["legacy"][0]), account.FamilyTron, true, ""},
		{account.Account{Kind: account.KindImported, KeyRef: "short"}, account.FamilySolana, false, "invalid"},
		{account.Account{Kind: account.KindImported, KeyRef: "zero"}, account.FamilyEVM, false, "invalid"},
		{account.Account{Kind: account.KindImported, KeyRef: "unknown"}, account.FamilySolana, false, "unknown curve"},
		{account.Account{Kind: account.KindImported, KeyRef: "missing"}, account.FamilySolana, false, "missing"},
		// Two keys under the reference, neither of them this wallet's: none
		// is taken by default.
		{importedItem(t, "two", KeyEntry{Curve: "secp256k1", PrivateKey: bytes.Repeat([]byte{0x11}, 32)}), account.FamilyEVM, false, "none of the 2 keys"},
		// The key is there, but it is not this wallet's.
		{account.Account{Kind: account.KindImported, KeyRef: "ed", Fingerprint: secpItem.Fingerprint, Addresses: secpItem.Addresses},
			account.FamilySolana, false, "fingerprint"},
		{edItem, account.Family("btc"), false, "unsupported"},
	}
	for _, tt := range tests {
		key, err := privateKeyFor(tt.item, payload, tt.family)
		if tt.wantOK {
			if err != nil {
				t.Errorf("privateKeyFor(%s, %s) error = %v", tt.item.KeyRef, tt.family, err)
				continue
			}
			exported, err := key.export()
			if err != nil || exported == "" {
				t.Errorf("export(%s) = %q, %v", tt.item.KeyRef, exported, err)
			}
			key.clear()
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("privateKeyFor(%s, %s) error = %v, want one about %q", tt.item.KeyRef, tt.family, err, tt.wantErr)
		}
	}
	// The vault's own bytes are never handed out: clearing a resolved key
	// leaves the entry intact.
	key, err := privateKeyFor(edItem, payload, account.FamilySolana)
	if err != nil {
		t.Fatal(err)
	}
	key.clear()
	if hex.EncodeToString(payload.ImportedKeys["ed"][0].PrivateKey) != "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb" {
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

// A vault written before key variants holds one key per reference. It reads
// as a reference with one variant — same reference, same bytes — and is
// written back as a list. Exact copies of a key are one variant, imported at
// the earlier time, and an entry from before curves were named is a
// secp256k1 key.
func TestAVaultWithOneKeyPerReferenceReadsAsOneVariant(t *testing.T) {
	t.Parallel()

	scalar := bytes.Repeat([]byte{7}, 32)
	old, err := json.Marshal(map[string]any{
		"version":       1,
		"imported_keys": map[string]any{"key_one": map[string]any{"curve": "secp256k1", "private_key": scalar, "imported_at": "2026-01-01T00:00:00Z"}},
		"created_at":    "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload Payload
	if err := json.Unmarshal(old, &payload); err != nil {
		t.Fatalf("decode a vault with one key per reference: %v", err)
	}
	variants := payload.ImportedKeys["key_one"]
	if len(payload.ImportedKeys) != 1 || len(variants) != 1 || !bytes.Equal(variants[0].PrivateKey, scalar) || variants[0].Curve != "secp256k1" {
		t.Fatalf("keys = %+v, want key_one with its one key", payload.ImportedKeys)
	}
	written, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(written, []byte(`"key_one":[{`)) {
		t.Errorf("written as %s, want a list of variants", written)
	}
	var again Payload
	if err := json.Unmarshal(written, &again); err != nil || !sameKeys(again.ImportedKeys, payload.ImportedKeys) {
		t.Fatalf("the list does not read back the same: %+v, %v", again.ImportedKeys, err)
	}

	early := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	copies := &Payload{ImportedKeys: map[string]KeyVariants{"key_one": {
		{PrivateKey: bytes.Clone(scalar), ImportedAt: early.Add(time.Hour)},
		{Curve: "secp256k1", PrivateKey: bytes.Clone(scalar), ImportedAt: early},
	}, "key_empty": {}}}
	normalizePayload(copies)
	got := copies.ImportedKeys["key_one"]
	if len(got) != 1 || got[0].Curve != "secp256k1" || !got[0].ImportedAt.Equal(early) || !bytes.Equal(got[0].PrivateKey, scalar) {
		t.Errorf("copies of one key = %+v, want one secp256k1 variant imported at %v", got, early)
	}
	if _, ok := copies.ImportedKeys["key_empty"]; ok {
		t.Error("a reference with no key was kept")
	}
}
