package account_test

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/decred/base58"
	"github.com/sxwebdev/walletspace/internal/account"
)

// Mnemonic → Solana vectors along m/44'/501'/i'/0'. Source: computed outside
// this repository with two independent stacks that agreed on every value —
// bip39 3.1.0 + ed25519-hd-key 1.3.0 + @solana/web3.js 1.98.0 Keypair.fromSeed
// (the recipe of the Solana cookbook and Phantom's documented path), and
// @scure/bip39 1.5.0 + micro-ed25519-hdkey 0.1.2. The address of index 0
// without a passphrase is also the widely published vector for this
// mnemonic. The keypair is the 64-byte base58 form those tools export.
var solanaVectors = []struct {
	passphrase string
	index      uint32
	address    string
	keypair    string
}{
	{
		"", 0, "HAgk14JpMQLgt6rVgv7cBQFJWFto5Dqxi472uT3DKpqk",
		"27npWoNE4HfmLeQo1TyWcW7NEA28qnsnDK7kcttDQEWrCWnro83HMJ97rMmpvYYZRwDAvG4KRuB7hTBacvwD7bgi",
	},
	{
		"", 1, "Hh8QwFUA6MtVu1qAoq12ucvFHNwCcVTV7hpWjeY1Hztb",
		"4j7ege68VuZqaYrPZcuTXXJR28FHiMtphxDeYDXM6XoswiddwrCFcresCn8r1Hiw4MuiYGfeWvqRe7ibnxw8Xzaw",
	},
	{
		"TREZOR", 0, "7zSmbu6gKkb6HB7UDPtHYjwCWuBHU1D4TpNZFm4sndQe",
		"5yTLvhbem7H3sbPnCM6nY4PHLUkjucF224WLqY1qx58AeNGa88fsQqBhq1LwoRESPvMm6fRFURFYkGt398dXZiGi",
	},
	{
		"TREZOR", 1, "8CW93AFYFKj2rKZRq3YjLesb4XbgtAiq8k2FgfruVZL5",
		"5QYycWKPj9uJ3PYuBh34mU24jmNkT4jAbf4Hfze7zT52GJGZt7KAJu7rMu87hyP1cRwHS4T8CDaRqYgSnue698uX",
	},
}

func TestDerivedSolanaMatchesIndependentWallets(t *testing.T) {
	t.Parallel()

	for _, tt := range solanaVectors {
		t.Run(fmt.Sprintf("%q/%d", tt.passphrase, tt.index), func(t *testing.T) {
			t.Parallel()
			address, err := account.DerivedAddress(testMnemonic, tt.passphrase, account.FamilySolana, tt.index)
			if err != nil {
				t.Fatalf("DerivedAddress() error = %v", err)
			}
			if address != tt.address {
				t.Errorf("address = %s, want %s", address, tt.address)
			}
			key, err := account.DeriveEd25519PrivateKey(testMnemonic, tt.passphrase, account.FamilySolana, tt.index)
			if err != nil {
				t.Fatalf("DeriveEd25519PrivateKey() error = %v", err)
			}
			keypair, err := account.Ed25519KeypairBase58(key)
			if err != nil {
				t.Fatalf("Ed25519KeypairBase58() error = %v", err)
			}
			if keypair != tt.keypair {
				t.Errorf("keypair = %s, want %s", keypair, tt.keypair)
			}
		})
	}
}

// Whitespace in the mnemonic is normalised for Solana exactly as for the
// other families.
func TestDerivedSolanaNormalisesMnemonic(t *testing.T) {
	t.Parallel()

	spaced := "  " + strings.ReplaceAll(testMnemonic, " ", " \t ") + "\n"
	address, err := account.DerivedAddress(spaced, "", account.FamilySolana, 0)
	if err != nil || address != solanaVectors[0].address {
		t.Fatalf("DerivedAddress(spaced) = %q, %v; want %s", address, err, solanaVectors[0].address)
	}
}

func TestDeriveEd25519Refusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mnemonic string
		family   account.Family
		index    uint32
		wantErr  error
	}{
		{"tron is secp256k1", testMnemonic, account.FamilyTron, 0, account.ErrCurveMismatch},
		{"evm is secp256k1", testMnemonic, account.FamilyEVM, 0, account.ErrCurveMismatch},
		{"unknown family", testMnemonic, account.Family("btc"), 0, account.ErrUnsupportedFamily},
		{"bad mnemonic", "abandon abandon", account.FamilySolana, 0, account.ErrInvalidMnemonic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key, err := account.DeriveEd25519PrivateKey(tt.mnemonic, "", tt.family, tt.index)
			if !errors.Is(err, tt.wantErr) || key != nil {
				t.Fatalf("DeriveEd25519PrivateKey() = %x, %v; want %v", key, err, tt.wantErr)
			}
		})
	}

	// Every step is hardened, so the index has to leave room for the flag.
	if _, err := account.DeriveEd25519PrivateKey(testMnemonic, "", account.FamilySolana, 1<<31); err == nil {
		t.Error("DeriveEd25519PrivateKey(2^31) error = nil")
	}
	if _, err := account.DeriveEd25519PrivateKey(testMnemonic, "", account.FamilySolana, 1<<31-1); err != nil {
		t.Errorf("DeriveEd25519PrivateKey(2^31-1) error = %v", err)
	}
	if _, err := account.DerivedAddress(testMnemonic, "", account.Family("btc"), 0); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Errorf("DerivedAddress(btc) error = %v, want ErrUnsupportedFamily", err)
	}
	if _, err := account.DerivedAddress("abandon", "", account.FamilySolana, 0); !errors.Is(err, account.ErrInvalidMnemonic) {
		t.Errorf("DerivedAddress(bad mnemonic) error = %v, want ErrInvalidMnemonic", err)
	}
}

// The secp256k1 functions refuse Solana by curve, not as an unknown family.
func TestSecp256k1FunctionsRefuseSolana(t *testing.T) {
	t.Parallel()

	if _, err := account.DerivePrivateKey(testMnemonic, "", account.FamilySolana, 0); !errors.Is(err, account.ErrCurveMismatch) {
		t.Errorf("DerivePrivateKey(solana) error = %v, want ErrCurveMismatch", err)
	}
	key, raw, err := account.ParsePrivateKey("0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	clear(raw)
	if _, err := account.AddressFromPrivateKey(key, account.FamilySolana); !errors.Is(err, account.ErrCurveMismatch) {
		t.Errorf("AddressFromPrivateKey(solana) error = %v, want ErrCurveMismatch", err)
	}
}

func TestCurveOf(t *testing.T) {
	t.Parallel()

	for family, want := range map[account.Family]account.Curve{
		account.FamilyTron:   account.CurveSecp256k1,
		account.FamilyEVM:    account.CurveSecp256k1,
		account.FamilySolana: account.CurveEd25519,
	} {
		got, err := account.CurveOf(family)
		if err != nil || got != want {
			t.Errorf("CurveOf(%s) = %q, %v; want %q", family, got, err, want)
		}
	}
	for _, family := range []account.Family{"", "btc", "Solana"} {
		if got, err := account.CurveOf(family); !errors.Is(err, account.ErrUnsupportedFamily) || got != "" {
			t.Errorf("CurveOf(%q) = %q, %v; want ErrUnsupportedFamily", family, got, err)
		}
	}
}

func TestAddressFromEd25519Key(t *testing.T) {
	t.Parallel()

	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for family, wantErr := range map[account.Family]error{
		account.FamilyTron:    account.ErrCurveMismatch,
		account.FamilyEVM:     account.ErrCurveMismatch,
		account.Family("btc"): account.ErrUnsupportedFamily,
		account.FamilySolana:  nil,
	} {
		_, err := account.AddressFromEd25519Key(key, family)
		if !errors.Is(err, wantErr) {
			t.Errorf("AddressFromEd25519Key(%s) error = %v, want %v", family, err, wantErr)
		}
	}
	for _, bad := range []ed25519.PrivateKey{nil, key[:32], append(key[:64:64], 0)} {
		if _, err := account.AddressFromEd25519Key(bad, account.FamilySolana); !errors.Is(err, account.ErrInvalidPrivateKey) {
			t.Errorf("AddressFromEd25519Key(len %d) error = %v", len(bad), err)
		}
		if _, err := account.Ed25519Fingerprint(bad); !errors.Is(err, account.ErrInvalidPrivateKey) {
			t.Errorf("Ed25519Fingerprint(len %d) error = %v", len(bad), err)
		}
		if _, err := account.Ed25519KeypairBase58(bad); !errors.Is(err, account.ErrInvalidPrivateKey) {
			t.Errorf("Ed25519KeypairBase58(len %d) error = %v", len(bad), err)
		}
		if _, err := account.ImportedEd25519Addresses(bad); !errors.Is(err, account.ErrInvalidPrivateKey) {
			t.Errorf("ImportedEd25519Addresses(len %d) error = %v", len(bad), err)
		}
	}
}

// Keys of RFC 8032 §7.1 tests 1 and 2, written as Solana keypairs. The
// keypair and address strings were produced by tweetnacl 1.0.3 and bs58
// 6.0.0; the fingerprints by Python's hashlib (SHA-256 of the RFC public key,
// first 16 bytes).
var rfc8032Keys = []struct {
	seed, public, keypair, address, fingerprint string
}{
	{
		"9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60",
		"d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
		"49W385L4rePHy6PAaQUovbD2aacgN4HsKXSMeUzRg4fmwXszN91JuMFrQRj3vMDpZuRF3ZknQBuRBoWQJEfXstMw",
		"FVen3X669xLzsi6N2V91DoiyzHzg1uAgqiT8jZ9nS96Z",
		"21fe31dfa154a261626bf854046fd227",
	},
	{
		"4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb",
		"3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c",
		"2Y4QjyJVZf9tTmTPP1SY9ACpFYTo7brW9iCQ8SunQht5yQ2r1U9KsVv5aMsCGnzj3NR8KG9P3NY7FKBiYbbTJ2no",
		"586Z7H2vpX9qNhN2T4e9Utugie3ogjbxzGaMtM3E6HR5",
		"39f713d0a644253f04529421b9f51b9b",
	},
}

// Base58 drops nothing at the edges: leading zero bytes become leading '1's
// and must come back as zero bytes. Vectors from tweetnacl 1.0.3 and bs58
// 6.0.0: a seed starting with two zero bytes, and a seed whose public key
// starts with one.
var leadingZeroKeys = []struct {
	seed, keypair, address string
}{
	{
		"000002030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		"117wqCarxmGjtSpupoHSxGDqTrNXgwCLnuSQN1VC9H8e8BD2SVvbiwNaAMmgWsw5PMDhEoqJtBzMnkTCGsj8G3",
		"9cHXFbcmKjg8SXvKBiBiM1bgvq4r6ZvSWjmYkTyQGabM",
	},
	{
		"5a000000000000000000000000000000000000000000000000000000000000a2",
		"2oN9F7kcCXw9xKS12vFnqxFZsPtt5vvsTi11DH9j3tPQFwWSkBU4i9EZHHpVEj8FoLeJpXv1uhazY7tzqoSZS4iA",
		"14izygPnHKer7XAc4vbvksZBq88BBeym11iuSo9ffNLv",
	},
}

func TestEd25519KeyEncodings(t *testing.T) {
	t.Parallel()

	type vector struct{ seed, keypair, address, fingerprint string }
	var vectors []vector
	for _, v := range rfc8032Keys {
		vectors = append(vectors, vector{v.seed, v.keypair, v.address, v.fingerprint})
	}
	for _, v := range leadingZeroKeys {
		vectors = append(vectors, vector{v.seed, v.keypair, v.address, ""})
	}
	for _, v := range vectors {
		t.Run(v.address, func(t *testing.T) {
			t.Parallel()
			seed, err := hex.DecodeString(v.seed)
			if err != nil {
				t.Fatal(err)
			}
			key := ed25519.NewKeyFromSeed(seed)
			keypair, err := account.Ed25519KeypairBase58(key)
			if err != nil || keypair != v.keypair {
				t.Errorf("Ed25519KeypairBase58() = %q, %v; want %s", keypair, err, v.keypair)
			}
			addresses, err := account.ImportedEd25519Addresses(key)
			if err != nil || len(addresses) != 1 || addresses[account.FamilySolana] != v.address {
				t.Errorf("ImportedEd25519Addresses() = %v, %v; want only solana %s", addresses, err, v.address)
			}
			if v.fingerprint != "" {
				fingerprint, err := account.Ed25519Fingerprint(key)
				if err != nil || fingerprint != v.fingerprint {
					t.Errorf("Ed25519Fingerprint() = %q, %v; want %s", fingerprint, err, v.fingerprint)
				}
			}
			parsed, err := account.ParseEd25519PrivateKey(v.keypair)
			if err != nil {
				t.Fatalf("ParseEd25519PrivateKey(base58) error = %v", err)
			}
			if !parsed.Equal(key) {
				t.Errorf("ParseEd25519PrivateKey(base58) = %x, want %x", parsed, key)
			}
		})
	}
}

// The secp256k1 fingerprint is byte-identical to what HEAD (98adcab) wrote
// into a space file for this key: the value comes from that file.
func TestSecp256k1FingerprintUnchanged(t *testing.T) {
	t.Parallel()

	key, raw, err := account.ParsePrivateKey("4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318")
	if err != nil {
		t.Fatal(err)
	}
	clear(raw)
	fingerprint, err := account.Fingerprint(key)
	if err != nil || fingerprint != "f49c63af9f6e657abc52a941bb7e812c" {
		t.Fatalf("Fingerprint() = %q, %v; want f49c63af9f6e657abc52a941bb7e812c", fingerprint, err)
	}
}

func solanaCLIArray(key []byte) string {
	parts := make([]string, len(key))
	for i, b := range key {
		parts[i] = fmt.Sprint(b)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestParseEd25519PrivateKeyFormats(t *testing.T) {
	t.Parallel()

	key, err := account.DeriveEd25519PrivateKey(testMnemonic, "", account.FamilySolana, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The id.json solana-keygen would write for this key, as printed by
	// @solana/web3.js for the same derivation.
	const cliJSON = "[55,223,87,59,58,196,173,91,82,46,6,78,37,182,62,161,107,203,231,157,68,158,129,160,38,141,16,71,148,139,180,69," +
		"240,54,39,98,70,167,91,157,227,52,158,212,43,21,226,50,246,81,143,194,15,95,205,79,29,100,232,31,155,210,88,247]"
	if got := solanaCLIArray(key); got != cliJSON {
		t.Fatalf("derived key as id.json = %s, want %s", got, cliJSON)
	}
	for name, input := range map[string]string{
		"base58":            solanaVectors[0].keypair,
		"base58 padded":     "\n  " + solanaVectors[0].keypair + " \t",
		"json":              cliJSON,
		"json spaced":       " [ " + strings.ReplaceAll(strings.Trim(cliJSON, "[]"), ",", " ,\n ") + " ] ",
		"json as generated": solanaCLIArray(key),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parsed, err := account.ParseEd25519PrivateKey(input)
			if err != nil {
				t.Fatalf("ParseEd25519PrivateKey() error = %v", err)
			}
			if !parsed.Equal(key) {
				t.Fatalf("parsed key = %x, want %x", parsed, key)
			}
		})
	}
}

func TestParseEd25519PrivateKeyRefusals(t *testing.T) {
	t.Parallel()

	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	foreign := ed25519.NewKeyFromSeed(append(make([]byte, ed25519.SeedSize-1), 1))
	// The seed of one key with the public half of another.
	mismatched := append(append([]byte(nil), key[:32]...), foreign[32:]...)
	flipped := append([]byte(nil), key...)
	flipped[63] ^= 1
	array := func(n int) string { return "[" + strings.TrimSuffix(strings.Repeat("7,", n), ",") + "]" }

	tests := map[string]string{
		"empty":                     "",
		"blank":                     "  \n",
		"hex secp256k1 key":         "0000000000000000000000000000000000000000000000000000000000000001",
		"base58 of 32 bytes":        "11111111111111111111111111111111",
		"base58 of 63 bytes":        solanaKeypairOf(key[:63]),
		"base58 of 65 bytes":        solanaKeypairOf(append(append([]byte(nil), key...), 0)),
		"base58 bad character 0":    "0" + solanaVectors[0].keypair[1:],
		"base58 bad character l":    solanaVectors[0].keypair[:10] + "l" + solanaVectors[0].keypair[11:],
		"base58 inner space":        solanaVectors[0].keypair[:10] + " " + solanaVectors[0].keypair[10:],
		"base58 mismatched halves":  solanaKeypairOf(mismatched),
		"base58 flipped public bit": solanaKeypairOf(flipped),
		"json mismatched halves":    solanaCLIArray(mismatched),
		"json 63 numbers":           array(63),
		"json 65 numbers":           array(65),
		// Out of range by exactly 256 either way: truncated to a byte, each
		// would be the valid keypair.
		"json element plus 256":  withFirst(key, int(key[0])+256),
		"json element minus 256": withFirst(key, int(key[0])-256),
		"json fraction":          "[1.5," + strings.TrimPrefix(array(63), "["),
		"json strings":           `["1"` + strings.Repeat(`,"1"`, 63) + "]",
		"json unterminated":      "[1,2,3",
		"json trailing data":     solanaCLIArray(key) + "x",
		"json empty":             "[]",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			parsed, err := account.ParseEd25519PrivateKey(input)
			if !errors.Is(err, account.ErrInvalidPrivateKey) || parsed != nil {
				t.Fatalf("ParseEd25519PrivateKey() = %x, %v; want ErrInvalidPrivateKey", parsed, err)
			}
			if strings.Contains(err.Error(), input) && input != "" {
				t.Errorf("error quotes the input: %v", err)
			}
			// A character outside the alphabet is named as such, not as a
			// wrong length: the person pasted a typo, not a short key.
			if strings.HasPrefix(name, "base58 bad character") && !strings.Contains(err.Error(), "base58") {
				t.Errorf("error = %v, want it to say the input is not base58", err)
			}
		})
	}
}

// solanaKeypairOf base58-encodes raw bytes that are not a valid keypair, to
// build malformed input; expected values never come from this.
// withFirst is key's id.json with the first element replaced by value.
func withFirst(key []byte, value int) string {
	return "[" + fmt.Sprint(value) + "," + strings.TrimPrefix(solanaCLIArray(key[1:]), "[")
}

func solanaKeypairOf(raw []byte) string {
	return base58.Encode(raw)
}

func FuzzParseEd25519PrivateKey(f *testing.F) {
	f.Add(solanaVectors[0].keypair)
	f.Add(leadingZeroKeys[0].keypair)
	f.Add("[" + strings.Repeat("0,", 63) + "0]")
	f.Add("[]")
	f.Add("1")
	f.Fuzz(func(t *testing.T, input string) {
		key, err := account.ParseEd25519PrivateKey(input)
		if err != nil {
			if !errors.Is(err, account.ErrInvalidPrivateKey) || key != nil {
				t.Fatalf("ParseEd25519PrivateKey() = %x, %v", key, err)
			}
			return
		}
		// Whatever was accepted is a consistent keypair that round-trips.
		if !key.Equal(ed25519.NewKeyFromSeed(key.Seed())) {
			t.Fatalf("accepted a keypair with a foreign public half: %x", key)
		}
		encoded, err := account.Ed25519KeypairBase58(key)
		if err != nil {
			t.Fatal(err)
		}
		again, err := account.ParseEd25519PrivateKey(encoded)
		if err != nil || !again.Equal(key) {
			t.Fatalf("round trip = %x, %v; want %x", again, err, key)
		}
	})
}
