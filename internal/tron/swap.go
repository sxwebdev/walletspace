package tron

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/address"
	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/pkg/tronutils"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/gotron/schema/pb/core"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/tron/sunswap"
	"golang.org/x/sync/errgroup"
)

// SwapProtocol names the SunSwap generation a route goes through.
type SwapProtocol string

const (
	SwapV2 SwapProtocol = "v2"
	SwapV3 SwapProtocol = "v3"
	SwapV4 SwapProtocol = "v4"
)

// SwapRoute is one pool a swap can go through.
type SwapRoute struct {
	Protocol SwapProtocol `json:"protocol"`
	// Fee is the pool fee in millionths: 3000 is 0.3%. V2 pools always charge
	// 0.3%.
	Fee uint32 `json:"fee"`
	// TickSpacing completes a V4 pool's identity; zero for V2 and V3.
	TickSpacing int `json:"tick_spacing,omitzero"`
}

// SwapStepKind is one transaction of a swap.
type SwapStepKind string

const (
	// SwapStepApprove lets the router (V2, V3) or Permit2 (V4) draw USDT.
	SwapStepApprove SwapStepKind = "approve"
	// SwapStepPermit2 lets the V4 universal router draw through Permit2.
	SwapStepPermit2 SwapStepKind = "permit2"
	// SwapStepSwap is the trade itself.
	SwapStepSwap SwapStepKind = "swap"
)

// SwapStep is what one transaction of a swap is expected to cost.
type SwapStep struct {
	Kind SwapStepKind `json:"kind"`
	// Contract is the contract the step calls.
	Contract string `json:"contract"`
	// Energy is what the call consumes in total, whoever pays for it.
	Energy int64 `json:"energy"`
	// Measured is set when Energy came from running this exact call against
	// the chain. A swap that still waits for its approvals cannot be run yet —
	// the router could not draw the tokens — so its energy is the typical
	// figure for the route instead.
	Measured bool `json:"measured"`
	// Fee is the TRX the step burns given the account's staked energy and
	// bandwidth, which the steps before it have already drawn on.
	Fee decimal.Decimal `json:"fee"`
}

// SwapQuote is what one route pays and costs.
type SwapQuote struct {
	Route     SwapRoute       `json:"route"`
	AmountOut decimal.Decimal `json:"amount_out"`
	Steps     []SwapStep      `json:"steps"`
	// Fee is the TRX all the steps burn together.
	Fee decimal.Decimal `json:"fee"`
	// Value is AmountOut less Fee, both in the asset bought: the figure the
	// routes are ranked by. Approvals cost several TRX each, so on a small swap
	// the route with the best price can still be the worse deal.
	Value decimal.Decimal `json:"value"`
}

// SwapQuoteRequest asks what selling Amount of In would return.
type SwapQuoteRequest struct {
	In     Asset
	Amount decimal.Decimal
	// Unlimited approves the router for any amount instead of exactly this
	// one. It only changes what an approval step signs, not whether one is
	// needed now.
	Unlimited bool
}

// SwapQuotes are the routes for one swap, best first.
type SwapQuotes struct {
	In       Asset           `json:"in"`
	Out      Asset           `json:"out"`
	AmountIn decimal.Decimal `json:"amount_in"`
	Quotes   []SwapQuote     `json:"quotes"`
	// Unavailable names the routes that could not be quoted and why.
	Unavailable []string `json:"unavailable,omitzero"`
	// Shortfall is the TRX the account is missing for the best route: its fees,
	// plus the amount sold when selling TRX. Zero when it is covered.
	Shortfall decimal.Decimal `json:"shortfall"`
}

// SwapRequest is one step of a swap the user approved.
type SwapRequest struct {
	In     Asset
	Amount decimal.Decimal
	// MinOut is the least the swap may pay out, as the user saw it. It is what
	// gets signed: the chain reverts the swap below it.
	MinOut    decimal.Decimal
	Route     SwapRoute
	Step      SwapStepKind
	Unlimited bool
}

const (
	// swapDeadline is how long a signed swap stays executable. The step is
	// signed right after it is built, so this only has to outlast a slow node.
	swapDeadline = 3 * time.Minute
	// permit2Lifetime is how long an exact Permit2 allowance lasts. It is spent
	// by the swap that follows within minutes; an unspent one should not
	// linger as authority.
	permit2Lifetime = time.Hour
	// permit2UnlimitedLifetime matches what wallets ask for when the user opts
	// into a standing approval.
	permit2UnlimitedLifetime = 30 * 24 * time.Hour
	// maxSwapSlippage is the furthest below the current price a signed minimum
	// may sit. The minimum is what protects a swap from being traded against;
	// one far below the market invites exactly that.
	maxSwapSlippageBps = 500
	// feeLimitHeadroom pads the measured energy of a call into its fee limit.
	// The limit caps energy rather than charging it, so headroom costs nothing
	// unless the call really needs it.
	feeLimitHeadroom = 1.3
	// swapTxOverhead approximates a contract call's bytes beyond its calldata —
	// addresses, header, signature — for the bandwidth estimate.
	swapTxOverhead = 300
)

var (
	// maxUint256 is an unlimited ERC20 approval.
	maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	// maxUint160 is an unlimited Permit2 allowance.
	maxUint160 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
)

// typicalSwapEnergy is what a 1000-unit swap on each route consumed on mainnet
// in October 2026, measured by simulating the calls this package builds and
// rounded up, with recent router receipts as a cross-check. It prices a swap
// that cannot be simulated yet because its approvals are not on chain; the
// real call is simulated again, and its fee limit set from that, before it is
// signed.
var typicalSwapEnergy = map[SwapProtocol]map[bool]int64{
	// keyed by whether TRX is the input
	SwapV2: {true: 160_000, false: 170_000},
	SwapV3: {true: 200_000, false: 190_000},
	SwapV4: {true: 275_000, false: 160_000},
}

// swapper is SunSwap on the service's network.
type swapper struct {
	deployment sunswap.Deployment
	addrs      sunswap.Addresses
}

func newSwapper(network, token string) *swapper {
	deployment, ok := sunswap.ForNetwork(network)
	// The configured token is what balances and transfers call USDT. A swap
	// against any other contract would trade something the wallet does not
	// show, so a mismatch simply turns swapping off.
	if !ok || deployment.Token != token {
		return nil
	}
	addrs, err := deployment.Addresses()
	if err != nil {
		return nil
	}
	return &swapper{deployment: deployment, addrs: addrs}
}

// SwapAvailable reports whether this network has SunSwap configured for the
// service's token.
func (s *Service) SwapAvailable() bool { return s.swap != nil }

// swapPlan is a swap with every amount in base units.
type swapPlan struct {
	from      string
	owner     common.Address
	in        Asset
	amountIn  *big.Int
	minOut    *big.Int
	route     SwapRoute
	unlimited bool
	now       time.Time
}

func (p swapPlan) trxIn() bool { return p.in == AssetTRX }

// swapCall is one transaction a swap still needs.
type swapCall struct {
	kind      SwapStepKind
	contract  string
	call      sunswap.Calldata
	callValue int64
}

func (s *Service) newSwapPlan(from string, in Asset, amount decimal.Decimal) (swapPlan, error) {
	if s.swap == nil {
		return swapPlan{}, fmt.Errorf("%w: swaps are not available on this network", ErrInvalidRequest)
	}
	if err := address.Validate(from); err != nil {
		return swapPlan{}, fmt.Errorf("%w: invalid address: %s", ErrInvalidRequest, err)
	}
	owner, err := sunswap.EVMAddress(from)
	if err != nil {
		return swapPlan{}, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}
	if amount.LessThanOrEqual(decimal.Zero) {
		return swapPlan{}, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidRequest)
	}
	units, err := s.swapUnits(in, amount)
	if err != nil {
		return swapPlan{}, err
	}
	return swapPlan{from: from, owner: owner, in: in, amountIn: units, now: time.Now()}, nil
}

// swapUnits converts an amount of asset to base units, refusing more decimals
// than the asset has.
func (s *Service) swapUnits(asset Asset, amount decimal.Decimal) (*big.Int, error) {
	switch asset {
	case AssetTRX:
		sun, err := trxAmount(amount)
		if err != nil {
			return nil, err
		}
		return big.NewInt(int64(sun)), nil
	case AssetUSDT:
		tokens, err := tokenAmount(amount, s.token.Decimals)
		if err != nil {
			return nil, err
		}
		return tokens.TokenUnits(), nil
	default:
		return nil, fmt.Errorf("%w: unknown asset %q", ErrInvalidRequest, asset)
	}
}

func (s *Service) swapDecimal(asset Asset, units *big.Int) decimal.Decimal {
	if asset == AssetTRX {
		return decimal.NewFromBigInt(units, -6)
	}
	return decimal.NewFromBigInt(units, -s.token.Decimals)
}

func swapOut(in Asset) Asset {
	if in == AssetTRX {
		return AssetUSDT
	}
	return AssetTRX
}

// swapRoutes lists every route the deployment offers.
func (s *Service) swapRoutes() []SwapRoute {
	routes := []SwapRoute{{Protocol: SwapV2, Fee: 3000}}
	for _, fee := range s.swap.deployment.V3Fees {
		routes = append(routes, SwapRoute{Protocol: SwapV3, Fee: fee})
	}
	for _, pool := range s.swap.deployment.V4Pools {
		routes = append(routes, SwapRoute{Protocol: SwapV4, Fee: pool.Fee, TickSpacing: pool.TickSpacing})
	}
	return routes
}

// knownRoute refuses a route the deployment does not list. A request names a
// route; it never names a contract.
func (s *Service) knownRoute(route SwapRoute) error {
	if slices.Contains(s.swapRoutes(), route) {
		return nil
	}
	return fmt.Errorf("%w: unknown swap route %s %d", ErrInvalidRequest, route.Protocol, route.Fee)
}

// QuoteSwap prices selling an amount of TRX for USDT or the other way round on
// every SunSwap route, and lists the transactions each would take.
func (s *Service) QuoteSwap(ctx context.Context, from string, req SwapQuoteRequest) (SwapQuotes, error) {
	plan, err := s.newSwapPlan(from, req.In, req.Amount)
	if err != nil {
		return SwapQuotes{}, err
	}
	plan.unlimited = req.Unlimited
	out := swapOut(req.In)
	// Routes share their reads: the V3 fee tiers ask about the same router
	// allowance, and every USDT route simulates an approval of the same token.
	ctx = withCallMemo(ctx)

	routes := s.swapRoutes()
	amounts := make([]*big.Int, len(routes))
	failures := make([]error, len(routes))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(s.workers)
	for i, route := range routes {
		group.Go(func() error {
			amounts[i], failures[i] = s.quoteRoute(groupCtx, route, plan.trxIn(), plan.amountIn)
			return nil
		})
	}
	var (
		account *api.AccountResourceMessage
		params  *client.ChainParams
		balance decimal.Decimal
	)
	group.Go(func() error {
		var err error
		account, err = retry(groupCtx, s.nodes, emptyIfMissing(func() (*api.AccountResourceMessage, error) {
			return s.client.GetAccountResource(groupCtx, from)
		}))
		return err
	})
	group.Go(func() error {
		var err error
		params, err = retry(groupCtx, s.nodes, func() (*client.ChainParams, error) {
			return s.client.ChainParams(groupCtx)
		})
		return err
	})
	group.Go(func() error {
		balances, errs := s.Balances(groupCtx, []string{from}, false)
		if err := errs[from]; err != nil {
			return err
		}
		balance = balances[from].TRX
		return nil
	})
	if err := group.Wait(); err != nil {
		return SwapQuotes{}, s.chainError("quote swap", err)
	}

	result := SwapQuotes{In: req.In, Out: out, AmountIn: req.Amount}
	quotes := make([]*SwapQuote, len(routes))
	group, groupCtx = errgroup.WithContext(ctx)
	group.SetLimit(s.workers)
	for i, route := range routes {
		if failures[i] != nil || amounts[i].Sign() <= 0 {
			continue
		}
		routePlan := plan
		routePlan.route = route
		group.Go(func() error {
			quote, err := s.priceRoute(groupCtx, routePlan, amounts[i], account, params)
			if err != nil {
				failures[i] = err
				return nil
			}
			quotes[i] = &quote
			return nil
		})
	}
	_ = group.Wait()
	for i, route := range routes {
		if failures[i] != nil {
			result.Unavailable = append(result.Unavailable, fmt.Sprintf("%s %s: %v", route.Protocol, feeLabel(route.Fee), failures[i]))
		} else if quotes[i] != nil {
			result.Quotes = append(result.Quotes, *quotes[i])
		}
	}
	slices.SortStableFunc(result.Quotes, func(a, b SwapQuote) int { return b.Value.Cmp(a.Value) })
	if len(result.Quotes) == 0 {
		return result, nil
	}

	need := result.Quotes[0].Fee
	if plan.trxIn() {
		need = need.Add(req.Amount)
	}
	if missing := need.Sub(balance); missing.IsPositive() {
		result.Shortfall = missing
	}
	return result, nil
}

func feeLabel(fee uint32) string {
	return decimal.New(int64(fee), -4).String() + "%"
}

// priceRoute lists the transactions a quoted route still needs and what each
// costs.
func (s *Service) priceRoute(
	ctx context.Context, plan swapPlan, amountOut *big.Int,
	account *api.AccountResourceMessage, params *client.ChainParams,
) (SwapQuote, error) {
	// Any minimum prices the call the same; zero cannot make the simulation
	// fail on price.
	plan.minOut = new(big.Int)
	calls, err := s.swapCalls(ctx, plan)
	if err != nil {
		return SwapQuote{}, err
	}

	energyLeft := s.client.AvailableEnergy(account)
	freeBandwidth := s.client.AvailableFreeBandwidth(account)
	stakedBandwidth := s.client.AvailableBandwidthWithoutFree(account)
	energyPrice := decimal.NewFromInt(params.EnergyFee)
	bytePrice := decimal.NewFromInt(params.TransactionFee)

	quote := SwapQuote{Route: plan.route, AmountOut: s.swapDecimal(swapOut(plan.in), amountOut), Fee: decimal.Zero}
	blocked := false
	for _, call := range calls {
		step := SwapStep{Kind: call.kind, Contract: call.contract}
		// A call can only be run once the ones before it are on chain: the
		// swap needs the approval, the Permit2 grant needs nothing.
		if !blocked || call.kind == SwapStepPermit2 {
			if energy, err := s.simulate(ctx, plan.from, call); err == nil {
				step.Energy, step.Measured = energy, true
			} else if call.kind != SwapStepSwap {
				return SwapQuote{}, err
			}
		}
		if !step.Measured {
			step.Energy = typicalSwapEnergy[plan.route.Protocol][plan.trxIn()]
		}
		if call.kind == SwapStepApprove {
			blocked = true
		}

		energy := decimal.NewFromInt(step.Energy)
		burned := energy.Sub(energyLeft)
		if burned.IsNegative() {
			burned = decimal.Zero
		}
		energyLeft = decimal.Max(decimal.Zero, energyLeft.Sub(energy))
		step.Fee = burned.Mul(energyPrice)

		// Bandwidth is paid whole from one pool or burned per byte — the
		// pools never combine.
		size := decimal.NewFromInt(int64(len(call.call.Data) + swapTxOverhead))
		switch {
		case stakedBandwidth.GreaterThanOrEqual(size):
			stakedBandwidth = stakedBandwidth.Sub(size)
		case freeBandwidth.GreaterThanOrEqual(size):
			freeBandwidth = freeBandwidth.Sub(size)
		default:
			step.Fee = step.Fee.Add(size.Mul(bytePrice))
		}
		step.Fee = step.Fee.Shift(-6)
		quote.Fee = quote.Fee.Add(step.Fee)
		quote.Steps = append(quote.Steps, step)
	}

	quote.Value = quote.AmountOut
	if plan.trxIn() {
		// The fee is paid in TRX while the output is USDT: value it at the
		// route's own price.
		price := quote.AmountOut.Div(s.swapDecimal(AssetTRX, plan.amountIn))
		quote.Value = quote.Value.Sub(quote.Fee.Mul(price))
	} else {
		quote.Value = quote.Value.Sub(quote.Fee)
	}
	return quote, nil
}

// quoteRoute returns what route pays for amountIn, in base units of the other
// asset.
func (s *Service) quoteRoute(ctx context.Context, route SwapRoute, trxIn bool, amountIn *big.Int) (*big.Int, error) {
	a := s.swap.addrs
	tokenIn, tokenOut := a.Token, a.WTRX
	if trxIn {
		tokenIn, tokenOut = a.WTRX, a.Token
	}
	switch route.Protocol {
	case SwapV2:
		call, err := sunswap.V2GetAmountsOut(amountIn, []common.Address{tokenIn, tokenOut})
		if err != nil {
			return nil, err
		}
		result, _, err := s.constantCall(ctx, "", s.swap.deployment.V2Router, call, 0)
		if err != nil {
			return nil, err
		}
		return sunswap.DecodeV2AmountsOut(result)
	case SwapV3:
		call, err := sunswap.V3QuoteExactInputSingle(tokenIn, tokenOut, route.Fee, amountIn)
		if err != nil {
			return nil, err
		}
		result, _, err := s.constantCall(ctx, "", s.swap.deployment.V3Quoter, call, 0)
		if err != nil {
			return nil, err
		}
		return sunswap.DecodeV3Quote(result)
	case SwapV4:
		return s.quoteV4(ctx, route, trxIn, amountIn)
	default:
		return nil, fmt.Errorf("%w: unknown swap protocol %q", ErrInvalidRequest, route.Protocol)
	}
}

// quoteV4 computes a V4 pool's output from its state. SunSwap publishes no V4
// quoter on Tron, so the pool's own arithmetic is repeated locally.
func (s *Service) quoteV4(ctx context.Context, route SwapRoute, trxIn bool, amountIn *big.Int) (*big.Int, error) {
	key := sunswap.NewPoolKey(s.swap.addrs.Token, route.Fee, route.TickSpacing)
	id, err := key.ID()
	if err != nil {
		return nil, err
	}
	manager := s.swap.deployment.V4PoolManager
	slotCall, err := sunswap.V4GetSlot0(id)
	if err != nil {
		return nil, err
	}
	raw, _, err := s.constantCall(ctx, "", manager, slotCall, 0)
	if err != nil {
		return nil, err
	}
	slot, err := sunswap.DecodeV4Slot0(raw)
	if err != nil {
		return nil, err
	}
	if slot.SqrtPriceX96.Sign() == 0 {
		return nil, errors.New("the pool does not exist")
	}
	liquidityCall, err := sunswap.V4GetLiquidity(id)
	if err != nil {
		return nil, err
	}
	raw, _, err = s.constantCall(ctx, "", manager, liquidityCall, 0)
	if err != nil {
		return nil, err
	}
	liquidity, err := sunswap.DecodeV4Liquidity(raw)
	if err != nil {
		return nil, err
	}
	// The native coin is currency0, so selling TRX moves the price down.
	zeroForOne := trxIn
	pool := sunswap.Pool{
		SqrtPriceX96: slot.SqrtPriceX96, Tick: slot.Tick, Liquidity: liquidity,
		TickSpacing: key.TickSpacing(), FeePips: sunswap.SwapFee(slot.ProtocolFee, slot.LPFee, zeroForOne),
	}
	return sunswap.QuoteExactInput(ctx, pool, &v4State{service: s, manager: manager, id: id}, amountIn, zeroForOne)
}

// v4State reads a V4 pool's bitmap and ticks through constant calls, each
// once.
type v4State struct {
	service *Service
	manager string
	id      [32]byte

	mu    sync.Mutex
	words map[int16]*big.Int
}

func (v *v4State) BitmapWord(ctx context.Context, word int16) (*big.Int, error) {
	v.mu.Lock()
	cached, ok := v.words[word]
	v.mu.Unlock()
	if ok {
		return cached, nil
	}
	call, err := sunswap.V4GetPoolBitmapInfo(v.id, word)
	if err != nil {
		return nil, err
	}
	raw, _, err := v.service.constantCall(ctx, "", v.manager, call, 0)
	if err != nil {
		return nil, err
	}
	value, err := sunswap.DecodeV4BitmapWord(raw)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	if v.words == nil {
		v.words = make(map[int16]*big.Int)
	}
	v.words[word] = value
	v.mu.Unlock()
	return value, nil
}

func (v *v4State) LiquidityNet(ctx context.Context, tick int) (*big.Int, error) {
	call, err := sunswap.V4GetPoolTickInfo(v.id, tick)
	if err != nil {
		return nil, err
	}
	raw, _, err := v.service.constantCall(ctx, "", v.manager, call, 0)
	if err != nil {
		return nil, err
	}
	return sunswap.DecodeV4LiquidityNet(raw)
}

// swapCalls lists the transactions the swap still needs, in order, judged by
// the allowances on chain right now. Selling TRX needs no approval; selling
// USDT needs the router (V2, V3) or Permit2 and then the universal router (V4)
// to be allowed to draw it.
func (s *Service) swapCalls(ctx context.Context, plan swapPlan) ([]swapCall, error) {
	d, a := s.swap.deployment, s.swap.addrs
	var calls []swapCall
	if !plan.trxIn() {
		spenderAddr := a.V2Router
		switch plan.route.Protocol {
		case SwapV3:
			spenderAddr = a.V3Router
		case SwapV4:
			spenderAddr = a.Permit2
		}
		allowance, err := s.allowance(ctx, plan.owner, spenderAddr)
		if err != nil {
			return nil, err
		}
		if allowance.Cmp(plan.amountIn) < 0 {
			amount := plan.amountIn
			if plan.unlimited {
				amount = maxUint256
			}
			call, err := sunswap.Approve(spenderAddr, amount)
			if err != nil {
				return nil, err
			}
			calls = append(calls, swapCall{kind: SwapStepApprove, contract: d.Token, call: call})
		}
		if plan.route.Protocol == SwapV4 {
			amount, expiration, err := s.permit2Allowance(ctx, plan.owner)
			if err != nil {
				return nil, err
			}
			// The grant has to outlive the swap that spends it.
			if amount.Cmp(plan.amountIn) < 0 || expiration < uint64(plan.now.Add(swapDeadline).Unix()) {
				grant, lifetime := plan.amountIn, permit2Lifetime
				if plan.unlimited {
					grant, lifetime = maxUint160, permit2UnlimitedLifetime
				}
				call, err := sunswap.Permit2Approve(a.Token, a.UniversalRouter, grant, uint64(plan.now.Add(lifetime).Unix()))
				if err != nil {
					return nil, err
				}
				calls = append(calls, swapCall{kind: SwapStepPermit2, contract: d.Permit2, call: call})
			}
		}
	}
	swap, err := s.swapCall(plan)
	if err != nil {
		return nil, err
	}
	return append(calls, swap), nil
}

// swapCall builds the trade itself.
func (s *Service) swapCall(plan swapPlan) (swapCall, error) {
	d, a := s.swap.deployment, s.swap.addrs
	deadline := plan.now.Add(swapDeadline).Unix()
	var (
		call      sunswap.Calldata
		contract  string
		callValue int64
		err       error
	)
	if plan.trxIn() {
		callValue = plan.amountIn.Int64()
	}
	switch plan.route.Protocol {
	case SwapV2:
		contract = d.V2Router
		if plan.trxIn() {
			call, err = sunswap.V2SwapExactETHForTokens(plan.minOut, []common.Address{a.WTRX, a.Token}, plan.owner, deadline)
		} else {
			call, err = sunswap.V2SwapExactTokensForETH(plan.amountIn, plan.minOut, []common.Address{a.Token, a.WTRX}, plan.owner, deadline)
		}
	case SwapV3:
		contract = d.V3Router
		params := sunswap.V3ExactInputSingleParams{
			TokenIn: a.WTRX, TokenOut: a.Token, Fee: big.NewInt(int64(plan.route.Fee)),
			Recipient: plan.owner, Deadline: big.NewInt(deadline), AmountIn: plan.amountIn,
			AmountOutMinimum: plan.minOut, SqrtPriceLimitX96: new(big.Int),
		}
		if plan.trxIn() {
			// The router wraps the call value into WTRX itself.
			call, err = sunswap.V3ExactInputSingle(params)
			break
		}
		// Paid out as WTRX to the router (recipient zero means the router
		// itself), then unwrapped to the owner in the same transaction.
		params.TokenIn, params.TokenOut, params.Recipient = a.Token, a.WTRX, common.Address{}
		var swap, unwrap sunswap.Calldata
		if swap, err = sunswap.V3ExactInputSingle(params); err != nil {
			break
		}
		if unwrap, err = sunswap.V3UnwrapWETH9(plan.minOut, plan.owner); err != nil {
			break
		}
		call, err = sunswap.V3Multicall(swap, unwrap)
	case SwapV4:
		contract = d.UniversalRouter
		key := sunswap.NewPoolKey(a.Token, plan.route.Fee, plan.route.TickSpacing)
		call, err = sunswap.V4ExactInputSingle(key, plan.trxIn(), plan.amountIn, plan.minOut, deadline)
	default:
		err = fmt.Errorf("%w: unknown swap protocol %q", ErrInvalidRequest, plan.route.Protocol)
	}
	if err != nil {
		return swapCall{}, err
	}
	return swapCall{kind: SwapStepSwap, contract: contract, call: call, callValue: callValue}, nil
}

func (s *Service) allowance(ctx context.Context, owner, spender common.Address) (*big.Int, error) {
	call, err := sunswap.Allowance(owner, spender)
	if err != nil {
		return nil, err
	}
	raw, _, err := s.constantCall(ctx, "", s.swap.deployment.Token, call, 0)
	if err != nil {
		return nil, err
	}
	return sunswap.DecodeAllowance(raw)
}

func (s *Service) permit2Allowance(ctx context.Context, owner common.Address) (*big.Int, uint64, error) {
	a := s.swap.addrs
	call, err := sunswap.Permit2Allowance(owner, a.Token, a.UniversalRouter)
	if err != nil {
		return nil, 0, err
	}
	raw, _, err := s.constantCall(ctx, "", s.swap.deployment.Permit2, call, 0)
	if err != nil {
		return nil, 0, err
	}
	return sunswap.DecodePermit2Allowance(raw)
}

// simulate runs a call as the owner without broadcasting it and returns the
// energy it consumed. A call that would revert fails here, for free.
func (s *Service) simulate(ctx context.Context, from string, call swapCall) (int64, error) {
	_, energy, err := s.constantCall(ctx, from, call.contract, call.call, call.callValue)
	return energy, err
}

// constantCall runs calldata against a contract without broadcasting it. An
// empty from calls as the zero address, which is enough for reads.
//
// Only transport failures move on to the next node. A revert is the VM's
// verdict on the call and every node would repeat it.
func (s *Service) constantCall(
	ctx context.Context, from, contract string, call sunswap.Calldata, callValue int64,
) ([]byte, int64, error) {
	memo, _ := ctx.Value(callMemoKey{}).(*callMemo)
	if memo == nil {
		return s.runConstantCall(ctx, from, contract, call, callValue)
	}
	key := fmt.Sprintf("%s|%s|%x|%d", from, contract, call.Data, callValue)
	memo.mu.Lock()
	cached, ok := memo.results[key]
	memo.mu.Unlock()
	if ok {
		return cached.data, cached.energy, cached.err
	}
	data, energy, err := s.runConstantCall(ctx, from, contract, call, callValue)
	// A transport failure is not an answer about the call, so it is not kept.
	if err == nil || errors.Is(err, ErrInvalidRequest) {
		memo.mu.Lock()
		memo.results[key] = memoResult{data: data, energy: energy, err: err}
		memo.mu.Unlock()
	}
	return data, energy, err
}

// callMemo lets the reads of one quote share their answers. It lives on the
// context of a single QuoteSwap call and dies with it, so nothing read for one
// quote is ever reused for a signature.
type callMemo struct {
	mu      sync.Mutex
	results map[string]memoResult
}

type (
	callMemoKey struct{}
	memoResult  struct {
		data   []byte
		energy int64
		err    error
	}
)

func withCallMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, callMemoKey{}, &callMemo{results: make(map[string]memoResult)})
}

func (s *Service) runConstantCall(
	ctx context.Context, from, contract string, call sunswap.Calldata, callValue int64,
) ([]byte, int64, error) {
	owner := make([]byte, 21)
	owner[0] = 0x41
	if from != "" {
		decoded, err := tronutils.DecodeCheck(from)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: invalid address: %s", ErrInvalidRequest, err)
		}
		owner = decoded
	}
	target, err := tronutils.DecodeCheck(contract)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid contract address %s: %w", contract, err)
	}
	type answer struct {
		ext *api.TransactionExtention
		err error
	}
	got, err := retry(ctx, s.nodes, func() (answer, error) {
		ext, err := s.client.TriggerConstantContract(ctx, &core.TriggerSmartContract{
			OwnerAddress: owner, ContractAddress: target, Data: call.Data, CallValue: callValue,
		})
		if err != nil && (errors.Is(err, client.ErrContractCallFailed) || errors.Is(err, client.ErrContractNotExist)) {
			return answer{ext: ext, err: err}, nil
		}
		return answer{ext: ext}, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("%s on %s: %w", call.Method, contract, err)
	}
	if got.err != nil {
		return nil, got.ext.GetEnergyUsed(), fmt.Errorf("%w: %s on %s would fail: %w", ErrInvalidRequest, call.Method, contract, got.err)
	}
	results := got.ext.GetConstantResult()
	if len(results) == 0 {
		return nil, got.ext.GetEnergyUsed(), nil
	}
	return results[0], got.ext.GetEnergyUsed(), nil
}

// SwapWithSigner signs and broadcasts one step of a swap and returns its txid.
//
// The step is checked against the chain first. Approvals are judged by the
// allowances on chain: an approval that is already in place, or a swap whose
// approval has not been confirmed, is refused with chain.ErrStateChanged so the
// caller moves on or waits instead of paying twice. A swap is quoted again and
// refused when the price has moved past the minimum the user approved — or so
// far above it that the minimum no longer protects anything — and then run as a
// simulation, so a trade that would revert costs nothing.
func (s *Service) SwapWithSigner(ctx context.Context, from string, req SwapRequest, signer chain.Signer) (string, error) {
	if signer == nil || signer.Family() != chain.FamilyTron {
		return "", errors.New("Tron signer is required")
	}
	plan, err := s.newSwapPlan(from, req.In, req.Amount)
	if err != nil {
		return "", err
	}
	if err := s.knownRoute(req.Route); err != nil {
		return "", err
	}
	plan.route, plan.unlimited = req.Route, req.Unlimited
	if !req.MinOut.IsPositive() {
		return "", fmt.Errorf("%w: minimum output must be greater than zero", ErrInvalidRequest)
	}
	if plan.minOut, err = s.swapUnits(swapOut(req.In), req.MinOut); err != nil {
		return "", err
	}

	calls, err := s.swapCalls(ctx, plan)
	if err != nil {
		return "", s.chainError("prepare swap", err)
	}
	next := calls[0]
	if next.kind != req.Step {
		return "", stepOutOfOrder(req.Step, next.kind)
	}

	if next.kind == SwapStepSwap {
		current, err := s.quoteRoute(ctx, plan.route, plan.trxIn(), plan.amountIn)
		if err != nil {
			return "", s.chainError("quote swap", err)
		}
		if current.Cmp(plan.minOut) < 0 {
			return "", fmt.Errorf("%w: the pool now pays %s, below the approved minimum of %s; review the swap again",
				chain.ErrStateChanged, s.swapDecimal(swapOut(req.In), current), req.MinOut)
		}
		floor := new(big.Int).Mul(current, big.NewInt(10_000-maxSwapSlippageBps))
		floor.Quo(floor, big.NewInt(10_000))
		if plan.minOut.Cmp(floor) < 0 {
			return "", fmt.Errorf("%w: the approved minimum of %s is more than %s below the current price; review the swap again",
				chain.ErrStateChanged, req.MinOut, decimal.New(maxSwapSlippageBps, -2).String()+"%")
		}
	}

	energy, err := s.simulate(ctx, from, next)
	if err != nil {
		return "", s.chainError("simulate "+string(next.kind), err)
	}
	feeLimit, err := s.callFeeLimit(ctx, energy)
	if err != nil {
		return "", err
	}

	words, err := next.call.Words()
	if err != nil {
		return "", err
	}
	tx, err := retry(ctx, s.nodes, func() (*api.TransactionExtention, error) {
		return s.client.TriggerContract(ctx, from, next.contract, next.call.Method, wordParams(words), feeLimit, next.callValue, "", 0)
	})
	if err != nil {
		return "", s.chainError("build "+string(next.kind), err)
	}
	intent := Intent{
		Kind: IntentTriggerContract, Owner: from, Contract: next.contract,
		Data: next.call.Data, CallValue: next.callValue, FeeLimit: int64(feeLimit),
	}
	txid, err := s.submitWithSigner(ctx, intent, tx, signer)
	if err != nil {
		return txid, err
	}
	s.invalidate(from)
	return txid, nil
}

// stepOrder ranks the steps the way swapCalls emits them.
var stepOrder = map[SwapStepKind]int{SwapStepApprove: 0, SwapStepPermit2: 1, SwapStepSwap: 2}

func stepOutOfOrder(requested, next SwapStepKind) error {
	if stepOrder[requested] < stepOrder[next] {
		return fmt.Errorf("%w: the %s is already in place; continue with the %s",
			chain.ErrStateChanged, stepName(requested), stepName(next))
	}
	return fmt.Errorf("%w: the %s is not confirmed on chain yet; wait for it, then continue",
		chain.ErrStateChanged, stepName(next))
}

func stepName(kind SwapStepKind) string {
	switch kind {
	case SwapStepApprove:
		return "token approval"
	case SwapStepPermit2:
		return "Permit2 approval"
	default:
		return "swap"
	}
}

// callFeeLimit caps a contract call's energy at what the simulation measured,
// with headroom, and never below the configured limit.
func (s *Service) callFeeLimit(ctx context.Context, energy int64) (client.SUN, error) {
	params, err := retry(ctx, s.nodes, func() (*client.ChainParams, error) {
		return s.client.ChainParams(ctx)
	})
	if err != nil {
		return 0, s.chainError("read chain parameters", err)
	}
	needed := decimal.NewFromInt(energy).Mul(decimal.NewFromInt(params.EnergyFee)).
		Mul(decimal.NewFromFloat(feeLimitHeadroom)).Ceil().IntPart()
	return max(s.feeLimit, client.SUN(needed)), nil
}

// wordParams spells calldata words as gotron's bytes32 argument list.
func wordParams(words [][32]byte) string {
	var b []byte
	b = append(b, '[')
	for i, word := range words {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"bytes32":"`...)
		b = append(b, common.Bytes2Hex(word[:])...)
		b = append(b, `"}`...)
	}
	return string(append(b, ']'))
}
