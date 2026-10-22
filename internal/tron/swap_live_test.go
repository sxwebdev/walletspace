package tron

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/tron/sunswap"
)

// liveSwapService connects to the network's public nodes, or skips.
func liveSwapService(t *testing.T, network string) *Service {
	t.Helper()
	if os.Getenv("WALLETSPACE_LIVE_TRON_TESTS") != "1" {
		t.Skip("set WALLETSPACE_LIVE_TRON_TESTS=1 to use public Tron services")
	}
	cfg := &config.Config{Network: network, FeeLimitTRX: 50}
	if network == config.NetworkMainnet {
		cfg.USDTContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	} else {
		cfg.USDTContract = "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf"
	}
	nodes := []config.Node{{Address: "https://tron-rpc.publicnode.com", HTTP: true}, {Address: "https://api.trongrid.io", HTTP: true, Tier: 1}}
	if network == config.NetworkNile {
		nodes = []config.Node{{Address: "https://nile.trongrid.io", HTTP: true}}
	}
	s, err := New(t.Context(), cfg, nodes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if !s.SwapAvailable() {
		t.Fatalf("swaps are not available on %s", network)
	}
	return s
}

// v3State reads a V3 pool the way v4State reads a V4 one, so the local
// arithmetic can be held against the V3 quoter, which runs the real thing.
type v3State struct {
	service *Service
	pool    string
}

func (v v3State) BitmapWord(ctx context.Context, word int16) (*big.Int, error) {
	call, err := sunswap.V3TickBitmap(word)
	if err != nil {
		return nil, err
	}
	raw, _, err := v.service.constantCall(ctx, "", v.pool, call, 0)
	if err != nil {
		return nil, err
	}
	return sunswap.DecodeV3BitmapWord(raw)
}

func (v v3State) LiquidityNet(ctx context.Context, tick int) (*big.Int, error) {
	call, err := sunswap.V3Ticks(tick)
	if err != nil {
		return nil, err
	}
	raw, _, err := v.service.constantCall(ctx, "", v.pool, call, 0)
	if err != nil {
		return nil, err
	}
	return sunswap.DecodeV3LiquidityNet(raw)
}

// The V4 quote is computed locally from pool state, with no quoter to check it
// against on chain. The same arithmetic run over a V3 pool has one: if the two
// agree to the unit across amounts that cross several ticks, the port is the
// pool's own math.
func TestCLMathMatchesTheV3QuoterLive(t *testing.T) {
	s := liveSwapService(t, config.NetworkMainnet)
	const pool = "TSUUVjysXV8YqHytSNjfkNXnnB49QDvZpx" // USDT/WTRX 0.05%
	a := s.swap.addrs

	for _, tc := range []struct {
		name   string
		trxIn  bool
		amount int64
	}{
		{"10 TRX", true, 10_000_000},
		{"500k TRX", true, 500_000_000_000},
		{"5M TRX", true, 5_000_000_000_000},
		{"10 USDT", false, 10_000_000},
		{"2M USDT", false, 2_000_000_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			slotCall, _ := sunswap.V3Slot0()
			raw, _, err := s.constantCall(ctx, "", pool, slotCall, 0)
			if err != nil {
				t.Fatal(err)
			}
			sqrtPrice, tick, err := sunswap.DecodeV3Slot0(raw)
			if err != nil {
				t.Fatal(err)
			}
			liquidityCall, _ := sunswap.V3Liquidity()
			raw, _, err = s.constantCall(ctx, "", pool, liquidityCall, 0)
			if err != nil {
				t.Fatal(err)
			}
			liquidity, _ := sunswap.DecodeV3Liquidity(raw)

			// The pool orders its tokens by address.
			tokenIn, tokenOut := a.Token, a.WTRX
			if tc.trxIn {
				tokenIn, tokenOut = a.WTRX, a.Token
			}
			zeroForOne := tokenIn.Cmp(tokenOut) < 0

			local, err := sunswap.QuoteExactInput(ctx, sunswap.Pool{
				SqrtPriceX96: sqrtPrice, Tick: tick, Liquidity: liquidity, TickSpacing: 10, FeePips: 500,
			}, v3State{service: s, pool: pool}, big.NewInt(tc.amount), zeroForOne)
			if err != nil {
				t.Fatalf("QuoteExactInput() error = %v", err)
			}
			quoterCall, _ := sunswap.V3QuoteExactInputSingle(tokenIn, tokenOut, 500, big.NewInt(tc.amount))
			raw, _, err = s.constantCall(ctx, "", s.swap.deployment.V3Quoter, quoterCall, 0)
			if err != nil {
				t.Fatal(err)
			}
			quoted, _ := sunswap.DecodeV3Quote(raw)
			if local.Cmp(quoted) != 0 {
				t.Errorf("local quote %s, quoter %s (difference %s)", local, quoted, new(big.Int).Sub(local, quoted))
			}
		})
	}
}

// A simulation spends nothing and may name any address as its caller, so the
// swaps below run as real holders on mainnet. They are found on chain, not
// owned: a holder whose balance or allowance has since moved is skipped rather
// than failed.
const (
	// trxHolder holds millions of TRX.
	trxHolder = "TNXoiAJ3dct8Fjg4M9fkLFh9S2v9TXc32G"
	// v2USDTHolder holds a few USDT and an unlimited approval of the V2 router.
	v2USDTHolder = "THP4rgKDxdWHvQ8FtytX5XveKYHWmtDGKj"
	// v3USDTHolder holds USDT and an unlimited approval of the V3 router.
	v3USDTHolder = "TEbDZ23WRte5pfvrwNMJovcTVuuBGWryEP"
	// v4USDTHolder holds USDT, an unlimited approval of Permit2 and an
	// unlimited Permit2 grant to the universal router.
	v4USDTHolder = "TS3Rhjw3c8RFXxTXsakGtPpWkzZGx4RvVA"
)

// Each route's swap is run against the chain with the minimum set to the
// quote, where it has to go through, and one unit above it, where it has to
// revert. That pins both the calldata and, for V4, the locally computed quote
// to what the pool really does.
func TestSwapCalldataAgainstTheChainLive(t *testing.T) {
	s := liveSwapService(t, config.NetworkMainnet)
	for _, route := range s.swapRoutes() {
		t.Run("TRX to USDT/"+string(route.Protocol)+"/"+feeLabel(route.Fee), func(t *testing.T) {
			bracketSwap(t, s, trxHolder, AssetTRX, big.NewInt(1_000_000_000), route)
		})
	}
	for _, tc := range []struct {
		holder string
		route  SwapRoute
	}{
		{v2USDTHolder, SwapRoute{Protocol: SwapV2, Fee: 3000}},
		{v3USDTHolder, SwapRoute{Protocol: SwapV3, Fee: 500}},
		{v4USDTHolder, SwapRoute{Protocol: SwapV4, Fee: 500, TickSpacing: 10}},
	} {
		t.Run("USDT to TRX/"+string(tc.route.Protocol), func(t *testing.T) {
			bracketSwap(t, s, tc.holder, AssetUSDT, big.NewInt(1_000_000), tc.route)
		})
	}
}

func bracketSwap(t *testing.T, s *Service, holder string, in Asset, amount *big.Int, route SwapRoute) {
	t.Helper()
	ctx := t.Context()
	owner, err := sunswap.EVMAddress(holder)
	if err != nil {
		t.Fatal(err)
	}
	plan := swapPlan{from: holder, owner: owner, in: in, amountIn: amount, route: route, now: time.Now(), minOut: new(big.Int)}
	calls, err := s.swapCalls(ctx, plan)
	if err != nil {
		t.Fatalf("swapCalls() error = %v", err)
	}
	if len(calls) != 1 {
		t.Skipf("%s needs %d steps now; its approvals have moved", holder, len(calls))
	}
	quoted, err := s.quoteRoute(ctx, route, in == AssetTRX, amount)
	if err != nil {
		t.Fatalf("quoteRoute() error = %v", err)
	}
	if quoted.Sign() <= 0 {
		t.Skip("the pool pays nothing")
	}

	plan.minOut = quoted
	call, err := s.swapCall(plan)
	if err != nil {
		t.Fatal(err)
	}
	energy, err := s.simulate(ctx, plan.from, call)
	if err != nil {
		t.Fatalf("swap with the quoted minimum failed: %v", err)
	}
	t.Logf("quote %s, energy %d", quoted, energy)

	plan.minOut = new(big.Int).Add(quoted, big.NewInt(1))
	call, err = s.swapCall(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.simulate(ctx, plan.from, call); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("swap with a minimum one unit above the quote: error = %v, want a revert", err)
	}
}

// The whole quote, as the dialog asks for it: every route priced, the steps
// each still needs, and the best one first.
func TestQuoteSwapLive(t *testing.T) {
	s := liveSwapService(t, config.NetworkMainnet)
	for _, tc := range []struct {
		name   string
		holder string
		in     Asset
		amount string
	}{
		{"sell TRX", trxHolder, AssetTRX, "1000"},
		{"sell USDT without approvals", trxHolder, AssetUSDT, "100"},
		{"sell USDT with approvals", v4USDTHolder, AssetUSDT, "100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := time.Now()
			quotes, err := s.QuoteSwap(t.Context(), tc.holder, SwapQuoteRequest{In: tc.in, Amount: decimal.RequireFromString(tc.amount)})
			if err != nil {
				t.Fatalf("QuoteSwap() error = %v", err)
			}
			if len(quotes.Quotes) == 0 {
				t.Fatalf("no quotes; unavailable: %v", quotes.Unavailable)
			}
			for i := 1; i < len(quotes.Quotes); i++ {
				if quotes.Quotes[i].Value.GreaterThan(quotes.Quotes[i-1].Value) {
					t.Errorf("quote %d is worth more than the one ranked above it", i)
				}
			}
			t.Logf("%s in %s, shortfall %s, unavailable %v", tc.name, time.Since(started).Round(time.Millisecond), quotes.Shortfall, quotes.Unavailable)
			for _, q := range quotes.Quotes {
				t.Logf("  %s %s: out %s fee %s value %s steps %+v", q.Route.Protocol, feeLabel(q.Route.Fee), q.AmountOut, q.Fee, q.Value, q.Steps)
			}
		})
	}
}

// recordingState remembers every bitmap word and tick a quote reads, so the
// pool can be replayed offline.
type recordingState struct {
	inner sunswap.PoolState
	words map[string]string
	ticks map[string]string
}

func (r *recordingState) BitmapWord(ctx context.Context, word int16) (*big.Int, error) {
	v, err := r.inner.BitmapWord(ctx, word)
	if err == nil {
		r.words[strconv.Itoa(int(word))] = v.String()
	}
	return v, err
}

func (r *recordingState) LiquidityNet(ctx context.Context, tick int) (*big.Int, error) {
	v, err := r.inner.LiquidityNet(ctx, tick)
	if err == nil {
		r.ticks[strconv.Itoa(tick)] = v.String()
	}
	return v, err
}

// TestRecordV3PoolFixtureLive writes sunswap/testdata/v3_usdt_wtrx_500.json:
// one moment of the mainnet USDT/WTRX 0.05% pool together with what the V3
// quoter answered for it, for the offline test of the arithmetic.
func TestRecordV3PoolFixtureLive(t *testing.T) {
	if os.Getenv("WALLETSPACE_RECORD_FIXTURES") != "1" {
		t.Skip("set WALLETSPACE_RECORD_FIXTURES=1 to record the pool fixture")
	}
	s := liveSwapService(t, config.NetworkMainnet)
	const pool = "TSUUVjysXV8YqHytSNjfkNXnnB49QDvZpx"
	ctx := t.Context()
	a := s.swap.addrs

	slotCall, _ := sunswap.V3Slot0()
	raw, _, err := s.constantCall(ctx, "", pool, slotCall, 0)
	if err != nil {
		t.Fatal(err)
	}
	sqrtPrice, tick, _ := sunswap.DecodeV3Slot0(raw)
	liquidityCall, _ := sunswap.V3Liquidity()
	raw, _, err = s.constantCall(ctx, "", pool, liquidityCall, 0)
	if err != nil {
		t.Fatal(err)
	}
	liquidity, _ := sunswap.DecodeV3Liquidity(raw)

	state := &recordingState{inner: v3State{service: s, pool: pool}, words: map[string]string{}, ticks: map[string]string{}}
	type fixtureCase struct {
		AmountIn   string `json:"amount_in"`
		ZeroForOne bool   `json:"zero_for_one"`
		AmountOut  string `json:"amount_out"`
	}
	fixture := struct {
		Pool         string            `json:"pool"`
		SqrtPriceX96 string            `json:"sqrt_price_x96"`
		Tick         int               `json:"tick"`
		Liquidity    string            `json:"liquidity"`
		TickSpacing  int               `json:"tick_spacing"`
		FeePips      int64             `json:"fee_pips"`
		Words        map[string]string `json:"words"`
		Ticks        map[string]string `json:"ticks"`
		Cases        []fixtureCase     `json:"cases"`
	}{Pool: pool, SqrtPriceX96: sqrtPrice.String(), Tick: tick, Liquidity: liquidity.String(), TickSpacing: 10, FeePips: 500}

	for _, amount := range []struct {
		trxIn bool
		units int64
	}{{true, 10_000_000}, {true, 5_000_000_000_000}, {false, 10_000_000}, {false, 2_000_000_000_000}} {
		tokenIn, tokenOut := a.Token, a.WTRX
		if amount.trxIn {
			tokenIn, tokenOut = a.WTRX, a.Token
		}
		zeroForOne := tokenIn.Cmp(tokenOut) < 0
		pool := sunswap.Pool{SqrtPriceX96: sqrtPrice, Tick: tick, Liquidity: liquidity, TickSpacing: 10, FeePips: 500}
		if _, err := sunswap.QuoteExactInput(ctx, pool, state, big.NewInt(amount.units), zeroForOne); err != nil {
			t.Fatal(err)
		}
		quoterCall, _ := sunswap.V3QuoteExactInputSingle(tokenIn, tokenOut, 500, big.NewInt(amount.units))
		raw, _, err := s.constantCall(ctx, "", s.swap.deployment.V3Quoter, quoterCall, 0)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := sunswap.DecodeV3Quote(raw)
		fixture.Cases = append(fixture.Cases, fixtureCase{AmountIn: big.NewInt(amount.units).String(), ZeroForOne: zeroForOne, AmountOut: out.String()})
	}
	fixture.Words, fixture.Ticks = state.words, state.ticks
	data, err := json.Marshal(fixture, jsontext.Multiline(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("sunswap/testdata/v3_usdt_wtrx_500.json", append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Nile carries its own SunSwap deployment; every route on it has to quote,
// so swaps can be tried there before mainnet.
func TestQuoteSwapNileLive(t *testing.T) {
	s := liveSwapService(t, config.NetworkNile)
	for _, in := range []Asset{AssetTRX, AssetUSDT} {
		quotes, err := s.QuoteSwap(t.Context(), trxHolder, SwapQuoteRequest{In: in, Amount: decimal.NewFromInt(10)})
		if err != nil {
			t.Fatalf("QuoteSwap(%s) error = %v", in, err)
		}
		protocols := map[SwapProtocol]bool{}
		for _, q := range quotes.Quotes {
			protocols[q.Route.Protocol] = true
			t.Logf("%s %s %s: out %s fee %s", in, q.Route.Protocol, feeLabel(q.Route.Fee), q.AmountOut, q.Fee)
		}
		for _, p := range []SwapProtocol{SwapV2, SwapV3, SwapV4} {
			if !protocols[p] {
				t.Errorf("selling %s: no %s quote (unavailable: %v)", in, p, quotes.Unavailable)
			}
		}
	}
}
