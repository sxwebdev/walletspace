package solana

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
	"github.com/sxwebdev/walletspace/internal/network"
)

const (
	// MaxSwapSlippageBps is the most slippage a swap may be quoted with, and
	// the furthest below the current output an approved minimum may sit: a
	// minimum far below the market invites being traded against.
	MaxSwapSlippageBps = 500
	// swapQuoteDeviationBps is how far the Trade API's output for an AMM v4 or
	// CPMM route may be from what the pools' own curves give.
	swapQuoteDeviationBps = 50
	// swapTickArrays is how many tick arrays a CLMM hop carries: the walk's
	// first three, so a swap may cross two array boundaries.
	swapTickArrays = 3
	// closeComputeAllowance covers the closing of the wrapped-SOL account,
	// which the measuring simulation leaves out.
	closeComputeAllowance = 5000
	// maxDiscoveryCandidates is how many discovered pools are tried in turn.
	maxDiscoveryCandidates = 3
	// SwapSourceAPI and SwapSourceDiscovery name where a route came from.
	SwapSourceAPI       = "raydium_api"
	SwapSourceDiscovery = "discovery"
)

// intermediates are the tokens a two-hop route may go through: SOL, USDC
// and USDT on mainnet.
var intermediates = map[solana.PublicKey]bool{
	solana.WrappedSol: true,
	solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"): true,
	solana.MustPublicKeyFromBase58("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"): true,
}

// errNeedsMoreCompute means the final message ran out of its compute limit.
var errNeedsMoreCompute = errors.New("the swap needs more compute than measured")

// errRoute marks a refusal caused by the route — its pools, tables, size or
// what its pools pay — rather than by the wallet or the request: another
// route may work where this one does not.
var errRoute = errors.New("the route cannot be used")

type routeRefusal struct{ error }

func (e routeRefusal) Unwrap() []error { return []error{e.error, errRoute} }

// routeFault marks err as caused by the route.
func routeFault(err error) error { return routeRefusal{err} }

// ErrSwapMismatch means the message about to be signed is not the planned
// swap. It is always a refusal to sign.
var ErrSwapMismatch = fmt.Errorf("%w: transaction does not match the planned swap", chain.ErrInvalidRequest)

// SwapQuoteRequest asks what selling Amount of In for Out would return.
type SwapQuoteRequest struct {
	From        string
	In, Out     chain.Asset
	Amount      string
	SlippageBps uint16
}

// SwapHop is one pool of a route, as the client echoes it back.
type SwapHop struct {
	Pool       string `json:"pool"`
	Kind       string `json:"kind"`
	InputMint  string `json:"input_mint"`
	OutputMint string `json:"output_mint"`
}

// SwapRoute is what a swap goes through. It names pools and tables only; every
// account of the transaction is derived from their state on chain when the
// swap is built.
type SwapRoute struct {
	Hops         []SwapHop `json:"hops"`
	LookupTables []string  `json:"lookup_tables,omitempty"`
}

// SwapQuote is what a swap would pay and cost, measured by simulating it.
type SwapQuote struct {
	NetworkID string `json:"network_id"`
	AmountIn  string `json:"amount_in"`
	// AmountOut is what the simulation of this swap paid out.
	AmountOut   string `json:"amount_out"`
	MinOut      string `json:"min_out"`
	SlippageBps uint16 `json:"slippage_bps"`
	Source      string `json:"source"`
	// QuotedOut and PriceImpactPct are the Trade API's; empty for a route
	// found without it. QuoteDeviationPct is how far the simulated output
	// is below the API's (negative: above).
	//
	// A two-hop route sells for its second hop only the first hop's
	// guaranteed output, so its simulated output is not comparable with the
	// API's: QuoteDeviationPct is left out for two hops.
	QuotedOut         string `json:"quoted_out,omitempty"`
	PriceImpactPct    string `json:"price_impact_pct,omitempty"`
	QuoteDeviationPct string `json:"quote_deviation_pct,omitempty"`
	// APIRouteRejected says why a route found on chain is offered instead
	// of the Trade API's: the API failed, had no route, or its route could
	// not be used.
	APIRouteRejected string    `json:"api_route_rejected,omitempty"`
	Route            SwapRoute `json:"route"`
	// A two-hop swap sells for its second hop only the guaranteed part of
	// the first hop's output; what the first hop pays beyond it stays in the
	// wallet as the intermediate token.
	IntermediateMint     string `json:"intermediate_mint,omitempty"`
	IntermediateLeftover string `json:"intermediate_leftover,omitempty"`
	// UnwrapsWrappedSOL is wrapped SOL the wallet already held: the swap
	// closes the wrapped-SOL account and it comes back as SOL.
	UnwrapsWrappedSOL string `json:"unwraps_wrapped_sol,omitempty"`
	MessageVersion    string `json:"message_version"`
	MessageSize       int    `json:"message_size"`
	// The fee approval, echoed back on swap.
	FeeModel                      string `json:"fee_model"`
	Fee                           string `json:"fee"`
	ComputeUnitLimit              uint64 `json:"compute_unit_limit"`
	ComputeUnitPriceMicroLamports uint64 `json:"compute_unit_price_micro_lamports"`
	TokenAccountsCreated          int    `json:"token_accounts_created"`
	TokenAccountRent              string `json:"token_account_rent,omitempty"`
	NativeBalance                 string `json:"native_balance"`
}

// SwapApproval is the fee the user agreed to.
type SwapApproval struct {
	ComputeUnitLimit              uint64
	ComputeUnitPriceMicroLamports uint64
	TokenAccountsCreated          int
	TokenAccountRent              string
	Fee                           string
}

// SwapRequest is a swap the user approved.
type SwapRequest struct {
	From        string
	In, Out     chain.Asset
	Amount      string
	SlippageBps uint16
	// MinOut is the least the swap may pay, as the user saw it. It is what
	// gets signed: the pool reverts below it.
	MinOut   string
	Route    SwapRoute
	Approved SwapApproval
	// BeforeSigning and BeforeBroadcast are as on chain.TransferRequest.
	BeforeSigning   func(lastValidBlockHeight uint64) error
	BeforeBroadcast func(txID string) error
}

// hopSpec is a hop as a route names it, before its pool is read.
type hopSpec struct {
	pool, inMint, outMint solana.PublicKey
	kind                  raydium.Kind // KindUnknown when the route does not say
	// remaining are the Trade API's tick arrays; nil when there are none to
	// check.
	remaining []solana.PublicKey
}

type swapHopPlan struct {
	hopSpec
	ammv4 *raydium.AMMV4Pool
	cpmm  *raydium.CPMMPool
	clmm  *raydium.CLMMPool
	// The CLMM bitmap extension, when the account exists.
	extension  *raydium.CLMMBitmapExtension
	tickArrays []solana.PublicKey
}

func (h *swapHopPlan) vaults() []solana.PublicKey {
	switch {
	case h.ammv4 != nil:
		return []solana.PublicKey{h.ammv4.CoinVault, h.ammv4.PcVault}
	case h.cpmm != nil:
		return []solana.PublicKey{h.cpmm.Token0Vault, h.cpmm.Token1Vault}
	}
	return []solana.PublicKey{h.clmm.Vault0, h.clmm.Vault1}
}

// swap lays out the hop's Raydium instruction and its typed form.
func (h *swapHopPlan) swap(user, source, destination solana.PublicKey, amount, minOut uint64) (raydium.Instruction, any, error) {
	switch {
	case h.ammv4 != nil:
		s, err := h.ammv4.Swap(h.pool, h.inMint, source, destination, user, amount, minOut)
		return s.Instruction(), s, err
	case h.cpmm != nil:
		s, err := h.cpmm.Swap(h.pool, h.inMint, user, source, destination, amount, minOut)
		return s.Instruction(), s, err
	}
	s, err := h.clmm.Swap(h.pool, h.inMint, user, source, destination, h.extension != nil, h.tickArrays, amount, minOut)
	if err != nil {
		return raydium.Instruction{}, nil, err // not reachable: see instructions
	}
	in, err := s.Instruction()
	return in, s, err
}

type ataCreate struct{ account, mint solana.PublicKey }

// swapPlan is one swap as it will be built.
type swapPlan struct {
	user            solana.PublicKey
	inMint, outMint solana.PublicKey
	inSOL, outSOL   bool
	amountIn        uint64
	slippage        uint16
	hops            []*swapHopPlan
	tables          map[solana.PublicKey]solana.PublicKeySlice
	tableKeys       []solana.PublicKey

	balance   uint64
	tokenRent uint64

	// The user's token accounts. The wrapped-SOL account is opened when
	// missing and always closed at the end, so its rent is not a cost.
	wsol                 solana.PublicKey
	wsolCreate           bool
	source               solana.PublicKey
	mid, dest            solana.PublicKey
	midBefore, outBefore uint64
	midDecimals          uint8
	wsolBefore           uint64
	creates              []ataCreate

	// hopIn and hopMin are each hop's input and minimum output; the last
	// minimum is the swap's.
	hopIn, hopMin []uint64
	expectedOut   uint64
	midOut        uint64

	basePrice, price   uint64
	neededLimit, limit uint32
}

func (p *swapPlan) closes() bool { return !p.wsol.IsZero() }

// persistentCreates is how many token accounts the swap opens and leaves
// open; their rent is part of the cost.
func (p *swapPlan) persistentCreates() int {
	n := len(p.creates)
	if p.wsolCreate {
		n--
	}
	return n
}

func (p *swapPlan) rent() uint64 { return uint64(p.persistentCreates()) * p.tokenRent }

func (p *swapPlan) networkFee() uint64 { return lamportsPerSignature + priorityFee(p.limit, p.price) }

func (p *swapPlan) cost() uint64 { return p.networkFee() + p.rent() }

type swapStage int

const (
	// stageMid swaps only the first hop of a two-hop route, to learn what it
	// pays.
	stageMid swapStage = iota
	// stageMeasure is the whole swap without the closing of the
	// wrapped-SOL account, which would hide the output of a sale for SOL.
	stageMeasure
	// stageFinal is the message that gets signed.
	stageFinal
)

// instructions lays out the swap for a stage, with the typed Raydium swaps
// in order.
func (p *swapPlan) instructions(stage swapStage) ([]raydium.Instruction, []any, error) {
	limit, price := uint32(maxComputeUnitLimit), uint64(0)
	if stage == stageFinal {
		limit, price = p.limit, p.price
	}
	out := []raydium.Instruction{
		{Program: solana.ComputeBudget, Data: binary.LittleEndian.AppendUint32([]byte{2}, limit)},
		{Program: solana.ComputeBudget, Data: binary.LittleEndian.AppendUint64([]byte{3}, price)},
	}
	for _, create := range p.creates {
		out = append(out, raydium.Instruction{
			Program: solana.SPLAssociatedTokenAccountProgramID,
			Accounts: []solana.AccountMeta{
				{PublicKey: p.user, IsWritable: true, IsSigner: true},
				{PublicKey: create.account, IsWritable: true},
				{PublicKey: p.user},
				{PublicKey: create.mint},
				{PublicKey: solana.SystemProgramID},
				{PublicKey: solana.TokenProgramID},
			},
			Data: []byte{1},
		})
	}
	if p.inSOL {
		out = append(out,
			raydium.Instruction{
				Program:  solana.SystemProgramID,
				Accounts: []solana.AccountMeta{{PublicKey: p.user, IsWritable: true, IsSigner: true}, {PublicKey: p.wsol, IsWritable: true}},
				Data:     binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), p.amountIn),
			},
			raydium.Instruction{
				Program: solana.TokenProgramID, Accounts: []solana.AccountMeta{{PublicKey: p.wsol, IsWritable: true}}, Data: []byte{17},
			},
		)
	}
	var swaps []any
	var failed error
	add := func(hop *swapHopPlan, source, destination solana.PublicKey, amount, minOut uint64) {
		in, typed, err := hop.swap(p.user, source, destination, amount, minOut)
		failed = cmp.Or(failed, err)
		out = append(out, in)
		swaps = append(swaps, typed)
	}
	last := len(p.hops) - 1
	minOut := uint64(1)
	if stage == stageFinal {
		minOut = p.hopMin[last]
	}
	switch {
	case last == 0:
		add(p.hops[0], p.source, p.dest, p.amountIn, minOut)
	case stage == stageMid:
		add(p.hops[0], p.source, p.mid, p.amountIn, 1)
	default:
		add(p.hops[0], p.source, p.mid, p.amountIn, p.hopMin[0])
		add(p.hops[1], p.mid, p.dest, p.hopIn[1], minOut)
	}
	if failed != nil {
		// Not reachable: decodeHop checked the pool holds the hop's mints
		// and picked one to three tick arrays.
		return nil, nil, failed
	}
	if stage == stageFinal && p.closes() {
		out = append(out, raydium.Instruction{
			Program: solana.TokenProgramID,
			Accounts: []solana.AccountMeta{
				{PublicKey: p.wsol, IsWritable: true}, {PublicKey: p.user, IsWritable: true}, {PublicKey: p.user, IsSigner: true},
			},
			Data: []byte{9},
		})
	}
	return out, swaps, nil
}

// message serializes a stage, refusing one that does not fit a transaction.
func (p *swapPlan) message(stage swapStage, blockhash solana.Hash) ([]byte, bool, error) {
	instructions, _, err := p.instructions(stage)
	if err != nil {
		return nil, false, err // not reachable: see instructions
	}
	message, versioned, err := buildMessage(p.user, blockhash, instructions, p.tables)
	if err != nil {
		return nil, false, err // not reachable: see buildMessage
	}
	if len(message)+signedOverhead > maxTransactionSize {
		return nil, false, routeFault(refuse("the swap needs %d bytes, more than one transaction holds; try a direct pair", len(message)+signedOverhead))
	}
	return message, versioned, nil
}

func (p *swapPlan) buildFinal(blockhash solana.Hash) ([]byte, error) {
	message, _, err := p.message(stageFinal, blockhash)
	return message, err
}

// chainTime is the time of the confirmed head block: pools open and close by
// the chain's clock, not this computer's.
func chainTime(ctx context.Context, nodes *nodes) (time.Time, error) {
	var at time.Time
	err := nodes.do(ctx, "getBlockTime", func(client *rpc.Client) error {
		slot, err := client.GetSlot(ctx, readCommitment)
		if err = readable(err); err != nil {
			return err
		}
		blockTime, _, err := headBlockTime(ctx, client, slot)
		if err != nil {
			return err
		}
		if blockTime == nil || *blockTime <= 0 {
			return errors.New("the head block has no time")
		}
		at = blockTime.Time()
		return nil
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("read the chain time: %w", err)
	}
	return at, nil
}

// swapMint is the mint an asset trades as: wrapped SOL for SOL.
func swapMint(asset chain.Asset, networkID string) (solana.PublicKey, bool, error) {
	if asset.NetworkID != networkID {
		return solana.PublicKey{}, false, refuse("asset belongs to another network")
	}
	switch asset.Kind {
	case "native":
		if asset.Decimals != 9 {
			return solana.PublicKey{}, false, refuse("SOL has 9 decimals")
		}
		return solana.WrappedSol, true, nil
	case "spl":
		mint, err := solana.PublicKeyFromBase58(asset.Contract)
		if err != nil {
			return solana.PublicKey{}, false, refuse("invalid SPL mint")
		}
		if mint == solana.WrappedSol {
			return solana.PublicKey{}, false, refuse("swap SOL as the native asset")
		}
		return mint, false, nil
	}
	return solana.PublicKey{}, false, refuse("unsupported Solana asset kind %q", asset.Kind)
}

func associated(owner, mint solana.PublicKey) solana.PublicKey {
	// Cannot fail in practice: it needs all 256 bump seeds on the curve.
	address, _, err := solana.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		panic(err)
	}
	return address
}

// loadSwap reads everything a swap through hops depends on, in one
// getMultipleAccounts (and a rent read when an account has to be opened),
// and decides its accounts.
func (a *Adapter) loadSwap(
	ctx context.Context, nodes *nodes, networkID string, user solana.PublicKey, in, out chain.Asset,
	amountIn uint64, slippage uint16, hops []hopSpec, tables []solana.PublicKey, optionalTables bool, now time.Time,
) (*swapPlan, error) {
	inMint, inSOL, err := swapMint(in, networkID)
	if err != nil {
		return nil, err
	}
	outMint, outSOL, err := swapMint(out, networkID)
	if err != nil {
		return nil, err
	}
	if inMint == outMint {
		return nil, refuse("cannot swap an asset for itself")
	}
	if err := checkHops(hops, inMint, outMint); err != nil {
		return nil, routeFault(err)
	}
	p := &swapPlan{user: user, inMint: inMint, outMint: outMint, inSOL: inSOL, outSOL: outSOL, amountIn: amountIn, slippage: slippage}
	var midMint solana.PublicKey
	if len(hops) == 2 {
		midMint = hops[0].outMint
	}
	usesSOL := inSOL || outSOL || midMint == solana.WrappedSol
	if usesSOL {
		p.wsol = associated(user, solana.WrappedSol)
	}
	p.source = p.wsol
	if !inSOL {
		p.source = associated(user, inMint)
	}
	p.dest = p.wsol
	if !outSOL {
		p.dest = associated(user, outMint)
	}
	if len(hops) == 2 {
		p.mid = p.wsol
		if midMint != solana.WrappedSol {
			p.mid = associated(user, midMint)
		}
	}

	// One read: the wallet, the mints, the pools, their bitmap extensions,
	// the wallet's token accounts and the route's lookup tables.
	keys := []solana.PublicKey{user, inMint, outMint, p.source, p.dest}
	if len(hops) == 2 {
		keys = append(keys, midMint, p.mid)
	}
	if usesSOL {
		keys = append(keys, p.wsol)
	}
	poolAt := len(keys)
	for _, hop := range hops {
		keys = append(keys, hop.pool, raydium.CLMMBitmapExtensionAddress(hop.pool))
	}
	tableAt := len(keys)
	keys = append(keys, tables...)
	accounts, err := readAccounts(ctx, nodes, keys)
	if err != nil {
		return nil, err
	}
	if accounts[0] != nil {
		p.balance = accounts[0].Lamports
	}
	if p.balance == 0 {
		return nil, refuseState("the account holds no SOL to pay the fee")
	}
	for i, asset := range []chain.Asset{in, out} {
		decimals, err := mintDecimals(accounts[1+i])
		if err != nil {
			return nil, err
		}
		if decimals != asset.Decimals {
			return nil, refuse("the mint of %s has %d decimals, the asset says %d", asset.Symbol, decimals, asset.Decimals)
		}
	}
	if len(hops) == 2 {
		if p.midDecimals, err = mintDecimals(accounts[5]); err != nil {
			return nil, routeFault(fmt.Errorf("the intermediate token: %w", err))
		}
	}
	byKey := make(map[solana.PublicKey]*rpc.Account, len(keys))
	for i, key := range keys {
		byKey[key] = accounts[i]
	}

	for i, spec := range hops {
		hop, err := decodeHop(spec, accounts[poolAt+2*i], accounts[poolAt+2*i+1], now)
		if err != nil {
			return nil, routeFault(fmt.Errorf("pool %s: %w", spec.pool, err))
		}
		p.hops = append(p.hops, hop)
	}
	if len(tables) > 0 {
		p.tables = make(map[solana.PublicKey]solana.PublicKeySlice, len(tables))
		for i, table := range tables {
			account := accounts[tableAt+i]
			if account == nil {
				if optionalTables {
					continue
				}
				return nil, refuseState("lookup table %s does not exist", table)
			}
			content, err := decodeLookupTable(account.Owner, account.Data.GetBinary())
			if err != nil {
				if optionalTables {
					continue
				}
				return nil, refuseState("lookup table %s: %v", table, err)
			}
			p.tables[table] = content
			p.tableKeys = append(p.tableKeys, table)
		}
	}

	// The wallet's token accounts. Selling SOL funds the wrapped-SOL account
	// in the swap itself.
	if !inSOL {
		source := byKey[p.source]
		if source == nil {
			return nil, refuseState("the account holds no %s in its main token account", in.Symbol)
		}
		balance, err := tokenAccountBalance(source, inMint, user)
		if err != nil {
			return nil, err
		}
		if balance < amountIn {
			return nil, refuseState("the main token account holds %s %s",
				decimal.NewFromBigInt(new(big.Int).SetUint64(balance), -int32(in.Decimals)), in.Symbol)
		}
	}
	var wsolBalance uint64
	if usesSOL {
		if account := byKey[p.wsol]; account != nil {
			if wsolBalance, err = tokenAccountBalance(account, solana.WrappedSol, user); err != nil {
				return nil, fmt.Errorf("the wrapped SOL account: %w", err)
			}
			p.wsolBefore = wsolBalance
		} else {
			p.wsolCreate = true
			p.creates = append(p.creates, ataCreate{account: p.wsol, mint: solana.WrappedSol})
		}
	}
	balanceOf := func(account solana.PublicKey, mint solana.PublicKey) (uint64, error) {
		if account == p.wsol {
			return wsolBalance, nil
		}
		found := byKey[account]
		if found == nil {
			p.creates = append(p.creates, ataCreate{account: account, mint: mint})
			return 0, nil
		}
		return tokenAccountBalance(found, mint, user)
	}
	if len(hops) == 2 {
		if p.midBefore, err = balanceOf(p.mid, midMint); err != nil {
			// Frozen or otherwise unusable: only this route needs it.
			return nil, routeFault(fmt.Errorf("the intermediate token account: %w", err))
		}
	}
	if p.outBefore, err = balanceOf(p.dest, outMint); err != nil {
		return nil, fmt.Errorf("the receiving token account: %w", err)
	}
	if len(p.creates) > 0 {
		if p.tokenRent, err = a.rentExempt(ctx, nodes, networkID, tokenAccountSize); err != nil {
			return nil, err
		}
	}
	p.hopIn = make([]uint64, len(hops))
	p.hopMin = make([]uint64, len(hops))
	p.hopIn[0] = amountIn
	return p, nil
}

// checkHops checks that a route is one or two distinct pools that connect
// the two mints.
func checkHops(hops []hopSpec, inMint, outMint solana.PublicKey) error {
	if len(hops) == 0 || len(hops) > raydium.MaxHops {
		return refuse("a route has 1 or 2 pools, this one %d", len(hops))
	}
	from := inMint
	for i, hop := range hops {
		if hop.inMint != from || hop.outMint == hop.inMint {
			return refuse("hop %d does not continue the route", i)
		}
		from = hop.outMint
	}
	if from != outMint {
		return refuse("the route does not end at the asset bought")
	}
	if len(hops) == 2 && (hops[0].pool == hops[1].pool || hops[0].outMint == inMint) {
		return refuse("the route goes through the same pool or mint twice")
	}
	// A two-hop swap leaves what its first hop pays beyond the guaranteed
	// part in the wallet, and opens an account for it: only through SOL,
	// USDC or USDT is that worth having.
	if len(hops) == 2 && !intermediates[hops[0].outMint] {
		return refuse("a two-hop route goes through SOL, USDC or USDT, not %s", hops[0].outMint)
	}
	return nil
}

// decodeHop decodes a hop's pool by its owner, checks it trades the hop's two
// mints and allows a swap at the chain's time, and for CLMM picks the tick
// arrays the program will walk.
func decodeHop(spec hopSpec, pool, extension *rpc.Account, now time.Time) (*swapHopPlan, error) {
	if pool == nil {
		return nil, refuseState("the pool does not exist")
	}
	kind := raydium.KindOf(pool.Owner)
	if kind == raydium.KindUnknown {
		return nil, refuse("the pool belongs to program %s, not to Raydium AMM v4, CPMM or CLMM", pool.Owner)
	}
	if spec.kind != raydium.KindUnknown && spec.kind != kind {
		return nil, refuse("the route says %s, the pool is %s", spec.kind, kind)
	}
	hop := &swapHopPlan{hopSpec: spec}
	hop.kind = kind
	data := pool.Data.GetBinary()
	var mints [2]solana.PublicKey
	switch kind {
	case raydium.KindAMMV4:
		decoded, err := raydium.DecodeAMMV4Pool(pool.Owner, data)
		if err != nil {
			return nil, refuse("%w", err)
		}
		if err := decoded.Swappable(now); err != nil {
			return nil, refuseState("%w", err)
		}
		hop.ammv4, mints = &decoded, [2]solana.PublicKey{decoded.CoinMint, decoded.PcMint}
	case raydium.KindCPMM:
		decoded, err := raydium.DecodeCPMMPool(pool.Owner, data)
		if err != nil {
			return nil, refuse("%w", err)
		}
		if err := decoded.Swappable(now); err != nil {
			return nil, refuseState("%w", err)
		}
		hop.cpmm, mints = &decoded, [2]solana.PublicKey{decoded.Token0Mint, decoded.Token1Mint}
	default:
		decoded, err := raydium.DecodeCLMMPool(pool.Owner, data)
		if err != nil {
			return nil, refuse("%w", err)
		}
		if err := decoded.Swappable(now); err != nil {
			return nil, refuseState("%w", err)
		}
		hop.clmm, mints = &decoded, [2]solana.PublicKey{decoded.Mint0, decoded.Mint1}
	}
	if !(mints[0] == spec.inMint && mints[1] == spec.outMint) && !(mints[1] == spec.inMint && mints[0] == spec.outMint) {
		return nil, refuse("the pool does not trade %s for %s", spec.inMint, spec.outMint)
	}
	if hop.clmm == nil {
		if len(spec.remaining) > 0 {
			return nil, refuse("the route gives tick arrays for a %s pool", kind)
		}
		return hop, nil
	}
	if extension != nil {
		decoded, err := raydium.DecodeCLMMBitmapExtension(extension.Owner, extension.Data.GetBinary())
		if err != nil {
			return nil, refuse("the pool's bitmap extension: %w", err)
		}
		if decoded.Pool != spec.pool {
			return nil, refuse("the bitmap extension belongs to pool %s", decoded.Pool)
		}
		hop.extension = &decoded
	}
	zeroForOne, err := hop.clmm.ZeroForOne(spec.inMint)
	if err != nil {
		return nil, refuse("%w", err) // not reachable: the mints were checked above
	}
	if spec.remaining != nil {
		if err := hop.clmm.CheckTickArrays(spec.pool, hop.extension, zeroForOne, spec.remaining); err != nil {
			return nil, refuse("%w", err)
		}
	}
	for n := swapTickArrays; n > 0; n-- {
		hop.tickArrays, err = hop.clmm.TickArrays(spec.pool, hop.extension, zeroForOne, n)
		if !errors.Is(err, raydium.ErrNeedBitmapExtension) {
			break
		}
	}
	if err != nil {
		return nil, refuse("%w", err)
	}
	if len(hop.tickArrays) == 0 {
		return nil, refuseState("the pool has no liquidity in this direction")
	}
	return hop, nil
}

// simulateSwap runs a message without a signature and returns the runtime's
// answer, with the accounts in read as they would be afterwards.
func simulateSwap(ctx context.Context, nodes *nodes, message []byte, read []solana.PublicKey) (*rpc.SimulateTransactionResult, error) {
	raw := append([]byte{1}, make([]byte, ed25519.SignatureSize)...)
	raw = append(raw, message...)
	var result *rpc.SimulateTransactionResponse
	err := nodes.do(ctx, "simulateTransaction", func(client *rpc.Client) error {
		opts := &rpc.SimulateTransactionOpts{Commitment: readCommitment, ReplaceRecentBlockhash: true}
		if len(read) > 0 {
			opts.Accounts = &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: read}
		}
		var callErr error
		result, callErr = client.SimulateRawTransactionWithOpts(ctx, raw, opts)
		if callErr == nil && (result == nil || result.Value == nil) {
			callErr = errors.New("answer has no result")
		}
		if callErr == nil && result.Value.Err == nil && len(result.Value.Accounts) != len(read) {
			callErr = fmt.Errorf("answered %d accounts for %d addresses", len(result.Value.Accounts), len(read))
		}
		return callErr
	})
	if err != nil {
		return nil, fmt.Errorf("simulate the swap: %w", err)
	}
	return result.Value, nil
}

// tokenAfter reads the token amount of one of the wallet's token accounts
// from a simulation's account list.
func tokenAfter(result *rpc.SimulateTransactionResult, index int, mint, owner solana.PublicKey) (uint64, error) {
	account := result.Accounts[index]
	if account == nil {
		return 0, errors.New("simulate the swap: the receiving account would not exist")
	}
	data := account.Data.GetBinary()
	if account.Owner != solana.TokenProgramID || len(data) != tokenAccountSize ||
		solana.PublicKeyFromBytes(data[0:32]) != mint || solana.PublicKeyFromBytes(data[32:64]) != owner {
		return 0, errors.New("simulate the swap: the receiving account is not the wallet's token account")
	}
	return binary.LittleEndian.Uint64(data[64:72]), nil
}

// slippageErrors are the codes each Raydium program fails a swap with when
// it would pay less than the minimum: AMM v4 AmmError::ExceededSlippage
// (program/src/error.rs), CPMM ErrorCode::ExceededSlippage and CLMM
// ErrorCode::TooLittleOutputReceived (Anchor codes from 6000).
var slippageErrors = map[raydium.Kind]int64{raydium.KindAMMV4: 30, raydium.KindCPMM: 6005, raydium.KindCLMM: 6018}

// explainSwap words a runtime refusal of a swap.
func (p *swapPlan) explainSwap(result *rpc.SimulateTransactionResult, message []byte) error {
	if reason, ok := result.Err.(string); ok && reason == "InsufficientFundsForFee" {
		return refuseState("the SOL balance %s does not cover the fee %s", sol(p.balance), sol(p.networkFee()))
	}
	if p.slippageFailure(result.Err, message) {
		return routeFault(refuseState("the pool would now pay less than the minimum%s", lastLog(result.Logs)))
	}
	refusal := refuseState("the network would reject this swap: %s%s", describe(result.Err, message), lastLog(result.Logs))
	_, keys, kind, ok := p.failingInstruction(result.Err, message)
	switch {
	case ok && kind != raydium.KindUnknown:
		// A pool refused: another route may not.
		return routeFault(refusal)
	case ok && keys[0] == solana.SPLAssociatedTokenAccountProgramID && len(keys) > 2 && p.routeOnly(keys[2]):
		// Opening the route's intermediate account failed — for want of
		// its rent, as a rule: a direct pool needs no such account.
		return routeFault(refusal)
	}
	return refusal
}

// failedInstruction reads an InstructionError: the failing instruction's
// error detail and the Raydium kind of its program.
func (p *swapPlan) failedInstruction(failure any, message []byte) (any, raydium.Kind, bool) {
	detail, _, kind, ok := p.failingInstruction(failure, message)
	return detail, kind, ok
}

// failingInstruction is failedInstruction with the failing instruction's
// program and account keys.
func (p *swapPlan) failingInstruction(failure any, message []byte) (any, []solana.PublicKey, raydium.Kind, bool) {
	detail, ok := failure.(map[string]any)
	if !ok {
		return nil, nil, raydium.KindUnknown, false
	}
	pair, ok := detail["InstructionError"].([]any)
	if !ok || len(pair) != 2 {
		return nil, nil, raydium.KindUnknown, false
	}
	index, err := accountIndex(pair[0])
	if err != nil {
		return nil, nil, raydium.KindUnknown, false
	}
	tables := make(map[solana.PublicKey][]solana.PublicKey, len(p.tables))
	for table, content := range p.tables {
		tables[table] = content
	}
	decoded, err := decodeSwapMessage(message, tables)
	if err != nil || index < 0 || int(index) >= len(decoded.instructions) {
		return nil, nil, raydium.KindUnknown, false
	}
	in := decoded.instructions[index]
	keys := []solana.PublicKey{decoded.keys[in.program]}
	for _, account := range in.accounts {
		keys = append(keys, decoded.keys[account])
	}
	return pair[1], keys, raydium.KindOf(keys[0]), true
}

// computeExhausted reports whether a runtime error is an instruction that
// ran out of the transaction's compute limit. A program that does fails
// with ProgramFailedToComplete and logs "exceeded CUs meter at BPF
// instruction" (agave c744af01, program-runtime/src/invoke_context.rs,
// process_executable_chain: a non-syscall EbpfError such as
// ExceededMaxInstructions becomes InstructionError::ProgramFailedToComplete
// after stable_log::program_failure); a builtin that cannot consume its
// units fails with ComputationalBudgetExceeded.
func computeExhausted(failure any, logs []string) bool {
	detail, ok := failure.(map[string]any)
	if !ok {
		return false
	}
	pair, ok := detail["InstructionError"].([]any)
	if !ok || len(pair) != 2 {
		return false
	}
	switch pair[1] {
	case "ComputationalBudgetExceeded":
		return true
	case "ProgramFailedToComplete":
		for _, line := range logs {
			if strings.Contains(line, "exceeded CUs meter") {
				return true
			}
		}
	}
	return false
}

// slippageFailure reports whether a runtime error is a Raydium swap
// refusing to pay less than its minimum.
func (p *swapPlan) slippageFailure(failure any, message []byte) bool {
	detail, kind, ok := p.failedInstruction(failure, message)
	if !ok {
		return false
	}
	custom, ok := detail.(map[string]any)
	if !ok {
		return false
	}
	code, err := accountIndex(custom["Custom"])
	if err != nil {
		return false
	}
	want, known := slippageErrors[kind]
	return known && code == want
}

// measure simulates the swap to learn what it pays and the compute it uses.
// A two-hop swap first learns what its first hop pays, and sells for the
// second hop the part of it that the slippage guarantees.
func (p *swapPlan) measure(ctx context.Context, nodes *nodes) error {
	midMint := solana.PublicKey{}
	if len(p.hops) == 2 {
		midMint = p.hops[0].outMint
		message, _, err := p.message(stageMid, solana.Hash{})
		if err != nil {
			return err
		}
		result, err := simulateSwap(ctx, nodes, message, []solana.PublicKey{p.mid})
		if err != nil {
			return err
		}
		if result.Err != nil {
			return p.explainSwap(result, message)
		}
		after, err := tokenAfter(result, 0, midMint, p.user)
		if err != nil {
			return err
		}
		if after <= p.midBefore {
			return routeFault(refuseState("the first pool would pay nothing"))
		}
		p.midOut = after - p.midBefore
		p.hopMin[0] = withSlippage(p.midOut, p.slippage)
		p.hopIn[1] = p.hopMin[0]
		if p.hopIn[1] == 0 {
			return routeFault(refuseState("the first pool would pay too little to go on"))
		}
	}
	message, _, err := p.message(stageMeasure, solana.Hash{})
	if err != nil {
		return err
	}
	result, err := simulateSwap(ctx, nodes, message, []solana.PublicKey{p.dest})
	if err != nil {
		return err
	}
	if result.Err != nil {
		return p.explainSwap(result, message)
	}
	if result.UnitsConsumed == nil || *result.UnitsConsumed == 0 {
		return errors.New("simulate the swap: the node did not report the compute used")
	}
	after, err := tokenAfter(result, 0, p.outMint, p.user)
	if err != nil {
		return err
	}
	if after <= p.outBefore {
		return routeFault(refuseState("the pool would pay nothing"))
	}
	p.expectedOut = after - p.outBefore
	used := *result.UnitsConsumed
	if p.closes() {
		used += closeComputeAllowance
	}
	p.neededLimit = limitFor(used)
	p.limit = p.neededLimit
	return nil
}

// verdict simulates the final message: the runtime's answer on exactly what
// would be signed. When the output is a token, the simulation also shows it
// paying at least the minimum.
func (p *swapPlan) verdict(ctx context.Context, nodes *nodes, message []byte) (uint64, error) {
	var read []solana.PublicKey
	if !p.outSOL {
		read = []solana.PublicKey{p.dest}
	}
	result, err := simulateSwap(ctx, nodes, message, read)
	if err != nil {
		return 0, err
	}
	if result.Err != nil && computeExhausted(result.Err, result.Logs) {
		var used uint64
		if result.UnitsConsumed != nil {
			used = *result.UnitsConsumed
		}
		return max(used, uint64(p.limit)), fmt.Errorf("%w: it ran out of its %d compute units", errNeedsMoreCompute, p.limit)
	}
	if result.Err != nil {
		return 0, p.explainSwap(result, message)
	}
	if !p.outSOL {
		after, err := tokenAfter(result, 0, p.outMint, p.user)
		if err != nil {
			return 0, err
		}
		minOut := p.hopMin[len(p.hopMin)-1]
		if after < p.outBefore || after-p.outBefore < minOut {
			return 0, routeFault(refuseState("the pool would now pay less than the minimum"))
		}
	}
	var units uint64
	if result.UnitsConsumed != nil {
		units = *result.UnitsConsumed
	}
	return units, nil
}

// precheck refuses a swap the SOL balance cannot pay for: the fee, the rent
// of accounts it opens — the wrapped-SOL account's only until it is closed —
// and the SOL sold.
//
// What only the route needs — an account for its intermediate token — is
// the route's: a refusal for it alone is errRoute, since a direct pool may
// need no such account.
func (p *swapPlan) precheck() error {
	need := p.cost()
	if p.wsolCreate {
		need += p.tokenRent
	}
	if p.inSOL {
		need += p.amountIn
	}
	if p.balance >= need {
		return nil
	}
	refusal := refuseState("the SOL balance %s does not cover %s SOL for the swap, its fee and new token accounts: add at least %s SOL",
		sol(p.balance), sol(need), sol(need-p.balance))
	if p.balance >= need-p.routeOnlyRent() {
		return routeFault(refusal)
	}
	return refusal
}

// routeOnly reports whether account is the intermediate token account of a
// two-hop route: an account a direct swap would not touch. (The
// intermediate is never the token sold or bought, so when it is SOL the
// wrapped-SOL account is the route's alone.)
func (p *swapPlan) routeOnly(account solana.PublicKey) bool {
	return len(p.hops) == 2 && account == p.mid
}

// routeOnlyRent is the rent the swap needs only for its intermediate
// account, kept or — wrapped SOL — returned at the end.
func (p *swapPlan) routeOnlyRent() uint64 {
	for _, create := range p.creates {
		if p.routeOnly(create.account) {
			return p.tokenRent
		}
	}
	return 0
}

// priority reads the recent priority price for the accounts the swap writes.
func (p *swapPlan) priority(ctx context.Context, nodes *nodes) error {
	writable := []solana.PublicKey{p.user}
	for _, hop := range p.hops {
		writable = append(writable, hop.pool)
		writable = append(writable, hop.vaults()...)
	}
	var err error
	if p.basePrice, err = priorityPrice(ctx, nodes, writable); err != nil {
		return err
	}
	p.price = offeredPrice(p.basePrice)
	return nil
}

// withSlippage is floor(amount × (10000 − bps) / 10000).
func withSlippage(amount uint64, bps uint16) uint64 {
	v := new(big.Int).SetUint64(amount)
	v.Mul(v, big.NewInt(int64(10_000-int(bps))))
	return v.Quo(v, big.NewInt(10_000)).Uint64()
}

func amountText(value uint64, decimals uint8) string {
	return decimal.NewFromBigInt(new(big.Int).SetUint64(value), -int32(decimals)).String()
}

// swapContext is what both QuoteSwap and Swap check first.
func (a *Adapter) swapContext(ctx context.Context, networkID, from string, slippage uint16) (network.Network, *nodes, solana.PublicKey, error) {
	item, nodes, err := a.client(ctx, networkID)
	if err != nil {
		return network.Network{}, nil, solana.PublicKey{}, err
	}
	if item.ChainID != "mainnet" {
		return network.Network{}, nil, solana.PublicKey{}, refuse("swaps are available on Solana mainnet only")
	}
	user, err := solana.PublicKeyFromBase58(from)
	if err != nil {
		return network.Network{}, nil, solana.PublicKey{}, refuse("invalid sender address")
	}
	if slippage == 0 || slippage > MaxSwapSlippageBps {
		return network.Network{}, nil, solana.PublicKey{}, refuse("slippage must be between 1 and %d basis points", MaxSwapSlippageBps)
	}
	return item, nodes, user, nil
}

// QuoteSwap prices a swap: a route from the Raydium Trade API — or, when the
// API does not answer with one, the deepest Raydium pool found on chain — is
// built from the pools' state, simulated to learn exactly what it pays, and
// simulated again as the message that would be signed.
func (a *Adapter) QuoteSwap(ctx context.Context, networkID string, req SwapQuoteRequest) (SwapQuote, error) {
	item, nodes, user, err := a.swapContext(ctx, networkID, req.From, req.SlippageBps)
	if err != nil {
		return SwapQuote{}, err
	}
	inMint, _, err := swapMint(req.In, networkID)
	if err != nil {
		return SwapQuote{}, err
	}
	outMint, _, err := swapMint(req.Out, networkID)
	if err != nil {
		return SwapQuote{}, err
	}
	if inMint == outMint {
		return SwapQuote{}, refuse("cannot swap an asset for itself")
	}
	amountIn, err := units(req.Amount, req.In.Decimals)
	if err != nil {
		return SwapQuote{}, err
	}
	now, err := chainTime(ctx, nodes)
	if err != nil {
		return SwapQuote{}, err
	}
	quote := SwapQuote{NetworkID: networkID, AmountIn: amountText(amountIn, req.In.Decimals), SlippageBps: req.SlippageBps}

	quoter, err := a.raydiumQuoter(item)
	if err != nil {
		// Not reachable: the network's RPC already runs over the same
		// provider client, which passed the same check.
		return SwapQuote{}, err
	}
	route, apiErr := quoter.Quote(ctx, inMint, outMint, amountIn, req.SlippageBps)
	if apiErr == nil {
		quoted, err := a.quoteAPIRoute(ctx, nodes, quoter, route, user, req, amountIn, now, quote)
		if err == nil || !errors.Is(err, errRoute) {
			// A refusal of the wallet's own — its balance, its fee, a
			// token it cannot trade — would follow it to any route.
			return quoted, err
		}
		apiErr = err
	}
	quote.APIRouteRejected = apiErr.Error()
	if ctx.Err() != nil {
		return SwapQuote{}, ctx.Err()
	}
	// The API is down, refused, or answered with nothing usable: find the
	// pools on chain instead.
	candidates, err := raydium.Discover(ctx, discoveryRPC{nodes: nodes}, inMint, outMint, now)
	if err != nil {
		if errors.Is(err, raydium.ErrNoPool) {
			return SwapQuote{}, refuseState("no Raydium pool trades this pair (the Trade API: %v)", apiErr)
		}
		return SwapQuote{}, fmt.Errorf("find Raydium pools (the Trade API: %v): %w", apiErr, err)
	}
	var firstErr error
	for _, candidate := range candidates[:min(len(candidates), maxDiscoveryCandidates)] {
		hops := []hopSpec{{pool: candidate.Pool, inMint: inMint, outMint: outMint, kind: candidate.Kind}}
		p, err := a.loadSwap(ctx, nodes, networkID, user, req.In, req.Out, amountIn, req.SlippageBps, hops, nil, false, now)
		if err == nil {
			attempt := quote
			attempt.Source = SwapSourceDiscovery
			if err = a.finishQuote(ctx, nodes, p, &attempt, req.Out.Decimals); err == nil {
				return attempt, nil
			}
		}
		if ctx.Err() != nil {
			return SwapQuote{}, ctx.Err()
		}
		if !errors.Is(err, errRoute) {
			return SwapQuote{}, err
		}
		firstErr = cmpErr(firstErr, err)
	}
	return SwapQuote{}, fmt.Errorf("%w (the Trade API: %v)", firstErr, apiErr)
}

// quoteAPIRoute builds and measures the Trade API's route. Refusals caused
// by the route itself are marked errRoute: the quote then looks for a pool
// on chain instead.
func (a *Adapter) quoteAPIRoute(
	ctx context.Context, nodes *nodes, quoter *raydium.Quoter, route raydium.Route, user solana.PublicKey,
	req SwapQuoteRequest, amountIn uint64, now time.Time, quote SwapQuote,
) (SwapQuote, error) {
	hops := make([]hopSpec, len(route.Hops))
	var pools []solana.PublicKey
	for i, hop := range route.Hops {
		hops[i] = hopSpec{
			pool: hop.Pool, inMint: hop.InputMint, outMint: hop.OutputMint,
			kind: raydium.KindOf(hop.Program), remaining: hop.RemainingAccounts,
		}
		pools = append(pools, hop.Pool)
	}
	// A route that will be refused does not spend a paced pool-keys
	// request.
	inMint, _, _ := swapMint(req.In, quote.NetworkID)
	outMint, _, _ := swapMint(req.Out, quote.NetworkID)
	if err := checkHops(hops, inMint, outMint); err != nil {
		return SwapQuote{}, routeFault(err)
	}
	var tables []solana.PublicKey
	named, tablesErr := quoter.LookupTables(ctx, pools)
	for _, pool := range pools {
		if table, ok := named[pool]; ok && !containsKey(tables, table) {
			tables = append(tables, table)
		}
	}
	withTables := func(err error) error {
		if err != nil && tablesErr != nil {
			// The route may have failed for want of its tables: say why
			// there were none.
			return fmt.Errorf("%w (lookup tables: %v)", err, tablesErr)
		}
		return err
	}
	p, err := a.loadSwap(ctx, nodes, quote.NetworkID, user, req.In, req.Out, amountIn, req.SlippageBps, hops, tables, true, now)
	if err != nil {
		return SwapQuote{}, withTables(err)
	}
	if err := a.checkQuotedOut(ctx, nodes, p, route.AmountOut); err != nil {
		return SwapQuote{}, err
	}
	quote.Source = SwapSourceAPI
	quote.QuotedOut = amountText(route.AmountOut, req.Out.Decimals)
	quote.PriceImpactPct = strconv.FormatFloat(route.PriceImpactPct, 'f', -1, 64)
	if err := a.finishQuote(ctx, nodes, p, &quote, req.Out.Decimals); err != nil {
		return SwapQuote{}, withTables(err)
	}
	if len(p.hops) == 1 {
		deviation := new(big.Rat).SetFrac(
			new(big.Int).Sub(new(big.Int).SetUint64(route.AmountOut), new(big.Int).SetUint64(p.expectedOut)),
			new(big.Int).SetUint64(route.AmountOut),
		)
		quote.QuoteDeviationPct = deviation.Mul(deviation, big.NewRat(100, 1)).FloatString(4)
	}
	return quote, nil
}

func cmpErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

func containsKey(keys []solana.PublicKey, key solana.PublicKey) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// checkQuotedOut compares the API's output for a route of AMM v4 and CPMM
// pools with what their own curves give for the vault balances on chain. A
// route with a CLMM hop is left to the simulation.
func (a *Adapter) checkQuotedOut(ctx context.Context, nodes *nodes, p *swapPlan, quoted uint64) error {
	var keys []solana.PublicKey
	for _, hop := range p.hops {
		switch {
		case hop.clmm != nil:
			return nil
		case hop.cpmm != nil:
			keys = append(keys, hop.cpmm.AmmConfig)
		}
		keys = append(keys, hop.vaults()...)
	}
	accounts, err := readAccounts(ctx, nodes, keys)
	if err != nil {
		return err
	}
	amount := p.amountIn
	next := 0
	vaultAmount := func() (uint64, error) {
		account := accounts[next]
		next++
		if account == nil || account.Owner != solana.TokenProgramID || len(account.Data.GetBinary()) != tokenAccountSize {
			return 0, routeFault(refuse("a pool vault is not a token account"))
		}
		return binary.LittleEndian.Uint64(account.Data.GetBinary()[64:72]), nil
	}
	for _, hop := range p.hops {
		var config raydium.CPMMConfig
		if hop.cpmm != nil {
			account := accounts[next]
			next++
			if account == nil {
				return routeFault(refuse("the CPMM pool's config does not exist"))
			}
			if config, err = raydium.DecodeCPMMConfig(account.Owner, account.Data.GetBinary()); err != nil {
				return routeFault(refuse("%w", err))
			}
		}
		first, err := vaultAmount()
		if err != nil {
			return err
		}
		second, err := vaultAmount()
		if err != nil {
			return err
		}
		if hop.ammv4 != nil {
			amount, err = hop.ammv4.AmountOut(hop.inMint, amount, first, second)
		} else {
			amount, err = hop.cpmm.AmountOut(config, hop.inMint, amount, first, second)
		}
		if err != nil {
			return routeFault(refuseState("%w", err))
		}
	}
	if err := raydium.CheckQuotedOut(amount, quoted, swapQuoteDeviationBps); err != nil {
		return routeFault(refuse("%w", err))
	}
	return nil
}

// finishQuote prices, measures and simulates a loaded plan and fills in the
// quote.
func (a *Adapter) finishQuote(ctx context.Context, nodes *nodes, p *swapPlan, quote *SwapQuote, outDecimals uint8) error {
	if err := p.priority(ctx, nodes); err != nil {
		return err
	}
	if err := p.measure(ctx, nodes); err != nil {
		return err
	}
	p.hopMin[len(p.hopMin)-1] = withSlippage(p.expectedOut, p.slippage)
	if p.hopMin[len(p.hopMin)-1] == 0 {
		return routeFault(refuse("the amount is too small to swap"))
	}
	if err := p.precheck(); err != nil {
		return err
	}
	message, versioned, err := p.message(stageFinal, solana.Hash{})
	if err != nil {
		// Not reachable: the final message adds to the measured one only a
		// CloseAccount that the measured one's size check already allowed for
		// in practice (a few bytes); kept for a route at the very edge.
		return err
	}
	units, err := p.verdict(ctx, nodes, message)
	if err != nil {
		if !errors.Is(err, errNeedsMoreCompute) {
			return err
		}
		// The final message ran out of the compute the measure suggested:
		// raise the limit once.
		p.neededLimit = limitFor(units)
		p.limit = p.neededLimit
		if err := p.precheck(); err != nil {
			return err
		}
		if message, versioned, err = p.message(stageFinal, solana.Hash{}); err != nil {
			return err // not reachable: the same message as above with another limit
		}
		if _, err := p.verdict(ctx, nodes, message); err != nil {
			if errors.Is(err, errNeedsMoreCompute) {
				return routeFault(err)
			}
			return err
		}
	}
	if !versioned {
		// A legacy message loads nothing from tables: the swap does not
		// depend on them.
		p.tables, p.tableKeys = nil, nil
	}
	quote.AmountOut = amountText(p.expectedOut, outDecimals)
	quote.MinOut = amountText(p.hopMin[len(p.hopMin)-1], outDecimals)
	quote.Route = p.route()
	quote.MessageVersion = "legacy"
	if versioned {
		quote.MessageVersion = "v0"
	}
	quote.MessageSize = len(message) + signedOverhead
	quote.FeeModel = feeModel
	quote.Fee = sol(p.cost())
	quote.ComputeUnitLimit = uint64(p.limit)
	quote.ComputeUnitPriceMicroLamports = p.price
	quote.TokenAccountsCreated = p.persistentCreates()
	if quote.TokenAccountsCreated > 0 {
		quote.TokenAccountRent = sol(p.rent())
	}
	quote.NativeBalance = sol(p.balance)
	if p.wsolBefore > 0 {
		quote.UnwrapsWrappedSOL = sol(p.wsolBefore)
	}
	if len(p.hops) == 2 {
		mid := p.hops[0].outMint
		quote.IntermediateMint = mid.String()
		quote.IntermediateLeftover = amountText(p.midOut-p.hopIn[1], p.midDecimals)
	}
	return nil
}

func (p *swapPlan) route() SwapRoute {
	route := SwapRoute{}
	for _, hop := range p.hops {
		route.Hops = append(route.Hops, SwapHop{
			Pool: hop.pool.String(), Kind: hop.kind.String(), InputMint: hop.inMint.String(), OutputMint: hop.outMint.String(),
		})
	}
	for _, table := range p.tableKeys {
		route.LookupTables = append(route.LookupTables, table.String())
	}
	return route
}

// parseSwapRoute reads a route the client echoed back.
func parseSwapRoute(route SwapRoute) ([]hopSpec, []solana.PublicKey, error) {
	if len(route.Hops) == 0 || len(route.Hops) > raydium.MaxHops {
		return nil, nil, refuse("a route has 1 or 2 pools")
	}
	kinds := map[string]raydium.Kind{"amm-v4": raydium.KindAMMV4, "cpmm": raydium.KindCPMM, "clmm": raydium.KindCLMM}
	var hops []hopSpec
	for _, hop := range route.Hops {
		kind, ok := kinds[hop.Kind]
		if !ok {
			return nil, nil, refuse("unknown pool kind %q", hop.Kind)
		}
		pool, err1 := solana.PublicKeyFromBase58(hop.Pool)
		in, err2 := solana.PublicKeyFromBase58(hop.InputMint)
		out, err3 := solana.PublicKeyFromBase58(hop.OutputMint)
		if err := errors.Join(err1, err2, err3); err != nil {
			return nil, nil, refuse("the route names an invalid address")
		}
		hops = append(hops, hopSpec{pool: pool, inMint: in, outMint: out, kind: kind})
	}
	if len(route.LookupTables) > raydium.MaxHops {
		return nil, nil, refuse("a route names at most %d lookup tables", raydium.MaxHops)
	}
	var tables []solana.PublicKey
	for _, text := range route.LookupTables {
		table, err := solana.PublicKeyFromBase58(text)
		if err != nil || containsKey(tables, table) {
			return nil, nil, refuse("the route names an invalid lookup table")
		}
		tables = append(tables, table)
	}
	return hops, tables, nil
}

type swapApproval struct {
	limit   uint32
	price   uint64
	creates int
	rent    uint64
	fee     uint64
}

func parseSwapApproval(approved SwapApproval) (swapApproval, error) {
	if approved.ComputeUnitLimit == 0 || approved.ComputeUnitLimit > maxComputeUnitLimit {
		return swapApproval{}, refuse("the approved compute-unit limit is out of range")
	}
	if approved.ComputeUnitPriceMicroLamports > maxComputeUnitPrice {
		return swapApproval{}, refuse("the approved compute-unit price is above the %d micro-lamport ceiling", maxComputeUnitPrice)
	}
	if approved.TokenAccountsCreated < 0 || approved.TokenAccountsCreated > raydium.MaxHops {
		return swapApproval{}, refuse("the approved number of new token accounts is out of range")
	}
	out := swapApproval{
		limit: uint32(approved.ComputeUnitLimit), price: approved.ComputeUnitPriceMicroLamports, creates: approved.TokenAccountsCreated,
	}
	var err error
	if out.creates > 0 {
		if out.rent, err = lamports(approved.TokenAccountRent); err != nil || out.rent == 0 {
			return swapApproval{}, refuse("the approved token account rent is not an amount")
		}
	} else if approved.TokenAccountRent != "" {
		if rent, err := lamports(approved.TokenAccountRent); err != nil || rent != 0 {
			return swapApproval{}, refuse("a token account rent was approved without opening an account")
		}
	}
	if out.fee, err = lamports(approved.Fee); err != nil {
		return swapApproval{}, refuse("the approved fee is not an amount")
	}
	if total := lamportsPerSignature + priorityFee(out.limit, out.price) + out.rent; total != out.fee {
		return swapApproval{}, refuse("the approved fee %s does not add up to its parts %s", sol(out.fee), sol(total))
	}
	return out, nil
}

// Swap builds, checks, signs and broadcasts a swap the user approved.
//
// The route's pools are read again and the swap rebuilt from their state; it
// is simulated to learn what it pays now. It is refused with ErrStateChanged
// when that is below the approved minimum, or when the minimum is more than
// MaxSwapSlippageBps below it; with ErrFeeChanged when it needs more compute,
// a higher priority price or more new accounts than approved. The approved
// limit, price and minimum are what get signed. The message is decoded on its
// own and compared with the plan — lookup-table entries resolved against the
// tables' contents on chain — and simulated once more before the signature.
// The rest is as for Send: the expiry height goes to BeforeSigning, the
// transaction is broadcast once to the preferred node, and a lost answer is
// ErrBroadcastUnknown.
func (a *Adapter) Swap(ctx context.Context, networkID string, req SwapRequest, key chain.Key) (chain.Transaction, error) {
	signer, err := messageSigner(key, req.From)
	if err != nil {
		return chain.Transaction{}, err
	}
	approved, err := parseSwapApproval(req.Approved)
	if err != nil {
		return chain.Transaction{}, err
	}
	hops, tables, err := parseSwapRoute(req.Route)
	if err != nil {
		return chain.Transaction{}, err
	}
	_, nodes, user, err := a.swapContext(ctx, networkID, req.From, req.SlippageBps)
	if err != nil {
		return chain.Transaction{}, err
	}
	amountIn, err := units(req.Amount, req.In.Decimals)
	if err != nil {
		return chain.Transaction{}, err
	}
	minOut, err := units(req.MinOut, req.Out.Decimals)
	if err != nil {
		return chain.Transaction{}, refuse("the minimum output must be a positive amount")
	}
	now, err := chainTime(ctx, nodes)
	if err != nil {
		return chain.Transaction{}, err
	}
	p, err := a.loadSwap(ctx, nodes, networkID, user, req.In, req.Out, amountIn, req.SlippageBps, hops, tables, false, now)
	if err != nil {
		return chain.Transaction{}, stateChanged(err)
	}
	if err := p.priority(ctx, nodes); err != nil {
		return chain.Transaction{}, err
	}
	if err := p.measure(ctx, nodes); err != nil {
		return chain.Transaction{}, stateChanged(err)
	}
	if p.expectedOut < minOut {
		return chain.Transaction{}, fmt.Errorf("%w: the swap now pays %s, below the approved minimum of %s; review the swap again",
			chain.ErrStateChanged, amountText(p.expectedOut, req.Out.Decimals), amountText(minOut, req.Out.Decimals))
	}
	if minOut < withSlippage(p.expectedOut, MaxSwapSlippageBps) {
		return chain.Transaction{}, fmt.Errorf("%w: the approved minimum of %s is more than %s below the current output; review the swap again",
			chain.ErrStateChanged, amountText(minOut, req.Out.Decimals), decimal.New(MaxSwapSlippageBps, -2).String()+"%")
	}
	switch {
	case p.neededLimit > approved.limit:
		return chain.Transaction{}, fmt.Errorf("%w: the swap now needs %d compute units, %d were approved", chain.ErrFeeChanged, p.neededLimit, approved.limit)
	case p.basePrice > approved.price:
		return chain.Transaction{}, fmt.Errorf("%w: the priority price is now %d micro-lamports, %d was approved", chain.ErrFeeChanged, p.basePrice, approved.price)
	case p.persistentCreates() > approved.creates:
		return chain.Transaction{}, fmt.Errorf("%w: the swap now opens %d token accounts, %d were approved", chain.ErrFeeChanged, p.persistentCreates(), approved.creates)
	case p.rent() > approved.rent:
		return chain.Transaction{}, fmt.Errorf("%w: opening token accounts now costs %s SOL, %s was approved", chain.ErrFeeChanged, sol(p.rent()), sol(approved.rent))
	}
	p.limit, p.price = approved.limit, approved.price
	p.hopMin[len(p.hopMin)-1] = minOut
	if err := p.precheck(); err != nil {
		return chain.Transaction{}, stateChanged(err)
	}

	blockhash, lastValid, err := latestBlockhash(ctx, nodes)
	if err != nil {
		return chain.Transaction{}, err
	}
	message, err := a.swaps.finalBuilder()(p, blockhash)
	if err != nil {
		return chain.Transaction{}, err
	}
	want, swaps, err := p.instructions(stageFinal)
	if err != nil {
		return chain.Transaction{}, err // not reachable: see instructions
	}
	contents := make(map[solana.PublicKey][]solana.PublicKey, len(p.tables))
	for table, content := range p.tables {
		contents[table] = content
	}
	if err := verifySwapMessage(message, p.user, blockhash, want, swaps, contents); err != nil {
		return chain.Transaction{}, err
	}
	if _, err := p.verdict(ctx, nodes, message); err != nil {
		if errors.Is(err, errNeedsMoreCompute) {
			return chain.Transaction{}, fmt.Errorf("%w: %w", chain.ErrFeeChanged, err)
		}
		return chain.Transaction{}, stateChanged(err)
	}
	return a.signAndBroadcast(ctx, networkID, nodes, message, lastValid, signer, req.BeforeSigning, req.BeforeBroadcast)
}
