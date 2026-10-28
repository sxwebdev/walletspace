package devicesync_test

import (
	"bytes"
	"testing"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
)

const (
	testMnemonic  = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	otherMnemonic = "legal winner thank year wave sausage worth useful legal winner thank yellow"
)

func seedOf(t *testing.T, mnemonic, passphrase string) []byte {
	t.Helper()
	seed, err := account.Seed(mnemonic, passphrase)
	if err != nil {
		t.Fatalf("Seed() error = %v", err)
	}
	return seed
}

func keysOf(t *testing.T, mnemonic, passphrase string) *devicesync.Keys {
	t.Helper()
	keys, err := devicesync.DeriveKeys(seedOf(t, mnemonic, passphrase))
	if err != nil {
		t.Fatalf("DeriveKeys() error = %v", err)
	}
	t.Cleanup(keys.Clear)
	return keys
}

func TestTheSameSeedFindsTheSameFolder(t *testing.T) {
	t.Parallel()

	first := keysOf(t, testMnemonic, "")
	second := keysOf(t, testMnemonic, "")
	if first.SpaceTag() != second.SpaceTag() {
		t.Fatal("two derivations of one seed name different folders")
	}
	if !devicesync.ValidTag(first.SpaceTag()) {
		t.Fatalf("SpaceTag() = %q is not a tag", first.SpaceTag())
	}

	// Another phrase, or the same phrase with a BIP39 passphrase, is another
	// space and another folder.
	for name, keys := range map[string]*devicesync.Keys{
		"other phrase":      keysOf(t, otherMnemonic, ""),
		"with a passphrase": keysOf(t, testMnemonic, "extra"),
	} {
		if keys.SpaceTag() == first.SpaceTag() {
			t.Errorf("%s shares the folder of the test phrase", name)
		}
	}
}

func TestDeviceTagsDoNotLinkMachinesOrSpaces(t *testing.T) {
	t.Parallel()

	keys := keysOf(t, testMnemonic, "")
	other := keysOf(t, otherMnemonic, "")
	tags := map[string]string{
		"device a, space 1":           keys.DeviceTag("dev_a", "spc_one"),
		"device b, space 1":           keys.DeviceTag("dev_b", "spc_one"),
		"device a, space 2 same seed": keys.DeviceTag("dev_a", "spc_two"),
		"device a, other seed":        other.DeviceTag("dev_a", "spc_one"),
		"the space tag":               keys.SpaceTag(),
	}
	seen := make(map[string]string)
	for name, tag := range tags {
		if !devicesync.ValidTag(tag) {
			t.Errorf("%s: %q is not a tag", name, tag)
		}
		if previous, dup := seen[tag]; dup {
			t.Errorf("%s and %s share the tag %s", name, previous, tag)
		}
		seen[tag] = name
	}
	if keys.DeviceTag("dev_a", "spc_one") != keysOf(t, testMnemonic, "").DeviceTag("dev_a", "spc_one") {
		t.Error("a device's tag is not stable")
	}
}

func TestDeriveKeysRefusesWhatIsNotASeed(t *testing.T) {
	t.Parallel()

	if _, err := devicesync.DeriveKeys(nil); err == nil {
		t.Error("DeriveKeys(nil) succeeded")
	}
	if _, err := devicesync.DeriveKeys(make([]byte, 16)); err == nil {
		t.Error("DeriveKeys(16 bytes) succeeded")
	}
}

func TestClearedKeysNoLongerOpenAFile(t *testing.T) {
	t.Parallel()

	keys := keysOf(t, testMnemonic, "")
	spaceTag, deviceTag := keys.SpaceTag(), keys.DeviceTag("dev_a", "spc_one")
	blob, err := devicesync.Seal(keys, spaceTag, deviceTag, devicesync.Snapshot{Version: devicesync.SnapshotVersion})
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	keys.Clear()
	if _, err := devicesync.Open(keys, spaceTag, deviceTag, blob); err == nil {
		t.Fatal("cleared keys still opened the file")
	}
	var nilKeys *devicesync.Keys
	nilKeys.Clear()
}

func TestValidTag(t *testing.T) {
	t.Parallel()

	for tag, want := range map[string]bool{
		"0123456789abcdef0123456789abcdef":    true,
		"0123456789ABCDEF0123456789abcdef":    false,
		"0123456789abcdef0123456789abcde":     false,
		"0123456789abcdef0123456789abcdef0":   false,
		"../3456789abcdef0123456789abcdef":    false,
		"":                                    false,
		string(bytes.Repeat([]byte("g"), 32)): false,
	} {
		if got := devicesync.ValidTag(tag); got != want {
			t.Errorf("ValidTag(%q) = %v, want %v", tag, got, want)
		}
	}
}

// A sync in flight works on a clone, so the space locking mid-pass clears
// the original without pulling the keys out from under the pass — and the
// pass clearing its clone leaves the session's keys alone.
func TestAClonedKeyOutlivesTheOriginal(t *testing.T) {
	t.Parallel()

	original := keysOf(t, testMnemonic, "")
	tag := original.SpaceTag()
	clone := original.Clone()
	original.Clear()
	if clone.SpaceTag() != tag {
		t.Fatal("clearing the original changed the clone")
	}
	restored := keysOf(t, testMnemonic, "")
	copied := restored.Clone()
	copied.Clear()
	if restored.SpaceTag() != tag {
		t.Fatal("clearing the clone changed the original")
	}
}
