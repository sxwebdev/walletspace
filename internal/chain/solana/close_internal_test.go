package solana

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
)

func testClosePlan() *closePlan {
	return &closePlan{
		owner: testPayer, limit: 6000, price: 1000,
		accounts: []solana.PublicKey{solana.PublicKeyFromBytes(append(make([]byte, 31), 7)), solana.PublicKeyFromBytes(append(make([]byte, 31), 8))},
	}
}

// The close message is checked as strictly as a transfer: only the compute
// budget and one CloseAccount per account, rent to the owner, signed by the
// owner alone.
func TestTheCloseVerifier(t *testing.T) {
	t.Parallel()

	c := testClosePlan()
	message, err := c.build(testHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyInstructions(message, c.owner, testHash, c.expected()); err != nil {
		t.Fatalf("verifyInstructions() error = %v", err)
	}
	valid, err := decodeLegacyMessage(message)
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
	other := solana.PublicKeyFromBytes(append(make([]byte, 31), 9))
	tests := []struct {
		name    string
		tamper  func(*decodedMessage)
		wantErr string
	}{
		{name: "an extra close", tamper: func(m *decodedMessage) { m.instructions = append(m.instructions, m.instructions[2]) }, wantErr: "instructions"},
		{name: "another instruction of the token program", tamper: func(m *decodedMessage) { m.instructions[2].data = []byte{3} }, wantErr: "other data"},
		{name: "rent to another account", tamper: func(m *decodedMessage) {
			m.keys = append(m.keys, other)
			m.instructions[2].accounts[1] = len(m.keys) - 1
			m.readonlyUnsigned = 0
		}, wantErr: "account 1"},
		{name: "the accounts swapped", tamper: func(m *decodedMessage) {
			m.instructions[2].accounts[0], m.instructions[3].accounts[0] = m.instructions[3].accounts[0], m.instructions[2].accounts[0]
		}, wantErr: "account 0"},
		{name: "a closed account read-only", tamper: func(m *decodedMessage) { m.readonlyUnsigned++ }, wantErr: "writable flag"},
		{name: "a second signer", tamper: func(m *decodedMessage) { m.signers = 2 }, wantErr: "only signer"},
	}
	for _, tt := range tests {
		m := clone()
		tt.tamper(&m)
		err := verifyInstructions(encode(m), c.owner, testHash, c.expected())
		if !errors.Is(err, ErrIntentMismatch) || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: verifyInstructions() error = %v, want a mismatch mentioning %q", tt.name, err, tt.wantErr)
		}
	}
	// Built for one account fewer than asked.
	short := *testClosePlan()
	short.accounts = short.accounts[:1]
	message, err = short.build(testHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyInstructions(message, c.owner, testHash, c.expected()); !errors.Is(err, ErrIntentMismatch) {
		t.Errorf("a close of fewer accounts error = %v, want a mismatch", err)
	}
}
