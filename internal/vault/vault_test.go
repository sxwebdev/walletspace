package vault_test

import (
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/vault"
)

var testParams = vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}

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

// The work factors come out of the container, which is a file on disk. The
// ceilings stop a doctored header from turning one unlock attempt into a
// gigabyte allocation and ten passes over it; the floors stop this build from
// ever writing — or adopting and writing back — a container whose password is
// cheap to attack offline.
func TestKDFParametersAreBoundedInBothDirections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		params  vault.Params
		wantErr bool
	}{
		{name: "default", params: vault.DefaultParams},
		{name: "floor", params: vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}},
		{name: "memory below the floor", params: vault.Params{Time: 3, MemoryKiB: 8 * 1024, Parallelism: 1}, wantErr: true},
		{name: "one pass", params: vault.Params{Time: 1, MemoryKiB: 64 * 1024, Parallelism: 1}, wantErr: true},
		{name: "a gigabyte", params: vault.Params{Time: 3, MemoryKiB: 1024 * 1024, Parallelism: 1}, wantErr: true},
		{name: "ten passes", params: vault.Params{Time: 11, MemoryKiB: 64 * 1024, Parallelism: 1}, wantErr: true},
		{name: "no parallelism", params: vault.Params{Time: 3, MemoryKiB: 64 * 1024, Parallelism: 0}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := vault.Encrypt("correct-horse-battery", []byte("payload"), nil, tt.params)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Encrypt() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// Seal reuses the parameters from the header it was opened with, so a container
// whose header claims weak ones must not be opened and then written back with
// them — that would make the weakening permanent and invisible.
func TestAWeakenedHeaderIsNeverOpenedOrResealed(t *testing.T) {
	t.Parallel()

	container, err := vault.Encrypt("correct-horse-battery", []byte("payload"), nil, testParams)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	weakened := container
	weakened.KDF.MemoryKiB = 8
	weakened.KDF.Time = 1
	if _, _, err := vault.Unlock("correct-horse-battery", weakened, nil); err == nil {
		t.Fatal("Unlock() accepted a container claiming weak parameters")
	}

	_, session, err := vault.Unlock("correct-horse-battery", container, nil)
	if err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	t.Cleanup(session.Clear)
	if _, err := session.Seal([]byte("payload"), nil); err != nil {
		t.Errorf("Seal() with sound parameters error = %v", err)
	}
}
