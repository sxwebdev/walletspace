// Package wallet keeps the list of derived Tron wallets on disk.
//
// Only derivation indexes and addresses are persisted. Private keys are
// derived from the mnemonic on demand and never touch the filesystem.
package wallet

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sxwebdev/gotron/pkg/address"
)

// ErrNotFound is returned when no wallet exists for a derivation index.
var ErrNotFound = errors.New("wallet not found")

// Wallet is a single derived account.
type Wallet struct {
	Index     uint32    `json:"index"`
	Address   string    `json:"address"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

type file struct {
	// Fingerprint is the address at index 0. It guards against starting with a
	// different mnemonic or passphrase than the one the file was created with.
	Fingerprint string   `json:"fingerprint"`
	Wallets     []Wallet `json:"wallets"`
}

// Store is a concurrency-safe, file-backed list of wallets.
type Store struct {
	path       string
	mnemonic   string
	passphrase string
	// fingerprint is the address derived at index 0. It is what the stored file
	// is checked against, and what gets written back — never wallets[0], which
	// need not be index 0.
	fingerprint string

	mu      sync.RWMutex
	wallets []Wallet
}

// New loads the store from path, creating it with a single wallet at index 0
// when the file does not exist yet. It fails when the stored fingerprint does
// not match the address derived from the given mnemonic and passphrase.
func New(path, mnemonic, passphrase string) (*Store, error) {
	s := &Store{path: path, mnemonic: mnemonic, passphrase: passphrase}

	first, err := s.derive(0)
	if err != nil {
		return nil, fmt.Errorf("derive wallet 0: %w", err)
	}
	s.fingerprint = first.Address

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f file
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		// A file that lists wallets but carries no fingerprint cannot be proven
		// to belong to this mnemonic, so it is rejected just like a mismatch.
		// Accepting it would let the UI show addresses whose keys we do not
		// hold, and every send from those rows would sign with the wrong key.
		if len(f.Wallets) > 0 && f.Fingerprint != first.Address {
			stored := f.Fingerprint
			if stored == "" {
				stored = "none"
			}

			return nil, fmt.Errorf(
				"%s was created with a different mnemonic or passphrase: "+
					"expected wallet 0 to be %s, stored fingerprint is %s",
				path, first.Address, stored,
			)
		}

		s.wallets = f.Wallets
		if len(s.wallets) == 0 {
			s.wallets = []Wallet{{Index: 0, Address: first.Address, CreatedAt: time.Now().UTC()}}
			if err := s.save(); err != nil {
				return nil, err
			}
		}

	case errors.Is(err, os.ErrNotExist):
		s.wallets = []Wallet{{Index: 0, Address: first.Address, CreatedAt: time.Now().UTC()}}
		if err := s.save(); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return s, nil
}

// List returns a copy of all known wallets, ordered by derivation index.
func (s *Store) List() []Wallet {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Wallet, len(s.wallets))
	copy(out, s.wallets)

	return out
}

// Get returns the wallet with the given derivation index.
func (s *Store) Get(index uint32) (Wallet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, w := range s.wallets {
		if w.Index == index {
			return w, nil
		}
	}

	return Wallet{}, fmt.Errorf("%w: index %d", ErrNotFound, index)
}

// Create derives the next unused index, appends it and persists the store.
func (s *Store) Create(label string) (Wallet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var next uint32
	if n := len(s.wallets); n > 0 {
		last := s.wallets[n-1].Index
		if last == ^uint32(0) {
			return Wallet{}, errors.New("derivation index space exhausted")
		}
		next = last + 1
	}

	addr, err := s.derive(next)
	if err != nil {
		return Wallet{}, fmt.Errorf("derive wallet %d: %w", next, err)
	}

	w := Wallet{
		Index:     next,
		Address:   addr.Address,
		Label:     label,
		CreatedAt: time.Now().UTC(),
	}

	s.wallets = append(s.wallets, w)
	if err := s.save(); err != nil {
		s.wallets = s.wallets[:len(s.wallets)-1]
		return Wallet{}, err
	}

	return w, nil
}

// Rename sets the label of an existing wallet.
func (s *Store) Rename(index uint32, label string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.wallets {
		if s.wallets[i].Index != index {
			continue
		}

		prev := s.wallets[i].Label
		s.wallets[i].Label = label
		if err := s.save(); err != nil {
			s.wallets[i].Label = prev
			return err
		}

		return nil
	}

	return fmt.Errorf("%w: index %d", ErrNotFound, index)
}

// PrivateKey derives the signing key for a wallet. The key is derived on every
// call and is never stored.
func (s *Store) PrivateKey(index uint32) (*ecdsa.PrivateKey, error) {
	if _, err := s.Get(index); err != nil {
		return nil, err
	}

	addr, err := s.derive(index)
	if err != nil {
		return nil, fmt.Errorf("derive wallet %d: %w", index, err)
	}

	return addr.PrivateKeyECDSA, nil
}

func (s *Store) derive(index uint32) (*address.Address, error) {
	return address.FromMnemonic(s.mnemonic, s.passphrase, index)
}

// save writes the store atomically and durably. Callers must hold the write lock.
func (s *Store) save() error {
	data, err := json.MarshalIndent(file{Fingerprint: s.fingerprint, Wallets: s.wallets}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode wallets: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".wallets-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmp.Name())

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}

	// Without this the rename can outlive the data on a crash, leaving a
	// zero-length wallets.json behind.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("replace %s: %w", s.path, err)
	}

	// Persist the rename itself, so the new name survives a crash too.
	if d, err := os.Open(dir); err == nil {
		defer d.Close()
		_ = d.Sync()
	}

	return nil
}
