package devicesync

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// The envelope is what a sync file holds, byte for byte:
//
//	"WSYN" | version (1 byte) | space tag (32) | device tag (32) | nonce (24) | ciphertext
//
// Everything before the nonce is the header, and the header is the AEAD's
// associated data: a file moved under another space's folder or another
// device's name no longer opens.
const (
	envelopeMagic   = "WSYN"
	EnvelopeVersion = 1
	headerLength    = len(envelopeMagic) + 1 + 2*TagLength
	// MaxEnvelopeBytes bounds a file before it is read whole. A space at its
	// wallet ceiling, every wallet an imported key, is well under a megabyte.
	MaxEnvelopeBytes = 8 << 20
)

// ErrUnreadable reports a file that does not open: damaged, truncated, under
// another name, or sealed with another seed's key. Such a file is skipped, the
// rest of the folder still counts.
var ErrUnreadable = errors.New("sync file does not open")

func header(spaceTag, deviceTag string) ([]byte, error) {
	if !ValidTag(spaceTag) || !ValidTag(deviceTag) {
		return nil, errors.New("invalid sync tag")
	}
	out := make([]byte, 0, headerLength)
	out = append(out, envelopeMagic...)
	out = append(out, EnvelopeVersion)
	out = append(out, spaceTag...)
	out = append(out, deviceTag...)
	return out, nil
}

// EnvelopeHeaderLength is how many leading bytes of a sealed file name it.
const EnvelopeHeaderLength = headerLength

// EnvelopeTags reads the space and device tag a sealed file was made for from
// its first EnvelopeHeaderLength bytes. It needs no key and proves nothing:
// the header is authenticated only when the file is opened. It is for a
// machine reading back a file it sealed itself.
func EnvelopeTags(blob []byte) (spaceTag, deviceTag string, ok bool) {
	if len(blob) < headerLength || string(blob[:len(envelopeMagic)]) != envelopeMagic ||
		blob[len(envelopeMagic)] != EnvelopeVersion {
		return "", "", false
	}
	tags := blob[len(envelopeMagic)+1 : headerLength]
	spaceTag, deviceTag = string(tags[:TagLength]), string(tags[TagLength:])
	if !ValidTag(spaceTag) || !ValidTag(deviceTag) {
		return "", "", false
	}
	return spaceTag, deviceTag, true
}

// Seal encodes and encrypts a snapshot for the file named by the two tags.
func Seal(keys *Keys, spaceTag, deviceTag string, snapshot Snapshot) ([]byte, error) {
	return seal(keys, spaceTag, deviceTag, snapshot, rand.Reader)
}

func seal(keys *Keys, spaceTag, deviceTag string, snapshot Snapshot, random io.Reader) ([]byte, error) {
	// Cleared keys are all zero, and a file sealed under them opens for anyone.
	if keys == nil || keys.enc == [32]byte{} {
		return nil, errors.New("sync keys are missing")
	}
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	head, err := header(spaceTag, deviceTag)
	if err != nil {
		return nil, err
	}
	// A Snapshot is plain data and the key is always 32 bytes: the two
	// errors below cannot happen and are not covered by tests.
	plaintext, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	defer clear(plaintext)
	aead, err := chacha20poly1305.NewX(keys.enc[:])
	if err != nil {
		return nil, err
	}
	if err := checkSealedSize(len(plaintext)); err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	out := make([]byte, 0, len(head)+len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, head...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, head), nil
}

// longestStamp is a write time in its longest encoding: nine fractional
// digits and a zone offset.
var longestStamp = time.Date(2000, 1, 1, 0, 0, 0, 999_999_999, time.FixedZone("", -(23*3600+59*60)))

// Sealable reports whether Seal would take snapshot whenever it is sealed: it
// holds together, and sealed it stays within what Open reads. The write time
// Seal is given is not known yet, so the size is checked with the longest one
// a time encodes to. A space checks this before it keeps a merge, so that a
// pass that cannot write its file changes nothing here.
func Sealable(snapshot Snapshot) error {
	snapshot.WrittenAt = longestStamp
	if err := snapshot.Validate(); err != nil {
		return err
	}
	plaintext, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	defer clear(plaintext)
	return checkSealedSize(len(plaintext))
}

// checkSealedSize refuses a plaintext that would seal past MaxEnvelopeBytes.
// Open refuses such a file, this machine's own included, and the snapshot's
// own bounds do not keep it under: refusing here makes the pass fail where the
// person sees it, instead of writing a file nobody reads.
func checkSealedSize(plaintext int) error {
	size := headerLength + chacha20poly1305.NonceSizeX + plaintext + chacha20poly1305.Overhead
	if size > MaxEnvelopeBytes {
		return fmt.Errorf("%w: %d bytes once sealed, more than %d", ErrInvalidSnapshot, size, MaxEnvelopeBytes)
	}
	return nil
}

// Open authenticates and decodes the file named by the two tags. Anything but
// a well-formed snapshot sealed for exactly that name under these keys is
// ErrUnreadable or ErrInvalidSnapshot.
func Open(keys *Keys, spaceTag, deviceTag string, blob []byte) (Snapshot, error) {
	if keys == nil {
		return Snapshot{}, errors.New("sync keys are missing")
	}
	head, err := header(spaceTag, deviceTag)
	if err != nil {
		return Snapshot{}, err
	}
	if len(blob) > MaxEnvelopeBytes {
		return Snapshot{}, fmt.Errorf("%w: larger than %d bytes", ErrUnreadable, MaxEnvelopeBytes)
	}
	aead, err := chacha20poly1305.NewX(keys.enc[:])
	if err != nil {
		return Snapshot{}, err // unreachable: the key is always 32 bytes
	}
	if len(blob) < headerLength+aead.NonceSize()+aead.Overhead() || !bytes.Equal(blob[:headerLength], head) {
		return Snapshot{}, ErrUnreadable
	}
	nonce := blob[headerLength : headerLength+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, blob[headerLength+aead.NonceSize():], head)
	if err != nil {
		return Snapshot{}, ErrUnreadable
	}
	defer clear(plaintext)
	var snapshot Snapshot
	if err := json.Unmarshal(plaintext, &snapshot); err != nil {
		// Keys decoded before the error are wiped like the rest.
		snapshot.Clear()
		return Snapshot{}, fmt.Errorf("%w: %v", ErrInvalidSnapshot, err)
	}
	if err := snapshot.Validate(); err != nil {
		snapshot.Clear()
		return Snapshot{}, err
	}
	return snapshot, nil
}
