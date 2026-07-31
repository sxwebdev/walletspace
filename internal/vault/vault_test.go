package vault_test

import (
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/vault"
)

var testParams = vault.Params{Time: 1, MemoryKiB: 8 * 1024, Parallelism: 1}

func TestRoundTripAndAuthentication(t *testing.T) {
	t.Parallel()

	container, err := vault.Encrypt("correct horse", []byte("secret"), []byte("spc_test:1"), testParams)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	got, err := vault.Decrypt("correct horse", container, []byte("spc_test:1"))
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if string(got) != "secret" {
		t.Errorf("Decrypt() = %q", got)
	}
	clear(got)

	for _, tc := range []struct {
		name     string
		password string
		aad      string
	}{
		{name: "wrong password", password: "wrong", aad: "spc_test:1"},
		{name: "wrong aad", password: "correct horse", aad: "spc_other:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := vault.Decrypt(tc.password, container, []byte(tc.aad))
			if !errors.Is(err, vault.ErrInvalidPassword) {
				t.Fatalf("Decrypt() error = %v, want ErrInvalidPassword", err)
			}
		})
	}
}

func TestRejectsTamperedAndUnsafeContainers(t *testing.T) {
	t.Parallel()

	container, err := vault.Encrypt("correct horse", []byte("secret"), []byte("space"), testParams)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	tampered := container
	replacement := "A"
	if tampered.Ciphertext[0] == 'A' {
		replacement = "B"
	}
	tampered.Ciphertext = replacement + tampered.Ciphertext[1:]
	if _, err := vault.Decrypt("correct horse", tampered, []byte("space")); err == nil {
		t.Fatal("Decrypt(tampered) error = nil")
	}

	unsafe := container
	unsafe.KDF.MemoryKiB = 2 * 1024 * 1024
	if _, err := vault.Decrypt("correct horse", unsafe, []byte("space")); err == nil {
		t.Fatal("Decrypt(unsafe KDF) error = nil")
	}

	truncated := container
	truncated.Ciphertext = "AA"
	if _, err := vault.Decrypt("correct horse", truncated, []byte("space")); err == nil {
		t.Fatal("Decrypt(truncated) error = nil")
	}
}
