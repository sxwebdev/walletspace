// Package account owns Walletspace account metadata and deterministic key derivation.
package account

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/decred/dcrd/hdkeychain/v3"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sxwebdev/go-bip39"
	tronaddress "github.com/sxwebdev/gotron/pkg/address"
)

type (
	Kind   string
	Family string
)

const (
	KindDerived  Kind   = "derived"
	KindImported Kind   = "imported"
	FamilyTron   Family = "tron"
	FamilyEVM    Family = "evm"
)

var (
	ErrInvalidPrivateKey = errors.New("invalid private key")
	ErrInvalidMnemonic   = errors.New("invalid mnemonic")
	ErrUnsupportedFamily = errors.New("unsupported address family")
)

type Account struct {
	ID                string            `json:"id"`
	Label             string            `json:"label"`
	Kind              Kind              `json:"kind"`
	Family            Family            `json:"family,omitempty"`
	NetworkIDs        []string          `json:"network_ids,omitempty"`
	Addresses         map[Family]string `json:"addresses"`
	Index             *uint32           `json:"index,omitempty"`
	DerivationProfile string            `json:"derivation_profile,omitempty"`
	KeyRef            string            `json:"key_ref,omitempty"`
	Fingerprint       string            `json:"fingerprint,omitempty"`
	ImportedAt        *time.Time        `json:"imported_at,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// BoundTo reports whether the wallet was explicitly enabled for networkID.
// Empty NetworkIDs are intentionally not treated as "all networks": records
// written before network bindings were introduced must be assigned by the
// user, because guessing their original network can expose the wrong wallet.
func (a Account) BoundTo(networkID string) bool {
	return slices.Contains(a.NetworkIDs, networkID)
}

type hdNetParams struct{}

func (hdNetParams) HDPrivKeyVersion() [4]byte { return [4]byte{0x04, 0x88, 0xad, 0xe4} }
func (hdNetParams) HDPubKeyVersion() [4]byte  { return [4]byte{0x04, 0x88, 0xb2, 0x1e} }

func NormalizeMnemonic(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func ValidateMnemonic(value string) error {
	if !bip39.IsMnemonicValid(NormalizeMnemonic(value)) {
		return ErrInvalidMnemonic
	}
	return nil
}

func GenerateMnemonic() (string, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return "", fmt.Errorf("generate entropy: %w", err)
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return "", fmt.Errorf("generate mnemonic: %w", err)
	}
	return mnemonic, nil
}

func DerivePrivateKey(mnemonic, passphrase string, family Family, index uint32) (*ecdsa.PrivateKey, error) {
	mnemonic = NormalizeMnemonic(mnemonic)
	if err := ValidateMnemonic(mnemonic); err != nil {
		return nil, err
	}
	var coinType uint32
	switch family {
	case FamilyTron:
		coinType = 195
	case FamilyEVM:
		coinType = 60
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFamily, family)
	}
	seed := bip39.NewSeed(mnemonic, passphrase)
	defer clear(seed)
	key, err := hdkeychain.NewMaster(seed, hdNetParams{})
	if err != nil {
		return nil, fmt.Errorf("create BIP32 master key: %w", err)
	}
	for _, child := range []uint32{
		hdkeychain.HardenedKeyStart + 44,
		hdkeychain.HardenedKeyStart + coinType,
		hdkeychain.HardenedKeyStart,
		0,
		index,
	} {
		key, err = key.ChildBIP32Std(child)
		if err != nil {
			return nil, fmt.Errorf("derive BIP44 child %d: %w", child, err)
		}
	}
	raw, err := key.SerializedPrivKey()
	if err != nil {
		return nil, fmt.Errorf("serialize derived private key: %w", err)
	}
	defer clear(raw)
	privateKey, err := crypto.ToECDSA(raw)
	if err != nil {
		return nil, fmt.Errorf("decode derived private key: %w", err)
	}
	return privateKey, nil
}

func DerivedAddresses(mnemonic, passphrase string, index uint32) (map[Family]string, error) {
	out := make(map[Family]string, 2)
	for _, family := range []Family{FamilyTron, FamilyEVM} {
		key, err := DerivePrivateKey(mnemonic, passphrase, family, index)
		if err != nil {
			return nil, err
		}
		addr, err := AddressFromPrivateKey(key, family)
		if err != nil {
			return nil, err
		}
		out[family] = addr
	}
	return out, nil
}

func DerivedAddress(mnemonic, passphrase string, family Family, index uint32) (string, error) {
	key, err := DerivePrivateKey(mnemonic, passphrase, family, index)
	if err != nil {
		return "", err
	}
	return AddressFromPrivateKey(key, family)
}

func ParsePrivateKey(value string) (*ecdsa.PrivateKey, []byte, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "0x")
	value = strings.TrimPrefix(value, "0X")
	if len(value) != 64 {
		return nil, nil, fmt.Errorf("%w: expected exactly 32 bytes", ErrInvalidPrivateKey)
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: expected hexadecimal input", ErrInvalidPrivateKey)
	}
	key, err := crypto.ToECDSA(raw)
	if err != nil {
		clear(raw)
		return nil, nil, fmt.Errorf("%w: scalar is outside secp256k1 range", ErrInvalidPrivateKey)
	}
	return key, raw, nil
}

func ImportedAddresses(key *ecdsa.PrivateKey) (map[Family]string, error) {
	if key == nil {
		return nil, ErrInvalidPrivateKey
	}
	tron, err := AddressFromPrivateKey(key, FamilyTron)
	if err != nil {
		return nil, err
	}
	evm, err := AddressFromPrivateKey(key, FamilyEVM)
	if err != nil {
		return nil, err
	}
	return map[Family]string{FamilyTron: tron, FamilyEVM: evm}, nil
}

func AddressFromPrivateKey(key *ecdsa.PrivateKey, family Family) (string, error) {
	if key == nil {
		return "", ErrInvalidPrivateKey
	}
	switch family {
	case FamilyEVM:
		return crypto.PubkeyToAddress(key.PublicKey).Hex(), nil
	case FamilyTron:
		raw := crypto.FromECDSA(key)
		defer clear(raw)
		item, err := tronaddress.FromPrivateKey(hex.EncodeToString(raw))
		if err != nil {
			return "", fmt.Errorf("derive Tron address: %w", err)
		}
		return item.Address, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedFamily, family)
	}
}

func Fingerprint(key *ecdsa.PrivateKey) (string, error) {
	if key == nil {
		return "", ErrInvalidPrivateKey
	}
	publicKey := crypto.CompressPubkey(&key.PublicKey)
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:16]), nil
}

func PrivateKeyHex(key *ecdsa.PrivateKey) (string, error) {
	if key == nil || key.D == nil {
		return "", ErrInvalidPrivateKey
	}
	raw := make([]byte, 32)
	key.D.FillBytes(raw)
	defer clear(raw)
	return hex.EncodeToString(raw), nil
}
