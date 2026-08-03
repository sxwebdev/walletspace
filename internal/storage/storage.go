// Package storage contains the durable filesystem primitives used by Walletspace.
package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const HomeEnv = "WALLETSPACE_HOME"

func ResolveHome(explicit string) (string, error) {
	if value := strings.TrimSpace(explicit); value != "" {
		return filepath.Abs(value)
	}
	if value := strings.TrimSpace(os.Getenv(HomeEnv)); value != "" {
		return filepath.Abs(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, ".walletspace"), nil
}

func EnsureHome(home string) error {
	if home == "" {
		return errors.New("walletspace home is empty")
	}
	for _, path := range []string{home, filepath.Join(home, "spaces"), filepath.Join(home, "cache")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("secure %s: %w", path, err)
		}
	}
	return nil
}

func AtomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	fail := func(operation string, cause error) error {
		_ = tmp.Close()
		return fmt.Errorf("%s %s: %w", operation, path, cause)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail("secure", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent directory %s: %w", dir, err)
	}
	if err := dirHandle.Sync(); err != nil {
		_ = dirHandle.Close()
		return fmt.Errorf("sync parent directory %s: %w", dir, err)
	}
	if err := dirHandle.Close(); err != nil {
		return fmt.Errorf("close parent directory %s: %w", dir, err)
	}
	return nil
}

func ValidID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) {
		return false
	}
	for _, r := range id[len(prefix):] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
