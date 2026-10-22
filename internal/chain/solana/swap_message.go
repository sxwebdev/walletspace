package solana

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

const (
	// maxTransactionSize is the most bytes a transaction may have on the wire.
	maxTransactionSize = 1232
	// signedOverhead is what one signature adds to a message: the count and
	// the signature.
	signedOverhead = 1 + 64
	// lookupTableMetaSize is LOOKUP_TABLE_META_SIZE: the table's header
	// before its addresses (address-lookup-table program state.rs).
	lookupTableMetaSize = 56
	// lookupTableActive is the deactivation slot of a table in use.
	lookupTableActive = math.MaxUint64
)

var addressLookupTableProgram = solana.MustPublicKeyFromBase58("AddressLookupTab1e1111111111111111111111111")

// decodeLookupTable reads an address lookup table account: owned by the
// lookup table program, initialized, not deactivated, and a whole number of
// addresses after the header. A table can only grow while it is active, so an
// index read here names the same address when the transaction executes.
func decodeLookupTable(owner solana.PublicKey, data []byte) ([]solana.PublicKey, error) {
	if owner != addressLookupTableProgram {
		return nil, fmt.Errorf("the lookup table is owned by %s", owner)
	}
	if len(data) < lookupTableMetaSize || (len(data)-lookupTableMetaSize)%32 != 0 {
		return nil, errors.New("the lookup table is malformed")
	}
	if binary.LittleEndian.Uint32(data) != 1 {
		return nil, errors.New("the lookup table is not initialized")
	}
	if binary.LittleEndian.Uint64(data[4:]) != lookupTableActive {
		return nil, errors.New("the lookup table is deactivated")
	}
	out := make([]solana.PublicKey, (len(data)-lookupTableMetaSize)/32)
	for i := range out {
		out[i] = solana.PublicKeyFromBytes(data[lookupTableMetaSize+32*i : lookupTableMetaSize+32*(i+1)])
	}
	return out, nil
}

// buildMessage serializes instructions for payer and blockhash: a legacy
// message when the signed transaction fits, otherwise a v0 message that loads
// what it can from tables. It reports the version it chose.
func buildMessage(
	payer solana.PublicKey, blockhash solana.Hash, instructions []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice,
) ([]byte, bool, error) {
	converted := make([]solana.Instruction, len(instructions))
	for i, in := range instructions {
		metas := make(solana.AccountMetaSlice, len(in.Accounts))
		for j, meta := range in.Accounts {
			metas[j] = &solana.AccountMeta{PublicKey: meta.PublicKey, IsWritable: meta.IsWritable, IsSigner: meta.IsSigner}
		}
		converted[i] = solana.NewInstruction(in.Program, metas, in.Data)
	}
	// NewTransaction fails only without instructions; marshaling a
	// message it built does not fail.
	tx, err := solana.NewTransaction(converted, blockhash, solana.TransactionPayer(payer))
	if err != nil {
		return nil, false, fmt.Errorf("build Solana transaction: %w", err)
	}
	legacy, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, false, err // not reachable
	}
	if len(legacy)+signedOverhead <= maxTransactionSize || len(tables) == 0 {
		return legacy, false, nil
	}
	tx, err = solana.NewTransaction(converted, blockhash, solana.TransactionPayer(payer), solana.TransactionAddressTables(tables))
	if err != nil {
		return nil, false, fmt.Errorf("build Solana v0 transaction: %w", err) // not reachable: built once above
	}
	v0, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, false, err // not reachable
	}
	return v0, true, nil
}

// swapMessage is a decoded legacy or v0 message with every account resolved:
// the static keys first, then the addresses loaded writable, then those loaded
// read-only, as the runtime orders them.
type swapMessage struct {
	versioned        bool
	signers          int
	readonlySigned   int
	readonlyUnsigned int
	static           int
	keys             []solana.PublicKey
	writableLoaded   int
	blockhash        solana.Hash
	instructions     []decodedInstruction
	lookups          []messageLookup
}

type messageLookup struct {
	table    solana.PublicKey
	writable []uint8
	readonly []uint8
}

func (m swapMessage) isWritable(index int) bool {
	if index < m.static {
		if index < m.signers {
			return index < m.signers-m.readonlySigned
		}
		return index < m.static-m.readonlyUnsigned
	}
	return index < m.static+m.writableLoaded
}

// decodeSwapMessage reads a legacy or v0 message and resolves the v0 lookups
// against tables, which hold what the chain says each table contains. A table
// the message names that is not in tables, an index past a table's end, a
// version other than 0, or bytes after the lookups are refused.
func decodeSwapMessage(data []byte, tables map[solana.PublicKey][]solana.PublicKey) (swapMessage, error) {
	var out swapMessage
	rest := data
	take := func(n int) ([]byte, bool) {
		if n < 0 || len(rest) < n {
			return nil, false
		}
		value := rest[:n]
		rest = rest[n:]
		return value, true
	}
	compact := func() (int, bool) {
		value := 0
		for shift := 0; shift < 21; shift += 7 {
			b, ok := take(1)
			if !ok {
				return 0, false
			}
			value |= int(b[0]&0x7f) << shift
			if b[0]&0x80 == 0 {
				return value, true
			}
		}
		return 0, false
	}
	if len(rest) > 0 && rest[0]&0x80 != 0 {
		if rest[0] != 0x80 {
			return out, fmt.Errorf("%w: message version %d", errMalformedMessage, rest[0]&0x7f)
		}
		out.versioned = true
		rest = rest[1:]
	}
	header, ok := take(3)
	if !ok {
		return out, errMalformedMessage
	}
	out.signers, out.readonlySigned, out.readonlyUnsigned = int(header[0]), int(header[1]), int(header[2])
	count, ok := compact()
	if !ok {
		return out, errMalformedMessage
	}
	for range count {
		key, ok := take(32)
		if !ok {
			return out, errMalformedMessage
		}
		out.keys = append(out.keys, solana.PublicKeyFromBytes(key))
	}
	out.static = len(out.keys)
	if out.readonlySigned > out.signers || out.signers > out.static || out.readonlyUnsigned > out.static-out.signers {
		return out, fmt.Errorf("%w: the header does not fit the account list", errMalformedMessage)
	}
	blockhash, ok := take(32)
	if !ok {
		return out, errMalformedMessage
	}
	copy(out.blockhash[:], blockhash)
	instructions, ok := compact()
	if !ok {
		return out, errMalformedMessage
	}
	type rawInstruction struct {
		program  int
		accounts []byte
		data     []byte
	}
	var raws []rawInstruction
	for range instructions {
		program, ok := take(1)
		if !ok {
			return out, errMalformedMessage
		}
		accounts, ok := compact()
		if !ok {
			return out, errMalformedMessage
		}
		indexes, ok := take(accounts)
		if !ok {
			return out, errMalformedMessage
		}
		size, ok := compact()
		if !ok {
			return out, errMalformedMessage
		}
		payload, ok := take(size)
		if !ok {
			return out, errMalformedMessage
		}
		raws = append(raws, rawInstruction{program: int(program[0]), accounts: indexes, data: bytes.Clone(payload)})
	}
	if out.versioned {
		lookups, ok := compact()
		if !ok {
			return out, errMalformedMessage
		}
		for range lookups {
			table, ok := take(32)
			if !ok {
				return out, errMalformedMessage
			}
			lookup := messageLookup{table: solana.PublicKeyFromBytes(table)}
			for _, target := range []*[]uint8{&lookup.writable, &lookup.readonly} {
				n, ok := compact()
				if !ok {
					return out, errMalformedMessage
				}
				indexes, ok := take(n)
				if !ok {
					return out, errMalformedMessage
				}
				*target = bytes.Clone(indexes)
			}
			out.lookups = append(out.lookups, lookup)
		}
	}
	if len(rest) != 0 {
		return out, fmt.Errorf("%w: %d bytes follow the message", errMalformedMessage, len(rest))
	}
	resolve := func(lookup messageLookup, indexes []uint8) ([]solana.PublicKey, error) {
		content, known := tables[lookup.table]
		if !known {
			return nil, fmt.Errorf("the message loads from lookup table %s, which is not one of the route's", lookup.table)
		}
		var keys []solana.PublicKey
		for _, index := range indexes {
			if int(index) >= len(content) {
				return nil, fmt.Errorf("lookup table %s has no entry %d", lookup.table, index)
			}
			keys = append(keys, content[index])
		}
		return keys, nil
	}
	for _, lookup := range out.lookups {
		keys, err := resolve(lookup, lookup.writable)
		if err != nil {
			return out, err
		}
		out.keys = append(out.keys, keys...)
		out.writableLoaded += len(keys)
	}
	for _, lookup := range out.lookups {
		keys, err := resolve(lookup, lookup.readonly)
		if err != nil {
			return out, err
		}
		out.keys = append(out.keys, keys...)
	}
	for _, raw := range raws {
		// The runtime only invokes programs named among the static keys.
		if raw.program >= out.static {
			return out, fmt.Errorf("%w: a program is not a static account", errMalformedMessage)
		}
		in := decodedInstruction{program: raw.program, data: raw.data}
		for _, index := range raw.accounts {
			if int(index) >= len(out.keys) {
				return out, errMalformedMessage
			}
			in.accounts = append(in.accounts, int(index))
		}
		out.instructions = append(out.instructions, in)
	}
	return out, nil
}

// verifySwapMessage decodes the message that is about to be signed on its own
// and checks it is exactly the planned swap: payer as the only signer, the
// blockhash, every instruction with its program, accounts and data, every
// Raydium swap equal to the one the pool state gives, and no account writable
// or present beyond what those instructions need. Addresses loaded from lookup
// tables are resolved against the tables' on-chain contents.
func verifySwapMessage(
	message []byte, payer solana.PublicKey, blockhash solana.Hash, want []raydium.Instruction,
	swaps []any, tables map[solana.PublicKey][]solana.PublicKey,
) error {
	mismatch := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrSwapMismatch}, args...)...)
	}
	decoded, err := decodeSwapMessage(message, tables)
	if err != nil {
		return mismatch("%v", err)
	}
	if decoded.signers != 1 || decoded.readonlySigned != 0 || decoded.static == 0 || decoded.keys[0] != payer {
		return mismatch("the wallet must be the only signer and the fee payer")
	}
	if decoded.blockhash != blockhash {
		return mismatch("the blockhash is not the one fetched")
	}
	seen := make(map[solana.PublicKey]bool, len(decoded.keys))
	for _, key := range decoded.keys {
		if seen[key] {
			return mismatch("account %s is listed twice", key)
		}
		seen[key] = true
	}
	if len(decoded.instructions) != len(want) {
		return mismatch("%d instructions, want %d", len(decoded.instructions), len(want))
	}
	writable := map[solana.PublicKey]bool{payer: true}
	used := map[solana.PublicKey]bool{payer: true}
	next := 0
	for i, expected := range want {
		got := decoded.instructions[i]
		program := decoded.keys[got.program]
		if program != expected.Program {
			return mismatch("instruction %d calls %s, want %s", i, program, expected.Program)
		}
		used[program] = true
		if !bytes.Equal(got.data, expected.Data) {
			return mismatch("instruction %d carries other data", i)
		}
		if len(got.accounts) != len(expected.Accounts) {
			return mismatch("instruction %d lists %d accounts, want %d", i, len(got.accounts), len(expected.Accounts))
		}
		keys := make([]solana.PublicKey, len(got.accounts))
		for j, meta := range expected.Accounts {
			keys[j] = decoded.keys[got.accounts[j]]
			if keys[j] != meta.PublicKey {
				return mismatch("instruction %d account %d is %s, want %s", i, j, keys[j], meta.PublicKey)
			}
			if meta.IsSigner && meta.PublicKey != payer {
				return mismatch("instruction %d wants %s to sign", i, meta.PublicKey)
			}
			used[meta.PublicKey] = true
			if meta.IsWritable {
				writable[meta.PublicKey] = true
			}
		}
		if kind := raydium.KindOf(program); kind != raydium.KindUnknown {
			if next >= len(swaps) {
				return mismatch("instruction %d is a swap the plan does not have", i)
			}
			if err := sameSwap(kind, program, keys, got.data, swaps[next]); err != nil {
				return mismatch("instruction %d: %v", i, err)
			}
			next++
		}
	}
	if next != len(swaps) {
		return mismatch("%d swaps, want %d", next, len(swaps))
	}
	for index, key := range decoded.keys {
		if !used[key] {
			return mismatch("account %s is not used by any instruction", key)
		}
		if decoded.isWritable(index) != writable[key] {
			return mismatch("account %s has the wrong writable flag", key)
		}
	}
	return nil
}

// sameSwap decodes a Raydium instruction with the package's strict decoder
// and compares it with the swap the plan derived from pool state.
func sameSwap(kind raydium.Kind, program solana.PublicKey, keys []solana.PublicKey, data []byte, want any) error {
	var got any
	var err error
	switch kind {
	case raydium.KindAMMV4:
		got, err = raydium.DecodeAMMV4Swap(program, keys, data)
	case raydium.KindCPMM:
		got, err = raydium.DecodeCPMMSwap(program, keys, data)
	default:
		got, err = raydium.DecodeCLMMSwap(program, keys, data)
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return errors.New("the swap differs from the planned one")
	}
	return nil
}
