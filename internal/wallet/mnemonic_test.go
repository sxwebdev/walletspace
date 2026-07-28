package wallet_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxwebdev/tronfaucet/internal/wallet"
)

func TestResolveMnemonicPrefersConfigured(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	got, generated, err := wallet.ResolveMnemonic("  "+testMnemonic+"  ", dir)
	if err != nil {
		t.Fatalf("ResolveMnemonic() error = %v", err)
	}

	if got != testMnemonic {
		t.Errorf("mnemonic = %q, want the configured one trimmed", got)
	}

	if generated {
		t.Error("generated = true, want false for a configured mnemonic")
	}

	// A configured mnemonic must not be written to disk.
	if _, err := os.Stat(filepath.Join(dir, wallet.MnemonicFileName)); !os.IsNotExist(err) {
		t.Errorf("Stat(%s) = %v, want the file to be absent", wallet.MnemonicFileName, err)
	}
}

func TestResolveMnemonicGeneratesOnceAndReuses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, generated, err := wallet.ResolveMnemonic("", dir)
	if err != nil {
		t.Fatalf("first ResolveMnemonic() error = %v", err)
	}

	if !generated {
		t.Error("generated = false on the first call, want true")
	}

	if words := len(strings.Fields(first)); words != 24 {
		t.Errorf("generated mnemonic has %d words, want 24", words)
	}

	path := filepath.Join(dir, wallet.MnemonicFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", path, err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("%s permissions = %o, want 600", wallet.MnemonicFileName, perm)
	}

	second, generated, err := wallet.ResolveMnemonic("", dir)
	if err != nil {
		t.Fatalf("second ResolveMnemonic() error = %v", err)
	}

	if generated {
		t.Error("generated = true on the second call, want false")
	}

	if second != first {
		t.Errorf("second call returned a different mnemonic:\n first = %q\nsecond = %q", first, second)
	}
}

func TestResolveMnemonicRejectsEmptyFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, wallet.MnemonicFileName)

	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, _, err := wallet.ResolveMnemonic("", dir); err == nil {
		t.Fatal("ResolveMnemonic() with an empty mnemonic file succeeded, want an error")
	}
}
