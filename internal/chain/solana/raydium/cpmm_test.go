package raydium_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

// The CPMM vault authority as Raydium's API names it.
var cpmmAuthority = key("GpMZbSM2GgvTKHJirzeGfMFoaZ8UR2X7F4v8vHTvxFbL")

func TestCPMMAuthority(t *testing.T) {
	t.Parallel()
	if raydium.CPMMAuthority != cpmmAuthority {
		t.Fatalf("CPMMAuthority = %s, want %s", raydium.CPMMAuthority, cpmmAuthority)
	}
}

func TestDecodeCPMMPoolMatchesIndependentSources(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4", // SOL/USDC
		"By3gjE18rtuqSXdjefh9oTDjyaswYrjL3mqG53tkWjYR", // USDC/USDT
		"Q2sPHPdUWFMg7M7wwrQKLrn619cAucfRsmhVJffodSp",  // SOL/USELESS, quoted by the API
	} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			a := loadAccount(t, id)
			p, err := raydium.DecodeCPMMPool(a.owner, a.data)
			if err != nil {
				t.Fatal(err)
			}
			api := poolKeys(t, id)
			checks := []struct {
				name      string
				got, want solana.PublicKey
			}{
				{"vault 0", p.Token0Vault, key(api.Vault.A)},
				{"vault 1", p.Token1Vault, key(api.Vault.B)},
				{"mint 0", p.Token0Mint, key(api.MintA.Address)},
				{"mint 1", p.Token1Mint, key(api.MintB.Address)},
				{"token program 0", p.Token0Program, key(api.MintA.ProgramID)},
				{"token program 1", p.Token1Program, key(api.MintB.ProgramID)},
				{"config", p.AmmConfig, key(api.Config.ID)},
				{"observation", p.Observation, key(api.ObservationID)},
				{"lp mint", p.LPMint, key(api.MintLp.Address)},
				{"authority", raydium.CPMMAuthority, key(api.Authority)},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
				}
			}
			if int(p.Mint0Decimals) != api.MintA.Decimals || int(p.Mint1Decimals) != api.MintB.Decimals {
				t.Errorf("decimals %d/%d", p.Mint0Decimals, p.Mint1Decimals)
			}
			if strconv.FormatUint(p.OpenTime, 10) != api.OpenTime {
				t.Errorf("open time %d, want %s", p.OpenTime, api.OpenTime)
			}
			// The auth bump is the bump of the authority PDA.
			if _, bump, err := solana.FindProgramAddress([][]byte{[]byte("vault_and_lp_mint_auth_seed")}, raydium.CPMMProgram); err != nil || bump != p.AuthBump {
				t.Errorf("auth bump %d, PDA bump %d (%v)", p.AuthBump, bump, err)
			}
			for _, v := range []struct{ vault, mint solana.PublicKey }{{p.Token0Vault, p.Token0Mint}, {p.Token1Vault, p.Token1Mint}} {
				mint, authority, _ := tokenAccount(t, v.vault.String())
				if mint != v.mint || authority != raydium.CPMMAuthority {
					t.Errorf("vault %s: mint %s authority %s", v.vault, mint, authority)
				}
			}
			if err := p.Swappable(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)); err != nil {
				t.Errorf("Swappable: %v", err)
			}

			c := loadAccount(t, api.Config.ID)
			cfg, err := raydium.DecodeCPMMConfig(c.owner, c.data)
			if err != nil {
				t.Fatal(err)
			}
			if int(cfg.Index) != api.Config.Index || cfg.TradeFeeRate != api.Config.TradeFeeRate ||
				cfg.ProtocolFeeRate != api.Config.ProtocolFeeRate || cfg.FundFeeRate != api.Config.FundFeeRate ||
				cfg.CreatorFeeRate != api.Config.CreatorFeeRate || strconv.FormatUint(cfg.CreatePoolFee, 10) != api.Config.CreatePoolFee {
				t.Errorf("config %+v, API %+v", cfg, api.Config)
			}
			// The config is the PDA of ["amm_config", index as u16 BE]
			// (states/config.rs AMM_CONFIG_SEED, admin/create_config.rs).
			pda, bump, err := solana.FindProgramAddress([][]byte{[]byte("amm_config"), binary.BigEndian.AppendUint16(nil, cfg.Index)}, raydium.CPMMProgram)
			if err != nil || pda != c.key || bump != cfg.Bump {
				t.Errorf("config PDA %s bump %d, want %s bump %d", pda, bump, c.key, cfg.Bump)
			}
		})
	}
}

func TestDecodeCPMMRefuses(t *testing.T) {
	t.Parallel()
	pool := loadAccount(t, "47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4").data
	config := loadAccount(t, "BgxH5ifebqHDuiADWKhLjXGP5hWZeZLoCdmeWJLkRqLP").data
	edit := func(data []byte, f func([]byte)) []byte {
		data = slices.Clone(data)
		f(data)
		return data
	}
	poolTests := []struct {
		name  string
		owner solana.PublicKey
		data  []byte
		want  error
	}{
		{"wrong owner", raydium.CLMMProgram, pool, raydium.ErrWrongOwner},
		{"short", raydium.CPMMProgram, pool[:636], raydium.ErrMalformedAccount},
		{"config as pool", raydium.CPMMProgram, config, raydium.ErrMalformedAccount},
		{"other discriminator", raydium.CPMMProgram, edit(pool, func(d []byte) { d[0] ^= 1 }), raydium.ErrMalformedAccount},
		{"enable_creator_fee 2", raydium.CPMMProgram, edit(pool, func(d []byte) { d[390] = 2 }), raydium.ErrMalformedAccount},
	}
	for _, tt := range poolTests {
		t.Run("pool "+tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := raydium.DecodeCPMMPool(tt.owner, tt.data); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	configTests := []struct {
		name  string
		owner solana.PublicKey
		data  []byte
		want  error
	}{
		{"wrong owner", raydium.CLMMProgram, config, raydium.ErrWrongOwner},
		{"long", raydium.CPMMProgram, append(slices.Clone(config), 0), raydium.ErrMalformedAccount},
		{"other discriminator", raydium.CPMMProgram, edit(config, func(d []byte) { d[7] ^= 1 }), raydium.ErrMalformedAccount},
	}
	for _, tt := range configTests {
		t.Run("config "+tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := raydium.DecodeCPMMConfig(tt.owner, tt.data); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCPMMSwappable(t *testing.T) {
	t.Parallel()
	base, err := raydium.DecodeCPMMPool(raydium.CPMMProgram, loadAccount(t, "47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4").data)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(int64(base.OpenTime), 0)
	tests := []struct {
		name string
		edit func(*raydium.CPMMPool)
		at   time.Time
		ok   bool
	}{
		{"open", func(*raydium.CPMMPool) {}, now, true},
		{"before open", func(*raydium.CPMMPool) {}, now.Add(-time.Second), false},
		{"deposit disabled", func(p *raydium.CPMMPool) { p.Status = 1 }, now, true},
		{"withdraw disabled", func(p *raydium.CPMMPool) { p.Status = 2 }, now, true},
		{"swap disabled", func(p *raydium.CPMMPool) { p.Status = 4 }, now, false},
		{"all disabled", func(p *raydium.CPMMPool) { p.Status = 7 }, now, false},
		{"token-2022 side 0", func(p *raydium.CPMMPool) { p.Token0Program = solana.Token2022ProgramID }, now, false},
		{"token-2022 side 1", func(p *raydium.CPMMPool) { p.Token1Program = solana.Token2022ProgramID }, now, false},
		{"same mints", func(p *raydium.CPMMPool) { p.Token1Mint = p.Token0Mint }, now, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := base
			tt.edit(&p)
			err := p.Swappable(tt.at)
			if tt.ok != (err == nil) || (err != nil && !errors.Is(err, raydium.ErrPoolNotSwappable)) {
				t.Fatalf("Swappable = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestCPMMSwapInstructionBytes(t *testing.T) {
	t.Parallel()
	// The Anchor discriminator is sha256("global:swap_base_input")[:8]; the
	// journal records 8fbe5adac41e33de from the published IDL.
	sum := sha256.Sum256([]byte("global:swap_base_input"))
	if !bytes.Equal(sum[:8], []byte{0x8f, 0xbe, 0x5a, 0xda, 0xc4, 0x1e, 0x33, 0xde}) {
		t.Fatalf("anchor rule gives %x", sum[:8])
	}
	s := raydium.CPMMSwap{
		Payer: key("11111111111111111111111111111112"), AmmConfig: key("BgxH5ifebqHDuiADWKhLjXGP5hWZeZLoCdmeWJLkRqLP"),
		Pool: key("47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4"), InputTokenAccount: key("11111111111111111111111111111113"),
		OutputTokenAccount: key("11111111111111111111111111111114"), InputVault: key("9tAoQUNB1wKnBSUyc6ukg2pWbeofZJVHxur2UaRBZNZc"),
		OutputVault: key("BArWLNarRzS7N1GCwBjNgVnC4866c8oJNRqhb5axf6y4"), InputTokenProgram: solana.TokenProgramID,
		OutputTokenProgram: solana.TokenProgramID, InputMint: wsol, OutputMint: usdc,
		Observation: key("AwXcFysn5vntDyiGo9hXDhTMFSg2in7hxj8ED51oZgqo"),
		AmountIn:    1_000_000_000, MinAmountOut: 0xfffffffffffffffe,
	}
	in := s.Instruction()
	want := []byte{
		0x8f, 0xbe, 0x5a, 0xda, 0xc4, 0x1e, 0x33, 0xde, // discriminator
		0x00, 0xca, 0x9a, 0x3b, 0, 0, 0, 0, // 1e9 LE
		0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	}
	if !bytes.Equal(in.Data, want) {
		t.Errorf("data %x, want %x", in.Data, want)
	}
	// swap_base_input.rs struct Swap, in order.
	wantAccounts := []solana.AccountMeta{
		{PublicKey: s.Payer, IsSigner: true},
		{PublicKey: cpmmAuthority},
		{PublicKey: s.AmmConfig},
		{PublicKey: s.Pool, IsWritable: true},
		{PublicKey: s.InputTokenAccount, IsWritable: true},
		{PublicKey: s.OutputTokenAccount, IsWritable: true},
		{PublicKey: s.InputVault, IsWritable: true},
		{PublicKey: s.OutputVault, IsWritable: true},
		{PublicKey: solana.TokenProgramID},
		{PublicKey: solana.TokenProgramID},
		{PublicKey: wsol},
		{PublicKey: usdc},
		{PublicKey: s.Observation, IsWritable: true},
	}
	if in.Program != raydium.CPMMProgram || !slices.Equal(in.Accounts, wantAccounts) {
		t.Errorf("program %s accounts\n got %v\nwant %v", in.Program, in.Accounts, wantAccounts)
	}
	got, err := raydium.DecodeCPMMSwap(in.Program, in.Keys(), in.Data)
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Errorf("round trip %+v, want %+v", got, s)
	}
}

func TestCPMMPoolSwapUsesPoolAccounts(t *testing.T) {
	t.Parallel()
	a := loadAccount(t, "47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4")
	p, err := raydium.DecodeCPMMPool(a.owner, a.data)
	if err != nil {
		t.Fatal(err)
	}
	payer, in, out := key("11111111111111111111111111111112"), key("11111111111111111111111111111113"), key("11111111111111111111111111111114")
	solVault, usdcVault := key("9tAoQUNB1wKnBSUyc6ukg2pWbeofZJVHxur2UaRBZNZc"), key("BArWLNarRzS7N1GCwBjNgVnC4866c8oJNRqhb5axf6y4")
	common := raydium.CPMMSwap{
		Payer: payer, AmmConfig: key("BgxH5ifebqHDuiADWKhLjXGP5hWZeZLoCdmeWJLkRqLP"), Pool: a.key,
		InputTokenAccount: in, OutputTokenAccount: out, InputTokenProgram: solana.TokenProgramID, OutputTokenProgram: solana.TokenProgramID,
		Observation: key("AwXcFysn5vntDyiGo9hXDhTMFSg2in7hxj8ED51oZgqo"), AmountIn: 7, MinAmountOut: 6,
	}
	sell := common
	sell.InputVault, sell.OutputVault, sell.InputMint, sell.OutputMint = solVault, usdcVault, wsol, usdc
	buy := common
	buy.InputVault, buy.OutputVault, buy.InputMint, buy.OutputMint = usdcVault, solVault, usdc, wsol
	for mint, want := range map[solana.PublicKey]raydium.CPMMSwap{wsol: sell, usdc: buy} {
		got, err := p.Swap(a.key, mint, payer, in, out, 7, 6)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", mint, got, err, want)
		}
	}
	if _, err := p.Swap(a.key, usdt, payer, in, out, 7, 6); !errors.Is(err, raydium.ErrPoolNotSwappable) {
		t.Errorf("other mint: %v", err)
	}
}

func TestDecodeCPMMSwapRefuses(t *testing.T) {
	t.Parallel()
	good := raydium.CPMMSwap{InputTokenProgram: solana.TokenProgramID, OutputTokenProgram: solana.Token2022ProgramID, AmountIn: 1}.Instruction()
	keys := good.Keys()
	if _, err := raydium.DecodeCPMMSwap(good.Program, keys, good.Data); err != nil {
		t.Fatalf("a Token-2022 output program is still a CPMM swap: %v", err)
	}
	replace := func(i int, k solana.PublicKey) []solana.PublicKey {
		out := slices.Clone(keys)
		out[i] = k
		return out
	}
	swapBaseOutput := sha256.Sum256([]byte("global:swap_base_output"))
	tests := []struct {
		name     string
		program  solana.PublicKey
		accounts []solana.PublicKey
		data     []byte
	}{
		{"other program", raydium.AMMV4Program, keys, good.Data},
		{"swap_base_output", raydium.CPMMProgram, keys, append(swapBaseOutput[:8:8], good.Data[8:]...)},
		{"trailing byte", raydium.CPMMProgram, keys, append(slices.Clone(good.Data), 0)},
		{"short data", raydium.CPMMProgram, keys, good.Data[:23]},
		{"12 accounts", raydium.CPMMProgram, keys[:12], good.Data},
		{"14 accounts", raydium.CPMMProgram, append(slices.Clone(keys), keys[0]), good.Data},
		{"other authority", raydium.CPMMProgram, replace(1, keys[0]), good.Data},
		{"input token program", raydium.CPMMProgram, replace(8, raydium.CPMMProgram), good.Data},
		{"output token program", raydium.CPMMProgram, replace(9, solana.SystemProgramID), good.Data},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := raydium.DecodeCPMMSwap(tt.program, tt.accounts, tt.data); !errors.Is(err, raydium.ErrMalformedInstruction) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestCPMMAmountOut(t *testing.T) {
	t.Parallel()
	base, err := raydium.DecodeCPMMPool(raydium.CPMMProgram, loadAccount(t, "47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4").data)
	if err != nil {
		t.Fatal(err)
	}
	base.ProtocolFees0, base.FundFees0, base.CreatorFees0 = 10, 20, 30
	base.ProtocolFees1, base.FundFees1, base.CreatorFees1 = 1, 2, 3
	cfg := raydium.CPMMConfig{TradeFeeRate: 2500, CreatorFeeRate: 500}
	// Reserves: 1e9+60-60 = 1e9 of token 0, 4e9+6-6 = 4e9 of token 1.
	tests := []struct {
		name    string
		feeOn   uint8
		enabled bool
		input   solana.PublicKey
		want    uint64
	}{
		// By hand (curve/calculator.rs swap_base_input, curve/fees.rs):
		// no creator fee: fee = ceil(1e6 × 2500 / 1e6) = 2500, in 997_500,
		// out = 4e9 × 997_500 / (1e9 + 997_500) = 3_986_023.8… → 3_986_023.
		{"creator fee disabled", 0, false, wsol, 3_986_023},
		// both tokens, on input: fee = ceil(1e6 × 3000 / 1e6) = 3000,
		// in 997_000, out = 4e9 × 997_000 / 1_000_997_000 = 3_984_027.…
		{"creator fee on input", 0, true, wsol, 3_984_027},
		// only token 1, selling token 0: creator fee from the output:
		// 3_986_023 − ceil(3_986_023 × 500 / 1e6) = 3_986_023 − 1994.
		{"creator fee on output", 2, true, wsol, 3_984_029},
		// only token 0, selling token 0: on input again.
		{"only token 0, zero for one", 1, true, wsol, 3_984_027},
		// selling token 1 with fee on token 1 only: on input;
		// out = 1e9 × 997_000 / (4e9 + 997_000) = 249_187.…
		{"one for zero, on input", 2, true, usdc, 249_187},
		// selling token 1 with fee on token 0 only: from the output;
		// 1e9 × 997_500 / 4_000_997_500 = 249_312.… → 249_312 − ceil(124.656) = 249_187.
		{"one for zero, on output", 1, true, usdc, 249_187},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := base
			p.CreatorFeeOn, p.EnableCreatorFee = tt.feeOn, tt.enabled
			got, err := p.AmountOut(cfg, tt.input, 1_000_000, 1_000_000_060, 4_000_000_006)
			if err != nil || got != tt.want {
				t.Fatalf("AmountOut = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
	bad := map[string]func() (uint64, error){
		"other mint":         func() (uint64, error) { return base.AmountOut(cfg, usdt, 1, 1e9, 1e9) },
		"vault 0 below fees": func() (uint64, error) { return base.AmountOut(cfg, wsol, 1, 59, 1e9) },
		"vault 1 below fees": func() (uint64, error) { return base.AmountOut(cfg, wsol, 1, 1e9, 5) },
		"fee rate of 100%": func() (uint64, error) {
			return base.AmountOut(raydium.CPMMConfig{TradeFeeRate: 1_000_000}, wsol, 1, 1e9, 1e9)
		},
		"creator rate of 100%": func() (uint64, error) {
			p := base
			p.EnableCreatorFee, p.CreatorFeeOn = true, 2
			return p.AmountOut(raydium.CPMMConfig{CreatorFeeRate: 1_000_000}, wsol, 1, 1e9, 1e9)
		},
		"unknown creator mode": func() (uint64, error) { p := base; p.CreatorFeeOn = 3; return p.AmountOut(cfg, wsol, 1, 1e9, 1e9) },
		"output reserve empty": func() (uint64, error) { return base.AmountOut(cfg, wsol, 1000, 1e9, 6) },
		"both reserves empty":  func() (uint64, error) { return base.AmountOut(cfg, wsol, 0, 60, 6) },
	}
	for name, call := range bad {
		if _, err := call(); !errors.Is(err, raydium.ErrPoolNotSwappable) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
