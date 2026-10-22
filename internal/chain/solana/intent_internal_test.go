package solana

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
)

var (
	testPayer     = solana.PublicKeyFromBytes(append(make([]byte, 31), 1))
	testRecipient = solana.MustPublicKeyFromBase58("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM")
	testMint      = solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	testHash      = solana.MustHashFromBase58("9zrUHnA1nCByPksy3aL8tQ47vqdaG2vnFs4HrxgcZj4F")
)

func solPlan() *plan {
	return &plan{from: testPayer, to: testRecipient, amount: 123_456, limit: 1540, price: 37_500}
}

func tokenPlanFor(create bool) *plan {
	source, _, _ := solana.FindAssociatedTokenAddress(testPayer, testMint)
	destination, _, _ := solana.FindAssociatedTokenAddress(testRecipient, testMint)
	p := solPlan()
	p.token = &tokenPlan{mint: testMint, decimals: 6, source: source, destination: destination, createAccount: create}
	return p
}

// encode is the inverse of decodeLegacyMessage, for tampering with a message.
func encode(m decodedMessage) []byte {
	compact := func(out []byte, n int) []byte {
		for {
			b := byte(n & 0x7f)
			n >>= 7
			if n == 0 {
				return append(out, b)
			}
			out = append(out, b|0x80)
		}
	}
	out := []byte{byte(m.signers), byte(m.readonlySigned), byte(m.readonlyUnsigned)}
	out = compact(out, len(m.keys))
	for _, key := range m.keys {
		out = append(out, key[:]...)
	}
	out = append(out, m.blockhash[:]...)
	out = compact(out, len(m.instructions))
	for _, instruction := range m.instructions {
		out = append(out, byte(instruction.program))
		out = compact(out, len(instruction.accounts))
		for _, index := range instruction.accounts {
			out = append(out, byte(index))
		}
		out = compact(out, len(instruction.data))
		out = append(out, instruction.data...)
	}
	return out
}

func build(t *testing.T, p *plan) []byte {
	t.Helper()
	message, err := p.build(testHash)
	if err != nil {
		t.Fatalf("build() error = %v", err)
	}
	return message
}

func TestTheBuilderMatchesTheIntent(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]*plan{
		"SOL": solPlan(), "token": tokenPlanFor(false), "token opening an account": tokenPlanFor(true),
	} {
		message := build(t, p)
		if err := verifyMessage(message, p.intent(testHash)); err != nil {
			t.Errorf("%s: verifyMessage() error = %v", name, err)
		}
		decoded, err := decodeLegacyMessage(message)
		if err != nil || !slices.Equal(encode(decoded), message) {
			t.Errorf("%s: the test encoder does not round-trip: %v", name, err)
		}
	}
	self := solPlan()
	self.to = self.from
	if err := verifyMessage(build(t, self), self.intent(testHash)); err != nil {
		t.Errorf("SOL to oneself: verifyMessage() error = %v", err)
	}
}

// A message built for anything other than the intent is refused, whichever
// part differs.
func TestTheVerifierRefusesAnotherTransfer(t *testing.T) {
	t.Parallel()

	other := solana.PublicKeyFromBytes(append(make([]byte, 31), 9))
	otherHash := solana.Hash{1}
	tests := []struct {
		name   string
		intent *plan
		built  func(*plan)
	}{
		{name: "payer", intent: solPlan(), built: func(p *plan) { p.from = other }},
		{name: "recipient", intent: solPlan(), built: func(p *plan) { p.to = other }},
		{name: "lamports", intent: solPlan(), built: func(p *plan) { p.amount++ }},
		{name: "limit", intent: solPlan(), built: func(p *plan) { p.limit++ }},
		{name: "price", intent: solPlan(), built: func(p *plan) { p.price++ }},
		{name: "SOL instead of a token", intent: tokenPlanFor(false), built: func(p *plan) { p.token = nil }},
		{name: "token instead of SOL", intent: solPlan(), built: func(p *plan) { *p = *tokenPlanFor(false) }},
		{name: "token amount", intent: tokenPlanFor(false), built: func(p *plan) { p.amount++ }},
		{name: "decimals", intent: tokenPlanFor(false), built: func(p *plan) { p.token.decimals = 9 }},
		{name: "mint", intent: tokenPlanFor(false), built: func(p *plan) { p.token.mint = other }},
		{name: "source account", intent: tokenPlanFor(false), built: func(p *plan) { p.token.source = other }},
		{name: "destination account", intent: tokenPlanFor(false), built: func(p *plan) { p.token.destination = other }},
		{name: "token recipient", intent: tokenPlanFor(true), built: func(p *plan) { p.to = other }},
		{name: "unrequested account opening", intent: tokenPlanFor(false), built: func(p *plan) { p.token.createAccount = true }},
		{name: "missing account opening", intent: tokenPlanFor(true), built: func(p *plan) { p.token.createAccount = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wrong := *tt.intent
			if tt.intent.token != nil {
				token := *tt.intent.token
				wrong.token = &token
			}
			tt.built(&wrong)
			err := verifyMessage(build(t, &wrong), tt.intent.intent(testHash))
			if !errors.Is(err, ErrIntentMismatch) {
				t.Errorf("verifyMessage() error = %v, want ErrIntentMismatch", err)
			}
		})
	}
	if err := verifyMessage(build(t, solPlan()), solPlan().intent(otherHash)); !errors.Is(err, ErrIntentMismatch) ||
		!strings.Contains(err.Error(), "blockhash") {
		t.Errorf("verifyMessage(other blockhash) error = %v", err)
	}
}

// The parts a builder could get wrong without changing any instruction: the
// header's signer and writable counts, extra or repeated accounts, extra
// instructions, and the encoding itself.
func TestTheVerifierRefusesATamperedMessage(t *testing.T) {
	t.Parallel()

	p := tokenPlanFor(true)
	want := p.intent(testHash)
	valid, err := decodeLegacyMessage(build(t, p))
	if err != nil {
		t.Fatal(err)
	}
	clone := func() decodedMessage {
		m := valid
		m.keys = slices.Clone(valid.keys)
		m.instructions = nil
		for _, instruction := range valid.instructions {
			instruction.accounts = slices.Clone(instruction.accounts)
			instruction.data = slices.Clone(instruction.data)
			m.instructions = append(m.instructions, instruction)
		}
		return m
	}
	tests := []struct {
		name    string
		tamper  func(*decodedMessage)
		wantErr string
	}{
		{name: "a second signer", tamper: func(m *decodedMessage) { m.signers = 2 }, wantErr: "only signer"},
		{name: "a read-only payer", tamper: func(m *decodedMessage) { m.readonlySigned = 1 }, wantErr: "only signer"},
		{name: "a writable program", tamper: func(m *decodedMessage) { m.readonlyUnsigned-- }, wantErr: "writable flag"},
		{name: "a read-only destination", tamper: func(m *decodedMessage) { m.readonlyUnsigned++ }, wantErr: "writable flag"},
		{name: "an unused account", tamper: func(m *decodedMessage) {
			// Appended read-only, so only its presence differs.
			m.keys = append(m.keys, solana.PublicKeyFromBytes(append(make([]byte, 31), 5)))
			m.readonlyUnsigned++
		}, wantErr: "not used"},
		{name: "a repeated account", tamper: func(m *decodedMessage) {
			m.keys = append(m.keys, m.keys[1])
			m.readonlyUnsigned++
		}, wantErr: "listed twice"},
		{name: "an extra instruction", tamper: func(m *decodedMessage) {
			m.instructions = append(m.instructions, m.instructions[0])
		}, wantErr: "instructions"},
		{name: "a missing instruction", tamper: func(m *decodedMessage) { m.instructions = m.instructions[1:] }, wantErr: "instructions"},
		{name: "an extra account in an instruction", tamper: func(m *decodedMessage) {
			last := &m.instructions[len(m.instructions)-1]
			last.accounts = append(last.accounts, 0)
		}, wantErr: "accounts"},
		{name: "source and destination swapped", tamper: func(m *decodedMessage) {
			accounts := m.instructions[3].accounts
			accounts[0], accounts[2] = accounts[2], accounts[0]
		}, wantErr: "account 0"},
		{name: "another program", tamper: func(m *decodedMessage) { m.instructions[0].program = m.instructions[3].program }, wantErr: "calls"},
		{name: "data with a trailing byte", tamper: func(m *decodedMessage) {
			m.instructions[3].data = append(m.instructions[3].data, 0)
		}, wantErr: "other data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := clone()
			tt.tamper(&m)
			err := verifyMessage(encode(m), want)
			if !errors.Is(err, ErrIntentMismatch) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("verifyMessage() error = %v, want a mismatch mentioning %q", err, tt.wantErr)
			}
		})
	}

	message := encode(valid)
	for size := range len(message) {
		if err := verifyMessage(message[:size], want); !errors.Is(err, ErrIntentMismatch) {
			t.Fatalf("verifyMessage(first %d bytes) error = %v, want a mismatch", size, err)
		}
	}
	if err := verifyMessage(append(slices.Clone(message), 0), want); !errors.Is(err, ErrIntentMismatch) ||
		!strings.Contains(err.Error(), "follow the instructions") {
		t.Errorf("verifyMessage(trailing byte) error = %v", err)
	}
	if err := verifyMessage(append([]byte{0x80}, message...), want); !errors.Is(err, ErrIntentMismatch) ||
		!strings.Contains(err.Error(), "versioned") {
		t.Errorf("verifyMessage(versioned) error = %v", err)
	}
	for name, tamper := range map[string]func(*decodedMessage){
		"header past the accounts":   func(m *decodedMessage) { m.signers = len(m.keys) + 1 },
		"read-only past the signers": func(m *decodedMessage) { m.readonlySigned = 2 },
		"read-only past the keys":    func(m *decodedMessage) { m.readonlyUnsigned = len(m.keys) },
		"program out of range":       func(m *decodedMessage) { m.instructions[0].program = len(m.keys) },
		"account out of range":       func(m *decodedMessage) { m.instructions[3].accounts[0] = len(m.keys) },
	} {
		m := clone()
		tamper(&m)
		if err := verifyMessage(encode(m), want); !errors.Is(err, ErrIntentMismatch) {
			t.Errorf("%s: verifyMessage() error = %v, want a mismatch", name, err)
		}
	}
	// A length that never ends.
	if _, err := decodeLegacyMessage([]byte{1, 0, 1, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Error("decodeLegacyMessage() accepted an endless length")
	}
}
