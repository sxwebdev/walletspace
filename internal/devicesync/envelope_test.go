package devicesync_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
)

var update = flag.Bool("update", false, "rewrite the golden sync vector")

var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

func derived(id string, index uint32, updated int) account.Account {
	return account.Account{
		ID: id, Label: "derived " + id, Kind: account.KindDerived, Family: account.FamilyTron,
		NetworkIDs: []string{"tron-nile"}, Addresses: map[account.Family]string{account.FamilyTron: "T" + id},
		Index: new(index), DerivationProfile: "bip44-v1", CreatedAt: at(0), UpdatedAt: at(updated),
	}
}

func imported(id, keyRef, fingerprint string, updated int) account.Account {
	return account.Account{
		ID: id, Label: "imported " + id, Kind: account.KindImported, NetworkIDs: []string{"eth-sepolia"},
		Addresses: map[account.Family]string{account.FamilyEVM: "0x" + id}, KeyRef: keyRef,
		Fingerprint: fingerprint, ImportedAt: new(at(0)), CreatedAt: at(0), UpdatedAt: at(updated),
	}
}

func sampleSnapshot() devicesync.Snapshot {
	return devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, WrittenAt: at(10),
		Name: "Main", NameUpdatedAt: at(1),
		Accounts:        []account.Account{derived("acc_one", 0, 2), imported("acc_two", "key_two", "fp2", 3)},
		DeletedAccounts: []devicesync.Tombstone{{ID: "acc_gone", DeletedAt: at(4)}},
		// Two variants under one reference, in variant id order, as two
		// machines that disagreed leave it.
		ImportedKeys: map[string][]devicesync.Key{
			"key_two": sortedVariants(
				devicesync.Key{Curve: "secp256k1", PrivateKey: bytes.Repeat([]byte{7}, 32), ImportedAt: at(0)},
				devicesync.Key{Curve: "ed25519", PrivateKey: bytes.Repeat([]byte{9}, 32), ImportedAt: at(1)},
			),
		},
		DeletedKeys: []devicesync.Tombstone{{ID: "key_gone", DeletedAt: at(5)}},
	}
}

func sortedVariants(keys ...devicesync.Key) []devicesync.Key {
	slices.SortFunc(keys, func(a, b devicesync.Key) int {
		return strings.Compare(devicesync.VariantID(a.Curve, a.PrivateKey), devicesync.VariantID(b.Curve, b.PrivateKey))
	})
	return keys
}

type vector struct {
	Mnemonic     string              `json:"mnemonic"`
	Passphrase   string              `json:"passphrase"`
	DeviceID     string              `json:"device_id"`
	LocalSpaceID string              `json:"local_space_id"`
	SpaceTag     string              `json:"space_tag"`
	DeviceTag    string              `json:"device_tag"`
	Nonce        string              `json:"nonce"`
	Snapshot     devicesync.Snapshot `json:"snapshot"`
	Envelope     string              `json:"envelope"`
}

// The vector pins the format: any client in any language that reads these
// files must produce these tags and this envelope from these inputs. A change
// that moves it is a change of format and needs a new version.
func TestGoldenVector(t *testing.T) {
	t.Parallel()

	path := filepath.Join("testdata", "envelope_v1.json")
	want := vector{
		Mnemonic: testMnemonic, Passphrase: "TREZOR", DeviceID: "dev_vector", LocalSpaceID: "spc_vector",
		Nonce: hex.EncodeToString(bytes.Repeat([]byte{0x42}, 24)), Snapshot: sampleSnapshot(),
	}
	keys := keysOf(t, want.Mnemonic, want.Passphrase)
	want.SpaceTag = keys.SpaceTag()
	want.DeviceTag = keys.DeviceTag(want.DeviceID, want.LocalSpaceID)
	nonce, _ := hex.DecodeString(want.Nonce)
	blob, err := devicesync.SealWith(keys, want.SpaceTag, want.DeviceTag, want.Snapshot, bytes.NewReader(nonce))
	if err != nil {
		t.Fatalf("SealWith() error = %v", err)
	}
	want.Envelope = hex.EncodeToString(blob)

	if *update {
		data, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vector (run with -update to create it): %v", err)
	}
	var golden vector
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	if golden.SpaceTag != want.SpaceTag || golden.DeviceTag != want.DeviceTag {
		t.Errorf("tags = %s / %s, golden %s / %s", want.SpaceTag, want.DeviceTag, golden.SpaceTag, golden.DeviceTag)
	}
	if golden.Envelope != want.Envelope {
		t.Errorf("envelope differs from the golden vector")
	}
	raw, _ := hex.DecodeString(golden.Envelope)
	opened, err := devicesync.Open(keys, golden.SpaceTag, golden.DeviceTag, raw)
	if err != nil {
		t.Fatalf("Open(golden) error = %v", err)
	}
	if !reflect.DeepEqual(opened, golden.Snapshot) {
		t.Errorf("Open(golden) = %+v\nwant %+v", opened, golden.Snapshot)
	}
}

func TestAFileOpensOnlyUnderItsOwnName(t *testing.T) {
	t.Parallel()

	keys := keysOf(t, testMnemonic, "")
	spaceTag, deviceTag := keys.SpaceTag(), keys.DeviceTag("dev_a", "spc_one")
	blob, err := devicesync.Seal(keys, spaceTag, deviceTag, sampleSnapshot())
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if _, err := devicesync.Open(keys, spaceTag, deviceTag, blob); err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	flipped := bytes.Clone(blob)
	flipped[len(flipped)-1] ^= 1
	tests := []struct {
		name      string
		keys      *devicesync.Keys
		spaceTag  string
		deviceTag string
		blob      []byte
	}{
		{name: "another seed", keys: keysOf(t, otherMnemonic, ""), spaceTag: spaceTag, deviceTag: deviceTag, blob: blob},
		{name: "another device's name", keys: keys, spaceTag: spaceTag, deviceTag: keys.DeviceTag("dev_b", "spc_one"), blob: blob},
		{name: "another space's folder", keys: keys, spaceTag: keysOf(t, otherMnemonic, "").SpaceTag(), deviceTag: deviceTag, blob: blob},
		{name: "one bit flipped", keys: keys, spaceTag: spaceTag, deviceTag: deviceTag, blob: flipped},
		{name: "truncated", keys: keys, spaceTag: spaceTag, deviceTag: deviceTag, blob: blob[:len(blob)-1]},
		{name: "header only", keys: keys, spaceTag: spaceTag, deviceTag: deviceTag, blob: blob[:69]},
		{name: "empty", keys: keys, spaceTag: spaceTag, deviceTag: deviceTag, blob: nil},
		{name: "too large", keys: keys, spaceTag: spaceTag, deviceTag: deviceTag, blob: make([]byte, devicesync.MaxEnvelopeBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := devicesync.Open(tt.keys, tt.spaceTag, tt.deviceTag, tt.blob); !errors.Is(err, devicesync.ErrUnreadable) {
				t.Fatalf("Open() error = %v, want ErrUnreadable", err)
			}
		})
	}
}

func TestSealAndOpenRefuseBadInput(t *testing.T) {
	t.Parallel()

	keys := keysOf(t, testMnemonic, "")
	spaceTag, deviceTag := keys.SpaceTag(), keys.DeviceTag("dev_a", "spc_one")

	if _, err := devicesync.Seal(nil, spaceTag, deviceTag, sampleSnapshot()); err == nil {
		t.Error("Seal(nil keys) succeeded")
	}
	// Cleared keys are zero: what they seal opens for anyone.
	cleared := keys.Clone()
	cleared.Clear()
	if _, err := devicesync.Seal(cleared, spaceTag, deviceTag, sampleSnapshot()); err == nil {
		t.Error("Seal(cleared keys) succeeded")
	}
	if _, err := devicesync.Open(nil, spaceTag, deviceTag, nil); err == nil {
		t.Error("Open(nil keys) succeeded")
	}
	if _, err := devicesync.Seal(keys, "../escape", deviceTag, sampleSnapshot()); err == nil {
		t.Error("Seal(bad space tag) succeeded")
	}
	if _, err := devicesync.Open(keys, spaceTag, "../escape", nil); err == nil {
		t.Error("Open(bad device tag) succeeded")
	}
	bad := sampleSnapshot()
	bad.Version = 2
	if _, err := devicesync.Seal(keys, spaceTag, deviceTag, bad); !errors.Is(err, devicesync.ErrInvalidSnapshot) {
		t.Errorf("Seal(invalid snapshot) error = %v, want ErrInvalidSnapshot", err)
	}
	if _, err := devicesync.SealWith(keys, spaceTag, deviceTag, sampleSnapshot(), bytes.NewReader(nil)); err == nil {
		t.Error("Seal without randomness succeeded")
	}

	// A file sealed correctly whose content is not a valid snapshot is
	// refused as invalid, not as unreadable.
	for name, plaintext := range map[string][]byte{
		"not JSON":    []byte("not json"),
		"new version": []byte(`{"version":2}`),
	} {
		blob, err := devicesync.SealRaw(keys, spaceTag, deviceTag, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := devicesync.Open(keys, spaceTag, deviceTag, blob); !errors.Is(err, devicesync.ErrInvalidSnapshot) {
			t.Errorf("%s: Open() error = %v, want ErrInvalidSnapshot", name, err)
		}
	}
}

func TestTwoSealsOfOneSnapshotDiffer(t *testing.T) {
	t.Parallel()

	keys := keysOf(t, testMnemonic, "")
	spaceTag, deviceTag := keys.SpaceTag(), keys.DeviceTag("dev_a", "spc_one")
	first, err := devicesync.Seal(keys, spaceTag, deviceTag, sampleSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	second, err := devicesync.Seal(keys, spaceTag, deviceTag, sampleSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two seals reused a nonce")
	}
	if bytes.Contains(first, []byte("Main")) || bytes.Contains(first, []byte("acc_one")) {
		t.Fatal("the envelope carries plaintext")
	}
}

// Every reader refuses a file past MaxEnvelopeBytes, the writer itself
// included. The snapshot's own bounds allow more than that — a space with
// tombstones near their ceiling — so Seal must refuse it rather than write a
// file that nobody will ever open while the pass reports a write.
func TestSealRefusesWhatOpenWouldRefuse(t *testing.T) {
	t.Parallel()

	keys := keysOf(t, testMnemonic, "")
	spaceTag, deviceTag := keys.SpaceTag(), keys.DeviceTag("dev_a", "spc_one")
	big := sampleSnapshot() // ids as long as the ones a space makes (storage.NewID)
	big.DeletedAccounts = make([]devicesync.Tombstone, devicesync.MaxTombstones)
	big.DeletedKeys = make([]devicesync.Tombstone, devicesync.MaxTombstones)
	for i := range devicesync.MaxTombstones {
		big.DeletedAccounts[i] = devicesync.Tombstone{ID: fmt.Sprintf("acc_%020d", i), DeletedAt: at(i)}
		big.DeletedKeys[i] = devicesync.Tombstone{ID: fmt.Sprintf("key_%020d", i), DeletedAt: at(i)}
	}
	if err := big.Validate(); err != nil {
		t.Fatalf("Validate() error = %v; the case needs a snapshot within its bounds", err)
	}
	blob, err := devicesync.Seal(keys, spaceTag, deviceTag, big)
	if err == nil {
		_, openErr := devicesync.Open(keys, spaceTag, deviceTag, blob)
		t.Fatalf("Seal() wrote %d bytes; Open() of it: %v", len(blob), openErr)
	}
	if !errors.Is(err, devicesync.ErrInvalidSnapshot) {
		t.Errorf("Seal() error = %v, want ErrInvalidSnapshot", err)
	}
}
