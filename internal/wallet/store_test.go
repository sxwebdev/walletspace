package wallet_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/wallet"
)

// Vectors taken from gotron's pkg/address tests, which in turn come from
// iancoleman.io/bip39. They pin the derivation path m/44'/195'/0'/0/{index}.
const testMnemonic = "recipe need harsh web order laptop seek filter among federal glory balcony video fault shed myself crush orient figure crack beach weather find match"

const (
	addrIndex0            = "TEeKaYdpN6ujnpVZ1SkohE6Ru6gd9vGC2A"
	addrIndex1            = "TUwbUgKvC1RsT3qShxmZcfMpvMdbE6JPST"
	addrIndex0Passphrased = "TLutkfK9N2BaBEzUngAuaNKTC9SZu3ER1K"
)

func TestNewDerivesKnownAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		passphrase string
		want       string
	}{
		{name: "no passphrase", passphrase: "", want: addrIndex0},
		{name: "with passphrase", passphrase: "test", want: addrIndex0Passphrased},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newStore(t, tt.passphrase)

			list := s.List()
			if len(list) != 1 {
				t.Fatalf("List() returned %d wallets, want 1", len(list))
			}

			if list[0].Index != 0 {
				t.Errorf("first wallet index = %d, want 0", list[0].Index)
			}

			if list[0].Address != tt.want {
				t.Errorf("first wallet address = %s, want %s", list[0].Address, tt.want)
			}
		})
	}
}

func TestCreateUsesNextIndex(t *testing.T) {
	t.Parallel()

	s := newStore(t, "")

	created, err := s.Create("second")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if created.Index != 1 {
		t.Errorf("created index = %d, want 1", created.Index)
	}

	if created.Address != addrIndex1 {
		t.Errorf("created address = %s, want %s", created.Address, addrIndex1)
	}

	if created.Label != "second" {
		t.Errorf("created label = %q, want %q", created.Label, "second")
	}

	if got := len(s.List()); got != 2 {
		t.Errorf("List() returned %d wallets, want 2", got)
	}
}

func TestReopenKeepsWallets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wallets.json")

	first, err := wallet.New(path, testMnemonic, "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := first.Create("second"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := first.Rename(0, "main"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}

	second, err := wallet.New(path, testMnemonic, "")
	if err != nil {
		t.Fatalf("New() on existing file error = %v", err)
	}

	list := second.List()
	if len(list) != 2 {
		t.Fatalf("reopened store has %d wallets, want 2", len(list))
	}

	if list[0].Label != "main" {
		t.Errorf("wallet 0 label = %q, want %q", list[0].Label, "main")
	}

	if list[1].Address != addrIndex1 {
		t.Errorf("wallet 1 address = %s, want %s", list[1].Address, addrIndex1)
	}
}

func TestNewRejectsForeignMnemonic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wallets.json")

	if _, err := wallet.New(path, testMnemonic, ""); err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// Same mnemonic, different passphrase: every address changes, so opening the
	// existing file must fail loudly instead of silently showing other wallets.
	_, err := wallet.New(path, testMnemonic, "test")
	if err == nil {
		t.Fatal("New() with a different passphrase succeeded, want an error")
	}

	if !strings.Contains(err.Error(), addrIndex0) {
		t.Errorf("error %q does not mention the expected fingerprint %s", err, addrIndex0)
	}
}

func TestNewRejectsFileWithoutFingerprint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wallets.json")

	// A file listing wallets but carrying no fingerprint cannot be proven to
	// belong to this mnemonic. Accepting it would show addresses whose keys we
	// do not hold, and every send from those rows would sign with the wrong key.
	raw := `{"wallets":[{"index":0,"address":"TSomeoneElsesAddress","label":"","created_at":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := wallet.New(path, testMnemonic, ""); err == nil {
		t.Fatal("New() accepted a wallets.json with no fingerprint, want an error")
	}
}

func TestSaveKeepsFingerprintOfIndexZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wallets.json")

	s, err := wallet.New(path, testMnemonic, "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := s.Create("second"); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Drop the index-0 row, as someone sweeping a spent wallet might. The
	// fingerprint must stay the index-0 address, not become wallets[0], or the
	// next start would reject a correct file with the correct mnemonic.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	var f struct {
		Fingerprint string           `json:"fingerprint"`
		Wallets     []map[string]any `json:"wallets"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	f.Wallets = f.Wallets[1:]
	trimmed, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if err := os.WriteFile(path, trimmed, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	reopened, err := wallet.New(path, testMnemonic, "")
	if err != nil {
		t.Fatalf("New() on a file without the index-0 row error = %v", err)
	}

	if err := reopened.Rename(1, "renamed"); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if err := json.Unmarshal(after, &f); err != nil {
		t.Fatalf("Unmarshal() after save error = %v", err)
	}

	if f.Fingerprint != addrIndex0 {
		t.Errorf("fingerprint after save = %s, want the index-0 address %s", f.Fingerprint, addrIndex0)
	}

	// The whole point: the file still opens on the next start.
	if _, err := wallet.New(path, testMnemonic, ""); err != nil {
		t.Errorf("New() after save error = %v, want the file to still be accepted", err)
	}
}

func TestPrivateKeyDerivesForKnownIndexOnly(t *testing.T) {
	t.Parallel()

	s := newStore(t, "")

	key, err := s.PrivateKey(0)
	if err != nil {
		t.Fatalf("PrivateKey(0) error = %v", err)
	}

	if key == nil {
		t.Fatal("PrivateKey(0) returned a nil key")
	}

	if _, err := s.PrivateKey(7); err == nil {
		t.Error("PrivateKey(7) succeeded for a wallet that was never created")
	}
}

func TestFileHoldsNoSecrets(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "wallets.json")

	s, err := wallet.New(path, testMnemonic, "")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := s.Create(""); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	// The file must be readable JSON that carries no key material at all.
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("stored file is not valid JSON: %v", err)
	}

	for _, secret := range []string{testMnemonic, "private", "privateKey", "private_key"} {
		if strings.Contains(strings.ToLower(string(data)), strings.ToLower(secret)) {
			t.Errorf("stored file contains %q", secret)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("wallets.json permissions = %o, want 600", perm)
	}
}

func TestRenameUnknownIndex(t *testing.T) {
	t.Parallel()

	s := newStore(t, "")

	if err := s.Rename(42, "nope"); err == nil {
		t.Error("Rename() on an unknown index succeeded, want an error")
	}
}

func newStore(t *testing.T, passphrase string) *wallet.Store {
	t.Helper()

	s, err := wallet.New(filepath.Join(t.TempDir(), "wallets.json"), testMnemonic, passphrase)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	return s
}
