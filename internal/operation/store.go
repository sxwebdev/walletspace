// Package operation persists idempotency keys and locally known transaction state.
package operation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sxwebdev/walletspace/internal/storage"
)

const SchemaVersion = 1

var ErrConflict = errors.New("idempotency key was already used for another request")

type Operation struct {
	Key         string    `json:"key"`
	RequestHash string    `json:"request_hash"`
	NetworkID   string    `json:"network_id"`
	TxHash      string    `json:"tx_hash,omitempty"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type file struct {
	SchemaVersion int                  `json:"schema_version"`
	Operations    map[string]Operation `json:"operations"`
}

type Store struct {
	home string
	mu   sync.Mutex
}

func New(home string) *Store { return &Store{home: home} }

func RequestHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode canonical request: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Begin reserves an idempotency key. existing is true when the same request was
// already reserved or completed.
func (s *Store) Begin(spaceID, key, requestHash, networkID string) (operation Operation, existing bool, err error) {
	if !storage.ValidID(spaceID, "spc_") {
		return Operation{}, false, errors.New("invalid space id")
	}
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 128 {
		return Operation{}, false, errors.New("Idempotency-Key is required and must be at most 128 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.loadLocked(spaceID)
	if err != nil {
		return Operation{}, false, err
	}
	if previous, ok := current.Operations[key]; ok {
		if previous.RequestHash != requestHash {
			return Operation{}, false, ErrConflict
		}
		return previous, true, nil
	}
	now := time.Now().UTC()
	created := Operation{
		Key: key, RequestHash: requestHash, NetworkID: networkID,
		Status: "building", CreatedAt: now, UpdatedAt: now,
	}
	current.Operations[key] = created
	if err := s.saveLocked(spaceID, current); err != nil {
		return Operation{}, false, err
	}
	return created, false, nil
}

func (s *Store) Update(spaceID, key, txHash, status string) (Operation, error) {
	if !storage.ValidID(spaceID, "spc_") {
		return Operation{}, errors.New("invalid space id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.loadLocked(spaceID)
	if err != nil {
		return Operation{}, err
	}
	item, ok := current.Operations[key]
	if !ok {
		return Operation{}, errors.New("operation not found")
	}
	item.TxHash = txHash
	item.Status = status
	item.UpdatedAt = time.Now().UTC()
	current.Operations[key] = item
	if err := s.saveLocked(spaceID, current); err != nil {
		return Operation{}, err
	}
	return item, nil
}

func (s *Store) UpdateByTxHash(spaceID, txHash, status string) (bool, error) {
	if !storage.ValidID(spaceID, "spc_") {
		return false, errors.New("invalid space id")
	}
	if txHash == "" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.loadLocked(spaceID)
	if err != nil {
		return false, err
	}
	for key, item := range current.Operations {
		if item.TxHash != txHash {
			continue
		}
		if item.Status == status {
			return true, nil
		}
		item.Status = status
		item.UpdatedAt = time.Now().UTC()
		current.Operations[key] = item
		if err := s.saveLocked(spaceID, current); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (s *Store) loadLocked(spaceID string) (file, error) {
	path := filepath.Join(s.home, "spaces", spaceID, "operations.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file{SchemaVersion: SchemaVersion, Operations: make(map[string]Operation)}, nil
	}
	if err != nil {
		return file{}, fmt.Errorf("read operations: %w", err)
	}
	var current file
	if err := json.Unmarshal(data, &current); err != nil {
		return file{}, fmt.Errorf("decode operations: %w", err)
	}
	if current.SchemaVersion != SchemaVersion {
		return file{}, fmt.Errorf("unsupported operations schema version %d", current.SchemaVersion)
	}
	if current.Operations == nil {
		current.Operations = make(map[string]Operation)
	}
	return current, nil
}

func (s *Store) saveLocked(spaceID string, current file) error {
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("encode operations: %w", err)
	}
	return storage.AtomicWrite(
		filepath.Join(s.home, "spaces", spaceID, "operations.json"),
		append(data, '\n'),
	)
}
