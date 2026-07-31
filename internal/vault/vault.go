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

const (
	maxKDFTime        = 10
	maxKDFMemoryKiB   = 1024 * 1024
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

func Encrypt(password string, plaintext, aad []byte, params Params) (Container, error) {
	if password == "" {
		return Container{}, errors.New("vault password is required")
	}
	if params.Time == 0 || params.MemoryKiB < 8 || params.Parallelism == 0 {
		return Container{}, errors.New("invalid Argon2id parameters")
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
	if container.KDF.Time == 0 || container.KDF.Time > maxKDFTime ||
		container.KDF.MemoryKiB < 8 || container.KDF.MemoryKiB > maxKDFMemoryKiB ||
		container.KDF.Parallelism == 0 || container.KDF.Parallelism > maxKDFParallelism {
		return nil, nil, errors.New("invalid vault KDF parameters")
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
