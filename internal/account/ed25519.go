package account

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/decred/base58"
	"github.com/sxwebdev/go-bip39"
)

// hardenedOffset is the first hardened child index, 2^31.
const hardenedOffset uint32 = 1 << 31

// solanaCoinType is SLIP-0044 coin type 501.
const solanaCoinType uint32 = 501

// ed25519KeypairSize is the Solana keypair layout: 32-byte seed then the
// 32-byte public key, as Phantom, Solflare and solana-keygen write it.
const ed25519KeypairSize = ed25519.PrivateKeySize

var errNotHardened = errors.New("SLIP-0010 ed25519 derives hardened children only")

// errIndexRange refuses an index that has no hardened form: adding 2^31 to it
// would wrap around.
var errIndexRange = errors.New("derivation index is out of the hardened range")

// DeriveEd25519PrivateKey derives family's ed25519 key at index from the
// BIP39 seed of mnemonic and passphrase, along m/44'/501'/index'/0' — every
// step hardened, which is the only derivation SLIP-0010 defines for ed25519,
// and the path Phantom and Solflare use for their accounts.
//
// The returned key is the 64-byte seed‖public-key form; the caller clears it.
func DeriveEd25519PrivateKey(mnemonic, passphrase string, family Family, index uint32) (ed25519.PrivateKey, error) {
	mnemonic = NormalizeMnemonic(mnemonic)
	if err := ValidateMnemonic(mnemonic); err != nil {
		return nil, err
	}
	var coinType uint32
	switch family {
	case FamilySolana:
		coinType = solanaCoinType
	case FamilyTron, FamilyEVM:
		return nil, fmt.Errorf("%w: %s keys are secp256k1", ErrCurveMismatch, family)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFamily, family)
	}
	if index >= hardenedOffset {
		return nil, fmt.Errorf("%w: %d", errIndexRange, index)
	}
	seed := bip39.NewSeed(mnemonic, passphrase)
	defer clear(seed)
	key, chainCode, err := slip10Ed25519(seed,
		hardenedOffset+44, hardenedOffset+coinType, hardenedOffset+index, hardenedOffset,
	)
	clear(chainCode)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	return ed25519.NewKeyFromSeed(key), nil
}

// slip10Ed25519 is SLIP-0010 master key generation and private child
// derivation for ed25519. It returns the private key and the chain code of
// the last node; the caller clears both.
func slip10Ed25519(seed []byte, path ...uint32) (key, chainCode []byte, err error) {
	sum := hmacSHA512([]byte("ed25519 seed"), seed)
	key, chainCode = sum[:32:32], sum[32:]
	data := make([]byte, 1+32+4)
	defer clear(data)
	for _, child := range path {
		if child < hardenedOffset {
			clear(sum)
			return nil, nil, errNotHardened
		}
		// 0x00 ‖ k_par ‖ ser32(i), keyed by the parent chain code.
		copy(data[1:33], key)
		binary.BigEndian.PutUint32(data[33:], child)
		next := hmacSHA512(chainCode, data)
		clear(sum)
		sum = next
		key, chainCode = sum[:32:32], sum[32:]
	}
	return key, chainCode, nil
}

func hmacSHA512(key, data []byte) []byte {
	mac := hmac.New(sha512.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// AddressFromEd25519Key returns family's address for an ed25519 key: the
// base58 of the 32-byte public key for Solana.
func AddressFromEd25519Key(key ed25519.PrivateKey, family Family) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidPrivateKey
	}
	switch family {
	case FamilySolana:
		return base58.Encode(key[32:]), nil
	case FamilyTron, FamilyEVM:
		return "", fmt.Errorf("%w: %s", ErrCurveMismatch, family)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedFamily, family)
	}
}

// ImportedEd25519Addresses is ImportedAddresses for an ed25519 key: the one
// family such a key has an address in.
func ImportedEd25519Addresses(key ed25519.PrivateKey) (map[Family]string, error) {
	address, err := AddressFromEd25519Key(key, FamilySolana)
	if err != nil {
		return nil, err
	}
	return map[Family]string{FamilySolana: address}, nil
}

// Ed25519Fingerprint identifies an ed25519 key the way Fingerprint identifies
// a secp256k1 one: the first 16 bytes of SHA-256 over the public key, in hex.
// The inputs differ in length (32 bytes here, a 33-byte compressed point
// there), so the two kinds cannot be mistaken for one another.
func Ed25519Fingerprint(key ed25519.PrivateKey) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidPrivateKey
	}
	sum := sha256.Sum256(key[32:])
	return hex.EncodeToString(sum[:16]), nil
}

// ParseEd25519PrivateKey reads an exported Solana keypair: the base58 of the
// 64-byte seed‖public-key (Phantom, Solflare) or the JSON array of the same 64
// numbers that solana-keygen writes to id.json. The public half has to be the
// one the seed produces; a keypair whose halves disagree is refused rather
// than trusted for either half.
//
// The returned key is a fresh 64-byte slice; the caller clears it.
func ParseEd25519PrivateKey(value string) (ed25519.PrivateKey, error) {
	value = strings.TrimSpace(value)
	var raw []byte
	if strings.HasPrefix(value, "[") {
		var numbers []int
		err := json.Unmarshal([]byte(value), &numbers)
		defer clear(numbers)
		if err != nil {
			return nil, fmt.Errorf("%w: expected a JSON array of 64 numbers", ErrInvalidPrivateKey)
		}
		if len(numbers) != ed25519KeypairSize {
			return nil, fmt.Errorf("%w: expected exactly 64 bytes", ErrInvalidPrivateKey)
		}
		raw = make([]byte, ed25519KeypairSize)
		for i, number := range numbers {
			if number < 0 || number > 0xff {
				clear(raw)
				return nil, fmt.Errorf("%w: array element is not a byte", ErrInvalidPrivateKey)
			}
			raw[i] = byte(number)
		}
	} else {
		// Decred's alphabet is Bitcoin's, which is Solana's. Decode answers
		// an empty slice for a character outside it.
		raw = base58.Decode(value)
		if len(raw) == 0 {
			return nil, fmt.Errorf("%w: expected a base58 keypair", ErrInvalidPrivateKey)
		}
	}
	defer clear(raw)
	if len(raw) != ed25519KeypairSize {
		return nil, fmt.Errorf("%w: expected exactly 64 bytes", ErrInvalidPrivateKey)
	}
	key := ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])
	if !bytes.Equal(key[ed25519.SeedSize:], raw[ed25519.SeedSize:]) {
		clear(key)
		return nil, fmt.Errorf("%w: the public key does not belong to the private key", ErrInvalidPrivateKey)
	}
	return key, nil
}

// Ed25519KeypairBase58 is the export form of an ed25519 key: base58 of the
// 64-byte seed‖public-key, which Phantom and Solflare import as is.
func Ed25519KeypairBase58(key ed25519.PrivateKey) (string, error) {
	if len(key) != ed25519.PrivateKeySize {
		return "", ErrInvalidPrivateKey
	}
	return base58.Encode(key), nil
}
