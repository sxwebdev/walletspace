package solana

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
)

// ErrIntentMismatch means the message about to be signed is not the transfer
// that was asked for. It is always a refusal to sign.
var ErrIntentMismatch = fmt.Errorf("%w: transaction does not match the requested transfer", chain.ErrInvalidRequest)

// intent is the transfer the user asked for, in the terms the chain uses. The
// message to be signed is decoded on its own and compared against it, so a
// mistake in building — an account in the wrong place, an amount in the wrong
// unit, an extra instruction — is refused instead of signed.
type intent struct {
	payer     solana.PublicKey
	blockhash solana.Hash
	limit     uint32
	price     uint64
	// to is the recipient wallet.
	to solana.PublicKey
	// lamports is the SOL amount of a SOL transfer.
	lamports uint64
	// token is set for an SPL transfer.
	token *tokenIntent
}

type tokenIntent struct {
	mint     solana.PublicKey
	decimals uint8
	amount   uint64
	// createAccount adds the idempotent creation of the recipient's
	// associated token account.
	createAccount bool
}

type expectedMeta struct {
	key      solana.PublicKey
	writable bool
	signer   bool
}

type expectedInstruction struct {
	program  solana.PublicKey
	accounts []expectedMeta
	data     []byte
}

// expected lists the instructions the intent allows, in order. The associated
// token accounts are derived here, from the wallets and the mint, not taken
// from whatever built the message.
func (in intent) expected() ([]expectedInstruction, error) {
	out := []expectedInstruction{
		{program: solana.ComputeBudget, data: binary.LittleEndian.AppendUint32([]byte{2}, in.limit)},
		{program: solana.ComputeBudget, data: binary.LittleEndian.AppendUint64([]byte{3}, in.price)},
	}
	if in.token == nil {
		return append(out, expectedInstruction{
			program: solana.SystemProgramID,
			accounts: []expectedMeta{
				{key: in.payer, writable: true, signer: true},
				{key: in.to, writable: true},
			},
			data: binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), in.lamports),
		}), nil
	}
	// The derivations cannot fail in practice: they need all 256 bump seeds
	// to land on the curve.
	source, _, err := solana.FindAssociatedTokenAddress(in.payer, in.token.mint)
	if err != nil {
		return nil, err
	}
	destination, _, err := solana.FindAssociatedTokenAddress(in.to, in.token.mint)
	if err != nil {
		return nil, err
	}
	if in.token.createAccount {
		out = append(out, expectedInstruction{
			program: solana.SPLAssociatedTokenAccountProgramID,
			accounts: []expectedMeta{
				{key: in.payer, writable: true, signer: true},
				{key: destination, writable: true},
				{key: in.to},
				{key: in.token.mint},
				{key: solana.SystemProgramID},
				{key: solana.TokenProgramID},
			},
			data: []byte{1},
		})
	}
	data := binary.LittleEndian.AppendUint64([]byte{12}, in.token.amount)
	return append(out, expectedInstruction{
		program: solana.TokenProgramID,
		accounts: []expectedMeta{
			{key: source, writable: true},
			{key: in.token.mint},
			{key: destination, writable: true},
			{key: in.payer, signer: true},
		},
		data: append(data, in.token.decimals),
	}), nil
}

// verifyMessage decodes a serialized legacy message and checks that it is
// exactly the intent: the payer as the only signer, the blockhash, every
// instruction with its program, accounts and data, and no account writable or
// signing beyond what those instructions need.
func verifyMessage(message []byte, in intent) error {
	want, err := in.expected()
	if err != nil {
		return err
	}
	return verifyInstructions(message, in.payer, in.blockhash, want)
}

// verifyInstructions decodes a serialized legacy message and checks it
// against a list of expected instructions, paid and signed by payer alone.
func verifyInstructions(message []byte, payer solana.PublicKey, blockhash solana.Hash, want []expectedInstruction) error {
	in := struct {
		payer     solana.PublicKey
		blockhash solana.Hash
	}{payer, blockhash}
	decoded, err := decodeLegacyMessage(message)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIntentMismatch, err)
	}
	mismatch := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrIntentMismatch}, args...)...)
	}
	if decoded.signers != 1 || decoded.readonlySigned != 0 || len(decoded.keys) == 0 || decoded.keys[0] != in.payer {
		return mismatch("the sender must be the only signer and the fee payer")
	}
	if decoded.blockhash != in.blockhash {
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
	writable := map[solana.PublicKey]bool{in.payer: true}
	used := map[solana.PublicKey]bool{in.payer: true}
	for i, expected := range want {
		got := decoded.instructions[i]
		if decoded.keys[got.program] != expected.program {
			return mismatch("instruction %d calls %s, want %s", i, decoded.keys[got.program], expected.program)
		}
		used[expected.program] = true
		if !bytes.Equal(got.data, expected.data) {
			return mismatch("instruction %d carries other data", i)
		}
		if len(got.accounts) != len(expected.accounts) {
			return mismatch("instruction %d lists %d accounts, want %d", i, len(got.accounts), len(expected.accounts))
		}
		for j, meta := range expected.accounts {
			if decoded.keys[got.accounts[j]] != meta.key {
				return mismatch("instruction %d account %d is %s, want %s", i, j, decoded.keys[got.accounts[j]], meta.key)
			}
			used[meta.key] = true
			if meta.writable {
				writable[meta.key] = true
			}
		}
	}
	// The header check above already makes the payer, at index 0, the one
	// and only signer; what is left to check per account is whether it is
	// used and writable exactly as the instructions need.
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

type decodedInstruction struct {
	program  int
	accounts []int
	data     []byte
}

type decodedMessage struct {
	signers          int
	readonlySigned   int
	readonlyUnsigned int
	keys             []solana.PublicKey
	blockhash        solana.Hash
	instructions     []decodedInstruction
}

func (m decodedMessage) isWritable(index int) bool {
	if index < m.signers {
		return index < m.signers-m.readonlySigned
	}
	return index < len(m.keys)-m.readonlyUnsigned
}

var errMalformedMessage = errors.New("the message cannot be decoded")

// decodeLegacyMessage reads a legacy message: the header, the account keys,
// the blockhash and the compiled instructions, with nothing left over. A
// versioned message is refused: this wallet builds legacy ones only.
func decodeLegacyMessage(data []byte) (decodedMessage, error) {
	var out decodedMessage
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
	header, ok := take(3)
	if !ok {
		return out, errMalformedMessage
	}
	if header[0]&0x80 != 0 {
		return out, fmt.Errorf("%w: versioned messages are not built here", errMalformedMessage)
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
	if out.readonlySigned > out.signers || out.signers > len(out.keys) ||
		out.readonlyUnsigned > len(out.keys)-out.signers {
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
	for range instructions {
		program, ok := take(1)
		if !ok || int(program[0]) >= len(out.keys) {
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
		instruction := decodedInstruction{program: int(program[0])}
		for _, index := range indexes {
			if int(index) >= len(out.keys) {
				return out, errMalformedMessage
			}
			instruction.accounts = append(instruction.accounts, int(index))
		}
		size, ok := compact()
		if !ok {
			return out, errMalformedMessage
		}
		payload, ok := take(size)
		if !ok {
			return out, errMalformedMessage
		}
		instruction.data = append([]byte(nil), payload...)
		out.instructions = append(out.instructions, instruction)
	}
	if len(rest) != 0 {
		return out, fmt.Errorf("%w: %d bytes follow the instructions", errMalformedMessage, len(rest))
	}
	return out, nil
}
