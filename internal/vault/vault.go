// Package vault implements the versioned encrypted container used by spaces.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
)

const Version = 1

var ErrInvalidPassword = errors.New("invalid password or damaged vault")

// The KDF parameters come out of the container, which is a file on disk, so
// they are attacker-controlled the moment anything can write to the home
// directory. They are bounded in both directions.
//
// The ceilings stop a doctored container from turning one unlock into a
// gigabyte allocation and ten passes over it. The floors matter more: they used
// to be "any non-zero", so a rewritten header could pin a vault to 8 KiB and a
// single pass, and — because Seal reuses the parameters from the header it was
// opened with — every later re-seal would keep those settings. A password that
// was protected by 64 MiB × 3 would quietly become brute-forceable, with the
// file still opening normally.
const (
	minKDFTime      = 2
	maxKDFTime      = 10
	minKDFMemoryKiB = 32 * 1024
	// 256 MiB is far above what this project writes (64 MiB) and far below what
	// makes an unlock a memory event on a laptop.
	maxKDFMemoryKiB   = 256 * 1024
	maxKDFParallelism = 32
	maxCiphertextSize = 16 << 20
)

type KDF struct {
	Name        string `json:"name"`
	Time        uint32 `json:"time"`
	MemoryKiB   uint32 `json:"memory_kib"`
	Parallelism uint8  `json:"parallelism"`
	Salt        string `json:"salt"`
}

type Cipher struct {
	Name  string `json:"name"`
	Nonce string `json:"nonce"`
}

type Container struct {
	Version    int    `json:"version"`
	KDF        KDF    `json:"kdf"`
	Cipher     Cipher `json:"cipher"`
	Ciphertext string `json:"ciphertext"`
}

type Params struct {
	Time        uint32
	MemoryKiB   uint32
	Parallelism uint8
}

var DefaultParams = Params{Time: 3, MemoryKiB: 64 * 1024, Parallelism: 2}

// SessionKey is the password-derived key retained by an unlocked space. It can
// re-seal changed payloads without keeping the user's password in memory.
type SessionKey struct {
	key [32]byte
	kdf KDF
}

func (s *SessionKey) Clear() {
	if s != nil {
		clear(s.key[:])
	}
}

// validateKDF holds the container's work factors to a range that is both
// affordable and worth doing. It is applied on the way in and on the way out,
// so a weakened header cannot be adopted and then written back.
func validateKDF(kdf KDF) error {
	if kdf.Time < minKDFTime || kdf.Time > maxKDFTime ||
		kdf.MemoryKiB < minKDFMemoryKiB || kdf.MemoryKiB > maxKDFMemoryKiB ||
		kdf.Parallelism == 0 || kdf.Parallelism > maxKDFParallelism {
		return errors.New("invalid vault KDF parameters")
	}
	return nil
}

func Encrypt(password string, plaintext, aad []byte, params Params) (Container, error) {
	if password == "" {
		return Container{}, errors.New("vault password is required")
	}
	if err := validateKDF(KDF{
		Time: params.Time, MemoryKiB: params.MemoryKiB, Parallelism: params.Parallelism,
	}); err != nil {
		return Container{}, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return Container{}, fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, params.Time, params.MemoryKiB, params.Parallelism, 32)
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return Container{}, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Container{}, fmt.Errorf("create AEAD: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Container{}, fmt.Errorf("generate nonce: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, aad)
	return Container{
		Version: Version,
		KDF: KDF{
			Name: "argon2id", Time: params.Time, MemoryKiB: params.MemoryKiB,
			Parallelism: params.Parallelism, Salt: base64.RawStdEncoding.EncodeToString(salt),
		},
		Cipher:     Cipher{Name: "aes-256-gcm", Nonce: base64.RawStdEncoding.EncodeToString(nonce)},
		Ciphertext: base64.RawStdEncoding.EncodeToString(sealed),
	}, nil
}

func Decrypt(password string, container Container, aad []byte) ([]byte, error) {
	plaintext, session, err := Unlock(password, container, aad)
	if session != nil {
		session.Clear()
	}
	return plaintext, err
}

func Unlock(password string, container Container, aad []byte) ([]byte, *SessionKey, error) {
	if container.Version != Version || container.KDF.Name != "argon2id" || container.Cipher.Name != "aes-256-gcm" {
		return nil, nil, errors.New("unsupported vault format")
	}
	if err := validateKDF(container.KDF); err != nil {
		return nil, nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(container.KDF.Salt)
	if err != nil || len(salt) != 16 {
		return nil, nil, errors.New("invalid vault salt")
	}
	nonce, err := base64.RawStdEncoding.DecodeString(container.Cipher.Nonce)
	if err != nil {
		return nil, nil, errors.New("invalid vault nonce")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(container.Ciphertext)
	if err != nil {
		return nil, nil, errors.New("invalid vault ciphertext")
	}
	if len(ciphertext) < 16 || len(ciphertext) > maxCiphertextSize {
		return nil, nil, errors.New("invalid vault ciphertext")
	}
	key := argon2.IDKey([]byte(password), salt, container.KDF.Time, container.KDF.MemoryKiB, container.KDF.Parallelism, 32)
	defer clear(key)
	session := &SessionKey{kdf: container.KDF}
	copy(session.key[:], key)
	plaintext, err := openWithKey(session.key[:], nonce, ciphertext, aad)
	if err != nil {
		session.Clear()
		return nil, nil, err
	}
	return plaintext, session, nil
}

func openWithKey(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AEAD: %w", err)
	}
	if len(nonce) != aead.NonceSize() {
		return nil, errors.New("invalid vault nonce")
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrInvalidPassword
	}
	return plaintext, nil
}

func (s *SessionKey) Seal(plaintext, aad []byte) (Container, error) {
	if s == nil {
		return Container{}, errors.New("vault session key is missing")
	}
	// Re-checked rather than trusted because these came from the file that was
	// opened. Writing them back is what would make a weakened header permanent.
	if err := validateKDF(s.kdf); err != nil {
		return Container{}, err
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return Container{}, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Container{}, fmt.Errorf("create AEAD: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Container{}, fmt.Errorf("generate nonce: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, aad)
	return Container{
		Version: Version, KDF: s.kdf,
		Cipher:     Cipher{Name: "aes-256-gcm", Nonce: base64.RawStdEncoding.EncodeToString(nonce)},
		Ciphertext: base64.RawStdEncoding.EncodeToString(sealed),
	}, nil
}

func (s *SessionKey) SealJSON(value any, aad []byte) (Container, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Container{}, fmt.Errorf("encode vault payload: %w", err)
	}
	defer clear(data)
	return s.Seal(data, aad)
}

func SealJSON(password string, value any, aad []byte, params Params) (Container, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Container{}, fmt.Errorf("encode vault payload: %w", err)
	}
	defer clear(data)
	return Encrypt(password, data, aad, params)
}

func OpenJSON(password string, container Container, aad []byte, target any) error {
	data, err := Decrypt(password, container, aad)
	if err != nil {
		return err
	}
	defer clear(data)
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode vault payload: %w", err)
	}
	return nil
}

func UnlockJSON(password string, container Container, aad []byte, target any) (*SessionKey, error) {
	data, session, err := Unlock(password, container, aad)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	if err := json.Unmarshal(data, target); err != nil {
		session.Clear()
		return nil, fmt.Errorf("decode vault payload: %w", err)
	}
	return session, nil
}
