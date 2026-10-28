package devicesync_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
)

func TestValidateRefusesWhatDoesNotHoldTogether(t *testing.T) {
	t.Parallel()

	if err := sampleSnapshot().Validate(); err != nil {
		t.Fatalf("Validate(sample) error = %v", err)
	}
	many := make([]account.Account, devicesync.MaxAccounts+1)
	for i := range many {
		many[i] = derived("acc_"+strings.Repeat("a", 1+i%40)+string(rune('a'+i%26)), 0, 0)
	}
	manyKeys := make(map[string][]devicesync.Key, devicesync.MaxAccounts+1)
	for i := range devicesync.MaxAccounts + 1 {
		manyKeys["key_"+strings.Repeat("k", 1+i/26)+string(rune('a'+i%26))] = key(1, 0)
	}
	// Within the reference count, over the variant count.
	manyVariants := make(map[string][]devicesync.Key, devicesync.MaxAccounts)
	for i := range devicesync.MaxKeyVariants + 1 {
		ref := "key_" + strings.Repeat("v", 1+i%devicesync.MaxAccounts/26) + string(rune('a'+i%devicesync.MaxAccounts%26))
		manyVariants[ref] = append(manyVariants[ref], variant(byte(i/devicesync.MaxAccounts), 0))
	}
	if len(manyVariants) > devicesync.MaxAccounts {
		t.Fatal("the case needs no more references than the ceiling")
	}
	// A zone offset Go decodes and does not encode, and a time whose year UTC
	// moves past 9999.
	badZone := at(0).In(time.FixedZone("", 24*3600))
	pastYears := time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("", -3600))
	for _, bad := range []time.Time{badZone, pastYears} {
		if _, err := bad.UTC().MarshalJSON(); err == nil {
			if _, err := bad.MarshalJSON(); err == nil {
				t.Fatalf("the case needs %v not to encode one way or the other", bad)
			}
		}
	}
	// Valid ids, so that only the count can refuse them.
	manyTombs := make([]devicesync.Tombstone, devicesync.MaxTombstones+1)
	manyKeyTombs := make([]devicesync.Tombstone, devicesync.MaxTombstones+1)
	for i := range manyTombs {
		manyTombs[i] = devicesync.Tombstone{ID: "acc_x"}
		manyKeyTombs[i] = devicesync.Tombstone{ID: "key_x"}
	}
	tests := []struct {
		name string
		edit func(*devicesync.Snapshot)
	}{
		{name: "another version", edit: func(s *devicesync.Snapshot) { s.Version = 2 }},
		{name: "a very long name", edit: func(s *devicesync.Snapshot) { s.Name = strings.Repeat("n", 257) }},
		{name: "too many wallets", edit: func(s *devicesync.Snapshot) { s.Accounts = many }},
		{name: "too many keys", edit: func(s *devicesync.Snapshot) { s.ImportedKeys = manyKeys }},
		{name: "too many wallet tombstones", edit: func(s *devicesync.Snapshot) { s.DeletedAccounts = manyTombs }},
		{name: "too many key tombstones", edit: func(s *devicesync.Snapshot) { s.DeletedKeys = manyKeyTombs }},
		{name: "a wallet id that is not one", edit: func(s *devicesync.Snapshot) { s.Accounts[0].ID = "../acc" }},
		{name: "a wallet id with capitals", edit: func(s *devicesync.Snapshot) { s.Accounts[0].ID = "acc_UPPER" }},
		{name: "an overlong wallet id", edit: func(s *devicesync.Snapshot) { s.Accounts[0].ID = "acc_" + strings.Repeat("a", 61) }},
		{name: "the same wallet twice", edit: func(s *devicesync.Snapshot) { s.Accounts[1] = s.Accounts[0] }},
		{name: "a derived wallet without an index", edit: func(s *devicesync.Snapshot) { s.Accounts[0].Index = nil }},
		{name: "an imported wallet without a key reference", edit: func(s *devicesync.Snapshot) { s.Accounts[1].KeyRef = "" }},
		{name: "an unknown kind", edit: func(s *devicesync.Snapshot) { s.Accounts[0].Kind = "watch" }},
		{name: "a key reference that is not one", edit: func(s *devicesync.Snapshot) {
			s.ImportedKeys["KEY_BAD"] = key(1, 0)
		}},
		{name: "too many key variants", edit: func(s *devicesync.Snapshot) { s.ImportedKeys = manyVariants }},
		{name: "an empty key", edit: func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_two"] = []devicesync.Key{{Curve: "secp256k1"}}
		}},
		{name: "an oversized key", edit: func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_two"] = []devicesync.Key{{Curve: "secp256k1", PrivateKey: make([]byte, 129)}}
		}},
		{name: "a key one byte short", edit: func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_two"] = []devicesync.Key{{Curve: "ed25519", PrivateKey: make([]byte, 31)}}
		}},
		{name: "a key without a curve", edit: func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_two"][0].Curve = ""
		}},
		{name: "a key on an unknown curve", edit: func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_two"][0].Curve = "sr25519"
		}},
		{name: "a key reference without variants", edit: func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_two"] = []devicesync.Key{}
		}},
		{name: "a write time that does not encode", edit: func(s *devicesync.Snapshot) { s.WrittenAt = badZone }},
		{name: "a name time that does not encode", edit: func(s *devicesync.Snapshot) { s.NameUpdatedAt = badZone }},
		{name: "a name time past 9999 in UTC", edit: func(s *devicesync.Snapshot) { s.NameUpdatedAt = pastYears }},
		{name: "a wallet creation time that does not encode", edit: func(s *devicesync.Snapshot) { s.Accounts[0].CreatedAt = badZone }},
		{name: "a wallet update time that does not encode", edit: func(s *devicesync.Snapshot) { s.Accounts[0].UpdatedAt = badZone }},
		{name: "a wallet import time that does not encode", edit: func(s *devicesync.Snapshot) { s.Accounts[1].ImportedAt = &badZone }},
		{name: "a wallet time past 9999 in UTC", edit: func(s *devicesync.Snapshot) { s.Accounts[0].UpdatedAt = pastYears }},
		{name: "a wallet deletion time that does not encode", edit: func(s *devicesync.Snapshot) {
			s.DeletedAccounts = []devicesync.Tombstone{{ID: "acc_gone", DeletedAt: badZone}}
		}},
		{name: "a key deletion time that does not encode", edit: func(s *devicesync.Snapshot) {
			s.DeletedKeys = []devicesync.Tombstone{{ID: "key_gone", DeletedAt: badZone}}
		}},
		{name: "a key deletion time past 9999 in UTC", edit: func(s *devicesync.Snapshot) {
			s.DeletedKeys = []devicesync.Tombstone{{ID: "key_gone", DeletedAt: pastYears}}
		}},
		{name: "a key import time that does not encode", edit: func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_two"][0].ImportedAt = badZone
		}},
		{name: "a wallet tombstone with a bad id", edit: func(s *devicesync.Snapshot) {
			s.DeletedAccounts = []devicesync.Tombstone{{ID: "key_x"}}
		}},
		{name: "a key tombstone with a bad id", edit: func(s *devicesync.Snapshot) {
			s.DeletedKeys = []devicesync.Tombstone{{ID: "acc_x"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := sampleSnapshot()
			tt.edit(&s)
			if err := s.Validate(); !errors.Is(err, devicesync.ErrInvalidSnapshot) {
				t.Fatalf("Validate() error = %v, want ErrInvalidSnapshot", err)
			}
		})
	}
}

func TestClearWipesTheKeys(t *testing.T) {
	t.Parallel()

	s := sampleSnapshot()
	var material [][]byte
	for _, k := range s.ImportedKeys["key_two"] {
		material = append(material, k.PrivateKey)
	}
	s.Clear()
	if len(s.ImportedKeys) != 0 {
		t.Fatal("Clear() left keys in the snapshot")
	}
	for _, m := range material {
		for _, b := range m {
			if b != 0 {
				t.Fatal("Clear() left key material in memory")
			}
		}
	}
}

// Imported wallets without a fingerprint, and keys that differ only in
// curve, are settled like the rest.
func TestMergeEdgeCases(t *testing.T) {
	t.Parallel()

	a := imported("acc_a", "key_a", "", 1)
	b := imported("acc_b", "key_b", "", 1)
	merged, report := devicesync.Merge([]devicesync.Snapshot{snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{a, b}
	})}, at(10))
	if len(merged.Accounts) != 2 || len(report.Duplicates) != 0 {
		t.Fatalf("wallets without fingerprints were treated as one key: %+v", report)
	}

	// One byte string on two curves is two keys.
	ed := devicesync.Key{Curve: "ed25519", PrivateKey: bytes.Repeat([]byte{1}, 32), ImportedAt: at(0)}
	secp := devicesync.Key{Curve: "secp256k1", PrivateKey: bytes.Repeat([]byte{1}, 32), ImportedAt: at(0)}
	want := []string{devicesync.VariantID(ed.Curve, ed.PrivateKey), devicesync.VariantID(secp.Curve, secp.PrivateKey)}
	slices.Sort(want)
	for _, order := range [][]devicesync.Key{{ed, secp}, {secp, ed}} {
		merged, report := devicesync.Merge([]devicesync.Snapshot{
			snapshot(func(s *devicesync.Snapshot) { s.ImportedKeys["key_x"] = order[:1] }),
			snapshot(func(s *devicesync.Snapshot) { s.ImportedKeys["key_x"] = order[1:] }),
		}, at(10))
		if got := variantIDs(merged.ImportedKeys["key_x"]); !slices.Equal(got, want) {
			t.Fatalf("key_x variants = %v, want both curves in variant id order %v", got, want)
		}
		if !slices.Equal(report.KeyConflicts, []string{"key_x"}) {
			t.Errorf("report.KeyConflicts = %v", report.KeyConflicts)
		}
	}
}

// The variant ceiling is a ceiling: exactly that many keys are taken.
func TestValidateTakesExactlyTheVariantCeiling(t *testing.T) {
	t.Parallel()

	s := sampleSnapshot()
	s.ImportedKeys = map[string][]devicesync.Key{}
	for i := range devicesync.MaxKeyVariants {
		ref := fmt.Sprintf("key_%d", i%devicesync.MaxAccounts)
		s.ImportedKeys[ref] = append(s.ImportedKeys[ref], variant(byte(i/devicesync.MaxAccounts), 0))
	}
	s.Accounts = s.Accounts[:1]
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate(%d variants) error = %v", devicesync.MaxKeyVariants, err)
	}
}
