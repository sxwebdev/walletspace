package solana_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/operation"
)

const (
	// With recent prices 10k–40k the 75th percentile is 30k; the signed price
	// adds a quarter of headroom.
	testPrice = 37_500
	// A simulation that used 450 units gets 450 + 90 + 1000.
	testLimit = 1540
	// 5000 per signature plus ceil(1540 × 37,500 / 1e6) = 58.
	testFee = 5058
)

var testFees = []uint64{40_000, 10_000, 30_000, 20_000}

type fixture struct {
	ledger  *ledger
	node    *node
	adapter *solanachain.Adapter
	signer  *testSigner
	from    string
	to      string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	l := newLedger(t)
	l.fees = testFees
	n := ledgerNode(t, l)
	adapter, _ := single(t, n)
	signer := newTestSigner(7)
	return &fixture{ledger: l, node: n, adapter: adapter, signer: signer, from: signer.address(), to: newTestSigner(8).address()}
}

func (f *fixture) sol(amount string) chain.TransferRequest {
	return chain.TransferRequest{From: f.from, To: f.to, Asset: native("solana-mainnet"), Amount: amount}
}

func (f *fixture) usdc(amount string) chain.TransferRequest {
	return chain.TransferRequest{From: f.from, To: f.to, Asset: spl("solana-mainnet", usdcMint, "USDC", 6), Amount: amount}
}

func approvalOf(estimate chain.TransferEstimate) *chain.FeeApproval {
	return &chain.FeeApproval{
		FeeModel: estimate.FeeModel, ComputeUnitLimit: estimate.ComputeUnitLimit,
		ComputeUnitPriceMicroLamports: estimate.ComputeUnitPriceMicroLamports,
		CreatesTokenAccount:           estimate.CreatesTokenAccount, TokenAccountRent: estimate.TokenAccountRent,
		Fee: estimate.Fee,
	}
}

// estimateAndSend estimates, approves exactly what was shown and sends.
func (f *fixture) estimateAndSend(t *testing.T, req chain.TransferRequest) (chain.Transaction, error) {
	t.Helper()
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatalf("EstimateTransfer() error = %v", err)
	}
	req.Approved = approvalOf(estimate)
	return f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer)
}

type sentInstruction struct {
	program  string
	accounts []string
	data     []byte
}

// decodeSent decodes a broadcast transaction with the SDK and checks its one
// signature over the exact message bytes that were sent.
func decodeSent(t *testing.T, raw []byte, signer *testSigner) (*solana.Transaction, []sentInstruction) {
	t.Helper()
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		t.Fatalf("decode sent transaction: %v", err)
	}
	if len(tx.Signatures) != 1 || raw[0] != 1 {
		t.Fatalf("signatures = %d, want 1", len(tx.Signatures))
	}
	if !ed25519.Verify(signer.PublicKey(), raw[65:], raw[1:65]) {
		t.Error("the signature does not verify over the broadcast message")
	}
	var out []sentInstruction
	for _, instruction := range tx.Message.Instructions {
		item := sentInstruction{program: tx.Message.AccountKeys[instruction.ProgramIDIndex].String(), data: instruction.Data}
		for _, index := range instruction.Accounts {
			item.accounts = append(item.accounts, tx.Message.AccountKeys[index].String())
		}
		out = append(out, item)
	}
	return tx, out
}

func computeBudget(limit uint32, price uint64) []sentInstruction {
	return []sentInstruction{
		{program: "ComputeBudget111111111111111111111111111111", data: binary.LittleEndian.AppendUint32([]byte{2}, limit)},
		{program: "ComputeBudget111111111111111111111111111111", data: binary.LittleEndian.AppendUint64([]byte{3}, price)},
	}
}

func equalInstructions(got, want []sentInstruction) bool {
	return slices.EqualFunc(got, want, func(a, b sentInstruction) bool {
		return a.program == b.program && slices.Equal(a.accounts, b.accounts) && bytes.Equal(a.data, b.data)
	})
}

func TestEstimateSOLTransfer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 2_000_000_000)
	f.ledger.wallet(f.to, 1)
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.5"))
	if err != nil {
		t.Fatalf("EstimateTransfer() error = %v", err)
	}
	want := chain.TransferEstimate{
		NetworkID: "solana-mainnet", Amount: "0.5", Fee: lamportsText(testFee), FeeModel: "solana",
		ComputeUnitLimit: testLimit, ComputeUnitPriceMicroLamports: testPrice, NativeBalance: "2",
	}
	if estimate != want {
		t.Errorf("EstimateTransfer() = %+v, want %+v", estimate, want)
	}
	// The simulation carried the real amount and no priority price, so the
	// payer ends it in the same rent state as the real transfer.
	simulated, err := solana.TransactionFromBytes(f.ledger.simulated[0])
	if err != nil {
		t.Fatalf("decode simulated transaction: %v", err)
	}
	if data := simulated.Message.Instructions[2].Data; binary.LittleEndian.Uint64(data[4:]) != 500_000_000 {
		t.Errorf("simulated amount = %d, want 500,000,000", binary.LittleEndian.Uint64(data[4:]))
	}
	if data := simulated.Message.Instructions[1].Data; binary.LittleEndian.Uint64(data[1:]) != 0 {
		t.Errorf("simulated price = %d, want 0", binary.LittleEndian.Uint64(data[1:]))
	}
}

func TestSendSOL(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 2_000_000_000)
	var recorded []uint64
	req := f.sol("0.5")
	req.BeforeSigning = func(height uint64) error {
		if f.signer.signs.Load() != 0 {
			t.Error("the expiry was handed over after the signature")
		}
		recorded = append(recorded, height)
		return nil
	}
	got, err := f.estimateAndSend(t, req)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	sent := f.ledger.sentTransactions()
	if len(sent) != 1 {
		t.Fatalf("broadcasts = %d, want 1", len(sent))
	}
	tx, instructions := decodeSent(t, sent[0], f.signer)
	txID := tx.Signatures[0].String()
	if got != (chain.Transaction{NetworkID: "solana-mainnet", Hash: txID, Status: operation.StatusPending}) {
		t.Errorf("Send() = %+v, want pending %s", got, txID)
	}
	if len(recorded) != 1 || recorded[0] != testLastValid {
		t.Errorf("expiry handed over = %v, want %d once", recorded, testLastValid)
	}
	if tx.Message.RecentBlockhash.String() != testBlockhash || tx.Message.AccountKeys[0].String() != f.from ||
		tx.Message.Header.NumRequiredSignatures != 1 || tx.Message.IsVersioned() {
		t.Errorf("message = %+v, want a legacy message paid by the sender with the fetched blockhash", tx.Message)
	}
	want := append(computeBudget(testLimit, testPrice), sentInstruction{
		program: systemProgram, accounts: []string{f.from, f.to},
		data: binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), 500_000_000),
	})
	if !equalInstructions(instructions, want) {
		t.Errorf("instructions = %+v, want %+v", instructions, want)
	}
	if got := f.signer.signs.Load(); got != 1 {
		t.Errorf("signatures made = %d, want 1", got)
	}
	// Recorded for Transaction too.
	f.node.handle("getSignatureStatuses", func([]json.RawMessage) reply { return reply{result: withContext([]any{nil})} })
	f.node.handle("getBlockHeight", func([]json.RawMessage) reply { return reply{result: testLastValid + 151} })
	if status, err := f.adapter.Transaction(t.Context(), "solana-mainnet", txID); err != nil || status.Status != "failed" {
		t.Errorf("Transaction() after expiry = %+v, %v, want failed", status, err)
	}
}

func TestMaxSOLEmptiesTheAccount(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 1_000_000_000)
	estimate, err := f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", f.sol("max"))
	if err != nil {
		t.Fatalf("EstimateMaxTransfer() error = %v", err)
	}
	if want := lamportsText(1_000_000_000 - testFee); estimate.Amount != want || estimate.Fee != lamportsText(testFee) {
		t.Errorf("max = %s fee %s, want %s fee %s", estimate.Amount, estimate.Fee, want, lamportsText(testFee))
	}
	req := f.sol(estimate.Amount)
	req.Approved = approvalOf(estimate)
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); err != nil {
		t.Fatalf("Send(max) error = %v", err)
	}
}

func TestSOLRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		balance   uint64
		recipient bool
		amount    string
		max       bool
		wantErr   string
	}{
		{name: "no SOL", balance: 0, amount: "0.1", wantErr: "holds no SOL"},
		{name: "fee not covered", balance: 5000, amount: "0.000000001", wantErr: "does not cover"},
		{name: "amount not covered", balance: 1_000_000_000, amount: "1", wantErr: "does not cover 1 plus the fee"},
		{
			name: "dust left behind", balance: 1_000_000_000, amount: lamportsText(1_000_000_000 - testFee - 1000),
			wantErr: "below the 0.00065024 SOL an account must keep",
		},
		{name: "new recipient below rent", balance: 1_000_000_000, amount: "0.0001", wantErr: "opening it takes at least 0.00065024 SOL"},
		// Below the rent minimum, the fee alone is refused before anything is
		// sent (SIMD-0392), so not even the maximum can go out.
		{name: "max of a balance below the minimum", balance: testFee, recipient: true, max: true, wantErr: "checks the fee before anything is sent"},
		{name: "too many decimals", balance: 1_000_000_000, amount: "0.0000000001", wantErr: "more than 9 decimals"},
		{name: "not positive", balance: 1_000_000_000, amount: "-1", wantErr: "positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.ledger.wallet(f.from, tt.balance)
			if tt.recipient {
				f.ledger.wallet(f.to, 1)
			}
			var err error
			if tt.max {
				_, err = f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", f.sol("max"))
			} else {
				_, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol(tt.amount))
			}
			if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want an invalid request mentioning %q", err, tt.wantErr)
			}
		})
	}
}

// A rent-exempt recipient may receive less than the rent minimum.
func TestSOLToAnExistingRecipientMayBeSmall(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 1_000_000_000)
	f.ledger.wallet(f.to, rentExempt0)
	if _, err := f.estimateAndSend(t, f.sol("0.000001")); err != nil {
		t.Errorf("Send() error = %v", err)
	}
}

// Sending to oneself moves nothing but the fee, and is allowed.
func TestSOLToOneself(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 1_000_000_000)
	f.to = f.from
	if _, err := f.estimateAndSend(t, f.sol("0.5")); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	_, instructions := decodeSent(t, f.ledger.sentTransactions()[0], f.signer)
	if got := instructions[2].accounts; !slices.Equal(got, []string{f.from, f.from}) {
		t.Errorf("transfer accounts = %v", got)
	}
}

func TestSendUSDC(t *testing.T) {
	t.Parallel()

	for _, opened := range []bool{true, false} {
		name := "recipient account exists"
		if !opened {
			name = "recipient account is opened"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.ledger.units = 30_000
			f.ledger.wallet(f.from, 100_000_000)
			f.ledger.mint(usdcMint, 6)
			f.ledger.tokens(f.from, usdcMint, 5_000_000)
			if opened {
				f.ledger.tokens(f.to, usdcMint, 1)
			}
			estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.usdc("2.5"))
			if err != nil {
				t.Fatalf("EstimateTransfer() error = %v", err)
			}
			limit := uint32(30_000 + 6000 + 1000)
			fee := uint64(5000 + (uint64(limit)*testPrice+999_999)/1_000_000)
			if !opened {
				fee += rentExemptToken
			}
			if estimate.Amount != "2.5" || estimate.Fee != lamportsText(fee) || estimate.ComputeUnitLimit != uint64(limit) ||
				estimate.CreatesTokenAccount == opened || (!opened && estimate.TokenAccountRent != lamportsText(rentExemptToken)) {
				t.Errorf("estimate = %+v, want fee %s", estimate, lamportsText(fee))
			}
			req := f.usdc("2.5")
			req.Approved = approvalOf(estimate)
			if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); err != nil {
				t.Fatalf("Send() error = %v", err)
			}
			_, instructions := decodeSent(t, f.ledger.sentTransactions()[0], f.signer)
			want := computeBudget(limit, testPrice)
			source, destination := ata(f.from, usdcMint), ata(f.to, usdcMint)
			if !opened {
				want = append(want, sentInstruction{
					program:  "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL",
					accounts: []string{f.from, destination, f.to, usdcMint, systemProgram, tokenProgram},
					data:     []byte{1},
				})
			}
			want = append(want, sentInstruction{
				program: tokenProgram, accounts: []string{source, usdcMint, destination, f.from},
				data: append(binary.LittleEndian.AppendUint64([]byte{12}, 2_500_000), 6),
			})
			if !equalInstructions(instructions, want) {
				t.Errorf("instructions = %+v, want %+v", instructions, want)
			}
		})
	}
}

func TestMaxUSDCIsTheMainTokenAccount(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.mint(usdcMint, 6)
	f.ledger.tokens(f.from, usdcMint, 3_000_000)
	// More USDC in a second account: not part of the maximum.
	f.ledger.set(key(77), ledgerAccount{owner: tokenProgram, lamports: 1, data: tokenAccountData(usdcMint, f.from, 9_000_000, 1)})
	estimate, err := f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", f.usdc("max"))
	if err != nil {
		t.Fatalf("EstimateMaxTransfer() error = %v", err)
	}
	if estimate.Amount != "3" {
		t.Errorf("max = %s, want the main account's 3", estimate.Amount)
	}
	// Asking for more than the main account holds, while the total would
	// cover it, names where the rest is.
	_, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.usdc("5"))
	if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "sits in other token accounts") {
		t.Errorf("EstimateTransfer(5) error = %v, want the outside-funds message", err)
	}
	_, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.usdc("50"))
	if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "holds 3 USDC") {
		t.Errorf("EstimateTransfer(50) error = %v, want the plain balance message", err)
	}
}

func TestTokenRefusals(t *testing.T) {
	t.Parallel()

	pda := ata(newTestSigner(9).address(), usdcMint)
	tests := []struct {
		name    string
		setup   func(*fixture)
		to      string
		asset   chain.Asset
		amount  string
		wantErr string
	}{
		{name: "recipient is a token account", setup: func(f *fixture) {
			f.ledger.tokens(f.to, usdcMint, 1)
		}, to: "ata", wantErr: "is a token account"},
		{name: "recipient is a Token-2022 account", setup: func(f *fixture) {
			f.ledger.set(f.to, ledgerAccount{owner: token2022, lamports: 1, data: make([]byte, 170)})
		}, wantErr: "is a token account"},
		{name: "recipient off the curve", to: pda, wantErr: "program-owned addresses are not supported"},
		{name: "no main token account", setup: func(f *fixture) {
			f.ledger.mu.Lock()
			delete(f.ledger.accounts, ata(f.from, usdcMint))
			f.ledger.mu.Unlock()
		}, wantErr: "holds 0 USDC"},
		{name: "frozen", setup: func(f *fixture) {
			f.ledger.set(ata(f.from, usdcMint), ledgerAccount{owner: tokenProgram, lamports: 1, data: tokenAccountData(usdcMint, f.from, 9_000_000, 2)})
		}, wantErr: "frozen"},
		{name: "decimals differ", asset: spl("solana-mainnet", usdcMint, "USDC", 9), wantErr: "the mint has 6 decimals"},
		{name: "Token-2022 mint", setup: func(f *fixture) {
			f.ledger.set(usdcMint, ledgerAccount{owner: token2022, lamports: 1, data: make([]byte, 234)})
		}, wantErr: "Token-2022"},
		{name: "recipient account of another owner", setup: func(f *fixture) {
			f.ledger.set(ata(f.to, usdcMint), ledgerAccount{owner: tokenProgram, lamports: 1, data: tokenAccountData(usdcMint, f.from, 1, 1)})
		}, wantErr: "recipient's token account"},
		{name: "no SOL for the fee", setup: func(f *fixture) { f.ledger.wallet(f.from, 1000) }, wantErr: "does not cover the fee"},
		{name: "simulation refuses", setup: func(f *fixture) {
			f.ledger.simErr = map[string]any{"InstructionError": []any{3, map[string]any{"Custom": 1}}}
			f.ledger.simLogs = []string{"Program log: Instruction: TransferChecked", "Program log: Error: insufficient funds", ""}
		}, wantErr: "Error: insufficient funds"},
		{name: "invalid recipient", to: "0xdead", wantErr: "invalid Solana address"},
		{name: "invalid mint", asset: spl("solana-mainnet", "not-a-mint", "X", 6), wantErr: "invalid SPL mint"},
		{name: "unknown kind", asset: chain.Asset{ID: "x", NetworkID: "solana-mainnet", Kind: "erc20", Decimals: 6}, wantErr: "unsupported Solana asset kind"},
		{name: "another network", asset: spl("solana-devnet", usdcMint, "USDC", 6), wantErr: "another network"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.ledger.wallet(f.from, 100_000_000)
			f.ledger.mint(usdcMint, 6)
			f.ledger.tokens(f.from, usdcMint, 9_000_000)
			if tt.setup != nil {
				tt.setup(f)
			}
			req := f.usdc(cmpOr(tt.amount, "1"))
			if tt.to == "ata" {
				req.To = ata(f.to, usdcMint)
			} else if tt.to != "" {
				req.To = tt.to
			}
			if tt.asset.ID != "" {
				req.Asset = tt.asset
			}
			_, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
			if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want an invalid request mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// What is signed is what was approved: a transfer that now needs more is
// refused with ErrFeeChanged, a malformed approval as a bad request, and a
// transfer the chain no longer allows with ErrStateChanged. Nothing is signed
// in any of these.
func TestSendRefusesWhatWasNotApproved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		token   bool
		change  func(*fixture, *chain.FeeApproval)
		wantIs  error
		wantErr string
	}{
		{name: "more compute", change: func(f *fixture, _ *chain.FeeApproval) { f.ledger.units = 5000 }, wantIs: chain.ErrFeeChanged, wantErr: "compute units"},
		{name: "higher price", change: func(f *fixture, _ *chain.FeeApproval) { f.ledger.fees = []uint64{90_000} }, wantIs: chain.ErrFeeChanged, wantErr: "priority price"},
		{name: "account to open", token: true, change: func(f *fixture, _ *chain.FeeApproval) {
			f.ledger.mu.Lock()
			delete(f.ledger.accounts, ata(f.to, usdcMint))
			f.ledger.mu.Unlock()
		}, wantIs: chain.ErrFeeChanged, wantErr: "has to be opened"},
		{name: "balance gone", change: func(f *fixture, _ *chain.FeeApproval) { f.ledger.wallet(f.from, 1000) }, wantIs: chain.ErrStateChanged, wantErr: "does not cover"},
		{name: "rejected by simulation", change: func(f *fixture, _ *chain.FeeApproval) { f.ledger.simErr = "AccountInUse" }, wantIs: chain.ErrStateChanged, wantErr: "would reject"},
		{name: "no approval", change: func(_ *fixture, a *chain.FeeApproval) { *a = chain.FeeApproval{} }, wantIs: chain.ErrInvalidRequest, wantErr: "out of range"},
		{name: "fee does not add up", change: func(_ *fixture, a *chain.FeeApproval) { a.Fee = "0.000001" }, wantIs: chain.ErrInvalidRequest, wantErr: "does not add up"},
		{name: "price above the ceiling", change: func(_ *fixture, a *chain.FeeApproval) {
			a.ComputeUnitPriceMicroLamports = 1_000_001
		}, wantIs: chain.ErrInvalidRequest, wantErr: "ceiling"},
		{name: "limit above the maximum", change: func(_ *fixture, a *chain.FeeApproval) { a.ComputeUnitLimit = 1_400_001 }, wantIs: chain.ErrInvalidRequest, wantErr: "out of range"},
		{name: "rent without an account", change: func(_ *fixture, a *chain.FeeApproval) { a.TokenAccountRent = "0.1" }, wantIs: chain.ErrInvalidRequest, wantErr: "without opening"},
		{name: "account without rent", change: func(_ *fixture, a *chain.FeeApproval) { a.CreatesTokenAccount = true }, wantIs: chain.ErrInvalidRequest, wantErr: "rent is not an amount"},
		{name: "fee of another family", change: func(_ *fixture, a *chain.FeeApproval) { a.FeeModel = "eip1559" }, wantIs: chain.ErrInvalidRequest, wantErr: "not a Solana fee"},
		{name: "fee not an amount", change: func(_ *fixture, a *chain.FeeApproval) { a.Fee = "lots" }, wantIs: chain.ErrInvalidRequest, wantErr: "not an amount"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.ledger.wallet(f.from, 100_000_000)
			req := f.sol("0.01")
			if tt.token {
				f.ledger.mint(usdcMint, 6)
				f.ledger.tokens(f.from, usdcMint, 9_000_000)
				f.ledger.tokens(f.to, usdcMint, 0)
				req = f.usdc("1")
			}
			estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
			if err != nil {
				t.Fatalf("EstimateTransfer() error = %v", err)
			}
			req.Approved = approvalOf(estimate)
			tt.change(f, req.Approved)
			_, err = f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer)
			if !errors.Is(err, tt.wantIs) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Send() error = %v, want %v mentioning %q", err, tt.wantIs, tt.wantErr)
			}
			if got := f.signer.signs.Load(); got != 0 {
				t.Errorf("signatures made = %d, want none", got)
			}
			if got := len(f.ledger.sentTransactions()); got != 0 {
				t.Errorf("broadcasts = %d, want none", got)
			}
		})
	}
}

func TestSendWithoutApprovalIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	_, err := f.adapter.Send(t.Context(), "solana-mainnet", f.sol("0.01"), f.signer)
	if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "not confirmed against a fee estimate") {
		t.Errorf("Send() error = %v", err)
	}
}

// A cheaper network than approved is fine: the approved values are signed.
func TestSendSignsTheApprovedPriceEvenWhenPricesFell(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	req := f.sol("0.01")
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	req.Approved = approvalOf(estimate)
	f.ledger.fees = nil
	f.ledger.units = 100
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	_, instructions := decodeSent(t, f.ledger.sentTransactions()[0], f.signer)
	if !equalInstructions(instructions[:2], computeBudget(testLimit, testPrice)) {
		t.Errorf("compute budget = %+v, want the approved one", instructions[:2])
	}
}

func TestSendRefusesTheWrongSigner(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	req := f.sol("0.01")
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	req.Approved = approvalOf(estimate)
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, digestOnly{}); !errors.Is(err, chain.ErrSignerMismatch) {
		t.Errorf("Send(digest signer) error = %v, want ErrSignerMismatch", err)
	}
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, nil); !errors.Is(err, chain.ErrSignerMismatch) {
		t.Errorf("Send(nil) error = %v, want ErrSignerMismatch", err)
	}
	tron := newTestSigner(7)
	tron.family = chain.FamilyTron
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, tron); !errors.Is(err, chain.ErrSignerMismatch) {
		t.Errorf("Send(Tron key) error = %v, want ErrSignerMismatch", err)
	}
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, newTestSigner(9)); !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "signer does not match sender") {
		t.Errorf("Send(other key) error = %v", err)
	}
	bad := newTestSigner(7)
	bad.corrupt = true
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, bad); err == nil || !strings.Contains(err.Error(), "does not verify") {
		t.Errorf("Send(corrupt signer) error = %v", err)
	}
	if got := len(f.ledger.sentTransactions()); got != 0 {
		t.Errorf("broadcasts = %d, want none", got)
	}
}

// A node that refuses the transaction is a rejection: nothing is on chain and
// a new transaction may be built. One whose answer is lost is
// broadcast_unknown, and the transaction is not sent anywhere else.
func TestBroadcastOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		answer     reply
		wantStatus string
		wantIs     error
		wantErr    string
	}{
		{
			name: "preflight refusal", answer: reply{err: &rpcError{Code: -32002, Message: "Transaction simulation failed: Blockhash not found"}},
			wantStatus: operation.StatusFailed, wantIs: chain.ErrInvalidRequest, wantErr: "Blockhash not found",
		},
		{name: "lost answer", answer: reply{status: http.StatusBadGateway, raw: "<html>"}, wantStatus: operation.StatusBroadcastUnknown, wantIs: chain.ErrBroadcastUnknown},
		{name: "unreadable answer", answer: reply{raw: "{"}, wantStatus: operation.StatusBroadcastUnknown, wantIs: chain.ErrBroadcastUnknown},
		{name: "another signature", answer: reply{result: solana.Signature{1}.String()}, wantStatus: operation.StatusBroadcastUnknown, wantIs: chain.ErrBroadcastUnknown, wantErr: "answered with signature"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			first := newLedger(t)
			first.fees = testFees
			signer := newTestSigner(7)
			first.wallet(signer.address(), 100_000_000)
			first.sendReply = &tt.answer
			second := newLedger(t)
			second.fees = testFees
			second.wallet(signer.address(), 100_000_000)
			mux := http.NewServeMux()
			mux.HandleFunc("/first", ledgerNode(t, first).serve)
			mux.HandleFunc("/second", ledgerNode(t, second).serve)
			adapter := newAdapter(t, &provider{endpoints: []string{"http://rpc.test/first", "http://rpc.test/second"}, client: newMuxClient(t, mux)})
			req := chain.TransferRequest{From: signer.address(), To: newTestSigner(8).address(), Asset: native("solana-mainnet"), Amount: "0.01"}
			estimate, err := adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
			if err != nil {
				t.Fatal(err)
			}
			req.Approved = approvalOf(estimate)
			got, err := adapter.Send(t.Context(), "solana-mainnet", req, signer)
			if !errors.Is(err, tt.wantIs) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Send() error = %v, want %v mentioning %q", err, tt.wantIs, tt.wantErr)
			}
			sent := first.sentTransactions()
			if len(sent) != 1 {
				t.Fatalf("broadcasts = %d, want 1", len(sent))
			}
			if want := solana.SignatureFromBytes(sent[0][1:65]).String(); got.Hash != want || got.Status != tt.wantStatus {
				t.Errorf("Send() = %+v, want %s with %s", got, tt.wantStatus, want)
			}
			if got := len(second.sentTransactions()); got != 0 {
				t.Errorf("broadcasts to the second node = %d, want none", got)
			}
		})
	}
}

func TestAFailedExpiryRecordStopsTheBroadcast(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	req := f.sol("0.01")
	req.BeforeSigning = func(uint64) error { return errors.New("disk full") }
	if _, err := f.estimateAndSend(t, req); err == nil || !strings.Contains(err.Error(), "disk full") ||
		errors.Is(err, chain.ErrBroadcastUnknown) {
		t.Errorf("Send() error = %v, want the record failure", err)
	}
	if got := f.signer.signs.Load(); got != 0 {
		t.Errorf("signatures made = %d, want none when the expiry cannot be recorded", got)
	}
	req = f.sol("0.01")
	req.BeforeBroadcast = func(string) error { return errors.New("no space") }
	if _, err := f.estimateAndSend(t, req); err == nil || !strings.Contains(err.Error(), "no space") {
		t.Errorf("Send() error = %v, want the record failure", err)
	}
	if got := len(f.ledger.sentTransactions()); got != 0 {
		t.Errorf("broadcasts = %d, want none", got)
	}
}

// A message that is not the transfer asked for is never signed, even when it
// is what the adapter itself built.
func TestSendNeverSignsAMessageOtherThanTheIntent(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	solanachain.TamperWithBuilder(f.adapter)
	_, err := f.estimateAndSend(t, f.sol("0.01"))
	if !errors.Is(err, solanachain.ErrIntentMismatch) {
		t.Errorf("Send() error = %v, want ErrIntentMismatch", err)
	}
	if f.signer.signs.Load() != 0 || len(f.ledger.sentTransactions()) != 0 {
		t.Error("a message other than the intent was signed or sent")
	}
}

// Every read a transfer depends on can fail or answer nonsense; each stops it
// before anything is signed.
func TestTransferStopsOnANodeFailure(t *testing.T) {
	t.Parallel()

	failing := reply{err: &rpcError{Code: -32005, Message: "Node is unhealthy"}}
	tests := []struct {
		name    string
		token   bool
		method  string
		answer  reply
		send    bool
		wantErr string
	}{
		{name: "rent", method: "getMinimumBalanceForRentExemption", answer: failing, wantErr: "rent-exempt minimum"},
		{name: "rent of zero", method: "getMinimumBalanceForRentExemption", answer: reply{result: 0}, wantErr: "answered zero"},
		{name: "accounts", method: "getMultipleAccounts", answer: failing, wantErr: "read accounts"},
		{name: "accounts miscounted", method: "getMultipleAccounts", answer: reply{result: withContext([]any{nil})}, wantErr: "answered 1 accounts for 2"},
		{name: "priority fees", method: "getRecentPrioritizationFees", answer: failing, wantErr: "priority fees"},
		{name: "token priority fees", token: true, method: "getRecentPrioritizationFees", answer: failing, wantErr: "priority fees"},
		{name: "simulation", method: "simulateTransaction", answer: failing, wantErr: "simulate the transaction"},
		{name: "token simulation", token: true, method: "simulateTransaction", answer: failing, wantErr: "simulate the transaction"},
		{name: "simulation without a result", method: "simulateTransaction", answer: reply{result: withContext(nil)}, wantErr: "no result"},
		{name: "simulation without compute", method: "simulateTransaction", answer: reply{result: withContext(map[string]any{"err": nil})}, wantErr: "compute used"},
		{name: "blockhash", method: "getLatestBlockhash", answer: failing, send: true, wantErr: "recent blockhash"},
		{name: "empty blockhash", method: "getLatestBlockhash", answer: reply{result: withContext(nil)}, send: true, wantErr: "no blockhash"},
		{name: "token account rent", token: true, method: "getMinimumBalanceForRentExemption", send: true, wantErr: "rent-exempt minimum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.ledger.wallet(f.from, 100_000_000)
			req := f.sol("0.01")
			if tt.token {
				f.ledger.mint(usdcMint, 6)
				f.ledger.tokens(f.from, usdcMint, 9_000_000)
				req = f.usdc("1")
			}
			var err error
			if tt.send {
				estimate, estimateErr := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
				if estimateErr != nil {
					t.Fatal(estimateErr)
				}
				req.Approved = approvalOf(estimate)
				if tt.name == "token account rent" {
					// The rent of an account without data is cached; the
					// token account's is asked again on a fresh adapter.
					f.adapter = newAdapter(t, &provider{endpoints: []string{f.node.server.URL}, client: f.node.server.Client()})
					f.node.handle("getMinimumBalanceForRentExemption", func(params []json.RawMessage) reply {
						if string(params[0]) == "0" {
							return reply{result: rentExempt0}
						}
						return failing
					})
				} else {
					f.node.handle(tt.method, func([]json.RawMessage) reply { return tt.answer })
				}
				_, err = f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer)
			} else {
				f.node.handle(tt.method, func([]json.RawMessage) reply { return tt.answer })
				_, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
			if f.signer.signs.Load() != 0 || len(f.ledger.sentTransactions()) != 0 {
				t.Error("something was signed or sent")
			}
		})
	}
}

func TestTransferRefusesMalformedRequests(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.mint(usdcMint, 6)
	f.ledger.set(ata(f.from, usdcMint), ledgerAccount{owner: tokenProgram, lamports: 1, data: tokenAccountData(usdcMint, f.from, 5, 0)})
	wrongDecimals := f.sol("1")
	wrongDecimals.Asset.Decimals = 18
	action := f.sol("1")
	action.Action = "wrap"
	sender := f.sol("1")
	sender.From = "0xabc"
	huge := f.sol("100000000000")
	for name, tt := range map[string]struct {
		req     chain.TransferRequest
		wantErr string
	}{
		"amount past uint64":       {req: huge, wantErr: "too large"},
		"an action":                {req: action, wantErr: `no "wrap" action`},
		"an invalid sender":        {req: sender, wantErr: "invalid sender"},
		"SOL with other decimals":  {req: wrongDecimals, wantErr: "9 decimals"},
		"an uninitialized account": {req: f.usdc("0.000001"), wantErr: "not initialized"},
		"a negative amount of SOL": {req: f.sol("-0.1"), wantErr: "positive"},
	} {
		if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", tt.req); !errors.Is(err, chain.ErrInvalidRequest) ||
			!strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error = %v, want %q", name, err, tt.wantErr)
		}
	}
	// On send, a refusal of the request itself stays a bad request rather
	// than a state change.
	sender.Approved = &chain.FeeApproval{ComputeUnitLimit: testLimit, ComputeUnitPriceMicroLamports: testPrice, Fee: lamportsText(testFee)}
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", sender, f.signer); errors.Is(err, chain.ErrStateChanged) ||
		!errors.Is(err, chain.ErrInvalidRequest) {
		t.Errorf("Send(invalid sender) error = %v, want a plain bad request", err)
	}
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-odd", f.sol("1")); !errors.Is(err, chain.ErrInvalidRequest) {
		t.Errorf("EstimateTransfer(unknown cluster) error = %v", err)
	}
	if _, err := f.adapter.Send(t.Context(), "solana-odd", f.sol("1"), f.signer); err == nil {
		t.Error("Send(unknown cluster) succeeded")
	}
}

func TestApprovalAmountsAreLamports(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	req := f.sol("0.01")
	req.Approved = &chain.FeeApproval{ComputeUnitLimit: testLimit, ComputeUnitPriceMicroLamports: testPrice, Fee: "0.0000050581"}
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "not an amount") {
		t.Errorf("Send(fee past lamports) error = %v", err)
	}
}

// Opening the recipient's token account was approved at one rent and now costs
// more.
func TestSendRefusesAHigherRentThanApproved(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.mint(usdcMint, 6)
	f.ledger.tokens(f.from, usdcMint, 9_000_000)
	req := f.usdc("1")
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	approved := approvalOf(estimate)
	approved.TokenAccountRent = lamportsText(rentExemptToken - 1)
	// Keep the approval self-consistent: the fee drops by the same lamport.
	approved.Fee = lamportsText(lamportsOf(t, estimate.Fee) - 1)
	req.Approved = approved
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); !errors.Is(err, chain.ErrFeeChanged) ||
		!strings.Contains(err.Error(), "opening the token account now costs") {
		t.Errorf("Send() error = %v, want ErrFeeChanged for the rent", err)
	}
}

func lamportsOf(t *testing.T, value string) uint64 {
	t.Helper()
	whole, frac, _ := strings.Cut(value, ".")
	frac += strings.Repeat("0", 9-len(frac))
	w, err1 := strconv.ParseUint(whole, 10, 64)
	f, err2 := strconv.ParseUint(frac, 10, 64)
	if err1 != nil || err2 != nil {
		t.Fatalf("parse %q", value)
	}
	return w*1_000_000_000 + f
}

func TestSendReportsASignerFailure(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	failing := &failingSigner{testSigner: f.signer}
	req := f.sol("0.01")
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	req.Approved = approvalOf(estimate)
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, failing); err == nil || !strings.Contains(err.Error(), "space is locked") {
		t.Errorf("Send() error = %v", err)
	}
	if len(f.ledger.sentTransactions()) != 0 {
		t.Error("sent without a signature")
	}
}

type failingSigner struct{ *testSigner }

func (failingSigner) SignMessage(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("space is locked")
}

// A long program log is cut, not passed on whole.
func TestSimulationLogsAreCut(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.simErr = "Custom"
	f.ledger.simLogs = []string{strings.Repeat("x", 500)}
	_, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.01"))
	if err == nil || strings.Contains(err.Error(), strings.Repeat("x", 201)) || !strings.Contains(err.Error(), strings.Repeat("x", 200)) {
		t.Errorf("error = %v, want the log cut at 200 characters", err)
	}
}

func TestTokenTransferStopsOnItsOwnReads(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 0)
	f.ledger.mint(usdcMint, 6)
	f.ledger.tokens(f.from, usdcMint, 9_000_000)
	// Holding the token but no SOL for the fee.
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.usdc("1")); !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "holds no SOL") {
		t.Errorf("EstimateTransfer(no SOL) error = %v", err)
	}
	f.ledger.wallet(f.from, 100_000_000)
	f.node.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
		return reply{err: &rpcError{Code: -32005, Message: "unhealthy"}}
	})
	// The listing only explains a refusal; when it cannot be read the plain
	// balance is the reason, and on send it is a change of state.
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.usdc("50")); !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "the account holds 9 USDC") {
		t.Errorf("EstimateTransfer(more than held) error = %v, want the plain balance refusal", err)
	}
	more := f.usdc("50")
	more.Approved = &chain.FeeApproval{ComputeUnitLimit: testLimit, ComputeUnitPriceMicroLamports: testPrice, Fee: lamportsText(testFee)}
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", more, f.signer); !errors.Is(err, chain.ErrStateChanged) ||
		!strings.Contains(err.Error(), "the account holds 9 USDC") {
		t.Errorf("Send(more than held) error = %v, want ErrStateChanged with the plain balance", err)
	}
	f.node.handle("getMultipleAccounts", func([]json.RawMessage) reply {
		return reply{err: &rpcError{Code: -32005, Message: "unhealthy"}}
	})
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.usdc("1")); err == nil ||
		!strings.Contains(err.Error(), "read accounts") {
		t.Errorf("EstimateTransfer(accounts unreadable) error = %v", err)
	}
	approved := f.sol("1")
	approved.Approved = &chain.FeeApproval{ComputeUnitLimit: testLimit, ComputeUnitPriceMicroLamports: testPrice, Fee: lamportsText(testFee)}
	if _, err := f.adapter.Send(t.Context(), "solana-odd", approved, f.signer); !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "unknown Solana cluster") {
		t.Errorf("Send(unknown cluster) error = %v", err)
	}
}

// However high recent prices are, the priority price signed is at most one
// lamport per compute unit.
func TestThePriorityPriceIsCapped(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.fees = []uint64{50_000_000, 90_000_000}
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.01"))
	if err != nil {
		t.Fatal(err)
	}
	if estimate.ComputeUnitPriceMicroLamports != 1_000_000 {
		t.Errorf("price = %d, want the 1,000,000 cap", estimate.ComputeUnitPriceMicroLamports)
	}
	// And a transfer approved at the cap is sent while prices stay above it.
	req := f.sol("0.01")
	req.Approved = approvalOf(estimate)
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); err != nil {
		t.Errorf("Send() at the cap error = %v", err)
	}
	// With no recent prices the price is the 1,000 floor: 5000 plus
	// ceil(1540 × 1,000 / 1e6) = 2.
	f.ledger.fees = nil
	if estimate, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.01")); err != nil ||
		estimate.ComputeUnitPriceMicroLamports != 1000 || estimate.Fee != "0.000005002" {
		t.Errorf("estimate without recent prices = %+v, %v, want the floor price", estimate, err)
	}
	// A low recent price is raised to the floor too; the headroom applies
	// above it.
	f.ledger.fees = []uint64{100}
	if estimate, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.01")); err != nil ||
		estimate.ComputeUnitPriceMicroLamports != 1000 {
		t.Errorf("estimate at a recent price of 100 = %+v, %v, want the floor", estimate, err)
	}
	f.ledger.fees = []uint64{1000}
	if estimate, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.01")); err != nil ||
		estimate.ComputeUnitPriceMicroLamports != 1250 {
		t.Errorf("estimate at a recent price of 1,000 = %+v, %v, want 1,250", estimate, err)
	}
}

// A wallet can be swept to zero when paying the fee leaves it rent-exempt;
// when the fee alone would drop it below the minimum, the network refuses
// before the transfer runs, and so does this wallet, in its own words.
func TestASweepAtTheRentEdge(t *testing.T) {
	t.Parallel()

	for name, fees := range map[string][]uint64{"the floor price": nil, "a priority price": testFees} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.ledger.fees = fees
			f.ledger.wallet(f.from, rentExempt0+8000)
			f.ledger.wallet(f.to, rentExempt0)
			estimate, err := f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", f.sol("max"))
			if err != nil {
				t.Fatalf("EstimateMaxTransfer() error = %v", err)
			}
			req := f.sol(estimate.Amount)
			req.Approved = approvalOf(estimate)
			if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); err != nil {
				t.Fatalf("Send(max) error = %v", err)
			}
			if got := len(f.ledger.sentTransactions()); got != 1 {
				t.Errorf("broadcasts = %d, want 1", got)
			}

			edge := newFixture(t)
			edge.ledger.fees = fees
			edge.ledger.wallet(edge.from, rentExempt0+3000)
			edge.ledger.wallet(edge.to, rentExempt0)
			_, err = edge.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", edge.sol("max"))
			if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "the network checks the fee before anything is sent") {
				t.Errorf("EstimateMaxTransfer(at the edge) error = %v, want the fee-first refusal", err)
			}
		})
	}
}

// A token transfer that leaves the payer just above the rent minimum after a
// non-zero priority fee is sent: the simulation is not charged a priority fee
// at its own large limit.
func TestATokenTransferJustAboveTheRentEdge(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.units = 30_000
	// 30,000 units → limit 37,000; priority ceil(37,000 × 37,500 / 1e6) = 1388.
	const priority = 1388
	f.ledger.wallet(f.from, rentExempt0+5000+rentExemptToken+priority+100)
	f.ledger.mint(usdcMint, 6)
	f.ledger.tokens(f.from, usdcMint, 9_000_000)
	if _, err := f.estimateAndSend(t, f.usdc("1")); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
}

// USDC held only outside the associated account: the refusal says where it
// is, rather than that an account does not exist.
func TestTokensOnlyOutsideTheMainAccountAreExplained(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.mint(usdcMint, 6)
	f.ledger.set(key(77), ledgerAccount{owner: tokenProgram, lamports: 1, data: tokenAccountData(usdcMint, f.from, 9_000_000, 1)})
	for name, req := range map[string]chain.TransferRequest{"an amount": f.usdc("1"), "the maximum": f.usdc("max")} {
		var err error
		if name == "the maximum" {
			_, err = f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", req)
		} else {
			_, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
		}
		if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "the main token account holds 0 USDC; the rest sits in other token accounts") {
			t.Errorf("%s: error = %v, want the outside-funds explanation", name, err)
		}
	}
}

// A token transfer may leave the payer at exactly zero: the rent of the
// account it opens and the priority fee take the rest. The price-free probe
// leaves the priority fee behind as dust, which the runtime refuses after
// executing it; the compute it reports is still what sizes the limit.
func TestATokenTransferMayEmptyThePayer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.units = 30_000
	// 30,000 units → limit 37,000; priority ceil(37,000 × 37,500 / 1e6) = 1388.
	f.ledger.wallet(f.from, 5000+rentExemptToken+1388)
	f.ledger.mint(usdcMint, 6)
	f.ledger.tokens(f.from, usdcMint, 9_000_000)
	if _, err := f.estimateAndSend(t, f.usdc("1")); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if got := len(f.ledger.sentTransactions()); got != 1 {
		t.Errorf("broadcasts = %d, want 1", got)
	}
}

// When this wallet's checks pass and the runtime refuses the final
// transaction, the runtime's reason is reported and nothing is signed — on
// estimate as a refusal, on send as a change of state.
func TestTheFinalSimulationDecides(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.mint(usdcMint, 6)
	f.ledger.tokens(f.from, usdcMint, 9_000_000)
	f.ledger.tokens(f.to, usdcMint, 0)
	req := f.usdc("1")
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	f.ledger.finalErr = map[string]any{"InstructionError": []any{2, "AccountFrozen"}}
	f.ledger.simLogs = []string{"Program log: Error: Account is frozen"}
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req); !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "Account is frozen") {
		t.Errorf("EstimateTransfer() error = %v, want the runtime's reason", err)
	}
	req.Approved = approvalOf(estimate)
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); !errors.Is(err, chain.ErrStateChanged) ||
		!strings.Contains(err.Error(), "Account is frozen") {
		t.Errorf("Send() error = %v, want ErrStateChanged with the runtime's reason", err)
	}
	if f.signer.signs.Load() != 0 || len(f.ledger.sentTransactions()) != 0 {
		t.Error("a transaction the runtime refused was signed or sent")
	}
}

// A probe that cannot be charged its fee executes nothing and reports no
// compute; the refusal is this wallet's own, with the fee it would charge.
func TestAProbeThatCannotPayIsRefusedInTheWalletsWords(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 4000)
	f.ledger.wallet(f.to, rentExempt0)
	_, err := f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", f.sol("max"))
	if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "does not cover the fee of at least 0.000005") {
		t.Errorf("EstimateMaxTransfer() error = %v, want the wallet's own refusal", err)
	}
}

// Where this wallet's checks would pass and the runtime still refuses, the
// runtime's reason is reported readably: here the rent-paying recipient rule,
// reached with the wallet's own recipient check out of the way.
func TestTheRuntimesRentReasonIsReadable(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 1_000_000_000)
	f.ledger.wallet(f.to, rentExempt0)
	req := f.sol("0.000001")
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req); err != nil {
		t.Fatal(err)
	}
	// The recipient drops below the minimum between the wallet's read and
	// the runtime's: only the runtime sees it.
	f.ledger.finalErr = map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": 1}}
	_, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
	if !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "account "+f.to+" would be left with less than its rent-exempt minimum") {
		t.Errorf("EstimateTransfer() error = %v, want the runtime's reason naming the recipient", err)
	}
}

// The final simulation's own RPC failing stops the transfer like any read.
func TestAFailedFinalSimulationStopsTheTransfer(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	calls := 0
	f.node.mu.Lock()
	inner := f.node.handlers["simulateTransaction"]
	f.node.mu.Unlock()
	f.node.handle("simulateTransaction", func(params []json.RawMessage) reply {
		calls++
		if calls == 2 {
			return reply{err: &rpcError{Code: -32005, Message: "Node is unhealthy"}}
		}
		return inner(params)
	})
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.01")); err == nil ||
		!strings.Contains(err.Error(), "simulate the transaction") {
		t.Errorf("EstimateTransfer() error = %v", err)
	}
}

// A probe the runtime refuses before executing it, for a reason this
// wallet's checks do not know, is refused with the runtime's reason.
func TestAProbeRefusedForAnotherReason(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.units = 0
	f.ledger.simErr = "AccountInUse"
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.01")); !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "AccountInUse") {
		t.Errorf("EstimateTransfer() error = %v, want the runtime's reason", err)
	}
	f.ledger.simErr = nil
	if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.01")); err == nil ||
		!strings.Contains(err.Error(), "did not report the compute used") {
		t.Errorf("EstimateTransfer(no compute, no error) error = %v", err)
	}
}

// The approved fee may be higher than what the probe was charged: a balance
// the probe could just pay may not cover the approved fee, which is a change
// of state at send time.
func TestSendChecksTheBalanceAgainstTheApprovedFee(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 100_000_000)
	f.ledger.wallet(f.to, rentExempt0)
	req := f.sol("0.01")
	estimate, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	req.Approved = approvalOf(estimate)
	// Exactly the amount plus the signature fee: the price-free probe ends
	// at zero, the approved priority fee does not fit.
	f.ledger.wallet(f.from, 10_000_000+5000)
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); !errors.Is(err, chain.ErrStateChanged) ||
		!strings.Contains(err.Error(), "does not cover") {
		t.Errorf("Send() error = %v, want ErrStateChanged for the balance", err)
	}
}

// The wallet does not decide which rent rules apply: they changed with
// SIMD-0392 and differ by cluster. Under both regimes its answer is the
// runtime's verdict on the final transaction, worded with real numbers.
func TestTheWalletDefersToTheRuntimesRentRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(*fixture) chain.TransferRequest
		max      bool
		pre, now string // "" accepted; otherwise a phrase of the refusal
	}{
		{
			name: "a recipient below the minimum credited a little",
			setup: func(f *fixture) chain.TransferRequest {
				f.ledger.wallet(f.from, 1_000_000_000)
				f.ledger.wallet(f.to, 1000)
				return f.sol("0.000001")
			},
			pre: "the recipient holds 0.000001 SOL",
		},
		{
			name: "a payer below the minimum sends",
			setup: func(f *fixture) chain.TransferRequest {
				f.ledger.wallet(f.from, 500_000)
				f.ledger.wallet(f.to, rentExempt0)
				return f.sol("0.0001")
			},
			now: "checks the fee before anything is sent",
		},
		{
			name: "a new recipient below the minimum",
			setup: func(f *fixture) chain.TransferRequest {
				f.ledger.wallet(f.from, 1_000_000_000)
				return f.sol("0.000001")
			},
			pre: "opening it takes at least 0.00065024 SOL", now: "opening it takes at least 0.00065024 SOL",
		},
		{
			name: "a payer left between zero and the minimum",
			setup: func(f *fixture) chain.TransferRequest {
				f.ledger.wallet(f.from, 1_000_000_000)
				f.ledger.wallet(f.to, rentExempt0)
				return f.sol(lamportsText(1_000_000_000 - testFee - 1000))
			},
			pre: "send the maximum, or at most", now: "send the maximum, or at most",
		},
		{
			name: "a token transfer leaving the payer with dust",
			setup: func(f *fixture) chain.TransferRequest {
				f.ledger.units = 30_000
				f.ledger.wallet(f.from, 5000+rentExemptToken+1388+1000)
				f.ledger.mint(usdcMint, 6)
				f.ledger.tokens(f.from, usdcMint, 9_000_000)
				return f.usdc("1")
			},
			pre: "add at least 0.00064924 SOL", now: "add at least 0.00064924 SOL",
		},
		{
			name: "a token transfer to an open account leaving the payer with dust",
			setup: func(f *fixture) chain.TransferRequest {
				f.ledger.units = 30_000
				f.ledger.wallet(f.from, 5000+1388+1000)
				f.ledger.mint(usdcMint, 6)
				f.ledger.tokens(f.from, usdcMint, 9_000_000)
				f.ledger.tokens(f.to, usdcMint, 0)
				return f.usdc("1")
			},
			// Before SIMD-0392 a payer already below the minimum may pay; after
			// it the fee alone is refused.
			now: "checks the fee before anything is sent",
		},
		{
			name: "the maximum of a balance that only covers the fee",
			setup: func(f *fixture) chain.TransferRequest {
				f.ledger.wallet(f.from, testFee)
				f.ledger.wallet(f.to, rentExempt0)
				return f.sol("max")
			},
			max: true,
			pre: "only covers the fee", now: "checks the fee before anything is sent",
		},
	}
	for _, tt := range tests {
		for _, relax := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/relax=%v", tt.name, relax), func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				f.ledger.relax = relax
				req := tt.setup(f)
				want := tt.pre
				if relax {
					want = tt.now
				}
				var err error
				if tt.max {
					_, err = f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", req)
				} else {
					_, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
				}
				f.ledger.mu.Lock()
				verdicts := slices.Clone(f.ledger.verdicts)
				f.ledger.mu.Unlock()
				// The wallet refuses without asking the runtime only for what no
				// rent rule could allow; otherwise its answer is the runtime's.
				runtimeAccepts := len(verdicts) > 0 && verdicts[len(verdicts)-1] == nil
				if (err == nil) != runtimeAccepts && !(err != nil && strings.Contains(err.Error(), "only covers the fee")) {
					t.Fatalf("wallet error = %v, runtime verdicts = %v: the wallet must answer as the runtime did", err, verdicts)
				}
				if want == "" {
					if err != nil {
						t.Errorf("error = %v, want it accepted", err)
					}
					return
				}
				if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to mention %q", err, want)
				}
			})
		}
	}
}

// advised reads the SOL a refusal says to add.
func advised(t *testing.T, err error) uint64 {
	t.Helper()
	text := err.Error()
	for _, marker := range []string{"add at least ", "until at least "} {
		if _, rest, ok := strings.Cut(text, marker); ok {
			amount, _, _ := strings.Cut(rest, " SOL")
			return lamportsOf(t, amount)
		}
	}
	t.Fatalf("the refusal %q gives no amount to add", err)
	return 0
}

// Advice that works: a transfer refused for lack of SOL is accepted after
// adding exactly the advised amount once, under either rent regime.
func TestFollowingTheAdviceOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*fixture) chain.TransferRequest
	}{
		{name: "SOL from a payer below the minimum", setup: func(f *fixture) chain.TransferRequest {
			f.ledger.wallet(f.from, 500_000)
			f.ledger.wallet(f.to, rentExempt0)
			return f.sol("0.0001")
		}},
		{name: "a token account opened by a payer below the minimum", setup: func(f *fixture) chain.TransferRequest {
			f.ledger.units = 30_000
			f.ledger.wallet(f.from, 100_000)
			f.ledger.mint(usdcMint, 6)
			f.ledger.tokens(f.from, usdcMint, 9_000_000)
			return f.usdc("1")
		}},
		{name: "a token account opened by a payer just at the minimum", setup: func(f *fixture) chain.TransferRequest {
			f.ledger.units = 30_000
			f.ledger.wallet(f.from, rentExempt0+5000)
			f.ledger.mint(usdcMint, 6)
			f.ledger.tokens(f.from, usdcMint, 9_000_000)
			return f.usdc("1")
		}},
	}
	for _, tt := range tests {
		for _, relax := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/relax=%v", tt.name, relax), func(t *testing.T) {
				t.Parallel()
				f := newFixture(t)
				f.ledger.relax = relax
				req := tt.setup(f)
				if _, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", req); err != nil {
					add := advised(t, err)
					f.ledger.mu.Lock()
					account := f.ledger.accounts[f.from]
					account.lamports += add
					f.ledger.accounts[f.from] = account
					f.ledger.mu.Unlock()
					t.Logf("refused (%v); adding %s SOL", err, lamportsText(add))
				}
				if _, err := f.estimateAndSend(t, req); err != nil {
					t.Fatalf("after following the advice: %v", err)
				}
			})
		}
	}
}

// The dust refusal offers "at most" only when some amount would go through;
// otherwise it says to send the maximum, and the maximum is accepted.
func TestTheDustAdviceOnlyOffersWhatWorks(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.to, rentExempt0)
	f.ledger.wallet(f.from, rentExempt0+20_000)
	_, err := f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.000015"))
	if err == nil || !strings.Contains(err.Error(), "send the maximum, or at most 0.000014942 SOL") {
		t.Errorf("EstimateTransfer() error = %v, want the largest amount that leaves the minimum", err)
	}
	if _, err := f.estimateAndSend(t, f.sol("0.000014942")); err != nil {
		t.Errorf("sending the advised amount: %v", err)
	}

	f = newFixture(t)
	f.ledger.wallet(f.to, rentExempt0)
	f.ledger.wallet(f.from, rentExempt0+testFee)
	_, err = f.adapter.EstimateTransfer(t.Context(), "solana-mainnet", f.sol("0.000001"))
	if err == nil || !strings.Contains(err.Error(), "send the maximum") || strings.Contains(err.Error(), "at most") {
		t.Errorf("EstimateTransfer() error = %v, want only the maximum offered", err)
	}
	estimate, err := f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", f.sol("max"))
	if err != nil {
		t.Fatalf("EstimateMaxTransfer() error = %v", err)
	}
	req := f.sol(estimate.Amount)
	req.Approved = approvalOf(estimate)
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); err != nil {
		t.Errorf("Send(max) error = %v", err)
	}
}

// For the maximum the advice does not count an amount — the maximum sends
// whatever there is — only the fee's upper bound and the minimum.
func TestTheAdviceForTheMaximum(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.ledger.wallet(f.from, 500_000)
	f.ledger.wallet(f.to, rentExempt0)
	_, err := f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", f.sol("max"))
	if err == nil {
		t.Fatal("EstimateMaxTransfer() accepted a fee the network refuses")
	}
	// 5000 + ceil(200,000 × 37,500 / 1e6) = 12,500, plus 650,240, less 500,000.
	if got := advised(t, err); got != 162_740 {
		t.Errorf("advised %d lamports, want 162,740", got)
	}
	f.ledger.wallet(f.from, 500_000+162_740)
	estimate, err := f.adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet", f.sol("max"))
	if err != nil {
		t.Fatalf("EstimateMaxTransfer() after following the advice: %v", err)
	}
	req := f.sol(estimate.Amount)
	req.Approved = approvalOf(estimate)
	if _, err := f.adapter.Send(t.Context(), "solana-mainnet", req, f.signer); err != nil {
		t.Errorf("Send(max) error = %v", err)
	}
}
