// Package raydium quotes swaps through the Raydium Trade API and builds,
// decodes and checks the Raydium swap instructions the wallet signs.
//
// The Trade API is used for the route only: which pools, in which order, and
// the amount it expects. Everything that ends up in a transaction is built
// here, from typed arguments and from pool accounts read on chain, so a signed
// swap can be compared byte for byte with what this package produced. Three
// programs are covered — AMM v4, CPMM and CLMM — with their layouts and
// instruction formats taken from the Apache-2.0 Rust programs
// (github.com/raydium-io/raydium-amm, raydium-cp-swap, raydium-clmm).
//
// The package makes no network calls other than the quote request; callers
// read chain state and hand the account data in.
package raydium

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/gagliardetto/solana-go"
)

// The mainnet program ids. Devnet deployments use other ids and the Trade API
// serves mainnet only, so nothing here accepts a program id from outside.
var (
	AMMV4Program = solana.MustPublicKeyFromBase58("675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8")
	CPMMProgram  = solana.MustPublicKeyFromBase58("CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C")
	CLMMProgram  = solana.MustPublicKeyFromBase58("CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK")
)

// Kind is the Raydium program a pool belongs to.
type Kind uint8

const (
	KindUnknown Kind = iota
	KindAMMV4
	KindCPMM
	KindCLMM
)

func (k Kind) String() string {
	switch k {
	case KindAMMV4:
		return "amm-v4"
	case KindCPMM:
		return "cpmm"
	case KindCLMM:
		return "clmm"
	}
	return "unknown"
}

// Program returns the program id of the kind, or the zero key for
// KindUnknown.
func (k Kind) Program() solana.PublicKey {
	switch k {
	case KindAMMV4:
		return AMMV4Program
	case KindCPMM:
		return CPMMProgram
	case KindCLMM:
		return CLMMProgram
	}
	return solana.PublicKey{}
}

// KindOf names the Raydium program behind a program id; any other program is
// KindUnknown.
func KindOf(program solana.PublicKey) Kind {
	switch program {
	case AMMV4Program:
		return KindAMMV4
	case CPMMProgram:
		return KindCPMM
	case CLMMProgram:
		return KindCLMM
	}
	return KindUnknown
}

var (
	// ErrMalformedAccount means account data does not have the layout of
	// the account it was decoded as.
	ErrMalformedAccount = errors.New("raydium: malformed account")
	// ErrWrongOwner means an account is not owned by the program its layout
	// belongs to.
	ErrWrongOwner = errors.New("raydium: account has the wrong owner")
	// ErrPoolNotSwappable means the pool exists but does not allow a swap
	// now: disabled, not open yet, or with a token this wallet does not
	// trade.
	ErrPoolNotSwappable = errors.New("raydium: pool does not allow this swap")
	// ErrMalformedInstruction means an instruction is not exactly one of the
	// swaps this package builds.
	ErrMalformedInstruction = errors.New("raydium: not a recognised swap instruction")
)

// Instruction is one instruction as it goes into a message: the program, the
// accounts with the privileges the program needs, and the data.
type Instruction struct {
	Program  solana.PublicKey
	Accounts []solana.AccountMeta
	Data     []byte
}

// Keys returns the account keys of the instruction in order.
func (in Instruction) Keys() []solana.PublicKey {
	keys := make([]solana.PublicKey, len(in.Accounts))
	for i, meta := range in.Accounts {
		keys[i] = meta.PublicKey
	}
	return keys
}

func writable(key solana.PublicKey) solana.AccountMeta {
	return solana.AccountMeta{PublicKey: key, IsWritable: true}
}

func readonly(key solana.PublicKey) solana.AccountMeta {
	return solana.AccountMeta{PublicKey: key}
}

func signer(key solana.PublicKey) solana.AccountMeta {
	return solana.AccountMeta{PublicKey: key, IsSigner: true}
}

// layout reads fixed offsets of an account whose length was checked first.
type layout []byte

func (l layout) key(offset int) solana.PublicKey {
	return solana.PublicKeyFromBytes(l[offset : offset+32])
}

func (l layout) u8(offset int) uint8 { return l[offset] }

func (l layout) u16(offset int) uint16 { return binary.LittleEndian.Uint16(l[offset:]) }

func (l layout) u32(offset int) uint32 { return binary.LittleEndian.Uint32(l[offset:]) }

func (l layout) i32(offset int) int32 { return int32(binary.LittleEndian.Uint32(l[offset:])) }

func (l layout) u64(offset int) uint64 { return binary.LittleEndian.Uint64(l[offset:]) }

// u128 reads a little-endian u128.
func (l layout) u128(offset int) *big.Int {
	hi := new(big.Int).SetUint64(l.u64(offset + 8))
	return hi.Lsh(hi, 64).Or(hi, new(big.Int).SetUint64(l.u64(offset)))
}

// checkAccount checks the owner, the exact length and, for Anchor accounts,
// the 8-byte discriminator.
func checkAccount(name string, program, owner solana.PublicKey, data []byte, size int, discriminator []byte) error {
	if owner != program {
		return fmt.Errorf("%w: %s owned by %s, want %s", ErrWrongOwner, name, owner, program)
	}
	if len(data) != size {
		return fmt.Errorf("%w: %s of %d bytes, want %d", ErrMalformedAccount, name, len(data), size)
	}
	if discriminator != nil && string(data[:8]) != string(discriminator) {
		return fmt.Errorf("%w: %s has another account discriminator", ErrMalformedAccount, name)
	}
	return nil
}

// pda is FindProgramAddress for seeds that are fixed in this package or are
// public keys and small integers. Finding no bump means all 255 candidate
// addresses landed on the curve, which does not happen in practice; it is
// treated as a broken invariant rather than an error every caller handles.
func pda(seeds [][]byte, program solana.PublicKey) solana.PublicKey {
	key, _, err := solana.FindProgramAddress(seeds, program)
	if err != nil {
		panic(fmt.Sprintf("raydium: no program address for %q under %s: %v", seeds, program, err))
	}
	return key
}

// mustHex decodes a constant written in hex; it runs at init only.
func mustHex(s string) []byte {
	out, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return out
}
