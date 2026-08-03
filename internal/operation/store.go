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
	"sort"
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

// NormalizeKey brings a client-supplied idempotency key to the one spelling
// this package stores it under. It exists so that every entry point agrees on
// it — which Begin and Update used not to do.
//
// Begin trimmed the key; Update took the raw header. net/textproto strips
// leading and trailing spaces and tabs from a header value, but not \v, \f or
// U+00A0, so a key padded with any of those was reserved under one name and
// looked up under another. The mismatch surfaced only after the transaction had
// been signed and broadcast: Update reported "operation not found", the txid
// was lost, the record stuck at building, and every retry got a permanent 409.
func NormalizeKey(key string) string {
	return strings.TrimSpace(key)
}

const (
	// maxOperations bounds the file. Every send, stake and deployment adds a
	// record and nothing ever removed one, so the file grew for the life of the
	// space — and it is read and rewritten in full on every operation, which
	// makes an unbounded file a cost paid on each one.
	maxOperations = 512
	// retention is how long a settled record is worth keeping. It answers the
	// replay of an interrupted request, which happens in seconds, and gives the
	// user a window in which the history is still there.
	retention = 30 * 24 * time.Hour
)

// resolved reports whether a record has reached a final state, so dropping it
// cannot lead to a duplicate transaction.
//
// This is a narrower question than InFlight asks. A confirmed transaction is
// on chain and its record is history; an unresolved one is what a replay is
// recognised by, and losing it would let the same intent be signed twice.
func resolved(status string) bool {
	return status == StatusConfirmed || status == StatusFailed
}

// prune keeps the file bounded, dropping the oldest droppable records and only
// as far as it has to.
//
// A record is droppable once it is resolved, or once it is old enough that an
// unresolved one is stale rather than pending. If nothing can be dropped, the
// new operation is refused rather than an unresolved record being displaced —
// being unable to start another transfer is recoverable; forgetting one that is
// already in flight is not.
func prune(operations map[string]Operation, now time.Time) error {
	if len(operations) < maxOperations {
		return nil
	}
	droppable := make([]Operation, 0, len(operations))
	for _, item := range operations {
		if !resolved(item.Status) && now.Sub(item.UpdatedAt) < retention {
			continue
		}
		droppable = append(droppable, item)
	}
	sort.Slice(droppable, func(i, j int) bool {
		return droppable[i].UpdatedAt.Before(droppable[j].UpdatedAt)
	})
	for _, item := range droppable {
		if len(operations) < maxOperations {
			break
		}
		delete(operations, item.Key)
	}
	if len(operations) >= maxOperations {
		return fmt.Errorf(
			"%d operations are still in progress; wait for them to settle", len(operations),
		)
	}
	return nil
}

// Begin reserves an idempotency key. existing is true when the same request was
// already reserved or completed.
func (s *Store) Begin(spaceID, key, requestHash, networkID string) (operation Operation, existing bool, err error) {
	if !storage.ValidID(spaceID, "spc_") {
		return Operation{}, false, errors.New("invalid space id")
	}
	key = NormalizeKey(key)
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
	if err := prune(current.Operations, now); err != nil {
		return Operation{}, false, err
	}
	created := Operation{
		Key: key, RequestHash: requestHash, NetworkID: networkID,
		Status: StatusBuilding, CreatedAt: now, UpdatedAt: now,
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
	key = NormalizeKey(key)
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
	// A transaction that is already known keeps its id. The recorder writes it
	// before the broadcast, and a later failure must not erase the one piece of
	// information that says which transaction may be on chain.
	if txHash != "" {
		item.TxHash = txHash
	}
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
