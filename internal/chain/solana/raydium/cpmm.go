package raydium

import (
	"encoding/binary"
	"fmt"
	"math/big"
	"time"

	"github.com/gagliardetto/solana-go"
)

// CPMM (raydium-cp-swap, programs/cp-swap/src/states/pool.rs PoolState and
// states/config.rs AmmConfig): Anchor zero-copy accounts, an 8-byte
// discriminator followed by the packed fields.
const (
	cpmmPoolSize   = 637
	cpmmConfigSize = 236

	cpmmAmmConfigOffset        = 8
	cpmmPoolCreatorOffset      = 40
	cpmmToken0VaultOffset      = 72
	cpmmToken1VaultOffset      = 104
	cpmmLPMintOffset           = 136
	cpmmToken0MintOffset       = 168
	cpmmToken1MintOffset       = 200
	cpmmToken0ProgramOffset    = 232
	cpmmToken1ProgramOffset    = 264
	cpmmObservationOffset      = 296
	cpmmAuthBumpOffset         = 328
	cpmmStatusOffset           = 329
	cpmmLPDecimalsOffset       = 330
	cpmmMint0DecimalsOffset    = 331
	cpmmMint1DecimalsOffset    = 332
	cpmmProtocolFees0Offset    = 341
	cpmmProtocolFees1Offset    = 349
	cpmmFundFees0Offset        = 357
	cpmmFundFees1Offset        = 365
	cpmmOpenTimeOffset         = 373
	cpmmCreatorFeeOnOffset     = 389
	cpmmEnableCreatorFeeOffset = 390
	cpmmCreatorFees0Offset     = 397
	cpmmCreatorFees1Offset     = 405

	cpmmConfigBumpOffset           = 8
	cpmmConfigDisableCreateOffset  = 9
	cpmmConfigIndexOffset          = 10
	cpmmConfigTradeFeeOffset       = 12
	cpmmConfigProtocolFeeOffset    = 20
	cpmmConfigFundFeeOffset        = 28
	cpmmConfigCreatePoolFeeOffset  = 36
	cpmmConfigProtocolOwnerOffset  = 44
	cpmmConfigFundOwnerOffset      = 76
	cpmmConfigCreatorFeeOffset     = 108
	cpmmConfigCreatorShareOffset   = 116
	cpmmSwapBaseInputDataLength    = 24
	cpmmSwapBaseInputAccounts      = 13
	cpmmSwapStatusBit              = 1 << 2
	cpmmFeeRateDenominator         = 1_000_000
	cpmmCreatorFeeOnBothTokens     = 0
	cpmmCreatorFeeOnToken0Only     = 1
	cpmmCreatorFeeOnToken1Only     = 2
	cpmmAuthoritySeed              = "vault_and_lp_mint_auth_seed"
	anchorDiscriminatorLength      = 8
	cpmmSwapBaseInputDiscriminator = "8fbe5adac41e33de"
)

var (
	// Anchor account discriminators: sha256("account:<Name>")[:8].
	poolStateDiscriminator = mustHex("f7ede3f5d7c3de46")
	ammConfigDiscriminator = mustHex("daf42168cbcb2b6f")
	// sha256("global:swap_base_input")[:8].
	cpmmSwapBaseInputData = mustHex(cpmmSwapBaseInputDiscriminator)

	// CPMMAuthority is the vault and LP mint authority: the PDA of
	// AUTH_SEED (lib.rs) under the CPMM program.
	CPMMAuthority = pda([][]byte{[]byte(cpmmAuthoritySeed)}, CPMMProgram)
)

// CPMMPool is what a swap and its check need from a CPMM pool account.
type CPMMPool struct {
	AmmConfig     solana.PublicKey
	PoolCreator   solana.PublicKey
	Token0Vault   solana.PublicKey
	Token1Vault   solana.PublicKey
	LPMint        solana.PublicKey
	Token0Mint    solana.PublicKey
	Token1Mint    solana.PublicKey
	Token0Program solana.PublicKey
	Token1Program solana.PublicKey
	Observation   solana.PublicKey
	AuthBump      uint8
	// Status bits: 1 deposit, 2 withdraw, 4 swap disabled.
	Status        uint8
	LPDecimals    uint8
	Mint0Decimals uint8
	Mint1Decimals uint8
	// The fees owed to the protocol, the fund and the creator sit in the
	// vaults but are not part of the reserves.
	ProtocolFees0    uint64
	ProtocolFees1    uint64
	FundFees0        uint64
	FundFees1        uint64
	CreatorFees0     uint64
	CreatorFees1     uint64
	OpenTime         uint64
	CreatorFeeOn     uint8
	EnableCreatorFee bool
}

// DecodeCPMMPool decodes a CPMM pool account owned by owner.
func DecodeCPMMPool(owner solana.PublicKey, data []byte) (CPMMPool, error) {
	if err := checkAccount("CPMM pool", CPMMProgram, owner, data, cpmmPoolSize, poolStateDiscriminator); err != nil {
		return CPMMPool{}, err
	}
	l := layout(data)
	enable := l.u8(cpmmEnableCreatorFeeOffset)
	if enable > 1 {
		return CPMMPool{}, fmt.Errorf("%w: CPMM enable_creator_fee %d", ErrMalformedAccount, enable)
	}
	return CPMMPool{
		AmmConfig:        l.key(cpmmAmmConfigOffset),
		PoolCreator:      l.key(cpmmPoolCreatorOffset),
		Token0Vault:      l.key(cpmmToken0VaultOffset),
		Token1Vault:      l.key(cpmmToken1VaultOffset),
		LPMint:           l.key(cpmmLPMintOffset),
		Token0Mint:       l.key(cpmmToken0MintOffset),
		Token1Mint:       l.key(cpmmToken1MintOffset),
		Token0Program:    l.key(cpmmToken0ProgramOffset),
		Token1Program:    l.key(cpmmToken1ProgramOffset),
		Observation:      l.key(cpmmObservationOffset),
		AuthBump:         l.u8(cpmmAuthBumpOffset),
		Status:           l.u8(cpmmStatusOffset),
		LPDecimals:       l.u8(cpmmLPDecimalsOffset),
		Mint0Decimals:    l.u8(cpmmMint0DecimalsOffset),
		Mint1Decimals:    l.u8(cpmmMint1DecimalsOffset),
		ProtocolFees0:    l.u64(cpmmProtocolFees0Offset),
		ProtocolFees1:    l.u64(cpmmProtocolFees1Offset),
		FundFees0:        l.u64(cpmmFundFees0Offset),
		FundFees1:        l.u64(cpmmFundFees1Offset),
		CreatorFees0:     l.u64(cpmmCreatorFees0Offset),
		CreatorFees1:     l.u64(cpmmCreatorFees1Offset),
		OpenTime:         l.u64(cpmmOpenTimeOffset),
		CreatorFeeOn:     l.u8(cpmmCreatorFeeOnOffset),
		EnableCreatorFee: enable == 1,
	}, nil
}

// Swappable reports whether the program would accept a swap at now
// (swap_base_input.rs: the swap status bit clear and now >= open_time) and
// whether this wallet trades both tokens: classic SPL Token only, so a pool
// with a Token-2022 side is refused.
func (p CPMMPool) Swappable(now time.Time) error {
	if p.Status&cpmmSwapStatusBit != 0 {
		return fmt.Errorf("%w: CPMM status %#x disables swaps", ErrPoolNotSwappable, p.Status)
	}
	if uint64(now.Unix()) < p.OpenTime {
		return fmt.Errorf("%w: CPMM pool opens at %d", ErrPoolNotSwappable, p.OpenTime)
	}
	if p.Token0Program != solana.TokenProgramID || p.Token1Program != solana.TokenProgramID {
		return fmt.Errorf("%w: CPMM pool holds a token outside the classic token program", ErrPoolNotSwappable)
	}
	if p.Token0Mint == p.Token1Mint {
		return fmt.Errorf("%w: CPMM pool trades a mint against itself", ErrPoolNotSwappable)
	}
	return nil
}

// CPMMConfig is a CPMM AmmConfig: the fee rates in millionths.
type CPMMConfig struct {
	Bump                uint8
	DisableCreatePool   bool
	Index               uint16
	TradeFeeRate        uint64
	ProtocolFeeRate     uint64
	FundFeeRate         uint64
	CreatePoolFee       uint64
	ProtocolOwner       solana.PublicKey
	FundOwner           solana.PublicKey
	CreatorFeeRate      uint64
	CreatorFeeShareRate uint64
}

// DecodeCPMMConfig decodes a CPMM AmmConfig account owned by owner.
func DecodeCPMMConfig(owner solana.PublicKey, data []byte) (CPMMConfig, error) {
	if err := checkAccount("CPMM config", CPMMProgram, owner, data, cpmmConfigSize, ammConfigDiscriminator); err != nil {
		return CPMMConfig{}, err
	}
	l := layout(data)
	return CPMMConfig{
		Bump:                l.u8(cpmmConfigBumpOffset),
		DisableCreatePool:   l.u8(cpmmConfigDisableCreateOffset) != 0,
		Index:               l.u16(cpmmConfigIndexOffset),
		TradeFeeRate:        l.u64(cpmmConfigTradeFeeOffset),
		ProtocolFeeRate:     l.u64(cpmmConfigProtocolFeeOffset),
		FundFeeRate:         l.u64(cpmmConfigFundFeeOffset),
		CreatePoolFee:       l.u64(cpmmConfigCreatePoolFeeOffset),
		ProtocolOwner:       l.key(cpmmConfigProtocolOwnerOffset),
		FundOwner:           l.key(cpmmConfigFundOwnerOffset),
		CreatorFeeRate:      l.u64(cpmmConfigCreatorFeeOffset),
		CreatorFeeShareRate: l.u64(cpmmConfigCreatorShareOffset),
	}, nil
}

// CPMMSwap is a swap_base_input instruction (swap_base_input.rs, struct
// Swap). The authority is not a field: it is always CPMMAuthority.
type CPMMSwap struct {
	Payer              solana.PublicKey
	AmmConfig          solana.PublicKey
	Pool               solana.PublicKey
	InputTokenAccount  solana.PublicKey
	OutputTokenAccount solana.PublicKey
	InputVault         solana.PublicKey
	OutputVault        solana.PublicKey
	InputTokenProgram  solana.PublicKey
	OutputTokenProgram solana.PublicKey
	InputMint          solana.PublicKey
	OutputMint         solana.PublicKey
	Observation        solana.PublicKey
	AmountIn           uint64
	MinAmountOut       uint64
}

// Swap lays out a swap of inputMint through the pool with the given id, with
// the pool's own config, vaults, token programs and observation account.
func (p CPMMPool) Swap(pool, inputMint, payer, inputTokenAccount, outputTokenAccount solana.PublicKey, amountIn, minAmountOut uint64) (CPMMSwap, error) {
	s := CPMMSwap{
		Payer:              payer,
		AmmConfig:          p.AmmConfig,
		Pool:               pool,
		InputTokenAccount:  inputTokenAccount,
		OutputTokenAccount: outputTokenAccount,
		Observation:        p.Observation,
		AmountIn:           amountIn,
		MinAmountOut:       minAmountOut,
	}
	switch inputMint {
	case p.Token0Mint:
		s.InputVault, s.OutputVault = p.Token0Vault, p.Token1Vault
		s.InputTokenProgram, s.OutputTokenProgram = p.Token0Program, p.Token1Program
		s.InputMint, s.OutputMint = p.Token0Mint, p.Token1Mint
	case p.Token1Mint:
		s.InputVault, s.OutputVault = p.Token1Vault, p.Token0Vault
		s.InputTokenProgram, s.OutputTokenProgram = p.Token1Program, p.Token0Program
		s.InputMint, s.OutputMint = p.Token1Mint, p.Token0Mint
	default:
		return CPMMSwap{}, fmt.Errorf("%w: CPMM pool %s does not hold %s", ErrPoolNotSwappable, pool, inputMint)
	}
	return s, nil
}

// Instruction encodes the swap in the account order of the Swap accounts
// struct; data is the Anchor discriminator, amount_in and
// minimum_amount_out, little-endian.
func (s CPMMSwap) Instruction() Instruction {
	data := make([]byte, 0, cpmmSwapBaseInputDataLength)
	data = append(data, cpmmSwapBaseInputData...)
	data = binary.LittleEndian.AppendUint64(data, s.AmountIn)
	data = binary.LittleEndian.AppendUint64(data, s.MinAmountOut)
	return Instruction{
		Program: CPMMProgram,
		Accounts: []solana.AccountMeta{
			signer(s.Payer),
			readonly(CPMMAuthority),
			readonly(s.AmmConfig),
			writable(s.Pool),
			writable(s.InputTokenAccount),
			writable(s.OutputTokenAccount),
			writable(s.InputVault),
			writable(s.OutputVault),
			readonly(s.InputTokenProgram),
			readonly(s.OutputTokenProgram),
			readonly(s.InputMint),
			readonly(s.OutputMint),
			writable(s.Observation),
		},
		Data: data,
	}
}

// DecodeCPMMSwap is the inverse of CPMMSwap.Instruction: the CPMM program,
// the swap_base_input discriminator, exactly 24 bytes of data and 13
// accounts, the CPMM authority in its place, and token programs that are SPL
// Token or Token-2022.
func DecodeCPMMSwap(program solana.PublicKey, accounts []solana.PublicKey, data []byte) (CPMMSwap, error) {
	if program != CPMMProgram {
		return CPMMSwap{}, fmt.Errorf("%w: program %s is not CPMM", ErrMalformedInstruction, program)
	}
	if len(data) != cpmmSwapBaseInputDataLength || string(data[:anchorDiscriminatorLength]) != string(cpmmSwapBaseInputData) {
		return CPMMSwap{}, fmt.Errorf("%w: CPMM data is not swap_base_input", ErrMalformedInstruction)
	}
	if len(accounts) != cpmmSwapBaseInputAccounts {
		return CPMMSwap{}, fmt.Errorf("%w: CPMM swap lists %d accounts, want %d", ErrMalformedInstruction, len(accounts), cpmmSwapBaseInputAccounts)
	}
	if accounts[1] != CPMMAuthority {
		return CPMMSwap{}, fmt.Errorf("%w: CPMM swap names authority %s", ErrMalformedInstruction, accounts[1])
	}
	for _, tokenProgram := range accounts[8:10] {
		if tokenProgram != solana.TokenProgramID && tokenProgram != solana.Token2022ProgramID {
			return CPMMSwap{}, fmt.Errorf("%w: CPMM swap names token program %s", ErrMalformedInstruction, tokenProgram)
		}
	}
	return CPMMSwap{
		Payer:              accounts[0],
		AmmConfig:          accounts[2],
		Pool:               accounts[3],
		InputTokenAccount:  accounts[4],
		OutputTokenAccount: accounts[5],
		InputVault:         accounts[6],
		OutputVault:        accounts[7],
		InputTokenProgram:  accounts[8],
		OutputTokenProgram: accounts[9],
		InputMint:          accounts[10],
		OutputMint:         accounts[11],
		Observation:        accounts[12],
		AmountIn:           binary.LittleEndian.Uint64(data[8:]),
		MinAmountOut:       binary.LittleEndian.Uint64(data[16:]),
	}, nil
}

// AmountOut is what swap_base_input pays for amountIn of inputMint, given the
// config and the two vault balances, for tokens without a transfer fee
// (classic SPL Token): reserves are the vault balances less the fees owed
// (pool.rs vault_amount_without_fee), the trade fee — plus the creator fee
// when it is taken from the input — is ceil(in × rate / 10⁶), and a creator
// fee taken from the output is ceil(out × rate / 10⁶) (curve/calculator.rs
// swap_base_input, curve/fees.rs).
func (p CPMMPool) AmountOut(config CPMMConfig, inputMint solana.PublicKey, amountIn, vault0Amount, vault1Amount uint64) (uint64, error) {
	fees0 := new(big.Int).SetUint64(p.ProtocolFees0)
	fees0.Add(fees0, new(big.Int).SetUint64(p.FundFees0)).Add(fees0, new(big.Int).SetUint64(p.CreatorFees0))
	fees1 := new(big.Int).SetUint64(p.ProtocolFees1)
	fees1.Add(fees1, new(big.Int).SetUint64(p.FundFees1)).Add(fees1, new(big.Int).SetUint64(p.CreatorFees1))
	reserve0 := new(big.Int).SetUint64(vault0Amount)
	reserve1 := new(big.Int).SetUint64(vault1Amount)
	if reserve0.Cmp(fees0) < 0 || reserve1.Cmp(fees1) < 0 {
		return 0, fmt.Errorf("%w: CPMM vaults hold less than the fees owed", ErrPoolNotSwappable)
	}
	reserve0.Sub(reserve0, fees0)
	reserve1.Sub(reserve1, fees1)

	var zeroForOne bool
	switch inputMint {
	case p.Token0Mint:
		zeroForOne = true
	case p.Token1Mint:
	default:
		return 0, fmt.Errorf("%w: CPMM pool does not hold %s", ErrPoolNotSwappable, inputMint)
	}
	var creatorFeeOnInput bool
	switch p.CreatorFeeOn {
	case cpmmCreatorFeeOnBothTokens:
		creatorFeeOnInput = true
	case cpmmCreatorFeeOnToken0Only:
		creatorFeeOnInput = zeroForOne
	case cpmmCreatorFeeOnToken1Only:
		creatorFeeOnInput = !zeroForOne
	default:
		return 0, fmt.Errorf("%w: CPMM creator_fee_on %d", ErrPoolNotSwappable, p.CreatorFeeOn)
	}
	creatorRate := uint64(0)
	if p.EnableCreatorFee {
		creatorRate = config.CreatorFeeRate
	}
	million := big.NewInt(cpmmFeeRateDenominator)
	feeOf := func(amount *big.Int, rate uint64) *big.Int {
		return ceilDiv(new(big.Int).Mul(amount, new(big.Int).SetUint64(rate)), million)
	}
	in := new(big.Int).SetUint64(amountIn)
	inputFeeRate := config.TradeFeeRate
	if creatorFeeOnInput {
		inputFeeRate += creatorRate
	}
	if inputFeeRate >= cpmmFeeRateDenominator || creatorRate >= cpmmFeeRateDenominator {
		return 0, fmt.Errorf("%w: CPMM fee rate %d", ErrPoolNotSwappable, inputFeeRate)
	}
	in.Sub(in, feeOf(in, inputFeeRate))

	reserveIn, reserveOut := reserve1, reserve0
	if zeroForOne {
		reserveIn, reserveOut = reserve0, reserve1
	}
	out, err := constantProductOut(in, reserveIn, reserveOut)
	if err != nil {
		return 0, err
	}
	if !creatorFeeOnInput {
		swapped := new(big.Int).SetUint64(out)
		out = swapped.Sub(swapped, feeOf(swapped, creatorRate)).Uint64()
	}
	return out, nil
}
