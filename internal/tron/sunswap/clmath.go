package sunswap

import (
	"context"
	"errors"
	"fmt"
	"math/big"
)

// The concentrated-liquidity arithmetic below is a port of Uniswap V3's
// TickMath, SqrtPriceMath, SwapMath and TickBitmap, which SunSwap V3 runs
// unchanged and SunSwap V4 inherits through its PancakeSwap Infinity lineage.
//
// It exists because SunSwap V4 publishes no quoter on Tron. The amount a V4
// swap pays out has to be computed from the pool's state, and the only way to
// get the same number the pool will produce is to repeat the pool's own integer
// arithmetic — rounding direction included. Every function keeps the name and
// rounding of the Solidity it mirrors, so the two can be read side by side, and
// overflow branches that change the result are reproduced rather than
// "fixed" by big integers.

const (
	minTick = -887272
	maxTick = 887272
	// feeDenominator is the unit of a pool fee: 500 means 0.05%.
	feeDenominator = 1_000_000
)

var (
	q96           = new(big.Int).Lsh(big.NewInt(1), 96)
	maxUint160    = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	maxUint256    = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	minSqrtRatio  = big.NewInt(4295128739)
	maxSqrtRatio  = mustBig("1461446703485210103287273052203988822378723970342")
	errPoolDrains = errors.New("the pool does not hold enough liquidity for this amount")
)

func mustBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 0)
	if !ok {
		panic("sunswap: bad constant " + s)
	}
	return v
}

// mulDiv is FullMath.mulDiv: floor(a·b / d).
func mulDiv(a, b, d *big.Int) *big.Int {
	p := new(big.Int).Mul(a, b)
	return p.Quo(p, d)
}

// mulDivRoundingUp is FullMath.mulDivRoundingUp: ceil(a·b / d).
func mulDivRoundingUp(a, b, d *big.Int) *big.Int {
	p := new(big.Int).Mul(a, b)
	q, r := new(big.Int).QuoRem(p, d, new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// divRoundingUp is UnsafeMath.divRoundingUp: ceil(a / b).
func divRoundingUp(a, b *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(a, b, new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// sqrtRatioAtTick is TickMath.getSqrtRatioAtTick: sqrt(1.0001^tick) as a
// Q64.96, computed with the same magic constants so it matches bit for bit.
func sqrtRatioAtTick(tick int) (*big.Int, error) {
	if tick < minTick || tick > maxTick {
		return nil, fmt.Errorf("tick %d is out of range", tick)
	}
	absTick := tick
	if absTick < 0 {
		absTick = -absTick
	}
	ratio := mustBig("0x100000000000000000000000000000000")
	if absTick&0x1 != 0 {
		ratio = mustBig("0xfffcb933bd6fad37aa2d162d1a594001")
	}
	for _, step := range tickRatios {
		if absTick&step.bit != 0 {
			ratio.Mul(ratio, step.factor)
			ratio.Rsh(ratio, 128)
		}
	}
	if tick > 0 {
		ratio = new(big.Int).Quo(maxUint256, ratio)
	}
	// Back from Q128.128 to Q128.96, rounding up so that getTickAtSqrtRatio of
	// the result is consistent.
	shifted := new(big.Int).Rsh(ratio, 32)
	if new(big.Int).And(ratio, big.NewInt(0xffffffff)).Sign() != 0 {
		shifted.Add(shifted, big.NewInt(1))
	}
	return shifted, nil
}

var tickRatios = []struct {
	bit    int
	factor *big.Int
}{
	{0x2, mustBig("0xfff97272373d413259a46990580e213a")},
	{0x4, mustBig("0xfff2e50f5f656932ef12357cf3c7fdcc")},
	{0x8, mustBig("0xffe5caca7e10e4e61c3624eaa0941cd0")},
	{0x10, mustBig("0xffcb9843d60f6159c9db58835c926644")},
	{0x20, mustBig("0xff973b41fa98c081472e6896dfb254c0")},
	{0x40, mustBig("0xff2ea16466c96a3843ec78b326b52861")},
	{0x80, mustBig("0xfe5dee046a99a2a811c461f1969c3053")},
	{0x100, mustBig("0xfcbe86c7900a88aedcffc83b479aa3a4")},
	{0x200, mustBig("0xf987a7253ac413176f2b074cf7815e54")},
	{0x400, mustBig("0xf3392b0822b70005940c7a398e4b70f3")},
	{0x800, mustBig("0xe7159475a2c29b7443b29c7fa6e889d9")},
	{0x1000, mustBig("0xd097f3bdfd2022b8845ad8f792aa5825")},
	{0x2000, mustBig("0xa9f746462d870fdf8a65dc1f90e061e5")},
	{0x4000, mustBig("0x70d869a156d2a1b890bb3df62baf32f7")},
	{0x8000, mustBig("0x31be135f97d08fd981231505542fcfa6")},
	{0x10000, mustBig("0x9aa508b5b7a84e1c677de54f3e99bc9")},
	{0x20000, mustBig("0x5d6af8dedb81196699c329225ee604")},
	{0x40000, mustBig("0x2216e584f5fa1ea926041bedfe98")},
	{0x80000, mustBig("0x48a170391f7dc42444e8fa2")},
}

// amount0Delta is SqrtPriceMath.getAmount0Delta.
func amount0Delta(a, b, liquidity *big.Int, roundUp bool) *big.Int {
	if a.Cmp(b) > 0 {
		a, b = b, a
	}
	numerator1 := new(big.Int).Lsh(liquidity, 96)
	numerator2 := new(big.Int).Sub(b, a)
	if roundUp {
		return divRoundingUp(mulDivRoundingUp(numerator1, numerator2, b), a)
	}
	return new(big.Int).Quo(mulDiv(numerator1, numerator2, b), a)
}

// amount1Delta is SqrtPriceMath.getAmount1Delta.
func amount1Delta(a, b, liquidity *big.Int, roundUp bool) *big.Int {
	if a.Cmp(b) > 0 {
		a, b = b, a
	}
	diff := new(big.Int).Sub(b, a)
	if roundUp {
		return mulDivRoundingUp(liquidity, diff, q96)
	}
	return mulDiv(liquidity, diff, q96)
}

// nextSqrtPriceFromInput is SqrtPriceMath.getNextSqrtPriceFromInput.
func nextSqrtPriceFromInput(sqrtPrice, liquidity, amountIn *big.Int, zeroForOne bool) *big.Int {
	if zeroForOne {
		return nextSqrtPriceFromAmount0RoundingUp(sqrtPrice, liquidity, amountIn)
	}
	return nextSqrtPriceFromAmount1RoundingDown(sqrtPrice, liquidity, amountIn)
}

// nextSqrtPriceFromAmount0RoundingUp is the add=true branch of
// SqrtPriceMath.getNextSqrtPriceFromAmount0RoundingUp. The first formula is
// only used when its intermediates fit in 256 bits; past that the contract
// falls back to a less precise one, and so must this.
func nextSqrtPriceFromAmount0RoundingUp(sqrtPrice, liquidity, amount *big.Int) *big.Int {
	if amount.Sign() == 0 {
		return new(big.Int).Set(sqrtPrice)
	}
	numerator1 := new(big.Int).Lsh(liquidity, 96)
	product := new(big.Int).Mul(amount, sqrtPrice)
	if product.Cmp(maxUint256) <= 0 {
		denominator := new(big.Int).Add(numerator1, product)
		if denominator.Cmp(maxUint256) <= 0 {
			return mulDivRoundingUp(numerator1, sqrtPrice, denominator)
		}
	}
	return divRoundingUp(numerator1, new(big.Int).Add(new(big.Int).Quo(numerator1, sqrtPrice), amount))
}

// nextSqrtPriceFromAmount1RoundingDown is the add=true branch of
// SqrtPriceMath.getNextSqrtPriceFromAmount1RoundingDown.
func nextSqrtPriceFromAmount1RoundingDown(sqrtPrice, liquidity, amount *big.Int) *big.Int {
	var quotient *big.Int
	if amount.Cmp(maxUint160) <= 0 {
		quotient = new(big.Int).Lsh(amount, 96)
		quotient.Quo(quotient, liquidity)
	} else {
		quotient = mulDiv(amount, q96, liquidity)
	}
	return quotient.Add(quotient, sqrtPrice)
}

// swapStep is SwapMath.computeSwapStep for an exact input.
type swapStep struct {
	sqrtNext  *big.Int
	amountIn  *big.Int
	amountOut *big.Int
	fee       *big.Int
}

func computeSwapStep(current, target, liquidity, remaining *big.Int, feePips int64) swapStep {
	zeroForOne := current.Cmp(target) >= 0
	fee := big.NewInt(feePips)
	keep := big.NewInt(feeDenominator - feePips)
	remainingLessFee := mulDiv(remaining, keep, big.NewInt(feeDenominator))

	var amountIn *big.Int
	if zeroForOne {
		amountIn = amount0Delta(target, current, liquidity, true)
	} else {
		amountIn = amount1Delta(current, target, liquidity, true)
	}

	var next *big.Int
	if remainingLessFee.Cmp(amountIn) >= 0 {
		next = new(big.Int).Set(target)
	} else {
		next = nextSqrtPriceFromInput(current, liquidity, remainingLessFee, zeroForOne)
	}
	reached := next.Cmp(target) == 0

	var amountOut *big.Int
	if zeroForOne {
		if !reached {
			amountIn = amount0Delta(next, current, liquidity, true)
		}
		amountOut = amount1Delta(next, current, liquidity, false)
	} else {
		if !reached {
			amountIn = amount1Delta(current, next, liquidity, true)
		}
		amountOut = amount0Delta(current, next, liquidity, false)
	}

	var feeAmount *big.Int
	if !reached {
		// The step ran out of input before the target: whatever was not
		// swapped is the fee.
		feeAmount = new(big.Int).Sub(remaining, amountIn)
	} else {
		feeAmount = mulDivRoundingUp(amountIn, fee, keep)
	}
	return swapStep{sqrtNext: next, amountIn: amountIn, amountOut: amountOut, fee: feeAmount}
}

// PoolState is what an exact-input quote needs to read from a pool beyond its
// current price: the tick bitmap and the liquidity change at each initialised
// tick. Reads go to the chain, so the quote asks only for the words and ticks
// the swap actually walks through.
type PoolState interface {
	// BitmapWord returns the 256-bit word at position word of the tick bitmap.
	BitmapWord(ctx context.Context, word int16) (*big.Int, error)
	// LiquidityNet returns the signed liquidity added when the price crosses
	// tick moving up.
	LiquidityNet(ctx context.Context, tick int) (*big.Int, error)
}

// Pool is the part of a pool's state that a quote starts from.
type Pool struct {
	SqrtPriceX96 *big.Int
	Tick         int
	Liquidity    *big.Int
	TickSpacing  int
	// FeePips is the whole fee taken from the input, in millionths. For a V4
	// pool that is the LP fee combined with any protocol fee.
	FeePips int64
}

// maxSteps bounds how many initialised ticks and bitmap words a quote may walk
// through. Each one costs a node call, and a swap that needs more than this is
// far beyond what a wallet's single-pool route should be used for.
const maxSteps = 64

// QuoteExactInput returns how much of the other token a pool pays for amountIn,
// following Pool.swap's loop with no price limit.
func QuoteExactInput(ctx context.Context, pool Pool, state PoolState, amountIn *big.Int, zeroForOne bool) (*big.Int, error) {
	if amountIn == nil || amountIn.Sign() <= 0 {
		return nil, errors.New("amount must be greater than zero")
	}
	if pool.TickSpacing <= 0 {
		return nil, errors.New("pool tick spacing must be positive")
	}
	if pool.FeePips < 0 || pool.FeePips >= feeDenominator {
		return nil, fmt.Errorf("pool fee %d is out of range", pool.FeePips)
	}
	limit := new(big.Int).Add(minSqrtRatio, big.NewInt(1))
	if !zeroForOne {
		limit = new(big.Int).Sub(maxSqrtRatio, big.NewInt(1))
	}

	remaining := new(big.Int).Set(amountIn)
	out := new(big.Int)
	sqrtPrice := new(big.Int).Set(pool.SqrtPriceX96)
	tick := pool.Tick
	liquidity := new(big.Int).Set(pool.Liquidity)

	for steps := 0; remaining.Sign() > 0 && sqrtPrice.Cmp(limit) != 0; steps++ {
		if steps >= maxSteps {
			return nil, fmt.Errorf("the swap crosses more than %d price ranges; try a smaller amount", maxSteps)
		}
		next, initialized, err := nextInitializedTick(ctx, state, tick, pool.TickSpacing, zeroForOne)
		if err != nil {
			return nil, err
		}
		next = min(max(next, minTick), maxTick)
		sqrtNext, err := sqrtRatioAtTick(next)
		if err != nil {
			// Unreachable: next was clamped into range on the line above.
			return nil, err
		}
		target := sqrtNext
		if (zeroForOne && sqrtNext.Cmp(limit) < 0) || (!zeroForOne && sqrtNext.Cmp(limit) > 0) {
			target = limit
		}

		step := computeSwapStep(sqrtPrice, target, liquidity, remaining, pool.FeePips)
		sqrtPrice = step.sqrtNext
		remaining.Sub(remaining, step.amountIn).Sub(remaining, step.fee)
		out.Add(out, step.amountOut)

		if sqrtPrice.Cmp(sqrtNext) != 0 {
			// The input ran out inside the current range.
			continue
		}
		if initialized {
			net, err := state.LiquidityNet(ctx, next)
			if err != nil {
				return nil, err
			}
			if zeroForOne {
				liquidity.Sub(liquidity, net)
			} else {
				liquidity.Add(liquidity, net)
			}
			if liquidity.Sign() < 0 {
				return nil, errors.New("pool liquidity went negative; its state changed while it was being read")
			}
		}
		if zeroForOne {
			tick = next - 1
		} else {
			tick = next
		}
	}
	if remaining.Sign() > 0 {
		return nil, errPoolDrains
	}
	return out, nil
}

// nextInitializedTick is TickBitmap.nextInitializedTickWithinOneWord.
func nextInitializedTick(ctx context.Context, state PoolState, tick, spacing int, lte bool) (int, bool, error) {
	compressed := tick / spacing
	if tick < 0 && tick%spacing != 0 {
		compressed--
	}
	if lte {
		wordPos, bitPos := int16(compressed>>8), uint(compressed&0xff)
		word, err := state.BitmapWord(ctx, wordPos)
		if err != nil {
			return 0, false, err
		}
		// All the bits at or below bitPos.
		mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bitPos+1), big.NewInt(1))
		masked := new(big.Int).And(word, mask)
		if masked.Sign() != 0 {
			msb := masked.BitLen() - 1
			return (compressed - int(bitPos) + msb) * spacing, true, nil
		}
		return (compressed - int(bitPos)) * spacing, false, nil
	}
	compressed++
	wordPos, bitPos := int16(compressed>>8), uint(compressed&0xff)
	word, err := state.BitmapWord(ctx, wordPos)
	if err != nil {
		return 0, false, err
	}
	// All the bits at or above bitPos.
	mask := new(big.Int).Lsh(new(big.Int).Rsh(maxUint256, bitPos), bitPos)
	masked := new(big.Int).And(word, mask)
	if masked.Sign() != 0 {
		lsb := int(masked.TrailingZeroBits())
		return (compressed + lsb - int(bitPos)) * spacing, true, nil
	}
	return (compressed + 255 - int(bitPos)) * spacing, false, nil
}

// SwapFee combines a V4 pool's LP fee with its protocol fee for one direction,
// as ProtocolFeeLibrary.calculateSwapFee does. The protocol fee word carries
// two 12-bit fees, zeroForOne in the low bits.
func SwapFee(protocolFee uint32, lpFee uint32, zeroForOne bool) int64 {
	pf := int64(protocolFee >> 12)
	if zeroForOne {
		pf = int64(protocolFee & 0xfff)
	}
	lp := int64(lpFee)
	if pf == 0 {
		return lp
	}
	return pf + lp - pf*lp/feeDenominator
}
