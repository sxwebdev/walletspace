package sunswap

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
)

// v3Fixture is one moment of the mainnet USDT/WTRX 0.05% V3 pool: the state a
// quote reads, and what the pool's own quoter answered for it. It is written by
// TestRecordV3PoolFixtureLive in the tron package.
type v3Fixture struct {
	SqrtPriceX96 string            `json:"sqrt_price_x96"`
	Tick         int               `json:"tick"`
	Liquidity    string            `json:"liquidity"`
	TickSpacing  int               `json:"tick_spacing"`
	FeePips      int64             `json:"fee_pips"`
	Words        map[string]string `json:"words"`
	Ticks        map[string]string `json:"ticks"`
	Cases        []struct {
		AmountIn   string `json:"amount_in"`
		ZeroForOne bool   `json:"zero_for_one"`
		AmountOut  string `json:"amount_out"`
	} `json:"cases"`
}

// fixtureState serves recorded reads and fails on anything not recorded, so a
// quote that walks somewhere the real one did not is caught rather than fed
// zeros.
type fixtureState struct {
	words map[string]string
	ticks map[string]string
	reads int
}

func (f *fixtureState) BitmapWord(_ context.Context, word int16) (*big.Int, error) {
	f.reads++
	v, ok := f.words[strconv.Itoa(int(word))]
	if !ok {
		return nil, fmt.Errorf("bitmap word %d was not recorded", word)
	}
	return mustBig(v), nil
}

func (f *fixtureState) LiquidityNet(_ context.Context, tick int) (*big.Int, error) {
	f.reads++
	v, ok := f.ticks[strconv.Itoa(tick)]
	if !ok {
		return nil, fmt.Errorf("tick %d was not recorded", tick)
	}
	return mustBig(v), nil
}

func loadV3Fixture(t *testing.T) v3Fixture {
	t.Helper()
	data, err := os.ReadFile("testdata/v3_usdt_wtrx_500.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture v3Fixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// The port has to be the pool's arithmetic, not an approximation of it: the
// quote is what the minimum is computed from, so being one unit high makes a
// swap at zero slippage revert. The recorded cases include a sale large enough
// to cross two initialised ticks.
func TestQuoteExactInputMatchesTheV3Quoter(t *testing.T) {
	t.Parallel()
	fixture := loadV3Fixture(t)
	if len(fixture.Cases) == 0 {
		t.Fatal("fixture has no cases")
	}
	for _, tc := range fixture.Cases {
		t.Run(fmt.Sprintf("%s zeroForOne=%v", tc.AmountIn, tc.ZeroForOne), func(t *testing.T) {
			t.Parallel()
			pool := Pool{
				SqrtPriceX96: mustBig(fixture.SqrtPriceX96), Tick: fixture.Tick,
				Liquidity: mustBig(fixture.Liquidity), TickSpacing: fixture.TickSpacing, FeePips: fixture.FeePips,
			}
			got, err := QuoteExactInput(t.Context(), pool, &fixtureState{words: fixture.Words, ticks: fixture.Ticks},
				mustBig(tc.AmountIn), tc.ZeroForOne)
			if err != nil {
				t.Fatalf("QuoteExactInput() error = %v", err)
			}
			if got.String() != tc.AmountOut {
				t.Errorf("QuoteExactInput() = %s, the quoter said %s", got, tc.AmountOut)
			}
		})
	}
}

// The pool's state is shared with the caller; a quote must not move it.
func TestQuoteExactInputLeavesThePoolUntouched(t *testing.T) {
	t.Parallel()
	fixture := loadV3Fixture(t)
	price, liquidity := mustBig(fixture.SqrtPriceX96), mustBig(fixture.Liquidity)
	pool := Pool{SqrtPriceX96: price, Tick: fixture.Tick, Liquidity: liquidity, TickSpacing: 10, FeePips: 500}
	if _, err := QuoteExactInput(t.Context(), pool, &fixtureState{words: fixture.Words, ticks: fixture.Ticks},
		big.NewInt(5_000_000_000_000), true); err != nil {
		t.Fatal(err)
	}
	if price.String() != fixture.SqrtPriceX96 || liquidity.String() != fixture.Liquidity {
		t.Errorf("pool state changed: price %s, liquidity %s", price, liquidity)
	}
}

func TestQuoteExactInputRefusesWhatItCannotQuote(t *testing.T) {
	t.Parallel()
	fixture := loadV3Fixture(t)
	base := Pool{
		SqrtPriceX96: mustBig(fixture.SqrtPriceX96), Tick: fixture.Tick,
		Liquidity: mustBig(fixture.Liquidity), TickSpacing: 10, FeePips: 500,
	}
	readErr := errors.New("node is down")
	tests := []struct {
		name    string
		pool    func(Pool) Pool
		state   PoolState
		amount  *big.Int
		wantErr error
	}{
		{name: "zero amount", amount: big.NewInt(0)},
		{name: "nil amount"},
		{name: "zero tick spacing", amount: big.NewInt(1), pool: func(p Pool) Pool { p.TickSpacing = 0; return p }},
		{name: "fee of 100%", amount: big.NewInt(1), pool: func(p Pool) Pool { p.FeePips = feeDenominator; return p }},
		{name: "negative fee", amount: big.NewInt(1), pool: func(p Pool) Pool { p.FeePips = -1; return p }},
		{name: "bitmap read fails", amount: big.NewInt(1), state: failingState{err: readErr}, wantErr: readErr},
		{
			// An empty pool: every word is blank, so the walk runs from
			// word to word without ever finding liquidity.
			name: "pool without liquidity", amount: big.NewInt(1_000_000),
			pool:  func(p Pool) Pool { p.Liquidity = new(big.Int); return p },
			state: blankState{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pool := base
			if tt.pool != nil {
				pool = tt.pool(pool)
			}
			state := tt.state
			if state == nil {
				state = &fixtureState{words: fixture.Words, ticks: fixture.Ticks}
			}
			got, err := QuoteExactInput(t.Context(), pool, state, tt.amount, true)
			if err == nil {
				t.Fatalf("QuoteExactInput() = %s, want an error", got)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// A tick whose liquidity change cannot be read stops the quote rather than
// being skipped as if it carried none.
func TestQuoteExactInputStopsOnAnUnreadableTick(t *testing.T) {
	t.Parallel()
	fixture := loadV3Fixture(t)
	pool := Pool{
		SqrtPriceX96: mustBig(fixture.SqrtPriceX96), Tick: fixture.Tick,
		Liquidity: mustBig(fixture.Liquidity), TickSpacing: 10, FeePips: 500,
	}
	state := &fixtureState{words: fixture.Words, ticks: map[string]string{}}
	if _, err := QuoteExactInput(t.Context(), pool, state, big.NewInt(5_000_000_000_000), true); err == nil {
		t.Fatal("QuoteExactInput() succeeded without the ticks it crosses")
	}
}

type failingState struct{ err error }

func (f failingState) BitmapWord(context.Context, int16) (*big.Int, error) { return nil, f.err }
func (f failingState) LiquidityNet(context.Context, int) (*big.Int, error) { return nil, f.err }

type blankState struct{}

func (blankState) BitmapWord(context.Context, int16) (*big.Int, error) { return new(big.Int), nil }
func (blankState) LiquidityNet(context.Context, int) (*big.Int, error) { return new(big.Int), nil }

// The bounds and the centre of TickMath are published constants of the
// contract; a port that drifts on any of them prices every pool wrongly.
func TestSqrtRatioAtTick(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tick int
		want string
	}{
		{minTick, "4295128739"},
		{maxTick, "1461446703485210103287273052203988822378723970342"},
		{0, q96.String()},
		// ceil(sqrt(1.0001^tick) · 2^96), computed independently.
		{1, "79232123823359799118286999568"},
		{-1, "79224201403219477170569942574"},
	}
	for _, tt := range tests {
		got, err := sqrtRatioAtTick(tt.tick)
		if err != nil {
			t.Fatalf("sqrtRatioAtTick(%d) error = %v", tt.tick, err)
		}
		if got.String() != tt.want {
			t.Errorf("sqrtRatioAtTick(%d) = %s, want %s", tt.tick, got, tt.want)
		}
	}
	for _, tick := range []int{minTick - 1, maxTick + 1} {
		if _, err := sqrtRatioAtTick(tick); err == nil {
			t.Errorf("sqrtRatioAtTick(%d) accepted a tick out of range", tick)
		}
	}
}

// Negative ticks round towards minus infinity when compressed: tick -1 with a
// spacing of 10 belongs to the range starting at -10, not 0.
func TestNextInitializedTickCompressesNegativeTicksDownwards(t *testing.T) {
	t.Parallel()
	// Bit 255 of word -1 is compressed tick -1, i.e. tick -10.
	word := new(big.Int).Lsh(big.NewInt(1), 255)
	state := &fixtureState{words: map[string]string{"-1": word.String(), "0": "0"}}

	next, initialized, err := nextInitializedTick(t.Context(), state, -1, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if next != -10 || !initialized {
		t.Errorf("searching down from -1: got %d (initialized %v), want -10 (true)", next, initialized)
	}

	next, initialized, err = nextInitializedTick(t.Context(), state, -11, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if next != -10 || !initialized {
		t.Errorf("searching up from -11: got %d (initialized %v), want -10 (true)", next, initialized)
	}

	// Nothing set above: the search stops at the end of word 0.
	next, initialized, err = nextInitializedTick(t.Context(), state, -10, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if next != 2550 || initialized {
		t.Errorf("searching up from -10: got %d (initialized %v), want 2550 (false)", next, initialized)
	}
}

func TestSwapFeeCombinesProtocolAndLPFees(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		protocolFee uint32
		lpFee       uint32
		zeroForOne  bool
		want        int64
	}{
		{"no protocol fee", 0, 500, true, 500},
		{"protocol fee on the other direction only", 100 << 12, 500, true, 500},
		// The product term is floored, as the contract's div is.
		{"protocol fee in this direction", 100, 500, true, 600},
		{"protocol fee one for zero", 100 << 12, 3000, false, 3100},
		{"product term counts", 1000, 500_000, true, 500_500},
	}
	for _, tt := range tests {
		if got := SwapFee(tt.protocolFee, tt.lpFee, tt.zeroForOne); got != tt.want {
			t.Errorf("%s: SwapFee() = %d, want %d", tt.name, got, tt.want)
		}
	}
}

// The overflow branch of getNextSqrtPriceFromAmount0RoundingUp changes the
// result, so it has to be taken exactly where the contract takes it.
func TestNextSqrtPriceFromAmount0FallsBackOnOverflow(t *testing.T) {
	t.Parallel()
	price, liquidity := new(big.Int).Set(q96), big.NewInt(1)
	small := nextSqrtPriceFromAmount0RoundingUp(price, liquidity, big.NewInt(1))
	if want := mulDivRoundingUp(new(big.Int).Lsh(liquidity, 96), price, new(big.Int).Add(new(big.Int).Lsh(liquidity, 96), price)); small.Cmp(want) != 0 {
		t.Errorf("in range: got %s, want %s", small, want)
	}
	huge := new(big.Int).Lsh(big.NewInt(1), 200)
	got := nextSqrtPriceFromAmount0RoundingUp(price, liquidity, huge)
	numerator := new(big.Int).Lsh(liquidity, 96)
	want := divRoundingUp(numerator, new(big.Int).Add(new(big.Int).Quo(numerator, price), huge))
	if got.Cmp(want) != 0 {
		t.Errorf("overflowing product: got %s, want %s", got, want)
	}
	if same := nextSqrtPriceFromAmount0RoundingUp(price, liquidity, new(big.Int)); same.Cmp(price) != 0 {
		t.Errorf("zero amount moved the price to %s", same)
	}
}

func TestNextSqrtPriceFromAmount1UsesMulDivAboveUint160(t *testing.T) {
	t.Parallel()
	price, liquidity := new(big.Int).Set(q96), big.NewInt(3)
	amount := new(big.Int).Add(maxUint160, big.NewInt(1))
	got := nextSqrtPriceFromAmount1RoundingDown(price, liquidity, amount)
	want := new(big.Int).Add(mulDiv(amount, q96, liquidity), price)
	if got.Cmp(want) != 0 {
		t.Errorf("got %s, want %s", got, want)
	}
}

// A walk through empty words costs a node call per word; past the bound the
// quote gives up instead of draining a public node's rate limit.
func TestQuoteExactInputBoundsTheWalk(t *testing.T) {
	t.Parallel()
	state := &fixtureState{words: map[string]string{}, ticks: map[string]string{}}
	counting := countingBlank{state: state}
	pool := Pool{SqrtPriceX96: new(big.Int).Set(q96), Tick: 0, Liquidity: big.NewInt(1), TickSpacing: 1, FeePips: 500}
	if _, err := QuoteExactInput(t.Context(), pool, &counting, new(big.Int).Lsh(big.NewInt(1), 100), true); err == nil {
		t.Fatal("an unbounded walk was allowed")
	}
	if counting.reads != maxSteps {
		t.Errorf("read %d words, want exactly %d", counting.reads, maxSteps)
	}
}

type countingBlank struct {
	state *fixtureState
	reads int
}

func (c *countingBlank) BitmapWord(context.Context, int16) (*big.Int, error) {
	c.reads++
	return new(big.Int), nil
}

func (c *countingBlank) LiquidityNet(context.Context, int) (*big.Int, error) {
	return new(big.Int), nil
}

// Crossing a tick that removes more liquidity than the range holds means the
// reads disagree with each other; the quote stops rather than go negative.
func TestQuoteExactInputRefusesInconsistentState(t *testing.T) {
	t.Parallel()
	// Tick -10 initialised in word -1 (compressed -10 → bit 246 of word -1).
	word := new(big.Int).Lsh(big.NewInt(1), 246)
	state := &fixtureState{
		words: map[string]string{"0": "0", "-1": word.String()},
		// Crossing down subtracts liquidityNet: +5 leaves 1-5 < 0.
		ticks: map[string]string{"-10": "5"},
	}
	pool := Pool{SqrtPriceX96: new(big.Int).Set(q96), Tick: 0, Liquidity: big.NewInt(1), TickSpacing: 1, FeePips: 500}
	_, err := QuoteExactInput(t.Context(), pool, state, big.NewInt(1_000_000), true)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("error = %v, want the negative-liquidity refusal", err)
	}
}

// Moving the price up, crossing a tick adds its net liquidity: a tick that
// doubles the liquidity has to pay out more than one that adds nothing.
func TestQuoteExactInputAddsLiquidityCrossingUp(t *testing.T) {
	t.Parallel()
	word := new(big.Int).Lsh(big.NewInt(1), 10).String()
	quote := func(net string) *big.Int {
		t.Helper()
		state := &fixtureState{words: map[string]string{"0": word}, ticks: map[string]string{"10": net}}
		pool := Pool{SqrtPriceX96: new(big.Int).Set(q96), Tick: 0, Liquidity: big.NewInt(1e18), TickSpacing: 1, FeePips: 500}
		out, err := QuoteExactInput(t.Context(), pool, state, big.NewInt(1e15), false)
		if err != nil {
			t.Fatalf("QuoteExactInput() error = %v", err)
		}
		return out
	}
	if flat, doubled := quote("0"), quote("1000000000000000000"); doubled.Cmp(flat) <= 0 {
		t.Errorf("crossing into twice the liquidity paid %s, no more than the flat %s", doubled, flat)
	}
}

func TestAmountDeltasAreSymmetric(t *testing.T) {
	t.Parallel()
	a, b, l := new(big.Int).Set(q96), new(big.Int).Mul(q96, big.NewInt(2)), big.NewInt(1_000_000)
	for _, roundUp := range []bool{true, false} {
		if amount0Delta(a, b, l, roundUp).Cmp(amount0Delta(b, a, l, roundUp)) != 0 {
			t.Error("amount0Delta depends on argument order")
		}
		if amount1Delta(a, b, l, roundUp).Cmp(amount1Delta(b, a, l, roundUp)) != 0 {
			t.Error("amount1Delta depends on argument order")
		}
	}
}

// Near the bottom of the price range the next tick is clamped to the minimum,
// where the price limit takes over as the target. A pool that runs out there
// has not paid for the whole input.
func TestQuoteExactInputStopsAtThePriceLimit(t *testing.T) {
	t.Parallel()
	price, err := sqrtRatioAtTick(minTick + 10)
	if err != nil {
		t.Fatal(err)
	}
	pool := Pool{SqrtPriceX96: price, Tick: minTick + 10, Liquidity: big.NewInt(1_000), TickSpacing: 200, FeePips: 500}
	_, err = QuoteExactInput(t.Context(), pool, blankState{}, new(big.Int).Lsh(big.NewInt(1), 120), true)
	if !errors.Is(err, errPoolDrains) {
		t.Fatalf("error = %v, want errPoolDrains", err)
	}
}

func TestNextInitializedTickPassesReadErrorsOn(t *testing.T) {
	t.Parallel()
	readErr := errors.New("node is down")
	for _, lte := range []bool{true, false} {
		if _, _, err := nextInitializedTick(t.Context(), failingState{err: readErr}, 0, 10, lte); !errors.Is(err, readErr) {
			t.Errorf("lte=%v: error = %v, want %v", lte, err, readErr)
		}
	}
}
