package solana

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/operation"
)

const (
	// feeModel names the Solana fee in an estimate and its approval.
	feeModel = "solana"
	// lamportsPerSignature is the base fee per signature, fixed by the fee
	// rate governor since genesis. Every transfer here has one signature.
	lamportsPerSignature = 5000
	// maxComputeUnitLimit is the most compute a transaction may ask for.
	maxComputeUnitLimit = 1_400_000
	// simulationComputeUnitLimit is what the simulation asks for; it only has
	// to be enough for the simulation to finish and report its use.
	simulationComputeUnitLimit = 200_000
	// maxComputeUnitPrice caps the priority price at 1 lamport per compute
	// unit: about 0.00004 SOL for a token transfer that opens an account. A
	// node cannot make the wallet pay more for priority than this, whatever
	// it reports, and an approval above it is refused.
	maxComputeUnitPrice = 1_000_000
	// minComputeUnitPrice is the least priority price an estimate offers,
	// 1,000 micro-lamports per unit — about 0.000002 SOL for a token
	// transfer — so a transfer is not left without priority when recent
	// prices for its accounts are zero.
	minComputeUnitPrice = 1_000
	// tokenAccountSize is the length of a classic SPL token account.
	tokenAccountSize = 165
	// tokenStateOffset is where a token account stores its state:
	// 1 initialized, 2 frozen.
	tokenStateOffset = 108
	// rentTTL is how long a rent-exempt minimum is reused. It changes only
	// with a feature activation.
	rentTTL = 10 * time.Minute
)

// errStateDependent marks a refusal that follows from what the chain holds
// now — balances, accounts — rather than from the request. On send, after the
// user approved an estimate, such a refusal means the chain moved since.
var errStateDependent = errors.New("refused by the current chain state")

// refuse is a refusal of the request itself.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{chain.ErrInvalidRequest}, args...)...)
}

// refuseState is a refusal that depends on the current chain state.
func refuseState(format string, args ...any) error {
	return stateRefusal{refuse(format, args...)}
}

type stateRefusal struct{ error }

func (e stateRefusal) Unwrap() []error { return []error{e.error, errStateDependent} }

// sol renders lamports as SOL.
func sol(lamports uint64) string {
	return decimal.NewFromBigInt(new(big.Int).SetUint64(lamports), -9).String()
}

// units converts a positive decimal amount to base units, refusing one with
// more decimals than the asset has or past uint64.
func units(amount string, decimals uint8) (uint64, error) {
	value, err := decimal.NewFromString(amount)
	if err != nil || !value.IsPositive() {
		return 0, refuse("amount must be a positive decimal")
	}
	scaled := value.Shift(int32(decimals))
	if !scaled.Equal(scaled.Truncate(0)) {
		return 0, refuse("amount has more than %d decimals", decimals)
	}
	if !scaled.BigInt().IsUint64() {
		return 0, refuse("amount is too large")
	}
	return scaled.BigInt().Uint64(), nil
}

// plan is one transfer as it will be built: who, what, how much, and what it
// costs.
type plan struct {
	from, to solana.PublicKey
	// amount is in lamports for SOL and in base units for a token.
	amount uint64
	token  *tokenPlan

	balance     uint64 // the sender's lamports
	toExists    bool   // the recipient account exists (SOL)
	toLamports  uint64 // the recipient's lamports (SOL)
	rentExempt0 uint64 // the rent-exempt minimum of an account without data

	// basePrice is the recent priority price; price adds headroom so that a
	// small rise between estimate and send does not invalidate the approval.
	basePrice   uint64
	price       uint64
	neededLimit uint32
	limit       uint32
	// probing marks the compute probe, whose fee is only the signature fee;
	// offered is then the priority price the real transfer will offer.
	probing bool
	offered uint64
	// max is a transfer of everything there is.
	max bool
}

type tokenPlan struct {
	mint          solana.PublicKey
	decimals      uint8
	source        solana.PublicKey
	destination   solana.PublicKey
	sourceBalance uint64
	createAccount bool
	rent          uint64 // the rent of the recipient token account, if created
}

func priorityFee(limit uint32, price uint64) uint64 {
	fee := new(big.Int).Mul(new(big.Int).SetUint64(uint64(limit)), new(big.Int).SetUint64(price))
	fee.Add(fee, big.NewInt(999_999))
	return fee.Div(fee, big.NewInt(1_000_000)).Uint64()
}

// networkFee is what the network charges: the signature and the priority fee.
func (p *plan) networkFee() uint64 { return lamportsPerSignature + priorityFee(p.limit, p.price) }

// cost is everything the sender's SOL pays beyond a SOL amount: the network
// fee and the rent of a token account it opens.
func (p *plan) cost() uint64 {
	cost := p.networkFee()
	if p.token != nil && p.token.createAccount {
		cost += p.token.rent
	}
	return cost
}

func (p *plan) intent(blockhash solana.Hash) intent {
	in := intent{payer: p.from, to: p.to, blockhash: blockhash, limit: p.limit, price: p.price}
	if p.token == nil {
		in.lamports = p.amount
		return in
	}
	in.token = &tokenIntent{
		mint: p.token.mint, decimals: p.token.decimals, amount: p.amount, createAccount: p.token.createAccount,
	}
	return in
}

// build serializes the plan's message for a blockhash.
func (p *plan) build(blockhash solana.Hash) ([]byte, error) {
	instructions := []solana.Instruction{
		solana.NewInstruction(solana.ComputeBudget, nil, binary.LittleEndian.AppendUint32([]byte{2}, p.limit)),
		solana.NewInstruction(solana.ComputeBudget, nil, binary.LittleEndian.AppendUint64([]byte{3}, p.price)),
	}
	if p.token == nil {
		instructions = append(instructions, solana.NewInstruction(solana.SystemProgramID,
			solana.AccountMetaSlice{solana.Meta(p.from).WRITE().SIGNER(), solana.Meta(p.to).WRITE()},
			binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), p.amount),
		))
	} else {
		if p.token.createAccount {
			instructions = append(instructions, solana.NewInstruction(solana.SPLAssociatedTokenAccountProgramID,
				solana.AccountMetaSlice{
					solana.Meta(p.from).WRITE().SIGNER(), solana.Meta(p.token.destination).WRITE(),
					solana.Meta(p.to), solana.Meta(p.token.mint), solana.Meta(solana.SystemProgramID),
					solana.Meta(solana.TokenProgramID),
				}, []byte{1}))
		}
		data := binary.LittleEndian.AppendUint64([]byte{12}, p.amount)
		instructions = append(instructions, solana.NewInstruction(solana.TokenProgramID,
			solana.AccountMetaSlice{
				solana.Meta(p.token.source).WRITE(), solana.Meta(p.token.mint),
				solana.Meta(p.token.destination).WRITE(), solana.Meta(p.from).SIGNER(),
			}, append(data, p.token.decimals)))
	}
	tx, err := solana.NewTransaction(instructions, blockhash, solana.TransactionPayer(p.from))
	if err != nil {
		// Not reachable: it fails only without instructions or a payer, and
		// both are always set above.
		return nil, fmt.Errorf("build Solana transaction: %w", err)
	}
	return tx.Message.MarshalBinary()
}

// rentCache keeps rent-exempt minimums per network and size.
type rentCache struct {
	mu    sync.Mutex
	items map[string]rentEntry
}

type rentEntry struct {
	lamports uint64
	at       time.Time
}

func (a *Adapter) rentExempt(ctx context.Context, nodes *nodes, networkID string, size uint64) (uint64, error) {
	key := fmt.Sprintf("%s/%d", networkID, size)
	a.rents.mu.Lock()
	entry, ok := a.rents.items[key]
	a.rents.mu.Unlock()
	if ok && time.Since(entry.at) < rentTTL {
		return entry.lamports, nil
	}
	var lamports uint64
	err := nodes.do(ctx, "getMinimumBalanceForRentExemption", func(client *rpc.Client) error {
		var callErr error
		lamports, callErr = client.GetMinimumBalanceForRentExemption(ctx, size, readCommitment)
		return callErr
	})
	if err != nil {
		return 0, fmt.Errorf("read the rent-exempt minimum: %w", err)
	}
	if lamports == 0 {
		return 0, errors.New("read the rent-exempt minimum: the node answered zero")
	}
	a.rents.mu.Lock()
	a.rents.items[key] = rentEntry{lamports: lamports, at: time.Now()}
	a.rents.mu.Unlock()
	return lamports, nil
}

// readAccounts reads accounts in one call; a missing account is nil.
func readAccounts(ctx context.Context, nodes *nodes, keys []solana.PublicKey) ([]*rpc.Account, error) {
	var result *rpc.GetMultipleAccountsResult
	err := nodes.do(ctx, "getMultipleAccounts", func(client *rpc.Client) error {
		var callErr error
		result, callErr = client.GetMultipleAccountsWithOpts(ctx, keys, &rpc.GetMultipleAccountsOpts{
			Encoding: solana.EncodingBase64, Commitment: readCommitment,
		})
		if callErr == nil && len(result.Value) != len(keys) {
			callErr = fmt.Errorf("answered %d accounts for %d addresses", len(result.Value), len(keys))
		}
		return callErr
	})
	if err != nil {
		return nil, fmt.Errorf("read accounts: %w", err)
	}
	return result.Value, nil
}

// priorityPrice is the 75th percentile of the recent priority prices paid
// for the accounts the transfer writes, capped at maxComputeUnitPrice.
func priorityPrice(ctx context.Context, nodes *nodes, writable []solana.PublicKey) (uint64, error) {
	var fees []rpc.PriorizationFeeResult
	err := nodes.do(ctx, "getRecentPrioritizationFees", func(client *rpc.Client) error {
		var callErr error
		fees, callErr = client.GetRecentPrioritizationFees(ctx, writable)
		return callErr
	})
	if err != nil {
		return 0, fmt.Errorf("read recent priority fees: %w", err)
	}
	if len(fees) == 0 {
		return 0, nil
	}
	prices := make([]uint64, len(fees))
	for i, fee := range fees {
		prices[i] = fee.PrioritizationFee
	}
	slices.Sort(prices)
	return min(prices[(len(prices)*3+3)/4-1], maxComputeUnitPrice), nil
}

// offeredPrice is the priority price an estimate offers: the recent price
// with a quarter of headroom, at least minComputeUnitPrice and at most
// maxComputeUnitPrice.
func offeredPrice(recent uint64) uint64 {
	return min(max(recent+recent/4, minComputeUnitPrice), maxComputeUnitPrice)
}

// simulate runs a message without a signature against any recent blockhash
// and returns what the runtime reported.
func simulate(ctx context.Context, nodes *nodes, message []byte) (*rpc.SimulateTransactionResult, error) {
	raw := append([]byte{1}, make([]byte, ed25519.SignatureSize)...)
	raw = append(raw, message...)
	var result *rpc.SimulateTransactionResponse
	err := nodes.do(ctx, "simulateTransaction", func(client *rpc.Client) error {
		var callErr error
		result, callErr = client.SimulateRawTransactionWithOpts(ctx, raw, &rpc.SimulateTransactionOpts{
			Commitment: readCommitment, ReplaceRecentBlockhash: true,
		})
		if callErr == nil && (result == nil || result.Value == nil) {
			callErr = errors.New("answer has no result")
		}
		return callErr
	})
	if err != nil {
		return nil, fmt.Errorf("simulate the transaction: %w", err)
	}
	return result.Value, nil
}

// verdict simulates the exact message that would be signed, and the
// runtime's answer on it decides. Which rent rules apply is the cluster's
// business — they changed with SIMD-0392 — so the wallet does not decide
// them; it only puts a refusal the runtime made into its own words.
func (p *plan) verdict(ctx context.Context, nodes *nodes, message []byte) error {
	result, err := simulate(ctx, nodes, message)
	if err != nil {
		return err
	}
	if result.Err != nil {
		return p.explain(result, message)
	}
	return nil
}

// explain words a runtime refusal with the transfer's real numbers: the rent
// errors for the payer and the SOL recipient, and a fee the balance cannot
// cover. Anything else is the runtime's own reason, made readable.
func (p *plan) explain(result *rpc.SimulateTransactionResult, message []byte) error {
	// No compute used means the runtime refused before executing anything:
	// it charges and checks the fee first.
	beforeExecution := result.UnitsConsumed == nil || *result.UnitsConsumed == 0
	fee := p.networkFee()
	// The compute probe is charged only the signature fee: the real fee is
	// at least that.
	feeText := sol(fee)
	if p.probing {
		feeText = "of at least " + feeText
	}
	if reason, ok := result.Err.(string); ok && reason == "InsufficientFundsForFee" {
		return refuseState("the SOL balance %s does not cover the fee %s", sol(p.balance), feeText)
	}
	account, ok := rentAccount(result.Err, message)
	switch {
	case !ok:
	case account == p.from && beforeExecution:
		afterFee := uint64(0)
		if p.balance > fee {
			afterFee = p.balance - fee
		}
		return refuseState(
			"paying the fee %s would leave %s SOL, below the %s SOL an account must keep, and the network checks the fee "+
				"before anything is sent: no transfer can go out from this balance until at least %s SOL is added",
			feeText, sol(afterFee), sol(p.rentExempt0), sol(p.shortfall()),
		)
	case account == p.from && p.token != nil:
		// A token transfer debits the payer's SOL only for the fee, checked
		// before execution, and the rent of an account it opens: a refusal
		// after execution is about that rent.
		cost := p.cost()
		left := uint64(0)
		if p.balance > cost {
			left = p.balance - cost
		}
		return refuseState(
			"after the fee %s and %s SOL to open the recipient's token account, %s SOL would be left, below the %s SOL an "+
				"account must keep: add at least %s SOL",
			sol(fee), sol(p.token.rent), sol(left), sol(p.rentExempt0), sol(p.rentExempt0-min(left, p.rentExempt0)),
		)
	case account == p.from:
		spent := p.cost() + p.amount
		left := uint64(0)
		if p.balance > spent {
			left = p.balance - spent
		}
		if p.balance <= p.cost()+p.rentExempt0 {
			return refuseState(
				"this would leave %s SOL, below the %s SOL an account must keep: send the maximum",
				sol(left), sol(p.rentExempt0),
			)
		}
		return refuseState(
			"this would leave %s SOL, below the %s SOL an account must keep: send the maximum, or at most %s SOL",
			sol(left), sol(p.rentExempt0), sol(p.balance-p.cost()-p.rentExempt0),
		)
	case account == p.to && p.token == nil && p.toLamports == 0:
		return refuseState("the recipient account does not exist yet, and opening it takes at least %s SOL", sol(p.rentExempt0))
	case account == p.to && p.token == nil && p.toLamports < p.rentExempt0:
		return refuseState(
			"the recipient holds %s SOL, below the %s SOL an account must keep, and may only receive enough to reach it: at least %s SOL",
			sol(p.toLamports), sol(p.rentExempt0), sol(p.rentExempt0-min(p.toLamports, p.rentExempt0)),
		)
	}
	return refuseState("the network would reject this transfer: %s%s", describe(result.Err, message), lastLog(result.Logs))
}

// shortfall is the SOL to add so that this transfer goes through and leaves
// the payer at the rent-exempt minimum or more: everything it costs —
// for the compute probe an upper bound, the priority fee at the offered price
// for the probe's whole limit — plus a SOL amount sent away, plus the minimum,
// less what the payer holds. A maximum sends whatever is there, so its
// amount is not counted.
func (p *plan) shortfall() uint64 {
	need := p.cost()
	if p.probing {
		need = lamportsPerSignature + priorityFee(simulationComputeUnitLimit, p.offered)
		if p.token != nil && p.token.createAccount {
			need += p.token.rent
		}
	}
	if p.token == nil && !p.max && p.to != p.from {
		need += p.amount
	}
	need += p.rentExempt0
	if need <= p.balance {
		return 0 // not reached for a refusal that names the payer; kept against underflow
	}
	return need - p.balance
}

// rentAccount is the account an InsufficientFundsForRent error names.
func rentAccount(failure any, message []byte) (solana.PublicKey, bool) {
	detail, ok := failure.(map[string]any)
	if !ok {
		return solana.PublicKey{}, false
	}
	rent, ok := detail["InsufficientFundsForRent"].(map[string]any)
	if !ok {
		return solana.PublicKey{}, false
	}
	position, err := accountIndex(rent["account_index"])
	decoded, decodeErr := decodeLegacyMessage(message)
	if err != nil || decodeErr != nil || position < 0 || int(position) >= len(decoded.keys) {
		return solana.PublicKey{}, false
	}
	return decoded.keys[position], true
}

// accountIndex reads an account index from a decoded JSON error.
func accountIndex(value any) (int64, error) {
	switch index := value.(type) {
	case json.Number:
		return index.Int64()
	case float64:
		return int64(index), nil
	}
	return 0, errors.New("no account index")
}

// describe renders a runtime error readably, naming the account a rent error
// is about.
func describe(failure any, message []byte) string {
	if reason, ok := failure.(string); ok {
		switch reason {
		case "InsufficientFundsForFee":
			return "the SOL balance does not cover the fee"
		case "AccountNotFound":
			return "the sending account does not exist"
		}
		return reason
	}
	if detail, ok := failure.(map[string]any); ok {
		if rent, ok := detail["InsufficientFundsForRent"].(map[string]any); ok {
			position, err := accountIndex(rent["account_index"])
			if decoded, decodeErr := decodeLegacyMessage(message); err == nil && decodeErr == nil &&
				position >= 0 && int(position) < len(decoded.keys) {
				return fmt.Sprintf("account %s would be left with less than its rent-exempt minimum", decoded.keys[position])
			}
			return "an account would be left with less than its rent-exempt minimum"
		}
	}
	return fmt.Sprintf("%v", failure)
}

// lastLog is the last program log line, which is usually the reason.
func lastLog(logs []string) string {
	for _, line := range slices.Backward(logs) {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			return " (" + line + ")"
		}
	}
	return ""
}

// limitFor turns simulated use into the limit to sign: a fifth more and a
// thousand units, so a little more work at execution does not fail it.
func limitFor(used uint64) uint32 {
	return uint32(min(used+used/5+1000, maxComputeUnitLimit))
}

// prepare reads what the transfer depends on and decides its amount (for max),
// accounts and compute: everything but the final fee checks.
func (a *Adapter) prepare(ctx context.Context, nodes *nodes, networkID string, req chain.TransferRequest, max bool) (*plan, error) {
	if req.Asset.NetworkID != networkID {
		return nil, refuse("asset belongs to another network")
	}
	if req.Action != "" {
		return nil, refuse("Solana has no %q action", req.Action)
	}
	from, err := solana.PublicKeyFromBase58(req.From)
	if err != nil {
		return nil, refuse("invalid sender address")
	}
	to, err := solana.PublicKeyFromBase58(req.To)
	if err != nil {
		return nil, refuse("invalid Solana address")
	}
	p := &plan{from: from, to: to}
	if !max {
		if p.amount, err = units(req.Amount, req.Asset.Decimals); err != nil {
			return nil, err
		}
	}
	rentExempt0, err := a.rentExempt(ctx, nodes, networkID, 0)
	if err != nil {
		return nil, err
	}
	p.rentExempt0 = rentExempt0
	switch req.Asset.Kind {
	case "native":
		if req.Asset.Decimals != 9 {
			return nil, refuse("SOL has 9 decimals")
		}
		err = a.prepareSOL(ctx, nodes, p, max)
	case "spl":
		err = a.prepareToken(ctx, nodes, networkID, p, req.Asset, max)
	default:
		return nil, refuse("unsupported Solana asset kind %q", req.Asset.Kind)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (a *Adapter) prepareSOL(ctx context.Context, nodes *nodes, p *plan, max bool) error {
	accounts, err := readAccounts(ctx, nodes, []solana.PublicKey{p.from, p.to})
	if err != nil {
		return err
	}
	if accounts[0] != nil {
		p.balance = accounts[0].Lamports
	}
	p.toExists = accounts[1] != nil || p.to == p.from
	if accounts[1] != nil {
		p.toLamports = accounts[1].Lamports
	}
	if p.balance == 0 {
		return refuseState("the account holds no SOL to pay the fee")
	}
	if p.basePrice, err = priorityPrice(ctx, nodes, []solana.PublicKey{p.from, p.to}); err != nil {
		return err
	}
	p.price = offeredPrice(p.basePrice)
	return p.measure(ctx, nodes, max)
}

// measure simulates the transfer to learn the compute it uses, and nothing
// else: whether the transfer may go ahead is decided on the final message by
// verdict.
//
// The probe carries the real instructions and amount at a priority price of
// 0, which does not change the compute used; a SOL maximum, whose amount is
// not known yet, sends all but the signature fee. Its balances after the fee
// may differ from the real transaction's, so a rent error the runtime raises
// after executing it is not a verdict: the compute it reports is used. Only a
// probe that could not execute at all — its fee could not be charged — reports
// no compute, and then the transfer is refused with the runtime's reason in
// this wallet's words.
func (p *plan) measure(ctx context.Context, nodes *nodes, max bool) error {
	probe := *p
	probe.limit, probe.price, probe.probing, probe.offered, probe.max = simulationComputeUnitLimit, 0, true, p.price, max
	if p.token == nil && max {
		probe.amount = 0
		if p.balance > lamportsPerSignature {
			probe.amount = p.balance - lamportsPerSignature
		}
	}
	message, err := probe.build(solana.Hash{})
	if err != nil {
		return err // not reachable: see build
	}
	result, err := simulate(ctx, nodes, message)
	if err != nil {
		return err
	}
	if result.UnitsConsumed == nil || *result.UnitsConsumed == 0 {
		if result.Err == nil {
			return errors.New("simulate the transaction: the node did not report the compute used")
		}
		// Worded with the probe's own numbers: the fee it could not pay is
		// the least any version of this transfer would cost.
		return probe.explain(result, message)
	}
	p.neededLimit = limitFor(*result.UnitsConsumed)
	p.limit = p.neededLimit
	return nil
}

func (a *Adapter) prepareToken(
	ctx context.Context, nodes *nodes, networkID string, p *plan, asset chain.Asset, max bool,
) error {
	mint, err := solana.PublicKeyFromBase58(asset.Contract)
	if err != nil {
		return refuse("invalid SPL mint")
	}
	// The derivations cannot fail in practice: they need all 256 bump seeds
	// to land on the curve.
	source, _, err := solana.FindAssociatedTokenAddress(p.from, mint)
	if err != nil {
		return err
	}
	destination, _, err := solana.FindAssociatedTokenAddress(p.to, mint)
	if err != nil {
		return err
	}
	p.token = &tokenPlan{mint: mint, decimals: asset.Decimals, source: source, destination: destination}
	accounts, err := readAccounts(ctx, nodes, []solana.PublicKey{p.from, p.to, mint, source, destination})
	if err != nil {
		return err
	}
	if accounts[0] != nil {
		p.balance = accounts[0].Lamports
	}
	if recipient := accounts[1]; recipient != nil &&
		(recipient.Owner == solana.TokenProgramID || recipient.Owner == solana.Token2022ProgramID) {
		return refuse("the recipient is a token account; send to the wallet address that owns it")
	}
	// A token account of an address off the curve belongs to a program. Such
	// recipients are not supported yet: a mistyped one cannot be told apart.
	if !p.to.IsOnCurve() {
		return refuse("program-owned addresses are not supported as token recipients")
	}
	decimals, err := mintDecimals(accounts[2])
	if err != nil {
		return err
	}
	if decimals != asset.Decimals {
		return refuse("the mint has %d decimals, the asset says %d", decimals, asset.Decimals)
	}
	// A missing associated account holds nothing; whatever the owner has
	// sits elsewhere, which the refusal below explains.
	var sourceBalance uint64
	if accounts[3] != nil {
		if sourceBalance, err = tokenAccountBalance(accounts[3], mint, p.from); err != nil {
			return err
		}
	}
	p.token.sourceBalance = sourceBalance
	if accounts[4] == nil {
		p.token.createAccount = true
		if p.token.rent, err = a.rentExempt(ctx, nodes, networkID, tokenAccountSize); err != nil {
			return err
		}
	} else if _, err := tokenAccountBalance(accounts[4], mint, p.to); err != nil {
		return fmt.Errorf("the recipient's token account: %w", err)
	}
	if max {
		p.amount = sourceBalance
	}
	if p.amount > sourceBalance || p.amount == 0 {
		// Only to explain the refusal: if the listing cannot be read, the
		// plain balance is reason enough.
		held, err := readTokens(ctx, nodes, p.from, nil)
		total := held[mint]
		if err == nil && total != nil && total.Sign() > 0 && (p.amount == 0 || total.Cmp(new(big.Int).SetUint64(p.amount)) >= 0) {
			return refuseState(
				"the main token account holds %s %s; the rest sits in other token accounts this wallet does not send from",
				decimal.NewFromBigInt(new(big.Int).SetUint64(sourceBalance), -int32(asset.Decimals)), asset.Symbol,
			)
		}
		return refuseState("the account holds %s %s",
			decimal.NewFromBigInt(new(big.Int).SetUint64(sourceBalance), -int32(asset.Decimals)), asset.Symbol)
	}
	if p.balance == 0 {
		return refuseState("the account holds no SOL to pay the fee")
	}
	if p.basePrice, err = priorityPrice(ctx, nodes, []solana.PublicKey{p.from, source, destination}); err != nil {
		return err
	}
	p.price = offeredPrice(p.basePrice)
	return p.measure(ctx, nodes, false)
}

// tokenAccountBalance reads a classic token account of mint owned by owner;
// the caller has checked that the account exists.
func tokenAccountBalance(account *rpc.Account, mint, owner solana.PublicKey) (uint64, error) {
	data := account.Data.GetBinary()
	if account.Owner != solana.TokenProgramID || len(data) != tokenAccountSize ||
		solana.PublicKeyFromBytes(data[0:32]) != mint || solana.PublicKeyFromBytes(data[32:64]) != owner {
		return 0, refuse("the address is not a token account of this mint and owner")
	}
	switch data[tokenStateOffset] {
	case 1:
	case 2:
		return 0, refuseState("the token account is frozen")
	default:
		return 0, refuse("the token account is not initialized")
	}
	return binary.LittleEndian.Uint64(data[64:72]), nil
}

// precheck sets the amount of a SOL max transfer and refuses what no rent
// rule could allow: a balance that does not cover the fee, the account rent
// and a SOL amount. Whether the accounts may end where the transfer leaves
// them is the runtime's call, made on the final message by verdict: the rent
// rules depend on the cluster's features (SIMD-0392 changed them), so this
// wallet does not apply them itself.
func (p *plan) precheck(max bool) error {
	cost := p.cost()
	p.max = max
	if p.balance < cost {
		return refuseState("the SOL balance %s does not cover the fee %s: add at least %s SOL", sol(p.balance), sol(cost), sol(cost-p.balance))
	}
	if p.token == nil && max {
		p.amount = p.balance - cost
		if p.amount == 0 {
			return refuseState("the SOL balance %s only covers the fee", sol(p.balance))
		}
	}
	if p.token == nil && p.balance-cost < p.amount {
		return refuseState("the SOL balance %s does not cover %s plus the fee %s: add at least %s SOL",
			sol(p.balance), sol(p.amount), sol(cost), sol(p.amount+cost-p.balance))
	}
	return nil
}

func (p *plan) estimate(networkID string, decimals uint8) chain.TransferEstimate {
	estimate := chain.TransferEstimate{
		NetworkID: networkID,
		Amount:    decimal.NewFromBigInt(new(big.Int).SetUint64(p.amount), -int32(decimals)).String(),
		Fee:       sol(p.cost()), FeeModel: feeModel,
		ComputeUnitLimit: uint64(p.limit), ComputeUnitPriceMicroLamports: p.price,
		NativeBalance: sol(p.balance),
	}
	if p.token != nil && p.token.createAccount {
		estimate.CreatesTokenAccount = true
		estimate.TokenAccountRent = sol(p.token.rent)
	}
	return estimate
}

// EstimateTransfer prices a transfer: the network fee at a recent priority
// price for the compute a simulation used, plus the rent of a recipient token
// account it has to open.
func (a *Adapter) EstimateTransfer(ctx context.Context, networkID string, req chain.TransferRequest) (chain.TransferEstimate, error) {
	return a.estimateTransfer(ctx, networkID, req, false)
}

// EstimateMaxTransfer is the largest amount that can be sent now: for SOL the
// whole balance less the fee, leaving exactly zero; for a token the balance of
// the sender's associated token account, which is the account sends go from.
func (a *Adapter) EstimateMaxTransfer(ctx context.Context, networkID string, req chain.TransferRequest) (chain.TransferEstimate, error) {
	return a.estimateTransfer(ctx, networkID, req, true)
}

func (a *Adapter) estimateTransfer(
	ctx context.Context, networkID string, req chain.TransferRequest, max bool,
) (chain.TransferEstimate, error) {
	_, nodes, err := a.client(ctx, networkID)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	p, err := a.prepare(ctx, nodes, networkID, req, max)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	if err := p.precheck(max); err != nil {
		return chain.TransferEstimate{}, err
	}
	message, err := p.build(solana.Hash{})
	if err != nil {
		return chain.TransferEstimate{}, err // not reachable: see build
	}
	if err := p.verdict(ctx, nodes, message); err != nil {
		return chain.TransferEstimate{}, err
	}
	return p.estimate(networkID, req.Asset.Decimals), nil
}

// approval is the fee the user agreed to, checked to be well-formed and to
// add up.
type approval struct {
	limit         uint32
	price         uint64
	createAccount bool
	rent          uint64
	fee           uint64
}

func lamports(value string) (uint64, error) {
	amount, err := decimal.NewFromString(value)
	if err != nil || amount.IsNegative() {
		return 0, errors.New("not an amount")
	}
	scaled := amount.Shift(9)
	if !scaled.Equal(scaled.Truncate(0)) || !scaled.BigInt().IsUint64() {
		return 0, errors.New("not an amount of lamports")
	}
	return scaled.BigInt().Uint64(), nil
}

func parseApproval(approved *chain.FeeApproval) (approval, error) {
	if approved == nil {
		return approval{}, refuse("this transaction was not confirmed against a fee estimate")
	}
	if approved.FeeModel != "" && approved.FeeModel != feeModel {
		return approval{}, refuse("the approved fee is not a Solana fee")
	}
	if approved.ComputeUnitLimit == 0 || approved.ComputeUnitLimit > maxComputeUnitLimit {
		return approval{}, refuse("the approved compute-unit limit is out of range")
	}
	if approved.ComputeUnitPriceMicroLamports > maxComputeUnitPrice {
		return approval{}, refuse("the approved compute-unit price is above the %d micro-lamport ceiling", maxComputeUnitPrice)
	}
	out := approval{
		limit: uint32(approved.ComputeUnitLimit), price: approved.ComputeUnitPriceMicroLamports,
		createAccount: approved.CreatesTokenAccount,
	}
	var err error
	if approved.CreatesTokenAccount {
		if out.rent, err = lamports(approved.TokenAccountRent); err != nil || out.rent == 0 {
			return approval{}, refuse("the approved token account rent is not an amount")
		}
	} else if approved.TokenAccountRent != "" {
		if rent, err := lamports(approved.TokenAccountRent); err != nil || rent != 0 {
			return approval{}, refuse("a token account rent was approved without opening an account")
		}
	}
	if out.fee, err = lamports(approved.Fee); err != nil {
		return approval{}, refuse("the approved fee is not an amount")
	}
	total := lamportsPerSignature + priorityFee(out.limit, out.price)
	if out.createAccount {
		total += out.rent
	}
	if total != out.fee {
		return approval{}, refuse("the approved fee %s does not add up to its parts %s", sol(out.fee), sol(total))
	}
	return out, nil
}

// Send builds, checks, signs and broadcasts a transfer at the fee the user
// approved.
//
// The approved compute-unit limit and price are what get signed, so the
// network fee is exactly the one shown. The chain is read again first: if the
// transfer now needs more compute, a higher priority price than approved, or
// a token account whose rent was not approved, nothing is signed and the
// answer is ErrFeeChanged; if balances or accounts no longer allow it, it is
// ErrStateChanged.
//
// The signed message is decoded and compared with the request before the
// signature is made. The transaction id is the signature; the expiry height
// goes to req.BeforeSigning before the signature is made. A node that refuses the
// transaction is a rejection; one whose answer is lost is ErrBroadcastUnknown,
// and the transaction is not sent again.
func (a *Adapter) Send(ctx context.Context, networkID string, req chain.TransferRequest, key chain.Key) (chain.Transaction, error) {
	signer, err := messageSigner(key, req.From)
	if err != nil {
		return chain.Transaction{}, err
	}
	approved, err := parseApproval(req.Approved)
	if err != nil {
		return chain.Transaction{}, err
	}
	_, nodes, err := a.client(ctx, networkID)
	if err != nil {
		return chain.Transaction{}, err
	}
	p, err := a.prepare(ctx, nodes, networkID, req, false)
	if err != nil {
		return chain.Transaction{}, stateChanged(err)
	}
	switch {
	case p.neededLimit > approved.limit:
		return chain.Transaction{}, fmt.Errorf("%w: the transfer now needs %d compute units, %d were approved",
			chain.ErrFeeChanged, p.neededLimit, approved.limit)
	case p.basePrice > approved.price:
		return chain.Transaction{}, fmt.Errorf("%w: the priority price is now %d micro-lamports, %d was approved",
			chain.ErrFeeChanged, p.basePrice, approved.price)
	case p.token != nil && p.token.createAccount && !approved.createAccount:
		return chain.Transaction{}, fmt.Errorf("%w: the recipient's token account has to be opened now", chain.ErrFeeChanged)
	case p.token != nil && p.token.createAccount && p.token.rent > approved.rent:
		return chain.Transaction{}, fmt.Errorf("%w: opening the token account now costs %s SOL, %s was approved",
			chain.ErrFeeChanged, sol(p.token.rent), sol(approved.rent))
	}
	p.limit, p.price = approved.limit, approved.price
	if err := p.precheck(false); err != nil {
		return chain.Transaction{}, stateChanged(err)
	}

	blockhash, lastValid, err := latestBlockhash(ctx, nodes)
	if err != nil {
		return chain.Transaction{}, err
	}
	message, err := a.buildMessage(p, blockhash)
	if err != nil {
		return chain.Transaction{}, err
	}
	if err := verifyMessage(message, p.intent(blockhash)); err != nil {
		return chain.Transaction{}, err
	}
	// The runtime's answer on exactly this transaction, before anything is
	// signed; the broadcast keeps its own preflight as well.
	if err := p.verdict(ctx, nodes, message); err != nil {
		return chain.Transaction{}, stateChanged(err)
	}
	return a.signAndBroadcast(ctx, networkID, nodes, message, lastValid, signer, req.BeforeSigning, req.BeforeBroadcast)
}

// messageSigner is the key as a Solana message signer for the account from.
func messageSigner(key chain.Key, from string) (chain.MessageSigner, error) {
	signer, ok := key.(chain.MessageSigner)
	if !ok || key == nil {
		return nil, fmt.Errorf("%w: Solana transactions are signed as messages", chain.ErrSignerMismatch)
	}
	if signer.Family() != chain.FamilySolana {
		return nil, fmt.Errorf("%w: the key is not a Solana key", chain.ErrSignerMismatch)
	}
	if pub := signer.PublicKey(); len(pub) != ed25519.PublicKeySize || solana.PublicKeyFromBytes(pub).String() != from {
		return nil, refuse("signer does not match sender")
	}
	return signer, nil
}

// latestBlockhash is a recent blockhash and the last block height at which a
// transaction built on it can land.
func latestBlockhash(ctx context.Context, nodes *nodes) (solana.Hash, uint64, error) {
	var latest *rpc.GetLatestBlockhashResult
	err := nodes.do(ctx, "getLatestBlockhash", func(client *rpc.Client) error {
		var callErr error
		latest, callErr = client.GetLatestBlockhashWithOpts(ctx, &rpc.GetLatestBlockhashOpts{Commitment: readCommitment})
		if callErr == nil && (latest == nil || latest.Value == nil || latest.Value.LastValidBlockHeight == 0) {
			callErr = errors.New("answer has no blockhash")
		}
		return callErr
	})
	if err != nil {
		return solana.Hash{}, 0, fmt.Errorf("read a recent blockhash: %w", err)
	}
	return latest.Value.Blockhash, latest.Value.LastValidBlockHeight, nil
}

// signAndBroadcast signs a verified message and sends it once. The expiry
// goes to beforeSigning before the signature is made, and the transaction id
// to beforeBroadcast before it leaves. A node that refuses the transaction is
// a rejection; one whose answer is lost is ErrBroadcastUnknown, and the
// transaction is not sent again.
func (a *Adapter) signAndBroadcast(
	ctx context.Context, networkID string, nodes *nodes, message []byte, lastValid uint64,
	signer chain.MessageSigner, beforeSigning func(uint64) error, beforeBroadcast func(string) error,
) (chain.Transaction, error) {
	if beforeSigning != nil {
		if err := beforeSigning(lastValid); err != nil {
			return chain.Transaction{}, fmt.Errorf("record the transaction expiry before signing: %w", err)
		}
	}
	signature, err := signer.SignMessage(ctx, message)
	if err != nil {
		return chain.Transaction{}, err
	}
	if len(signature) != ed25519.SignatureSize || !ed25519.Verify(signer.PublicKey(), message, signature) {
		return chain.Transaction{}, errors.New("the signer returned a signature that does not verify")
	}
	txID := solana.SignatureFromBytes(signature).String()
	a.expiry.record(networkID, txID, lastValid)
	if beforeBroadcast != nil {
		if err := beforeBroadcast(txID); err != nil {
			return chain.Transaction{}, fmt.Errorf("record signed transaction before broadcast: %w", err)
		}
	}

	raw := append([]byte{1}, signature...)
	raw = append(raw, message...)
	var answered solana.Signature
	err = nodes.once(ctx, "sendTransaction", func(client *rpc.Client) error {
		var callErr error
		answered, callErr = client.SendEncodedTransactionWithOpts(ctx, base64.StdEncoding.EncodeToString(raw), rpc.TransactionOpts{
			SkipPreflight: false, PreflightCommitment: readCommitment,
		})
		return callErr
	})
	if err != nil {
		if _, refused := errors.AsType[*jsonrpc.RPCError](err); refused {
			return chain.Transaction{NetworkID: networkID, Hash: txID, Status: operation.StatusFailed},
				fmt.Errorf("%w: the network refused transaction %s: %w", chain.ErrInvalidRequest, txID, err)
		}
		return chain.Transaction{NetworkID: networkID, Hash: txID, Status: operation.StatusBroadcastUnknown},
			fmt.Errorf("broadcast %s: %w", txID, errors.Join(chain.ErrBroadcastUnknown, err))
	}
	if answered.String() != txID {
		return chain.Transaction{NetworkID: networkID, Hash: txID, Status: operation.StatusBroadcastUnknown},
			fmt.Errorf("broadcast %s: %w: the node answered with signature %s", txID, chain.ErrBroadcastUnknown, answered)
	}
	return chain.Transaction{NetworkID: networkID, Hash: txID, Status: operation.StatusPending}, nil
}

// stateChanged turns a refusal that depends on chain state into
// ErrStateChanged: on send it means the chain moved since the estimate.
func stateChanged(err error) error {
	if errors.Is(err, errStateDependent) {
		return fmt.Errorf("%w: %w", chain.ErrStateChanged, err)
	}
	return err
}
