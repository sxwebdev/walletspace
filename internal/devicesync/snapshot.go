package devicesync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/storage"
)

// SnapshotVersion is the version of the plaintext inside an envelope.
const SnapshotVersion = 1

// Bounds on what a snapshot may hold. A snapshot comes from another machine
// through a folder anyone with write access can fill, so it is bounded before
// it is merged into anything. The space takes its wallet ceiling from here.
const (
	MaxAccounts = 512
	// MaxKeyVariants bounds the keys a snapshot carries, every variant of
	// every reference counted. A reference holds one key unless two machines
	// disagreed about it, so twice the wallet ceiling leaves room for that.
	MaxKeyVariants = 2 * MaxAccounts
	// MaxTombstones bounds each kind of tombstone. Tombstones are never
	// dropped, so this is set far beyond what a person deletes; a space that
	// passes it stops syncing with an error rather than send part of its
	// deletions. At ~70 bytes each it is a few megabytes at most.
	MaxTombstones = 64 * 1024
	MaxNameRunes  = 256
	// MaxIDLength bounds every id and key reference a snapshot carries.
	MaxIDLength = 64
)

// Snapshot is one machine's full view of a space. It is what a file holds and
// what Merge works on. The phrase, the BIP39 passphrase and the space password
// are never part of it.
type Snapshot struct {
	Version   int       `json:"version"`
	WrittenAt time.Time `json:"written_at"`

	Name          string    `json:"name"`
	NameUpdatedAt time.Time `json:"name_updated_at"`

	Accounts        []account.Account `json:"accounts"`
	DeletedAccounts []Tombstone       `json:"deleted_accounts"`

	// ImportedKeys are keyed by the KeyRef their accounts carry. A reference
	// holds every variant any machine has for it, sorted by VariantID: two
	// machines may hold different keys under one reference, and keeping only
	// one of them loses the other. Which variant a wallet uses is the space's
	// call, made against the wallet's own address.
	ImportedKeys map[string][]Key `json:"imported_keys"`
	DeletedKeys  []Tombstone      `json:"deleted_keys"`
}

// Tombstone records that a wallet or a key was deleted. Tombstones are never
// dropped: a deletion has to outlive every old snapshot that still holds the
// record, and there is no telling when the last of those is read.
type Tombstone struct {
	ID        string    `json:"id"`
	DeletedAt time.Time `json:"deleted_at"`
}

// Key is one variant of an imported private key, in the form the vault
// stores: a secp256k1 key as its 32-byte big-endian scalar, an ed25519 key as
// its 32-byte seed.
type Key struct {
	Curve      string    `json:"curve"`
	PrivateKey []byte    `json:"private_key"`
	ImportedAt time.Time `json:"imported_at"`
}

// The curves a key may be on, as a snapshot names them, and the length of a
// key on either.
const (
	CurveSecp256k1 = string(account.CurveSecp256k1)
	CurveEd25519   = string(account.CurveEd25519)
	KeyLength      = 32
)

// VariantID names a key variant: SHA-256 of the curve, a zero byte and the
// key, as lowercase hex. It is computed from the key every time, never read
// from a file, so a file cannot make two keys one variant or one key two.
func VariantID(curve string, privateKey []byte) string {
	input := make([]byte, 0, len(curve)+1+len(privateKey))
	input = append(input, curve...)
	input = append(input, 0)
	input = append(input, privateKey...)
	defer clear(input)
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:])
}

// ErrInvalidSnapshot reports a snapshot that does not hold together. A file
// carrying one is ignored as a whole.
var ErrInvalidSnapshot = errors.New("invalid sync snapshot")

// validID is the id shape a space keeps on disk, bounded in length because a
// snapshot comes from outside.
func validID(id, prefix string) bool {
	return len(id) <= MaxIDLength && storage.ValidID(id, prefix)
}

// Validate checks the shape of a snapshot: ids, sizes, the references between
// wallets and keys. Whether its addresses really come from the seed and its
// keys is checked by the space, which holds the seed.
func (s Snapshot) Validate() error {
	if s.Version != SnapshotVersion {
		return fmt.Errorf("%w: version %d", ErrInvalidSnapshot, s.Version)
	}
	if utf8.RuneCountInString(s.Name) > MaxNameRunes {
		return fmt.Errorf("%w: name too long", ErrInvalidSnapshot)
	}
	if !encodable(s.WrittenAt) || !encodable(s.NameUpdatedAt) {
		return fmt.Errorf("%w: a time that does not encode", ErrInvalidSnapshot)
	}
	if len(s.Accounts) > MaxAccounts {
		return fmt.Errorf("%w: more than %d wallets", ErrInvalidSnapshot, MaxAccounts)
	}
	if len(s.ImportedKeys) > MaxAccounts {
		return fmt.Errorf("%w: more than %d key references", ErrInvalidSnapshot, MaxAccounts)
	}
	if len(s.DeletedAccounts) > MaxTombstones || len(s.DeletedKeys) > MaxTombstones {
		return fmt.Errorf("%w: %d wallet and %d key deletions, more than the %d of each a sync file carries",
			ErrInvalidSnapshot, len(s.DeletedAccounts), len(s.DeletedKeys), MaxTombstones)
	}
	seen := make(map[string]struct{}, len(s.Accounts))
	for _, item := range s.Accounts {
		if !validID(item.ID, "acc_") {
			return fmt.Errorf("%w: wallet id %q", ErrInvalidSnapshot, item.ID)
		}
		if _, dup := seen[item.ID]; dup {
			return fmt.Errorf("%w: wallet %s twice", ErrInvalidSnapshot, item.ID)
		}
		seen[item.ID] = struct{}{}
		if !encodable(item.CreatedAt) || !encodable(item.UpdatedAt) || (item.ImportedAt != nil && !encodable(*item.ImportedAt)) {
			return fmt.Errorf("%w: wallet %s has a time that does not encode", ErrInvalidSnapshot, item.ID)
		}
		switch item.Kind {
		case account.KindDerived:
			if item.Index == nil {
				return fmt.Errorf("%w: derived wallet %s has no index", ErrInvalidSnapshot, item.ID)
			}
		case account.KindImported:
			if !validID(item.KeyRef, "key_") {
				return fmt.Errorf("%w: wallet %s key reference", ErrInvalidSnapshot, item.ID)
			}
		default:
			return fmt.Errorf("%w: wallet %s kind %q", ErrInvalidSnapshot, item.ID, item.Kind)
		}
	}
	variants := 0
	for ref, keys := range s.ImportedKeys {
		if !validID(ref, "key_") {
			return fmt.Errorf("%w: key reference %q", ErrInvalidSnapshot, ref)
		}
		if len(keys) == 0 {
			return fmt.Errorf("%w: key %s has no variants", ErrInvalidSnapshot, ref)
		}
		variants += len(keys)
		for _, key := range keys {
			// A variant is named by its curve and its key in the vault's form;
			// any other form would make one key two variants.
			if key.Curve != CurveSecp256k1 && key.Curve != CurveEd25519 {
				return fmt.Errorf("%w: key %s curve %q", ErrInvalidSnapshot, ref, key.Curve)
			}
			if len(key.PrivateKey) != KeyLength {
				return fmt.Errorf("%w: key %s is not %d bytes", ErrInvalidSnapshot, ref, KeyLength)
			}
			if !encodable(key.ImportedAt) {
				return fmt.Errorf("%w: key %s has a time that does not encode", ErrInvalidSnapshot, ref)
			}
		}
	}
	if variants > MaxKeyVariants {
		return fmt.Errorf("%w: more than %d keys", ErrInvalidSnapshot, MaxKeyVariants)
	}
	for _, tomb := range s.DeletedAccounts {
		if !validID(tomb.ID, "acc_") {
			return fmt.Errorf("%w: deleted wallet id %q", ErrInvalidSnapshot, tomb.ID)
		}
		if !encodable(tomb.DeletedAt) {
			return fmt.Errorf("%w: deletion of %s has a time that does not encode", ErrInvalidSnapshot, tomb.ID)
		}
	}
	for _, tomb := range s.DeletedKeys {
		if !validID(tomb.ID, "key_") {
			return fmt.Errorf("%w: deleted key reference %q", ErrInvalidSnapshot, tomb.ID)
		}
		if !encodable(tomb.DeletedAt) {
			return fmt.Errorf("%w: deletion of %s has a time that does not encode", ErrInvalidSnapshot, tomb.ID)
		}
	}
	return nil
}

// encodable reports whether a time can be written again, as it is and in UTC,
// which is how Merge keeps it. A time decodes with a zone offset out of range
// (+24:00) or a year that UTC moves past 9999, and then does not encode: a
// snapshot carrying one would stall every pass that tried to write it, here
// and on every machine it reached.
func encodable(t time.Time) bool {
	if _, err := t.MarshalJSON(); err != nil {
		return false
	}
	_, err := t.UTC().MarshalJSON()
	return err == nil
}

// Clear wipes the private keys a snapshot holds.
func (s *Snapshot) Clear() {
	for ref, keys := range s.ImportedKeys {
		for _, key := range keys {
			clear(key.PrivateKey)
		}
		delete(s.ImportedKeys, ref)
	}
}
