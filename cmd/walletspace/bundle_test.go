package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
)

var cliKDF = vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}

// homeWithSpaces creates a home with the given spaces and closes its manager,
// as a wallet that is not running would leave it.
func homeWithSpaces(t *testing.T, names ...string) (string, []string) {
	t.Helper()
	home := t.TempDir()
	manager, err := space.NewManager(home, 15*time.Minute, cliKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	defer manager.Close()
	ids := make([]string, len(names))
	for i, name := range names {
		created, err := manager.Create(space.CreateRequest{Name: name, Password: "a password for " + name})
		if err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
		ids[i] = created.Space.ID
	}
	return home, ids
}

func TestExportThenImportOnAnotherHome(t *testing.T) {
	t.Parallel()

	source, ids := homeWithSpaces(t, "first", "second")
	backup := filepath.Join(t.TempDir(), "spaces.wsbackup")
	var out bytes.Buffer
	if err := runExport([]string{"--home", source, "--out", backup}, &out); err != nil {
		t.Fatalf("runExport() error = %v", err)
	}
	for _, id := range ids {
		if !strings.Contains(out.String(), id) {
			t.Errorf("export output does not name %s:\n%s", id, out.String())
		}
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("backup mode = %o, want 0600", mode)
	}
	// An existing file is never replaced.
	if err := runExport([]string{"--home", source, "--out", backup}, &out); err == nil {
		t.Error("runExport() over an existing file succeeded")
	}

	target := t.TempDir()
	out.Reset()
	if err := runImport([]string{"--home", target, backup}, &out); err != nil {
		t.Fatalf("runImport() error = %v", err)
	}
	manager, err := space.NewManager(target, 15*time.Minute, cliKDF)
	if err != nil {
		t.Fatalf("NewManager(target) error = %v", err)
	}
	defer manager.Close()
	if got := len(manager.List()); got != 2 {
		t.Fatalf("spaces on the target = %d, want 2", got)
	}
	if err := manager.Unlock(ids[1], "a password for second"); err != nil {
		t.Fatalf("Unlock(imported) error = %v", err)
	}
}

func TestExportOnlyTheNamedSpaces(t *testing.T) {
	t.Parallel()

	source, ids := homeWithSpaces(t, "first", "second")
	backup := filepath.Join(t.TempDir(), "one.wsbackup")
	if err := runExport([]string{"--home", source, "--space", ids[1], "--out", backup}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runExport() error = %v", err)
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	files, err := space.ParseBundle(data)
	if err != nil || len(files) != 1 || files[0].ID != ids[1] {
		t.Fatalf("exported = %+v, %v; want only %s", files, err, ids[1])
	}
}

func TestImportWaitsForTheWalletToStop(t *testing.T) {
	t.Parallel()

	source, _ := homeWithSpaces(t, "first")
	backup := filepath.Join(t.TempDir(), "spaces.wsbackup")
	if err := runExport([]string{"--home", source, "--out", backup}, &bytes.Buffer{}); err != nil {
		t.Fatalf("runExport() error = %v", err)
	}
	target := t.TempDir()
	running, err := storage.AcquireLock(target)
	if err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	defer running.Close()
	if err := runImport([]string{"--home", target, backup}, &bytes.Buffer{}); !errors.Is(err, storage.ErrAlreadyLocked) {
		t.Fatalf("runImport() into a running home error = %v, want ErrAlreadyLocked", err)
	}
}

func TestExportAndImportArgumentErrors(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	for _, tt := range []struct {
		name string
		run  func() error
	}{
		{name: "export without --out", run: func() error { return runExport([]string{"--home", home}, &bytes.Buffer{}) }},
		{name: "export with a stray argument", run: func() error {
			return runExport([]string{"--home", home, "--out", filepath.Join(home, "x"), "extra"}, &bytes.Buffer{})
		}},
		{name: "export with an empty space id", run: func() error {
			return runExport([]string{"--home", home, "--space", " ", "--out", filepath.Join(home, "x")}, &bytes.Buffer{})
		}},
		{name: "import without a file", run: func() error { return runImport([]string{"--home", home}, &bytes.Buffer{}) }},
		{name: "import of a missing file", run: func() error {
			return runImport([]string{"--home", home, filepath.Join(home, "missing")}, &bytes.Buffer{})
		}},
		{name: "import of a file that is not a backup", run: func() error {
			path := filepath.Join(t.TempDir(), "not-a-backup")
			if err := os.WriteFile(path, []byte(`{"format":"other"}`), 0o600); err != nil {
				return nil
			}
			return runImport([]string{"--home", t.TempDir(), path}, &bytes.Buffer{})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err == nil {
				t.Fatal("error = nil")
			}
		})
	}
	// -h is answered, not failed.
	if err := runExport([]string{"-h"}, &bytes.Buffer{}); err != nil {
		t.Errorf("runExport(-h) error = %v", err)
	}
	if err := runImport([]string{"-h"}, &bytes.Buffer{}); err != nil {
		t.Errorf("runImport(-h) error = %v", err)
	}
}
