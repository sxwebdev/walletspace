package raydium

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"time"

	"github.com/gagliardetto/solana-go"
)

// AMM v4 (raydium-amm, program/src/state.rs AmmInfo): a packed 752-byte
// account with no discriminator. Offsets: 16 u64 fields, Fees (8 u64) at 128,
// StateData (144 bytes) at 192, then the keys from 336.
const (
	ammV4Size = 752

	ammV4StatusOffset           = 0
	ammV4NonceOffset            = 8
	ammV4CoinDecimalsOffset     = 32
	ammV4PcDecimalsOffset       = 40
	ammV4SwapFeeNumOffset       = 128 + 48
	ammV4SwapFeeDenOffset       = 128 + 56
	ammV4NeedTakePnlCoinOffset  = 192
	ammV4NeedTakePnlPcOffset    = 200
	ammV4PoolOpenTimeOffset     = 192 + 32
	ammV4CoinVaultOffset        = 336
	ammV4PcVaultOffset          = 368
	ammV4CoinMintOffset         = 400
	ammV4PcMintOffset           = 432
	ammV4LPMintOffset           = 464
	ammV4OpenOrdersOffset       = 496
	ammV4MarketOffset           = 528
	ammV4MarketProgramOffset    = 560
	ammV4TargetOrdersOffset     = 592
	ammV4OwnerOffset            = 688
	ammV4SwapBaseInV2Tag        = 16
	ammV4SwapBaseInV2DataLength = 17
	ammV4SwapBaseInV2Accounts   = 8
)

// ammV4AuthoritySeed is AUTHORITY_AMM in program/src/processor.rs.
var ammV4AuthoritySeed = []byte("amm authority")

// AMMV4 pool statuses (state.rs AmmStatus).
const (
	AMMV4StatusInitialized  = 1
	AMMV4StatusSwapOnly     = 6
	AMMV4StatusWaitingTrade = 7
)

// AMMV4Pool is what a swap and its check need from an AMM v4 pool account.
type AMMV4Pool struct {
	Status       uint64
	Nonce        uint8
	CoinDecimals uint64
	PcDecimals   uint64
	// SwapFeeNumerator / SwapFeeDenominator is the fee taken from the input.
	SwapFeeNumerator   uint64
	SwapFeeDenominator uint64
	// NeedTakePnlCoin and NeedTakePnlPc sit in the vaults but are not part
	// of the reserves the curve uses.
	NeedTakePnlCoin uint64
	NeedTakePnlPc   uint64
	PoolOpenTime    uint64
	CoinVault       solana.PublicKey
	PcVault         solana.PublicKey
	CoinMint        solana.PublicKey
	PcMint          solana.PublicKey
	LPMint          solana.PublicKey
	OpenOrders      solana.PublicKey
	Market          solana.PublicKey
	MarketProgram   solana.PublicKey
	TargetOrders    solana.PublicKey
	Owner           solana.PublicKey
	// Authority is derived as the program does it:
	// create_program_address(["amm authority", nonce]).
	Authority solana.PublicKey
}

// DecodeAMMV4Pool decodes an AMM v4 pool account. The owner is the account's
// owner as the node reported it.
func DecodeAMMV4Pool(owner solana.PublicKey, data []byte) (AMMV4Pool, error) {
	if err := checkAccount("AMM v4 pool", AMMV4Program, owner, data, ammV4Size, nil); err != nil {
		return AMMV4Pool{}, err
	}
	l := layout(data)
	nonce := l.u64(ammV4NonceOffset)
	status := l.u64(ammV4StatusOffset)
	// The program refuses status 0 (load_checked) and panics on anything
	// above 7 (AmmStatus::from_u64).
	if status == 0 || status > AMMV4StatusWaitingTrade {
		return AMMV4Pool{}, fmt.Errorf("%w: AMM v4 status %d", ErrMalformedAccount, status)
	}
	if nonce > 255 {
		return AMMV4Pool{}, fmt.Errorf("%w: AMM v4 nonce %d", ErrMalformedAccount, nonce)
	}
	authority, err := solana.CreateProgramAddress([][]byte{ammV4AuthoritySeed, {byte(nonce)}}, AMMV4Program)
	if err != nil {
		return AMMV4Pool{}, fmt.Errorf("%w: AMM v4 nonce %d gives no authority: %w", ErrMalformedAccount, nonce, err)
	}
	return AMMV4Pool{
		Status:             status,
		Nonce:              uint8(nonce),
		CoinDecimals:       l.u64(ammV4CoinDecimalsOffset),
		PcDecimals:         l.u64(ammV4PcDecimalsOffset),
		SwapFeeNumerator:   l.u64(ammV4SwapFeeNumOffset),
		SwapFeeDenominator: l.u64(ammV4SwapFeeDenOffset),
		NeedTakePnlCoin:    l.u64(ammV4NeedTakePnlCoinOffset),
		NeedTakePnlPc:      l.u64(ammV4NeedTakePnlPcOffset),
		PoolOpenTime:       l.u64(ammV4PoolOpenTimeOffset),
		CoinVault:          l.key(ammV4CoinVaultOffset),
		PcVault:            l.key(ammV4PcVaultOffset),
		CoinMint:           l.key(ammV4CoinMintOffset),
		PcMint:             l.key(ammV4PcMintOffset),
		LPMint:             l.key(ammV4LPMintOffset),
		OpenOrders:         l.key(ammV4OpenOrdersOffset),
		Market:             l.key(ammV4MarketOffset),
		MarketProgram:      l.key(ammV4MarketProgramOffset),
		TargetOrders:       l.key(ammV4TargetOrdersOffset),
		Owner:              l.key(ammV4OwnerOffset),
		Authority:          authority,
	}, nil
}

// Swappable reports whether the program would accept a swap at now:
// process_swap_base_in_v2 requires swap_permission (Initialized, SwapOnly or
// WaitingTrade) and, while WaitingTrade, now >= pool_open_time. It also
// refuses a pool whose two mints are the same, and a zero fee denominator
// would make the program's own fee division fail.
func (p AMMV4Pool) Swappable(now time.Time) error {
	switch p.Status {
	case AMMV4StatusInitialized, AMMV4StatusSwapOnly:
	case AMMV4StatusWaitingTrade:
		if uint64(now.Unix()) < p.PoolOpenTime {
			return fmt.Errorf("%w: AMM v4 pool opens at %d", ErrPoolNotSwappable, p.PoolOpenTime)
		}
	default:
		return fmt.Errorf("%w: AMM v4 status %d does not allow swaps", ErrPoolNotSwappable, p.Status)
	}
	if p.CoinMint == p.PcMint {
		return fmt.Errorf("%w: AMM v4 pool trades a mint against itself", ErrPoolNotSwappable)
	}
	if p.SwapFeeDenominator == 0 || p.SwapFeeNumerator >= p.SwapFeeDenominator {
		return fmt.Errorf("%w: AMM v4 swap fee %d/%d", ErrPoolNotSwappable, p.SwapFeeNumerator, p.SwapFeeDenominator)
	}
	return nil
}

// AMMV4Swap is a SwapBaseInV2 instruction (instruction.rs, tag 16).
type AMMV4Swap struct {
	Pool            solana.PublicKey
	Authority       solana.PublicKey
	CoinVault       solana.PublicKey
	PcVault         solana.PublicKey
	UserSource      solana.PublicKey
	UserDestination solana.PublicKey
	UserOwner       solana.PublicKey
	AmountIn        uint64
	MinAmountOut    uint64
}

// Swap lays out a swap of inputMint through the pool with the given id: the
// pool's own authority and vaults, the user's accounts and amounts.
func (p AMMV4Pool) Swap(pool, inputMint, userSource, userDestination, userOwner solana.PublicKey, amountIn, minAmountOut uint64) (AMMV4Swap, error) {
	if inputMint != p.CoinMint && inputMint != p.PcMint {
		return AMMV4Swap{}, fmt.Errorf("%w: AMM v4 pool %s does not hold %s", ErrPoolNotSwappable, pool, inputMint)
	}
	return AMMV4Swap{
		Pool:            pool,
		Authority:       p.Authority,
		CoinVault:       p.CoinVault,
		PcVault:         p.PcVault,
		UserSource:      userSource,
		UserDestination: userDestination,
		UserOwner:       userOwner,
		AmountIn:        amountIn,
		MinAmountOut:    minAmountOut,
	}, nil
}

// Instruction encodes the swap exactly as swap_base_in_v2 in instruction.rs
// does: the classic token program, the pool (writable), the authority, both
// vaults (writable), the user's source and destination (writable) and the
// user as the only signer; data is the tag, amount_in and
// minimum_amount_out, little-endian.
func (s AMMV4Swap) Instruction() Instruction {
	data := make([]byte, 0, ammV4SwapBaseInV2DataLength)
	data = append(data, ammV4SwapBaseInV2Tag)
	data = binary.LittleEndian.AppendUint64(data, s.AmountIn)
	data = binary.LittleEndian.AppendUint64(data, s.MinAmountOut)
	return Instruction{
		Program: AMMV4Program,
		Accounts: []solana.AccountMeta{
			readonly(solana.TokenProgramID),
			writable(s.Pool),
			readonly(s.Authority),
			writable(s.CoinVault),
			writable(s.PcVault),
			writable(s.UserSource),
			writable(s.UserDestination),
			signer(s.UserOwner),
		},
		Data: data,
	}
}

// DecodeAMMV4Swap is the inverse of AMMV4Swap.Instruction. It refuses
// anything that is not exactly a SwapBaseInV2: another program or tag,
// another token program, another account count, or data of another length —
// the program itself ignores trailing bytes, this decoder does not.
func DecodeAMMV4Swap(program solana.PublicKey, accounts []solana.PublicKey, data []byte) (AMMV4Swap, error) {
	if program != AMMV4Program {
		return AMMV4Swap{}, fmt.Errorf("%w: program %s is not AMM v4", ErrMalformedInstruction, program)
	}
	if len(data) != ammV4SwapBaseInV2DataLength || data[0] != ammV4SwapBaseInV2Tag {
		return AMMV4Swap{}, fmt.Errorf("%w: AMM v4 data is not SwapBaseInV2", ErrMalformedInstruction)
	}
	if len(accounts) != ammV4SwapBaseInV2Accounts {
		return AMMV4Swap{}, fmt.Errorf("%w: AMM v4 swap lists %d accounts, want %d", ErrMalformedInstruction, len(accounts), ammV4SwapBaseInV2Accounts)
	}
	if accounts[0] != solana.TokenProgramID {
		return AMMV4Swap{}, fmt.Errorf("%w: AMM v4 swap names token program %s", ErrMalformedInstruction, accounts[0])
	}
	return AMMV4Swap{
		Pool:            accounts[1],
		Authority:       accounts[2],
		CoinVault:       accounts[3],
		PcVault:         accounts[4],
		UserSource:      accounts[5],
		UserDestination: accounts[6],
		UserOwner:       accounts[7],
		AmountIn:        binary.LittleEndian.Uint64(data[1:]),
		MinAmountOut:    binary.LittleEndian.Uint64(data[9:]),
	}, nil
}

// AmountOut is what process_swap_base_in_v2 pays for amountIn of inputMint,
// given the two vault balances: the fee is ceil(amount_in × num / den), the
// reserves are the vault balances less the pending PnL, and the output is
// reserve_out × in / (reserve_in + in), rounded down (math.rs
// swap_token_amount_base_in).
func (p AMMV4Pool) AmountOut(inputMint solana.PublicKey, amountIn, coinVaultAmount, pcVaultAmount uint64) (uint64, error) {
	if p.SwapFeeDenominator == 0 {
		return 0, fmt.Errorf("%w: AMM v4 fee denominator is zero", ErrPoolNotSwappable)
	}
	if coinVaultAmount < p.NeedTakePnlCoin || pcVaultAmount < p.NeedTakePnlPc {
		return 0, fmt.Errorf("%w: AMM v4 vaults hold less than the pending PnL", ErrPoolNotSwappable)
	}
	coin := new(big.Int).SetUint64(coinVaultAmount - p.NeedTakePnlCoin)
	pc := new(big.Int).SetUint64(pcVaultAmount - p.NeedTakePnlPc)
	var reserveIn, reserveOut *big.Int
	switch inputMint {
	case p.CoinMint:
		reserveIn, reserveOut = coin, pc
	case p.PcMint:
		reserveIn, reserveOut = pc, coin
	default:
		return 0, fmt.Errorf("%w: AMM v4 pool does not hold %s", ErrPoolNotSwappable, inputMint)
	}
	in := new(big.Int).SetUint64(amountIn)
	fee := ceilDiv(new(big.Int).Mul(in, new(big.Int).SetUint64(p.SwapFeeNumerator)), new(big.Int).SetUint64(p.SwapFeeDenominator))
	in.Sub(in, fee)
	return constantProductOut(in, reserveIn, reserveOut)
}

// constantProductOut is reserve_out × in / (reserve_in + in), rounded down,
// required to fit a u64 and to stay below the output reserve.
func constantProductOut(in, reserveIn, reserveOut *big.Int) (uint64, error) {
	denominator := new(big.Int).Add(reserveIn, in)
	if denominator.Sign() == 0 {
		return 0, fmt.Errorf("%w: empty pool", ErrPoolNotSwappable)
	}
	out := new(big.Int).Mul(reserveOut, in)
	out.Quo(out, denominator)
	if out.Cmp(reserveOut) >= 0 || !out.IsUint64() {
		return 0, fmt.Errorf("%w: the pool cannot pay %s", ErrPoolNotSwappable, out)
	}
	return out.Uint64(), nil
}

func ceilDiv(numerator, denominator *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}
