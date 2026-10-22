package solana_test

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
)

const (
	wrappedSOL = "So11111111111111111111111111111111111111112"
	usdtTest   = usdtMint
)

// tokenAccountWith is a token account with a close authority and, for
// wrapped SOL, the native flag.
func tokenAccountWith(mint, owner string, amount uint64, state byte, closeAuthority string) []byte {
	data := tokenAccountData(mint, owner, amount, state)
	if closeAuthority != "" {
		binary.LittleEndian.PutUint32(data[129:], 1)
		copy(data[133:165], solana.MustPublicKeyFromBase58(closeAuthority).Bytes())
	}
	if mint == wrappedSOL {
		binary.LittleEndian.PutUint32(data[109:], 1)
		binary.LittleEndian.PutUint64(data[113:], rentExemptToken)
	}
	return data
}

// closeFixture is a wallet with token accounts of every kind.
type closeFixture struct {
	*fixture
	empty, emptyOwnAuthority, wrapped         string
	full, frozen, foreignAuthority, token2022 string
	otherOwner                                string
}

func newCloseFixture(t *testing.T) *closeFixture {
	t.Helper()
	f := &closeFixture{fixture: newFixture(t)}
	f.ledger.wallet(f.from, 100_000_000)
	f.empty, f.emptyOwnAuthority, f.wrapped = ata(f.from, usdcMint), key(81), ata(f.from, wrappedSOL)
	f.full, f.frozen, f.foreignAuthority, f.token2022, f.otherOwner = key(82), key(83), key(84), key(85), key(86)
	set := func(address, owner string, data []byte) {
		f.ledger.set(address, ledgerAccount{owner: owner, lamports: rentExemptToken, data: data})
	}
	set(f.empty, tokenProgram, tokenAccountWith(usdcMint, f.from, 0, 1, ""))
	set(f.emptyOwnAuthority, tokenProgram, tokenAccountWith(usdtTest, f.from, 0, 1, f.from))
	set(f.wrapped, tokenProgram, tokenAccountWith(wrappedSOL, f.from, 0, 1, ""))
	set(f.full, tokenProgram, tokenAccountWith(usdcMint, f.from, 5, 1, ""))
	set(f.frozen, tokenProgram, tokenAccountWith(usdcMint, f.from, 0, 2, ""))
	set(f.foreignAuthority, tokenProgram, tokenAccountWith(usdcMint, f.from, 0, 1, key(99)))
	set(f.token2022, token2022, append(tokenAccountWith(usdcMint, f.from, 0, 1, ""), make([]byte, 5)...))
	set(f.otherOwner, tokenProgram, tokenAccountWith(usdcMint, key(98), 0, 1, ""))
	return f
}

func (f *closeFixture) request(accounts ...string) solanachain.CloseRequest {
	return solanachain.CloseRequest{Owner: f.from, Accounts: accounts}
}

func closeApproval(estimate solanachain.CloseEstimate) *chain.FeeApproval {
	return &chain.FeeApproval{
		FeeModel: estimate.FeeModel, ComputeUnitLimit: estimate.ComputeUnitLimit,
		ComputeUnitPriceMicroLamports: estimate.ComputeUnitPriceMicroLamports, Fee: estimate.Fee,
	}
}

// Only empty accounts the wallet can close are listed: no other close
// authority, classic token program, and not frozen (the wallet's own rule;
// the program would close it); wrapped SOL with nothing wrapped is one of
// them.
func TestEmptyTokenAccountsAreTheClosableOnes(t *testing.T) {
	t.Parallel()

	f := newCloseFixture(t)
	listed, err := f.adapter.EmptyTokenAccounts(t.Context(), "solana-mainnet", f.from)
	if err != nil {
		t.Fatalf("EmptyTokenAccounts() error = %v", err)
	}
	var got []string
	for _, account := range listed {
		got = append(got, account.Address)
		if account.Rent != lamportsText(rentExemptToken) {
			t.Errorf("%s rent = %s, want the account's lamports", account.Address, account.Rent)
		}
	}
	want := []string{f.empty, f.emptyOwnAuthority, f.wrapped}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("listed %v, want %v", got, want)
	}
	for _, account := range listed {
		if account.Address == f.wrapped && account.Mint != wrappedSOL {
			t.Errorf("wrapped SOL account mint = %s", account.Mint)
		}
	}
	if _, err := f.adapter.EmptyTokenAccounts(t.Context(), "solana-mainnet", "0xabc"); !errors.Is(err, chain.ErrInvalidRequest) {
		t.Errorf("EmptyTokenAccounts(invalid owner) error = %v", err)
	}
}

func TestCloseEmptyAccounts(t *testing.T) {
	t.Parallel()

	f := newCloseFixture(t)
	req := f.request(f.empty, f.wrapped)
	estimate, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatalf("EstimateClose() error = %v", err)
	}
	if estimate.Returned != lamportsText(2*rentExemptToken) || len(estimate.Accounts) != 2 || estimate.FeeModel != "solana" {
		t.Errorf("estimate = %+v, want both rents returned", estimate)
	}
	var recorded uint64
	req.Approved = closeApproval(estimate)
	req.BeforeSigning = func(height uint64) error {
		if f.signer.signs.Load() != 0 {
			t.Error("the expiry was handed over after the signature")
		}
		recorded = height
		return nil
	}
	got, err := f.adapter.Close(t.Context(), "solana-mainnet", req, f.signer)
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if recorded != testLastValid {
		t.Errorf("expiry handed over = %d, want %d", recorded, testLastValid)
	}
	sent := f.ledger.sentTransactions()
	if len(sent) != 1 {
		t.Fatalf("broadcasts = %d, want 1", len(sent))
	}
	tx, instructions := decodeSent(t, sent[0], f.signer)
	if got.Hash != tx.Signatures[0].String() || got.Status != "pending" {
		t.Errorf("Close() = %+v", got)
	}
	want := computeBudget(uint32(estimate.ComputeUnitLimit), estimate.ComputeUnitPriceMicroLamports)
	for _, account := range []string{f.empty, f.wrapped} {
		want = append(want, sentInstruction{program: tokenProgram, accounts: []string{account, f.from, f.from}, data: []byte{9}})
	}
	if !equalInstructions(instructions, want) {
		t.Errorf("instructions = %+v, want %+v", instructions, want)
	}
	// The rent goes back to the owner: the runtime's run of the final
	// message moved it there.
	f.ledger.mu.Lock()
	defer f.ledger.mu.Unlock()
	if last := f.ledger.verdicts[len(f.ledger.verdicts)-1]; last != nil {
		t.Errorf("runtime verdict = %v", last)
	}
}

// Every account named is read again when closing; what cannot be closed now
// is refused, and nothing is signed.
func TestCloseRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		accounts func(*closeFixture) []string
		change   func(*closeFixture)
		wantIs   error
		wantErr  string
	}{
		{name: "received tokens since listed", accounts: func(f *closeFixture) []string { return []string{f.empty} }, change: func(f *closeFixture) {
			f.ledger.set(f.empty, ledgerAccount{owner: tokenProgram, lamports: rentExemptToken, data: tokenAccountWith(usdcMint, f.from, 1, 1, "")})
		}, wantIs: chain.ErrStateChanged, wantErr: "holds tokens"},
		{name: "frozen since listed", accounts: func(f *closeFixture) []string { return []string{f.empty} }, change: func(f *closeFixture) {
			f.ledger.set(f.empty, ledgerAccount{owner: tokenProgram, lamports: rentExemptToken, data: tokenAccountWith(usdcMint, f.from, 0, 2, "")})
		}, wantIs: chain.ErrStateChanged, wantErr: "frozen"},
		{name: "closed since listed", accounts: func(f *closeFixture) []string { return []string{f.empty} }, change: func(f *closeFixture) {
			f.ledger.mu.Lock()
			delete(f.ledger.accounts, f.empty)
			f.ledger.mu.Unlock()
		}, wantIs: chain.ErrStateChanged, wantErr: "no longer exists"},
		{name: "not empty", accounts: func(f *closeFixture) []string { return []string{f.full} }, wantIs: chain.ErrStateChanged, wantErr: "holds tokens"},
		{name: "frozen", accounts: func(f *closeFixture) []string { return []string{f.frozen} }, wantIs: chain.ErrStateChanged, wantErr: "frozen"},
		{name: "another close authority", accounts: func(f *closeFixture) []string { return []string{f.foreignAuthority} }, wantIs: chain.ErrInvalidRequest, wantErr: "close authority"},
		{name: "another owner", accounts: func(f *closeFixture) []string { return []string{f.otherOwner} }, wantIs: chain.ErrInvalidRequest, wantErr: "another wallet"},
		{name: "Token-2022", accounts: func(f *closeFixture) []string { return []string{f.token2022} }, wantIs: solanachain.ErrToken2022},
		{name: "not a token account", accounts: func(f *closeFixture) []string { return []string{f.to} }, change: func(f *closeFixture) {
			f.ledger.wallet(f.to, 1)
		}, wantIs: chain.ErrInvalidRequest, wantErr: "not a token account"},
		{name: "one bad among good", accounts: func(f *closeFixture) []string { return []string{f.empty, f.frozen} }, wantIs: chain.ErrStateChanged, wantErr: f2(83)},
		{name: "listed twice", accounts: func(f *closeFixture) []string { return []string{f.empty, f.empty} }, wantIs: chain.ErrInvalidRequest, wantErr: "twice"},
		{name: "the wallet itself", accounts: func(f *closeFixture) []string { return []string{f.from} }, wantIs: chain.ErrInvalidRequest, wantErr: "the wallet itself"},
		{name: "none", accounts: func(*closeFixture) []string { return nil }, wantIs: chain.ErrInvalidRequest, wantErr: "between 1 and 20"},
		{name: "invalid address", accounts: func(*closeFixture) []string { return []string{"0xabc"} }, wantIs: chain.ErrInvalidRequest, wantErr: "invalid token account"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCloseFixture(t)
			req := f.request(tt.accounts(f)...)
			req.Approved = &chain.FeeApproval{ComputeUnitLimit: 10_000, ComputeUnitPriceMicroLamports: 37_500, Fee: lamportsText(5000 + 375)}
			if tt.change != nil {
				tt.change(f)
			}
			_, err := f.adapter.Close(t.Context(), "solana-mainnet", req, f.signer)
			if !errors.Is(err, tt.wantIs) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Close() error = %v, want %v mentioning %q", err, tt.wantIs, tt.wantErr)
			}
			if f.signer.signs.Load() != 0 || len(f.ledger.sentTransactions()) != 0 {
				t.Error("something was signed or sent")
			}
		})
	}
}

func f2(seed byte) string { return key(seed) }

// One transaction closes at most MaxCloseAccounts; the legacy message with
// that many stays under 1232 bytes with its signature.
func TestCloseBatchSize(t *testing.T) {
	t.Parallel()

	f := newCloseFixture(t)
	var accounts []string
	for i := range solanachain.MaxCloseAccounts + 1 {
		address := key(byte(120 + i))
		f.ledger.set(address, ledgerAccount{owner: tokenProgram, lamports: rentExemptToken, data: tokenAccountWith(usdcMint, f.from, 0, 1, "")})
		accounts = append(accounts, address)
	}
	if _, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", f.request(accounts...)); !errors.Is(err, chain.ErrInvalidRequest) {
		t.Errorf("EstimateClose(%d) error = %v, want a refusal", len(accounts), err)
	}
	req := f.request(accounts[:solanachain.MaxCloseAccounts]...)
	estimate, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatalf("EstimateClose(%d) error = %v", solanachain.MaxCloseAccounts, err)
	}
	req.Approved = closeApproval(estimate)
	if _, err := f.adapter.Close(t.Context(), "solana-mainnet", req, f.signer); err != nil {
		t.Fatalf("Close(%d) error = %v", solanachain.MaxCloseAccounts, err)
	}
	if size := len(f.ledger.sentTransactions()[0]); size > 1232 {
		t.Errorf("transaction size = %d bytes, above 1232", size)
	}
}

func TestCloseNeverSignsAnotherDestination(t *testing.T) {
	t.Parallel()

	f := newCloseFixture(t)
	req := f.request(f.empty)
	estimate, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	req.Approved = closeApproval(estimate)
	solanachain.TamperWithCloseBuilder(f.adapter, key(99))
	if _, err := f.adapter.Close(t.Context(), "solana-mainnet", req, f.signer); !errors.Is(err, solanachain.ErrIntentMismatch) {
		t.Errorf("Close() error = %v, want ErrIntentMismatch", err)
	}
	if f.signer.signs.Load() != 0 || len(f.ledger.sentTransactions()) != 0 {
		t.Error("a close to another destination was signed or sent")
	}
}

func TestCloseFeeApproval(t *testing.T) {
	t.Parallel()

	f := newCloseFixture(t)
	req := f.request(f.empty)
	estimate, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		change  func(*chain.FeeApproval)
		before  func()
		wantIs  error
		wantErr string
	}{
		"no approval": {change: func(a *chain.FeeApproval) { *a = chain.FeeApproval{} }, wantIs: chain.ErrInvalidRequest},
		"an account opened": {change: func(a *chain.FeeApproval) {
			// Self-consistent, so only the close's own rule refuses it.
			a.CreatesTokenAccount, a.TokenAccountRent = true, "0.1"
			a.Fee = lamportsText(lamportsOf(t, a.Fee) + 100_000_000)
		}, wantIs: chain.ErrInvalidRequest, wantErr: "opens none"},
		"more compute": {before: func() { f.ledger.units = 50_000 }, wantIs: chain.ErrFeeChanged, wantErr: "compute units"},
		"higher price": {before: func() { f.ledger.fees = []uint64{90_000} }, wantIs: chain.ErrFeeChanged, wantErr: "priority price"},
	} {
		approved := closeApproval(estimate)
		if tt.change != nil {
			tt.change(approved)
		}
		f.ledger.units, f.ledger.fees = 450, testFees
		if tt.before != nil {
			tt.before()
		}
		req.Approved = approved
		if _, err := f.adapter.Close(t.Context(), "solana-mainnet", req, f.signer); !errors.Is(err, tt.wantIs) || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: Close() error = %v, want %v", name, err, tt.wantIs)
		}
	}
	if _, err := f.adapter.Close(t.Context(), "solana-mainnet", req, digestOnly{}); !errors.Is(err, chain.ErrSignerMismatch) {
		t.Errorf("Close(digest signer) error = %v", err)
	}
	if f.signer.signs.Load() != 0 {
		t.Error("something was signed")
	}
}

// The rent comes back only once the accounts are closed: a wallet with no
// SOL for the fee is told how much to add, and that is enough.
func TestCloseWithoutSOLForTheFee(t *testing.T) {
	t.Parallel()

	for _, balance := range []uint64{0, 3000} {
		f := newCloseFixture(t)
		f.ledger.wallet(f.from, balance)
		if balance == 0 {
			f.ledger.mu.Lock()
			delete(f.ledger.accounts, f.from)
			f.ledger.mu.Unlock()
		}
		req := f.request(f.empty)
		_, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
		if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "before the rent comes back: add at least") {
			t.Fatalf("balance %d: EstimateClose() error = %v", balance, err)
		}
		f.ledger.wallet(f.from, balance+advised(t, err))
		if _, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req); err != nil {
			t.Errorf("balance %d: EstimateClose() after following the advice: %v", balance, err)
		}
	}
}

// When the runtime refuses the final close for another reason, its reason
// is reported and nothing is signed.
func TestCloseFinalSimulationDecides(t *testing.T) {
	t.Parallel()

	f := newCloseFixture(t)
	req := f.request(f.empty)
	estimate, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
	if err != nil {
		t.Fatal(err)
	}
	f.ledger.finalErr = map[string]any{"InstructionError": []any{2, map[string]any{"Custom": 11}}}
	f.ledger.simLogs = []string{"Program log: Error: Non-native account can only be closed if its balance is zero"}
	if _, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req); !errors.Is(err, chain.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), "balance is zero") {
		t.Errorf("EstimateClose() error = %v", err)
	}
	req.Approved = closeApproval(estimate)
	if _, err := f.adapter.Close(t.Context(), "solana-mainnet", req, f.signer); !errors.Is(err, chain.ErrStateChanged) {
		t.Errorf("Close() error = %v, want ErrStateChanged", err)
	}
	if f.signer.signs.Load() != 0 {
		t.Error("signed despite the runtime's refusal")
	}
}

// Every read a close depends on can fail or answer nonsense; each stops it
// before anything is signed.
func TestCloseStopsOnANodeFailure(t *testing.T) {
	t.Parallel()

	failing := reply{err: &rpcError{Code: -32005, Message: "Node is unhealthy"}}
	tests := []struct {
		name    string
		method  string
		answer  reply
		step    string // list, estimate or close
		wantErr string
	}{
		{name: "listing", method: "getTokenAccountsByOwner", answer: failing, step: "list", wantErr: "list token accounts"},
		{name: "listing without a result", method: "getTokenAccountsByOwner", answer: reply{result: nil}, step: "list", wantErr: "no result"},
		{name: "listing an empty entry", method: "getTokenAccountsByOwner", answer: reply{result: withContext([]any{nil})}, step: "list", wantErr: "empty token account"},
		{name: "accounts", method: "getMultipleAccounts", answer: failing, step: "estimate", wantErr: "read accounts"},
		{name: "rent", method: "getMinimumBalanceForRentExemption", answer: failing, step: "estimate", wantErr: "rent-exempt minimum"},
		{name: "priority fees", method: "getRecentPrioritizationFees", answer: failing, step: "estimate", wantErr: "priority fees"},
		{name: "simulation", method: "simulateTransaction", answer: failing, step: "estimate", wantErr: "simulate"},
		{name: "simulation without compute", method: "simulateTransaction", answer: reply{result: withContext(map[string]any{"err": nil})}, step: "estimate", wantErr: "compute used"},
		{name: "blockhash", method: "getLatestBlockhash", answer: failing, step: "close", wantErr: "recent blockhash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCloseFixture(t)
			req := f.request(f.empty)
			var err error
			switch tt.step {
			case "list":
				f.node.handle(tt.method, func([]json.RawMessage) reply { return tt.answer })
				_, err = f.adapter.EmptyTokenAccounts(t.Context(), "solana-mainnet", f.from)
			case "estimate":
				f.node.handle(tt.method, func([]json.RawMessage) reply { return tt.answer })
				_, err = f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
			case "close":
				estimate, estimateErr := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
				if estimateErr != nil {
					t.Fatal(estimateErr)
				}
				req.Approved = closeApproval(estimate)
				f.node.handle(tt.method, func([]json.RawMessage) reply { return tt.answer })
				_, err = f.adapter.Close(t.Context(), "solana-mainnet", req, f.signer)
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

func TestCloseRefusesUnusableRequests(t *testing.T) {
	t.Parallel()

	f := newCloseFixture(t)
	uninitialized := key(87)
	f.ledger.set(uninitialized, ledgerAccount{owner: tokenProgram, lamports: 1, data: tokenAccountWith(usdcMint, f.from, 0, 0, "")})
	approved := &chain.FeeApproval{ComputeUnitLimit: 10_000, ComputeUnitPriceMicroLamports: 37_500, Fee: lamportsText(5000 + 375)}
	cases := map[string]func() error{
		"an uninitialized account": func() error {
			_, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", f.request(uninitialized))
			return err
		},
		"an invalid owner": func() error {
			_, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", solanachain.CloseRequest{Owner: "0xabc", Accounts: []string{f.empty}})
			return err
		},
		"an unknown cluster (list)": func() error {
			_, err := f.adapter.EmptyTokenAccounts(t.Context(), "solana-odd", f.from)
			return err
		},
		"an unknown cluster (estimate)": func() error {
			_, err := f.adapter.EstimateClose(t.Context(), "solana-odd", f.request(f.empty))
			return err
		},
		"an unknown cluster (close)": func() error {
			req := f.request(f.empty)
			req.Approved = approved
			_, err := f.adapter.Close(t.Context(), "solana-odd", req, f.signer)
			return err
		},
	}
	for name, run := range cases {
		if err := run(); !errors.Is(err, chain.ErrInvalidRequest) {
			t.Errorf("%s: error = %v, want ErrInvalidRequest", name, err)
		}
	}
}

// A rent-exempt wallet whose fee would drop it below the minimum is told
// how much to add; the runtime's other refusals of the final close are
// reported as its own.
func TestCloseFeeFirstAdvice(t *testing.T) {
	t.Parallel()

	f := newCloseFixture(t)
	f.ledger.wallet(f.from, rentExempt0+1000)
	req := f.request(f.empty)
	_, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req)
	if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "checks the fee before the rent comes back") {
		t.Fatalf("EstimateClose() error = %v", err)
	}
	f.ledger.wallet(f.from, rentExempt0+1000+advised(t, err))
	if _, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req); err != nil {
		t.Errorf("EstimateClose() after following the advice: %v", err)
	}
	// A failure of the final simulation's own request.
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
	if _, err := f.adapter.EstimateClose(t.Context(), "solana-mainnet", req); err == nil || !strings.Contains(err.Error(), "simulate") {
		t.Errorf("EstimateClose(final simulation fails) error = %v", err)
	}
}
