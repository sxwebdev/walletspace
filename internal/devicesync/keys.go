// Package devicesync keeps one space current across the machines that hold its
// recovery phrase.
//
// Every machine writes one file per space into a shared folder and only reads
// the others'. A file is a snapshot of the space as that machine sees it,
// sealed with a key derived from the space's BIP39 seed, so the folder — and
// whoever stores it — holds nothing but ciphertext under tags it cannot relate
// to anything. Snapshots are merged here, by a pure function, rather than by
// the folder's provider. The design is docs/device-sync.md.
package devicesync

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// TagLength is the length of a space or device tag: 128 bits as hex.
const TagLength = 32

// Keys are what a space's seed yields for sync. They exist only while the
// space is unlocked and are cleared with it.
type Keys struct {
	enc [32]byte
	tag [32]byte
}

// DeriveKeys turns a BIP39 seed into the sync keys. Every machine holding the
// same phrase and passphrase gets the same keys, and nothing about them leads
// back to the seed.
func DeriveKeys(seed []byte) (*Keys, error) {
	if len(seed) < 32 {
		return nil, errors.New("sync keys need a BIP39 seed")
	}
	// hkdf.Key fails only for an output longer than 255 hashes; these errors
	// cannot happen and are not covered by tests.
	root, err := hkdf.Key(sha256.New, seed, nil, "walletspace/sync/v1/root", 32)
	if err != nil {
		return nil, err
	}
	defer clear(root)
	keys := &Keys{}
	for _, part := range []struct {
		info string
		into *[32]byte
	}{{"enc", &keys.enc}, {"tag", &keys.tag}} {
		derived, err := hkdf.Key(sha256.New, root, nil, part.info, 32)
		if err != nil {
			keys.Clear()
			return nil, err
		}
		copy(part.into[:], derived)
		clear(derived)
	}
	return keys, nil
}

// Clone returns an independent copy, for work done outside whatever guards
// the original: clearing the original when its space locks does not reach a
// sync already in flight, and the copy is cleared by whoever made it.
func (k *Keys) Clone() *Keys {
	clone := *k
	return &clone
}

// Clear wipes the keys.
func (k *Keys) Clear() {
	if k != nil {
		clear(k.enc[:])
		clear(k.tag[:])
	}
}

func (k *Keys) tagOf(message string) string {
	mac := hmac.New(sha256.New, k.tag[:])
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))[:TagLength]
}

// SpaceTag names the space's folder. Every machine with the seed computes the
// same one.
func (k *Keys) SpaceTag() string {
	return k.tagOf("space")
}

// DeviceTag names one machine's file in the space's folder. It mixes in the
// local space id because one machine may hold the same phrase twice, and two
// writers must never share a file; and it differs per space, so the folder does
// not show which spaces live on the same machine.
func (k *Keys) DeviceTag(deviceID, localSpaceID string) string {
	return k.tagOf("device:" + deviceID + ":" + localSpaceID)
}

// ValidTag reports whether s has the shape of a tag. Folder entries that do
// not are not ours and are ignored.
func ValidTag(s string) bool {
	if len(s) != TagLength {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
