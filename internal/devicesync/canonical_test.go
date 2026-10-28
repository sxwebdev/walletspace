package devicesync_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
)

// The canonical encoding of a wallet record, byte for byte (docs/device-sync.md,
// "Canonical encoding"). The expected encodings were produced by a separate
// implementation of the spec — Python's JSON encoder with sorted keys and no
// ASCII escaping, which matches RFC 8785 for these strings, with keys sorted by
// UTF-16 code units — and are quoted as JSON strings here, so that every byte
// of them is visible.
func TestCanonicalEncoding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		record string
		want   string
	}{
		{
			// Every field; times in three zones with fractions; an index of 0;
			// networks repeated and out of order; address keys that UTF-8 and
			// UTF-16 order differently; every escape JCS makes, and characters
			// Go's own encoder would escape (<, >, &, U+2028) that JCS does not.
			name:   "a full record",
			record: `{"id": "acc_full", "label": "q\"\\\b\f\n\r\t\u0001\u001f\u007f<>& /é😀", "kind": "imported", "family": "evm", "network_ids": ["b", "a", "b"], "addresses": {"｡": "x", "😀": "y", "evm": "0xE"}, "index": 0, "derivation_profile": "p", "key_ref": "key_r", "fingerprint": "f", "imported_at": "2026-10-26T13:00:00.120000000+01:00", "created_at": "2026-10-26T12:00:00.000Z", "updated_at": "2026-10-26T06:30:00.000000001-05:30"}`,
			want:   `"{\"addresses\":{\"evm\":\"0xE\",\"😀\":\"y\",\"｡\":\"x\"},\"created_at\":\"2026-10-26T12:00:00Z\",\"derivation_profile\":\"p\",\"family\":\"evm\",\"fingerprint\":\"f\",\"id\":\"acc_full\",\"imported_at\":\"2026-10-26T12:00:00.12Z\",\"index\":0,\"key_ref\":\"key_r\",\"kind\":\"imported\",\"label\":\"q\\\"\\\\\\b\\f\\n\\r\\t\\u0001\\u001f\u007f<>& /é😀\",\"network_ids\":[\"a\",\"b\"],\"updated_at\":\"2026-10-26T12:00:00.000000001Z\"}"`,
		},
		{
			// Empty optional strings, no index, no import time, no networks,
			// no addresses at all: the fields that are always there, and
			// addresses as an empty object.
			name:   "a minimal record",
			record: `{"id": "acc_min", "label": "", "kind": "derived", "family": "", "network_ids": [], "addresses": null, "derivation_profile": "", "key_ref": "", "fingerprint": "", "created_at": "0001-01-01T00:00:00Z", "updated_at": "2026-10-26T12:00:00Z"}`,
			want:   `"{\"addresses\":{},\"created_at\":\"0001-01-01T00:00:00Z\",\"id\":\"acc_min\",\"kind\":\"derived\",\"label\":\"\",\"updated_at\":\"2026-10-26T12:00:00Z\"}"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var item account.Account
			if err := json.Unmarshal([]byte(tt.record), &item); err != nil {
				t.Fatal(err)
			}
			var want string
			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatal(err)
			}
			if got := string(devicesync.Canonical(item)); got != want {
				t.Errorf("canonical =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// Sealable says beforehand what Seal would refuse: a snapshot past its own
// bounds, and one within them that seals past what Open reads.
func TestSealableRefusesWhatSealRefuses(t *testing.T) {
	t.Parallel()

	if err := devicesync.Sealable(sampleSnapshot()); err != nil {
		t.Fatalf("Sealable(sample) error = %v", err)
	}
	invalid := sampleSnapshot()
	invalid.DeletedAccounts = make([]devicesync.Tombstone, devicesync.MaxTombstones+1)
	for i := range invalid.DeletedAccounts {
		invalid.DeletedAccounts[i] = devicesync.Tombstone{ID: "acc_x", DeletedAt: at(0)}
	}
	big := sampleSnapshot()
	big.DeletedAccounts = make([]devicesync.Tombstone, devicesync.MaxTombstones)
	big.DeletedKeys = make([]devicesync.Tombstone, devicesync.MaxTombstones)
	for i := range devicesync.MaxTombstones {
		big.DeletedAccounts[i] = devicesync.Tombstone{ID: fmt.Sprintf("acc_%020d", i), DeletedAt: at(i)}
		big.DeletedKeys[i] = devicesync.Tombstone{ID: fmt.Sprintf("key_%020d", i), DeletedAt: at(i)}
	}
	if err := big.Validate(); err != nil {
		t.Fatalf("Validate(big) error = %v; the case needs a snapshot within its bounds", err)
	}
	for name, snap := range map[string]devicesync.Snapshot{"too many tombstones": invalid, "too large once sealed": big} {
		if err := devicesync.Sealable(snap); !errors.Is(err, devicesync.ErrInvalidSnapshot) {
			t.Errorf("%s: Sealable() error = %v, want ErrInvalidSnapshot", name, err)
		}
	}
}

// Ties are settled by the canonical encoding, so every field of a wallet
// record has to be in it: a field it left out could differ between two copies
// that encode the same, and the copy kept would then depend on the order the
// snapshots came in. A field added to account.Account later fails here until
// the encoding carries it.
func TestEveryWalletFieldIsInTheCanonicalEncoding(t *testing.T) {
	t.Parallel()

	base := imported("acc_base", "key_base", "fp", 1)
	base.Family, base.DerivationProfile, base.Index = account.FamilyEVM, "p", new(uint32(1))
	want := string(devicesync.Canonical(base))
	other := at(9)
	fields := reflect.TypeFor[account.Account]()
	for i := range fields.NumField() {
		field := fields.Field(i)
		changed := base.Clone()
		value := reflect.ValueOf(&changed).Elem().Field(i)
		switch {
		case field.Type == reflect.TypeFor[time.Time]():
			value.Set(reflect.ValueOf(other))
		case field.Type == reflect.TypeFor[*time.Time]():
			value.Set(reflect.ValueOf(&other))
		case field.Type == reflect.TypeFor[*uint32]():
			value.Set(reflect.ValueOf(new(uint32(7))))
		case field.Type.Kind() == reflect.String:
			value.SetString(value.String() + "x")
		case field.Type.Kind() == reflect.Slice:
			value.Set(reflect.Append(value, reflect.ValueOf("zz").Convert(field.Type.Elem())))
		case field.Type.Kind() == reflect.Map:
			value.SetMapIndex(reflect.ValueOf("zz").Convert(field.Type.Key()), reflect.ValueOf("zz").Convert(field.Type.Elem()))
		default:
			t.Fatalf("field %s has a type this test does not know how to change: %s", field.Name, field.Type)
		}
		if string(devicesync.Canonical(changed)) == want {
			t.Errorf("changing %s does not change the canonical encoding", field.Name)
		}
	}
	// And the encoding has no member the record does not.
	if got, want := reflect.TypeFor[devicesync.CanonicalRecord]().NumField(), fields.NumField(); got != want {
		t.Errorf("the canonical record has %d fields, a wallet %d", got, want)
	}
}

// Sealable is asked before the write time is known; Seal stamps one later. A
// snapshot that seals to exactly the ceiling under the shortest write time
// would not seal under a real one, so Sealable must judge it by the longest.
func TestSealableAllowsForTheWriteTime(t *testing.T) {
	t.Parallel()

	keys := keysOf(t, testMnemonic, "")
	spaceTag, deviceTag := keys.SpaceTag(), keys.DeviceTag("dev_a", "spc_one")
	sealedSize := func(s devicesync.Snapshot) int {
		blob, err := devicesync.Seal(keys, spaceTag, deviceTag, s)
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		return len(blob)
	}
	withTombstones := func(n int) devicesync.Snapshot {
		s := sampleSnapshot()
		s.WrittenAt = time.Time{}
		// Each kind has its own bound; ids of one width on both.
		s.DeletedAccounts, s.DeletedKeys = nil, nil
		for i := range n {
			if i < devicesync.MaxTombstones {
				s.DeletedAccounts = append(s.DeletedAccounts, devicesync.Tombstone{ID: fmt.Sprintf("acc_%020d", i), DeletedAt: at(0)})
			} else {
				s.DeletedKeys = append(s.DeletedKeys, devicesync.Tombstone{ID: fmt.Sprintf("key_%020d", i), DeletedAt: at(0)})
			}
		}
		return s
	}
	// Tombstones of one width grow the file by the same amount each; a label
	// fills what is left to the byte.
	base, step := sealedSize(withTombstones(1)), sealedSize(withTombstones(2))-sealedSize(withTombstones(1))
	exact := withTombstones(1 + (devicesync.MaxEnvelopeBytes-base)/step)
	exact.Accounts[0].Label += strings.Repeat("l", devicesync.MaxEnvelopeBytes-sealedSize(exact))
	if got := sealedSize(exact); got != devicesync.MaxEnvelopeBytes {
		t.Fatalf("the case needs a snapshot that seals to exactly %d bytes, got %d", devicesync.MaxEnvelopeBytes, got)
	}
	if err := devicesync.Sealable(exact); !errors.Is(err, devicesync.ErrInvalidSnapshot) {
		t.Errorf("Sealable(exactly at the ceiling with no write time) error = %v, want ErrInvalidSnapshot", err)
	}
	stamped := exact
	stamped.WrittenAt = time.Date(2026, 10, 27, 12, 0, 0, 123456789, time.UTC)
	if _, err := devicesync.Seal(keys, spaceTag, deviceTag, stamped); err == nil {
		t.Fatal("the case needs a real write time to push it past the ceiling")
	}

	// The longest a write time encodes to: nine fractional digits and a zone
	// offset, 35 characters — 15 more than the zero time's 20. Sealable
	// checks with exactly that.
	stamp, err := devicesync.LongestStamp.MarshalJSON()
	if err != nil || len(stamp) != len(`"2000-01-01T00:00:00.999999999-23:59"`) {
		t.Fatalf("Sealable's write time encodes to %s (%v), want the longest form, 35 characters", stamp, err)
	}
	trimmed := func(n int) devicesync.Snapshot {
		s := exact
		s.Accounts = slices.Clone(exact.Accounts)
		s.Accounts[0].Label = s.Accounts[0].Label[:len(s.Accounts[0].Label)-n]
		return s
	}
	// One byte short of room for the longest time is refused; room for it
	// is taken, and seals under any time at all.
	if err := devicesync.Sealable(trimmed(14)); !errors.Is(err, devicesync.ErrInvalidSnapshot) {
		t.Errorf("Sealable(one byte past room for the longest write time) error = %v, want ErrInvalidSnapshot", err)
	}
	roomy := trimmed(15)
	if err := devicesync.Sealable(roomy); err != nil {
		t.Fatalf("Sealable(with room for the write time) error = %v", err)
	}
	roomy.WrittenAt = time.Date(2026, 10, 27, 12, 0, 0, 999999999, time.FixedZone("", -(23*3600+59*60)))
	if _, err := devicesync.Seal(keys, spaceTag, deviceTag, roomy); err != nil {
		t.Errorf("Seal() of what Sealable took, under the longest write time, error = %v", err)
	}
}
