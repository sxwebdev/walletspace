package raydium

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"time"

	"github.com/gagliardetto/solana-go"
)

// CLMM (raydium-clmm, programs/amm/src/states/pool.rs PoolState and
// states/config.rs AmmConfig): Anchor accounts, an 8-byte discriminator
// followed by the packed fields.
const (
	clmmPoolSize   = 1544
	clmmConfigSize = 117

	clmmBumpOffset            = 8
	clmmAmmConfigOffset       = 9
	clmmOwnerOffset           = 41
	clmmMint0Offset           = 73
	clmmMint1Offset           = 105
	clmmVault0Offset          = 137
	clmmVault1Offset          = 169
	clmmObservationOffset     = 201
	clmmDecimals0Offset       = 233
	clmmDecimals1Offset       = 234
	clmmTickSpacingOffset     = 235
	clmmLiquidityOffset       = 237
	clmmSqrtPriceOffset       = 253
	clmmTickCurrentOffset     = 269
	clmmStatusOffset          = 389
	clmmFeeOnOffset           = 390
	clmmTickArrayBitmapOffset = 904
	clmmOpenTimeOffset        = 1080

	clmmConfigBumpOffset        = 8
	clmmConfigIndexOffset       = 9
	clmmConfigOwnerOffset       = 11
	clmmConfigProtocolFeeOffset = 43
	clmmConfigTradeFeeOffset    = 47
	clmmConfigTickSpacingOffset = 51
	clmmConfigFundFeeOffset     = 53
	clmmConfigFundOwnerOffset   = 61

	// PoolStatusBitIndex::Swap is bit 4.
	clmmSwapStatusBit = 1 << 4

	clmmSwapV2DataLength    = 41
	clmmSwapV2FixedAccounts = 13
	// MaxTickArrays bounds the tick arrays one swap may carry. The program
	// takes any number; the Raydium client sends the current array and up
	// to five more, the Trade API usually one or two.
	MaxTickArrays = 10
)

// sha256("global:swap_v2")[:8].
var clmmSwapV2Data = mustHex("2b04ed0b1ac91e62")

// CLMMPool is what a swap and its check need from a CLMM pool account.
type CLMMPool struct {
	Bump        uint8
	AmmConfig   solana.PublicKey
	Owner       solana.PublicKey
	Mint0       solana.PublicKey
	Mint1       solana.PublicKey
	Vault0      solana.PublicKey
	Vault1      solana.PublicKey
	Observation solana.PublicKey
	Decimals0   uint8
	Decimals1   uint8
	TickSpacing uint16
	Liquidity   *big.Int
	// SqrtPriceX64 is sqrt(token1/token0) as Q64.64.
	SqrtPriceX64 *big.Int
	TickCurrent  int32
	// Status bit 4 disables swaps.
	Status uint8
	FeeOn  uint8
	// TickArrayBitmap marks the initialized tick arrays whose start index
	// lies within ±512 arrays of zero; the rest are in the bitmap
	// extension account.
	TickArrayBitmap [16]uint64
	OpenTime        uint64
}

// DecodeCLMMPool decodes a CLMM pool account owned by owner.
func DecodeCLMMPool(owner solana.PublicKey, data []byte) (CLMMPool, error) {
	if err := checkAccount("CLMM pool", CLMMProgram, owner, data, clmmPoolSize, poolStateDiscriminator); err != nil {
		return CLMMPool{}, err
	}
	l := layout(data)
	p := CLMMPool{
		Bump:         l.u8(clmmBumpOffset),
		AmmConfig:    l.key(clmmAmmConfigOffset),
		Owner:        l.key(clmmOwnerOffset),
		Mint0:        l.key(clmmMint0Offset),
		Mint1:        l.key(clmmMint1Offset),
		Vault0:       l.key(clmmVault0Offset),
		Vault1:       l.key(clmmVault1Offset),
		Observation:  l.key(clmmObservationOffset),
		Decimals0:    l.u8(clmmDecimals0Offset),
		Decimals1:    l.u8(clmmDecimals1Offset),
		TickSpacing:  l.u16(clmmTickSpacingOffset),
		Liquidity:    l.u128(clmmLiquidityOffset),
		SqrtPriceX64: l.u128(clmmSqrtPriceOffset),
		TickCurrent:  l.i32(clmmTickCurrentOffset),
		Status:       l.u8(clmmStatusOffset),
		FeeOn:        l.u8(clmmFeeOnOffset),
		OpenTime:     l.u64(clmmOpenTimeOffset),
	}
	for i := range p.TickArrayBitmap {
		p.TickArrayBitmap[i] = l.u64(clmmTickArrayBitmapOffset + 8*i)
	}
	if p.TickSpacing == 0 || p.TickCurrent < minTick || p.TickCurrent > maxTick {
		return CLMMPool{}, fmt.Errorf("%w: CLMM tick spacing %d, current tick %d", ErrMalformedAccount, p.TickSpacing, p.TickCurrent)
	}
	return p, nil
}

// Swappable reports whether the program would accept a swap at now
// (swap_v2.rs: block_timestamp > open_time; swap.rs swap_internal: the swap
// status bit clear). Whether the mints are classic SPL tokens is not stored
// in the pool: the caller checks the owners of the two mint accounts.
func (p CLMMPool) Swappable(now time.Time) error {
	if p.Status&clmmSwapStatusBit != 0 {
		return fmt.Errorf("%w: CLMM status %#x disables swaps", ErrPoolNotSwappable, p.Status)
	}
	if uint64(now.Unix()) <= p.OpenTime {
		return fmt.Errorf("%w: CLMM pool opens after %d", ErrPoolNotSwappable, p.OpenTime)
	}
	if p.Mint0 == p.Mint1 {
		return fmt.Errorf("%w: CLMM pool trades a mint against itself", ErrPoolNotSwappable)
	}
	return nil
}

// ZeroForOne reports the swap direction for inputMint: true when token 0 is
// sold for token 1.
func (p CLMMPool) ZeroForOne(inputMint solana.PublicKey) (bool, error) {
	switch inputMint {
	case p.Mint0:
		return true, nil
	case p.Mint1:
		return false, nil
	}
	return false, fmt.Errorf("%w: CLMM pool does not hold %s", ErrPoolNotSwappable, inputMint)
}

// CLMMConfig is a CLMM AmmConfig: fee rates in millionths and the tick
// spacing pools under it use.
type CLMMConfig struct {
	Bump            uint8
	Index           uint16
	Owner           solana.PublicKey
	ProtocolFeeRate uint32
	TradeFeeRate    uint32
	TickSpacing     uint16
	FundFeeRate     uint32
	FundOwner       solana.PublicKey
}

// DecodeCLMMConfig decodes a CLMM AmmConfig account owned by owner.
func DecodeCLMMConfig(owner solana.PublicKey, data []byte) (CLMMConfig, error) {
	if err := checkAccount("CLMM config", CLMMProgram, owner, data, clmmConfigSize, ammConfigDiscriminator); err != nil {
		return CLMMConfig{}, err
	}
	l := layout(data)
	return CLMMConfig{
		Bump:            l.u8(clmmConfigBumpOffset),
		Index:           l.u16(clmmConfigIndexOffset),
		Owner:           l.key(clmmConfigOwnerOffset),
		ProtocolFeeRate: l.u32(clmmConfigProtocolFeeOffset),
		TradeFeeRate:    l.u32(clmmConfigTradeFeeOffset),
		TickSpacing:     l.u16(clmmConfigTickSpacingOffset),
		FundFeeRate:     l.u32(clmmConfigFundFeeOffset),
		FundOwner:       l.key(clmmConfigFundOwnerOffset),
	}, nil
}

// CLMMSwap is a swap_v2 instruction (swap_v2.rs, struct SwapSingleV2) for
// an exact input with no price limit.
//
// sqrt_price_limit_x64 is always 0: the program then walks to
// MIN_SQRT_PRICE_X64+1 or MAX_SQRT_PRICE_X64-1 for the direction and, only
// in that case, refuses a partial fill (swap_v2.rs, "Does't allow partial
// filled without specified limit_price"), so the whole input is swapped or
// the transaction fails. is_base_input is always true.
type CLMMSwap struct {
	Payer              solana.PublicKey
	AmmConfig          solana.PublicKey
	Pool               solana.PublicKey
	InputTokenAccount  solana.PublicKey
	OutputTokenAccount solana.PublicKey
	InputVault         solana.PublicKey
	OutputVault        solana.PublicKey
	Observation        solana.PublicKey
	InputVaultMint     solana.PublicKey
	OutputVaultMint    solana.PublicKey
	// WithBitmapExtension adds the pool's bitmap extension account — the PDA
	// of ["pool_tick_array_bitmap_extension", pool] — as the first remaining
	// account. Leave it out only when that account does not exist: the
	// program stops reading remaining accounts at the first one that is
	// neither a tick array nor an extension.
	WithBitmapExtension bool
	TickArrays          []solana.PublicKey
	Amount              uint64
	// OtherAmountThreshold is the minimum output.
	OtherAmountThreshold uint64
}

// Swap lays out a swap of inputMint through the pool with the given id, with
// the pool's config, vaults, observation account and the tick arrays the
// caller checked (see CheckTickArrays).
func (p CLMMPool) Swap(pool, inputMint, payer, inputTokenAccount, outputTokenAccount solana.PublicKey, withBitmapExtension bool, tickArrays []solana.PublicKey, amount, minAmountOut uint64) (CLMMSwap, error) {
	zeroForOne, err := p.ZeroForOne(inputMint)
	if err != nil {
		return CLMMSwap{}, err
	}
	s := CLMMSwap{
		Payer:                payer,
		AmmConfig:            p.AmmConfig,
		Pool:                 pool,
		InputTokenAccount:    inputTokenAccount,
		OutputTokenAccount:   outputTokenAccount,
		Observation:          p.Observation,
		WithBitmapExtension:  withBitmapExtension,
		TickArrays:           append([]solana.PublicKey(nil), tickArrays...),
		Amount:               amount,
		OtherAmountThreshold: minAmountOut,
	}
	if zeroForOne {
		s.InputVault, s.OutputVault = p.Vault0, p.Vault1
		s.InputVaultMint, s.OutputVaultMint = p.Mint0, p.Mint1
	} else {
		s.InputVault, s.OutputVault = p.Vault1, p.Vault0
		s.InputVaultMint, s.OutputVaultMint = p.Mint1, p.Mint0
	}
	return s, nil
}

// Instruction encodes the swap in the account order of SwapSingleV2, then
// the remaining accounts: the bitmap extension, then the tick arrays, all
// writable — the program may flip a bit in the extension (pool.rs
// flip_tick_array_bit loads it mutably) and updates the tick arrays. Data is
// the Anchor discriminator, amount, other_amount_threshold,
// sqrt_price_limit_x64 (u128) and is_base_input (bool), little-endian.
func (s CLMMSwap) Instruction() (Instruction, error) {
	if len(s.TickArrays) == 0 || len(s.TickArrays) > MaxTickArrays {
		return Instruction{}, fmt.Errorf("%w: %d tick arrays, want 1..%d", ErrMalformedInstruction, len(s.TickArrays), MaxTickArrays)
	}
	accounts := []solana.AccountMeta{
		signer(s.Payer),
		readonly(s.AmmConfig),
		writable(s.Pool),
		writable(s.InputTokenAccount),
		writable(s.OutputTokenAccount),
		writable(s.InputVault),
		writable(s.OutputVault),
		writable(s.Observation),
		readonly(solana.TokenProgramID),
		readonly(solana.Token2022ProgramID),
		readonly(solana.MemoProgramID),
		readonly(s.InputVaultMint),
		readonly(s.OutputVaultMint),
	}
	if s.WithBitmapExtension {
		accounts = append(accounts, writable(CLMMBitmapExtensionAddress(s.Pool)))
	}
	for _, key := range s.TickArrays {
		accounts = append(accounts, writable(key))
	}
	data := make([]byte, 0, clmmSwapV2DataLength)
	data = append(data, clmmSwapV2Data...)
	data = binary.LittleEndian.AppendUint64(data, s.Amount)
	data = binary.LittleEndian.AppendUint64(data, s.OtherAmountThreshold)
	data = append(data, make([]byte, 16)...) // sqrt_price_limit_x64 = 0
	data = append(data, 1)                   // is_base_input = true
	return Instruction{Program: CLMMProgram, Accounts: accounts, Data: data}, nil
}

// DecodeCLMMSwap is the inverse of CLMMSwap.Instruction. It refuses another
// program or discriminator, data of another length, a price limit other than
// 0, an exact-output swap, other token, Token-2022 or memo programs, and
// remaining accounts other than the optional bitmap extension of this pool
// followed by 1..MaxTickArrays distinct tick arrays.
func DecodeCLMMSwap(program solana.PublicKey, accounts []solana.PublicKey, data []byte) (CLMMSwap, error) {
	if program != CLMMProgram {
		return CLMMSwap{}, fmt.Errorf("%w: program %s is not CLMM", ErrMalformedInstruction, program)
	}
	if len(data) != clmmSwapV2DataLength || string(data[:anchorDiscriminatorLength]) != string(clmmSwapV2Data) {
		return CLMMSwap{}, fmt.Errorf("%w: CLMM data is not swap_v2", ErrMalformedInstruction)
	}
	for _, b := range data[24:40] {
		if b != 0 {
			return CLMMSwap{}, fmt.Errorf("%w: CLMM swap carries a price limit", ErrMalformedInstruction)
		}
	}
	if data[40] != 1 {
		return CLMMSwap{}, fmt.Errorf("%w: CLMM swap is not an exact-input swap", ErrMalformedInstruction)
	}
	if len(accounts) < clmmSwapV2FixedAccounts+1 {
		return CLMMSwap{}, fmt.Errorf("%w: CLMM swap lists %d accounts", ErrMalformedInstruction, len(accounts))
	}
	if accounts[8] != solana.TokenProgramID || accounts[9] != solana.Token2022ProgramID || accounts[10] != solana.MemoProgramID {
		return CLMMSwap{}, fmt.Errorf("%w: CLMM swap names other token or memo programs", ErrMalformedInstruction)
	}
	s := CLMMSwap{
		Payer:                accounts[0],
		AmmConfig:            accounts[1],
		Pool:                 accounts[2],
		InputTokenAccount:    accounts[3],
		OutputTokenAccount:   accounts[4],
		InputVault:           accounts[5],
		OutputVault:          accounts[6],
		Observation:          accounts[7],
		InputVaultMint:       accounts[11],
		OutputVaultMint:      accounts[12],
		Amount:               binary.LittleEndian.Uint64(data[8:]),
		OtherAmountThreshold: binary.LittleEndian.Uint64(data[16:]),
	}
	extension := CLMMBitmapExtensionAddress(s.Pool)
	rest := accounts[clmmSwapV2FixedAccounts:]
	if rest[0] == extension {
		s.WithBitmapExtension = true
		rest = rest[1:]
	}
	if len(rest) == 0 || len(rest) > MaxTickArrays {
		return CLMMSwap{}, fmt.Errorf("%w: CLMM swap carries %d tick arrays, want 1..%d", ErrMalformedInstruction, len(rest), MaxTickArrays)
	}
	seen := make(map[solana.PublicKey]bool, len(rest))
	for _, key := range rest {
		if seen[key] || key == extension {
			return CLMMSwap{}, fmt.Errorf("%w: CLMM swap repeats account %s", ErrMalformedInstruction, key)
		}
		seen[key] = true
	}
	s.TickArrays = append([]solana.PublicKey(nil), rest...)
	return s, nil
}
