package solana_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
)

const (
	rentExempt0     = 650_240
	rentExemptToken = 1_488_440
	testBlockhash   = "9zrUHnA1nCByPksy3aL8tQ47vqdaG2vnFs4HrxgcZj4F"
	testLastValid   = 300_000_123
)

// testSigner is a Solana key held by the test.
type testSigner struct {
	key     ed25519.PrivateKey
	family  chain.Family
	signs   atomic.Int32
	corrupt bool
	onSign  func()
}

func newTestSigner(seed byte) *testSigner {
	raw := make([]byte, ed25519.SeedSize)
	for i := range raw {
		raw[i] = seed
	}
	return &testSigner{key: ed25519.NewKeyFromSeed(raw), family: chain.FamilySolana}
}

func (s *testSigner) Family() chain.Family { return s.family }

func (s *testSigner) PublicKey() []byte {
	return append([]byte(nil), s.key.Public().(ed25519.PublicKey)...)
}

func (s *testSigner) address() string { return solana.PublicKeyFromBytes(s.PublicKey()).String() }

func (s *testSigner) SignMessage(_ context.Context, message []byte) ([]byte, error) {
	s.signs.Add(1)
	if s.onSign != nil {
		s.onSign()
	}
	signature := ed25519.Sign(s.key, message)
	if s.corrupt {
		signature[0] ^= 1
	}
	return signature, nil
}

// digestOnly is a secp256k1-style signer.
type digestOnly struct{}

func (digestOnly) Family() chain.Family { return chain.FamilySolana }
func (digestOnly) PublicKey() []byte    { return make([]byte, 65) }
func (digestOnly) SignDigest(context.Context, []byte) ([]byte, error) {
	return make([]byte, 65), nil
}

type ledgerAccount struct {
	owner    string
	lamports uint64
	data     []byte
}

// ledger is a fake cluster: accounts, recent priority prices, what a
// simulation reports, and every transaction sent to it.
type ledger struct {
	t *testing.T

	mu       sync.Mutex
	accounts map[string]ledgerAccount
	fees     []uint64
	units    uint64
	simErr   any
	// finalErr is what the simulation of a final transaction (not the
	// compute probe) answers, as if the chain moved between the two.
	finalErr  any
	simLogs   []string
	sendReply *reply
	// relax models SIMD-0392, active on mainnet and devnet.
	relax bool
	// verdicts is the runtime's answer on every simulation, in order.
	verdicts  []any
	simulated [][]byte
	sent      [][]byte
	// events records the order of what reached the node.
	events []string
}

func newLedger(t *testing.T) *ledger {
	return &ledger{t: t, accounts: make(map[string]ledgerAccount), units: 450, relax: true}
}

func (l *ledger) set(address string, account ledgerAccount) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accounts[address] = account
}

func (l *ledger) wallet(address string, lamports uint64) {
	l.set(address, ledgerAccount{owner: systemProgram, lamports: lamports})
}

func (l *ledger) event(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, name)
}

func (l *ledger) sentTransactions() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][]byte(nil), l.sent...)
}

// tokenAccountData is a classic SPL token account.
func tokenAccountData(mint, owner string, amount uint64, state byte) []byte {
	data := make([]byte, 165)
	copy(data[0:32], solana.MustPublicKeyFromBase58(mint).Bytes())
	copy(data[32:64], solana.MustPublicKeyFromBase58(owner).Bytes())
	binary.LittleEndian.PutUint64(data[64:72], amount)
	data[108] = state
	return data
}

func ata(owner, mint string) string {
	address, _, err := solana.FindAssociatedTokenAddress(solana.MustPublicKeyFromBase58(owner), solana.MustPublicKeyFromBase58(mint))
	if err != nil {
		panic(err)
	}
	return address.String()
}

// tokens gives owner an associated token account of mint.
func (l *ledger) tokens(owner, mint string, amount uint64) {
	l.set(ata(owner, mint), ledgerAccount{owner: tokenProgram, lamports: rentExemptToken, data: tokenAccountData(mint, owner, amount, 1)})
}

func (l *ledger) mint(mint string, decimals uint8) {
	l.set(mint, ledgerAccount{owner: tokenProgram, lamports: 1, data: mintData(decimals, true)})
}

func ledgerNode(t *testing.T, l *ledger) *node {
	t.Helper()
	n := newNode(t, mainnetGenesis)
	n.handle("getMultipleAccounts", func(params []json.RawMessage) reply {
		l.mu.Lock()
		defer l.mu.Unlock()
		var value []any
		for _, address := range requestedKeys(t, params) {
			if found, ok := l.accounts[address]; ok {
				value = append(value, account(found.owner, found.lamports, found.data))
			} else {
				value = append(value, nil)
			}
		}
		return reply{result: withContext(value)}
	})
	n.handle("getTokenAccountsByOwner", func(params []json.RawMessage) reply {
		l.mu.Lock()
		defer l.mu.Unlock()
		owner := requestedOwner(t, params)
		value := []any{}
		for address, found := range l.accounts {
			if found.owner == tokenProgram && len(found.data) == 165 &&
				solana.PublicKeyFromBytes(found.data[32:64]).String() == owner {
				data := found.data
				// The balance stream asks for the first 72 bytes only.
				if len(params) > 2 && strings.Contains(string(params[2]), "dataSlice") {
					data = data[:72]
				}
				value = append(value, map[string]any{"pubkey": address, "account": account(tokenProgram, found.lamports, data)})
			}
		}
		return reply{result: withContext(value)}
	})
	n.handle("getMinimumBalanceForRentExemption", func(params []json.RawMessage) reply {
		switch string(params[0]) {
		case "0":
			return reply{result: rentExempt0}
		case "165":
			return reply{result: rentExemptToken}
		}
		t.Errorf("rent asked for size %s", params[0])
		return reply{err: &rpcError{Code: -32602, Message: "unexpected size"}}
	})
	n.handle("getRecentPrioritizationFees", func([]json.RawMessage) reply {
		l.mu.Lock()
		defer l.mu.Unlock()
		value := []map[string]any{}
		for i, fee := range l.fees {
			value = append(value, map[string]any{"slot": i + 1, "prioritizationFee": fee})
		}
		return reply{result: value}
	})
	n.handle("simulateTransaction", func(params []json.RawMessage) reply {
		var encoded string
		var opts struct {
			Encoding  string `json:"encoding"`
			SigVerify bool   `json:"sigVerify"`
			Replace   bool   `json:"replaceRecentBlockhash"`
		}
		if len(params) != 2 || json.Unmarshal(params[0], &encoded) != nil || json.Unmarshal(params[1], &opts) != nil ||
			opts.Encoding != "base64" || opts.SigVerify || !opts.Replace {
			t.Errorf("simulateTransaction params = %s", params)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("simulated transaction is not base64: %v", err)
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		l.simulated = append(l.simulated, raw)
		l.events = append(l.events, "simulate")
		failure, units := l.execute(t, raw)
		if l.simErr != nil {
			failure, units = l.simErr, l.units
		}
		if failure == nil && l.finalErr != nil && !probe(raw) {
			failure, units = l.finalErr, l.units
		}
		l.verdicts = append(l.verdicts, failure)
		return reply{result: withContext(map[string]any{"err": failure, "logs": l.simLogs, "unitsConsumed": units})}
	})
	n.handle("getLatestBlockhash", func(params []json.RawMessage) reply {
		if len(params) != 1 || string(params[0]) != `{"commitment":"confirmed"}` {
			t.Errorf("getLatestBlockhash params = %s", params)
		}
		l.event("blockhash")
		return reply{result: withContext(map[string]any{"blockhash": testBlockhash, "lastValidBlockHeight": testLastValid})}
	})
	n.handle("sendTransaction", func(params []json.RawMessage) reply {
		var encoded string
		var opts map[string]any
		if len(params) != 2 || json.Unmarshal(params[0], &encoded) != nil || json.Unmarshal(params[1], &opts) != nil {
			t.Errorf("sendTransaction params = %s", params)
		}
		if opts["encoding"] != "base64" || opts["skipPreflight"] != false || opts["preflightCommitment"] != "confirmed" {
			t.Errorf("sendTransaction options = %v, want base64 with preflight at confirmed", opts)
		}
		if _, set := opts["maxRetries"]; set {
			t.Errorf("sendTransaction sets maxRetries: %v", opts)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("sent transaction is not base64: %v", err)
		}
		l.mu.Lock()
		l.sent = append(l.sent, raw)
		l.events = append(l.events, "send")
		answer := l.sendReply
		l.mu.Unlock()
		if answer != nil {
			return *answer
		}
		return reply{result: solana.SignatureFromBytes(raw[1:65]).String()}
	})
	return n
}

// lamportsText renders lamports as the SOL decimal the API uses.
func lamportsText(lamports uint64) string {
	whole, frac := lamports/1_000_000_000, lamports%1_000_000_000
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	text := strconv.FormatUint(whole, 10) + "." + leftPad(strconv.FormatUint(frac, 10), 9)
	for text[len(text)-1] == '0' {
		text = text[:len(text)-1]
	}
	return text
}

func leftPad(value string, width int) string {
	for len(value) < width {
		value = "0" + value
	}
	return value
}

// rentState is Agave's RentState for an account without data.
type rentState int

const (
	uninitialized rentState = iota // 0 lamports
	rentPaying                     // 0 < lamports < minimum
	rentExempt                     // lamports >= minimum
)

func stateOf(lamports, minimum uint64) rentState {
	switch {
	case lamports == 0:
		return uninitialized
	case lamports >= minimum:
		return rentExempt
	}
	return rentPaying
}

// preState is Agave's get_pre_exec_account_rent_state: with SIMD-0392
// (relax) an account below the minimum counts as rent-exempt.
func preState(lamports, minimum uint64, relax bool) rentState {
	state := stateOf(lamports, minimum)
	if relax && state == rentPaying {
		return rentExempt
	}
	return state
}

// postState is Agave's get_post_exec_account_rent_state: with SIMD-0392 an
// account that was rent-exempt before and did not lose lamports stays
// rent-exempt even below the minimum.
func postState(lamports, minimum uint64, pre rentState, preLamports uint64, relax bool) rentState {
	if !relax {
		return stateOf(lamports, minimum)
	}
	switch {
	case lamports == 0:
		return uninitialized
	case lamports >= minimum:
		return rentExempt
	case pre == rentExempt && lamports >= preLamports:
		return rentExempt
	}
	return rentPaying
}

// transitionAllowed is Agave's transition_allowed: any account may end
// uninitialized or rent-exempt; it may end rent-paying only if it was
// rent-paying and was not credited.
//
// All from svm/src/rent_calculator.rs, anza-xyz/agave c9b06b08.
func transitionAllowed(pre, post rentState, preLamports, postLamports uint64) bool {
	if post != rentPaying {
		return true
	}
	return pre == rentPaying && postLamports <= preLamports
}

// allowed applies Agave's check_static_account_rent_state_transition to one
// account's balance before and after.
func allowed(before, after uint64, relax bool) bool {
	pre := preState(before, rentExempt0, relax)
	return transitionAllowed(pre, postState(after, rentExempt0, pre, before, relax), before, after)
}

// execute is what the runtime does with a simulated transaction, for the
// instructions this wallet builds. It follows Agave, with or without the
// SIMD-0392 rent relaxation (l.relax), which mainnet and devnet have
// activated (feature BY4JhHLahVzS9ynfDz4exzGPbVXhFmJvEyMWsXbDBqME):
//   - validate_fee_payer (svm/src/account_loader.rs): an empty payer is
//     AccountNotFound, one that cannot cover the fee InsufficientFundsForFee,
//     and the fee may not leave it in a rent state it may not move to —
//     InsufficientFundsForRent for account 0. These fail before execution and
//     report no compute.
//   - after execution, every account whose balance changed is checked with
//     transition_allowed against its balance before execution (the payer's
//     after the fee): InsufficientFundsForRent with its index, reported with
//     the compute used.
func (l *ledger) execute(t *testing.T, raw []byte) (failure any, units uint64) {
	t.Helper()
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		t.Errorf("decode simulated transaction: %v", err)
		return "decode", 0
	}
	keys := tx.Message.AccountKeys
	payer := keys[0].String()
	var limit, price uint64
	for _, instruction := range tx.Message.Instructions {
		data := []byte(instruction.Data)
		if keys[instruction.ProgramIDIndex].String() == "ComputeBudget111111111111111111111111111111" {
			switch data[0] {
			case 2:
				limit = uint64(binary.LittleEndian.Uint32(data[1:]))
			case 3:
				price = binary.LittleEndian.Uint64(data[1:])
			}
		}
	}
	fee := 5000 + (limit*price+999_999)/1_000_000
	balance := l.accounts[payer].lamports
	switch {
	case balance == 0:
		return "AccountNotFound", 0
	case balance < fee:
		return "InsufficientFundsForFee", 0
	case !allowed(balance, balance-fee, l.relax):
		return map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": 0}}, 0
	}
	before := map[string]uint64{payer: balance - fee}
	after := map[string]uint64{payer: balance - fee}
	touch := func(address string) {
		if _, seen := before[address]; !seen {
			before[address] = l.accounts[address].lamports
			after[address] = before[address]
		}
	}
	for _, instruction := range tx.Message.Instructions {
		program, data := keys[instruction.ProgramIDIndex].String(), []byte(instruction.Data)
		switch program {
		case systemProgram:
			from, to := keys[instruction.Accounts[0]].String(), keys[instruction.Accounts[1]].String()
			amount := binary.LittleEndian.Uint64(data[4:])
			touch(from)
			touch(to)
			if after[from] < amount {
				return map[string]any{"InstructionError": []any{2, map[string]any{"Custom": 1}}}, l.units
			}
			after[from] -= amount
			after[to] += amount
		case "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL":
			account := keys[instruction.Accounts[1]].String()
			if _, exists := l.accounts[account]; !exists {
				if after[payer] < rentExemptToken {
					return map[string]any{"InstructionError": []any{2, map[string]any{"Custom": 1}}}, l.units
				}
				after[payer] -= rentExemptToken
			}
		case tokenProgram:
			if len(data) != 1 || data[0] != 9 {
				break
			}
			// CloseAccount: the account must hold no tokens and be closed by
			// its owner or close authority; its lamports go to the
			// destination. Like the token program (classic and p-token
			// process_close_account), it does not look at the frozen state.
			account, destination := keys[instruction.Accounts[0]].String(), keys[instruction.Accounts[1]].String()
			authority := keys[instruction.Accounts[2]].String()
			found := l.accounts[account]
			if len(found.data) != 165 {
				return map[string]any{"InstructionError": []any{2, "InvalidAccountData"}}, l.units
			}
			closer := solana.PublicKeyFromBytes(found.data[32:64]).String()
			if binary.LittleEndian.Uint32(found.data[129:]) != 0 {
				closer = solana.PublicKeyFromBytes(found.data[133:165]).String()
			}
			switch {
			case binary.LittleEndian.Uint64(found.data[64:72]) != 0:
				return map[string]any{"InstructionError": []any{2, map[string]any{"Custom": 11}}}, l.units
			case authority != closer:
				return map[string]any{"InstructionError": []any{2, map[string]any{"Custom": 4}}}, l.units
			}
			touch(account)
			touch(destination)
			after[destination] += after[account]
			after[account] = 0
		}
	}
	for index, key := range keys {
		address := key.String()
		pre, touched := before[address]
		if !touched || after[address] == pre {
			continue
		}
		if !allowed(pre, after[address], l.relax) {
			return map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": index}}, l.units
		}
	}
	return nil, l.units
}

// probe reports whether a simulated transaction is the compute probe: it
// asks for the 200,000-unit limit.
func probe(raw []byte) bool {
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		return false
	}
	for _, instruction := range tx.Message.Instructions {
		data := []byte(instruction.Data)
		if tx.Message.AccountKeys[instruction.ProgramIDIndex].String() == "ComputeBudget111111111111111111111111111111" &&
			data[0] == 2 && binary.LittleEndian.Uint32(data[1:]) == 200_000 {
			return true
		}
	}
	return false
}

// The fake runtime follows the Agave rules it models; the transfer tests are
// only as good as it is.
func TestTheFakeRuntimeFollowsAgave(t *testing.T) {
	t.Parallel()

	payer, to := newTestSigner(7).address(), newTestSigner(8).address()
	transfer := func(lamports uint64, price uint64) []byte {
		instructions := []solana.Instruction{
			solana.NewInstruction(solana.ComputeBudget, nil, binary.LittleEndian.AppendUint32([]byte{2}, 1000)),
			solana.NewInstruction(solana.ComputeBudget, nil, binary.LittleEndian.AppendUint64([]byte{3}, price)),
			solana.NewInstruction(solana.SystemProgramID, solana.AccountMetaSlice{
				solana.Meta(solana.MustPublicKeyFromBase58(payer)).WRITE().SIGNER(), solana.Meta(solana.MustPublicKeyFromBase58(to)).WRITE(),
			}, binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), lamports)),
		}
		tx, err := solana.NewTransaction(instructions, solana.Hash{}, solana.TransactionPayer(solana.MustPublicKeyFromBase58(payer)))
		if err != nil {
			t.Fatal(err)
		}
		message, err := tx.Message.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return append(append([]byte{1}, make([]byte, 64)...), message...)
	}
	rent := func(index int) any {
		return map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": index}}
	}
	tests := []struct {
		name      string
		payer, to uint64
		toExists  bool
		amount    uint64
		// want is the verdict before SIMD-0392, relaxed after it.
		want, relaxed any
		wantUnits     bool
	}{
		{name: "ordinary", payer: 1_000_000_000, to: rentExempt0, toExists: true, amount: 1000, wantUnits: true},
		{name: "empty payer", payer: 0, amount: 1, want: "AccountNotFound", relaxed: "AccountNotFound"},
		{name: "fee not covered", payer: 4000, amount: 1, want: "InsufficientFundsForFee", relaxed: "InsufficientFundsForFee"},
		{name: "fee drops an exempt payer below the minimum", payer: rentExempt0 + 3000, to: rentExempt0, toExists: true, amount: rentExempt0 - 2000, want: rent(0), relaxed: rent(0)},
		{name: "sweep to zero", payer: rentExempt0 + 5000, to: rentExempt0, toExists: true, amount: rentExempt0, wantUnits: true},
		{name: "dust left after the transfer", payer: 1_000_000_000, to: rentExempt0, toExists: true, amount: 1_000_000_000 - 5000 - 10, want: rent(0), relaxed: rent(0), wantUnits: true},
		// Before SIMD-0392 a payer below the minimum may go lower; after it
		// the fee alone is refused, before execution.
		{name: "a payer below the minimum sends", payer: 500_000, to: rentExempt0, toExists: true, amount: 100_000, relaxed: rent(0), wantUnits: true},
		{name: "a new recipient below the minimum", payer: 1_000_000_000, amount: 1000, want: rent(1), relaxed: rent(1), wantUnits: true},
		{name: "a new recipient at the minimum", payer: 1_000_000_000, amount: rentExempt0, wantUnits: true},
		// Before SIMD-0392 a recipient below the minimum may not be credited
		// unless it reaches it; after it, it may.
		{name: "a recipient below the minimum credited a little", payer: 1_000_000_000, to: 1000, toExists: true, amount: 1000, want: rent(1), wantUnits: true},
		{name: "a recipient below the minimum credited to it", payer: 1_000_000_000, to: 1000, toExists: true, amount: rentExempt0 - 1000, wantUnits: true},
	}
	for _, tt := range tests {
		for _, relax := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/relax=%v", tt.name, relax), func(t *testing.T) {
				t.Parallel()
				l := newLedger(t)
				l.relax = relax
				if tt.payer > 0 {
					l.wallet(payer, tt.payer)
				}
				if tt.toExists {
					l.wallet(to, tt.to)
				}
				want := tt.want
				if relax {
					want = tt.relaxed
				}
				failure, units := l.execute(t, transfer(tt.amount, 0))
				if fmt.Sprint(failure) != fmt.Sprint(want) {
					t.Errorf("failure = %v, want %v", failure, want)
				}
				// Refusals before execution report no compute.
				wantUnits := tt.wantUnits
				if relax && tt.name == "a payer below the minimum sends" {
					wantUnits = false
				}
				if (units > 0) != wantUnits {
					t.Errorf("units = %d, want compute reported = %v", units, wantUnits)
				}
			})
		}
	}
}
