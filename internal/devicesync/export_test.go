package devicesync

import (
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

// SealWith seals with a chosen nonce source, for the golden vector.
func SealWith(keys *Keys, spaceTag, deviceTag string, snapshot Snapshot, random io.Reader) ([]byte, error) {
	return seal(keys, spaceTag, deviceTag, snapshot, random)
}

// SealRaw seals arbitrary plaintext under a correct header, which Seal never
// does: it is how a test hands Open a well-sealed file whose content is not a
// valid snapshot.
func SealRaw(keys *Keys, spaceTag, deviceTag string, plaintext []byte) ([]byte, error) {
	head, err := header(spaceTag, deviceTag)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(keys.enc[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	out := append(append([]byte(nil), head...), nonce...)
	return aead.Seal(out, nonce, plaintext, head), nil
}

// Canonical is the encoding account ties are settled by.
var Canonical = canonical

// LongestStamp is the write time Sealable checks a snapshot with.
var LongestStamp = longestStamp

// CanonicalRecord is the shape the canonical encoding writes.
type CanonicalRecord = canonicalRecord

// RecordSecretCopies makes every copy of key material Merge takes from now
// on land in *copies, until the returned function puts the copier back.
func RecordSecretCopies(copies *[][]byte) (restore func()) {
	original := cloneSecret
	cloneSecret = func(secret []byte) []byte {
		out := original(secret)
		*copies = append(*copies, out)
		return out
	}
	return func() { cloneSecret = original }
}
