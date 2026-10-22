package raydium_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

var clmmPools = []string{
	"3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv", // SOL/USDC 0.04%
	"BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU", // USDC/USDT 0.01%
	"3nMFwZXwY1s1M5s8vYAHqd4wGs4iSxXE4LRoUMMYqEgF", // SOL/USDT
	"2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv", // SOL/USDC 0.05%
}

// apiPrices is pool_info_api_v3.json: the price api-v3 reported per pool.
func apiPrice(t *testing.T, id string) (float64, bool) {
	t.Helper()
	var f struct {
		Pools map[string]struct {
			Price float64 `json:"price"`
		} `json:"pools"`
	}
	readJSON(t, "pool_info_api_v3.json", &f)
	p, ok := f.Pools[id]
	return p.Price, ok
}

func TestDecodeCLMMPoolMatchesIndependentSources(t *testing.T) {
	t.Parallel()
	for _, id := range clmmPools {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			a := loadAccount(t, id)
			p, err := raydium.DecodeCLMMPool(a.owner, a.data)
			if err != nil {
				t.Fatal(err)
			}
			api := poolKeys(t, id)
			extension := raydium.CLMMBitmapExtensionAddress(a.key)
			checks := []struct {
				name      string
				got, want solana.PublicKey
			}{
				{"vault 0", p.Vault0, key(api.Vault.A)},
				{"vault 1", p.Vault1, key(api.Vault.B)},
				{"mint 0", p.Mint0, key(api.MintA.Address)},
				{"mint 1", p.Mint1, key(api.MintB.Address)},
				{"config", p.AmmConfig, key(api.Config.ID)},
				{"observation", p.Observation, key(api.ObservationID)},
				{"bitmap extension", extension, key(api.ExBitmapAccount)},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
				}
			}
			if int(p.Decimals0) != api.MintA.Decimals || int(p.Decimals1) != api.MintB.Decimals || int(p.TickSpacing) != api.Config.TickSpacing {
				t.Errorf("decimals %d/%d spacing %d", p.Decimals0, p.Decimals1, p.TickSpacing)
			}
			if strconv.FormatUint(p.OpenTime, 10) != api.OpenTime {
				t.Errorf("open time %d, want %s", p.OpenTime, api.OpenTime)
			}
			// CLMM vaults are token accounts held by the pool itself.
			for _, v := range []struct{ vault, mint solana.PublicKey }{{p.Vault0, p.Mint0}, {p.Vault1, p.Mint1}} {
				mint, authority, _ := tokenAccount(t, v.vault.String())
				if mint != v.mint || authority != a.key {
					t.Errorf("vault %s: mint %s authority %s", v.vault, mint, authority)
				}
			}
			// The pool is the PDA of ["pool", config, mint0, mint1]
			// (states/pool.rs seeds, legacy pools: no seed index).
			pda, bump, err := solana.FindProgramAddress([][]byte{[]byte("pool"), p.AmmConfig[:], p.Mint0[:], p.Mint1[:]}, raydium.CLMMProgram)
			if err != nil || pda != a.key || bump != p.Bump {
				t.Errorf("pool PDA %s bump %d, want %s bump %d", pda, bump, a.key, p.Bump)
			}
			// sqrt price and tick agree with each other (tick =
			// floor(log_1.0001(price))) and with api-v3's price.
			sqrt, _ := new(big.Float).Quo(new(big.Float).SetInt(p.SqrtPriceX64), new(big.Float).SetMantExp(big.NewFloat(1), 64)).Float64()
			raw := sqrt * sqrt
			if tick := math.Floor(math.Log(raw) / math.Log(1.0001)); math.Abs(tick-float64(p.TickCurrent)) > 1 {
				t.Errorf("tick %d, sqrt price gives %v", p.TickCurrent, tick)
			}
			if price, ok := apiPrice(t, id); ok {
				human := raw * math.Pow10(int(p.Decimals0)-int(p.Decimals1))
				if math.Abs(human/price-1) > 0.01 {
					t.Errorf("price %v, api-v3 %v", human, price)
				}
			}
			if p.Liquidity.Sign() <= 0 {
				t.Errorf("liquidity %s", p.Liquidity)
			}
			if err := p.Swappable(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)); err != nil {
				t.Errorf("Swappable: %v", err)
			}

			c := loadAccount(t, api.Config.ID)
			cfg, err := raydium.DecodeCLMMConfig(c.owner, c.data)
			if err != nil {
				t.Fatal(err)
			}
			if int(cfg.Index) != api.Config.Index || uint64(cfg.TradeFeeRate) != api.Config.TradeFeeRate ||
				uint64(cfg.ProtocolFeeRate) != api.Config.ProtocolFeeRate || uint64(cfg.FundFeeRate) != api.Config.FundFeeRate ||
				int(cfg.TickSpacing) != api.Config.TickSpacing {
				t.Errorf("config %+v, API %+v", cfg, api.Config)
			}
			// admin/create_amm_config.rs seeds: ["amm_config", index BE].
			cpda, cbump, err := solana.FindProgramAddress([][]byte{[]byte("amm_config"), binary.BigEndian.AppendUint16(nil, cfg.Index)}, raydium.CLMMProgram)
			if err != nil || cpda != c.key || cbump != cfg.Bump {
				t.Errorf("config PDA %s bump %d, want %s bump %d", cpda, cbump, c.key, cfg.Bump)
			}
		})
	}
}

func TestDecodeCLMMRefuses(t *testing.T) {
	t.Parallel()
	pool := loadAccount(t, "BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU").data
	config := loadAccount(t, "9iFER3bpjf1PTTCQCfTRu17EJgvsxo9pVyA9QWwEuX4x").data
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
		{"wrong owner", raydium.CPMMProgram, pool, raydium.ErrWrongOwner},
		{"short", raydium.CLMMProgram, pool[:1543], raydium.ErrMalformedAccount},
		{"other discriminator", raydium.CLMMProgram, edit(pool, func(d []byte) { d[3] ^= 1 }), raydium.ErrMalformedAccount},
		{"tick spacing 0", raydium.CLMMProgram, edit(pool, func(d []byte) { binary.LittleEndian.PutUint16(d[235:], 0) }), raydium.ErrMalformedAccount},
		{"tick above MAX_TICK", raydium.CLMMProgram, edit(pool, func(d []byte) { binary.LittleEndian.PutUint32(d[269:], 443637) }), raydium.ErrMalformedAccount},
		{"tick below MIN_TICK", raydium.CLMMProgram, edit(pool, func(d []byte) { binary.LittleEndian.PutUint32(d[269:], uint32(0xFFFFFFFF-443636)) }), raydium.ErrMalformedAccount},
	}
	for _, tt := range poolTests {
		t.Run("pool "+tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := raydium.DecodeCLMMPool(tt.owner, tt.data); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	edge := edit(pool, func(d []byte) { binary.LittleEndian.PutUint32(d[269:], uint32(0xFFFFFFFF-443635)) })
	if p, err := raydium.DecodeCLMMPool(raydium.CLMMProgram, edge); err != nil || p.TickCurrent != -443636 {
		t.Errorf("MIN_TICK itself: %d, %v", p.TickCurrent, err)
	}
	configTests := []struct {
		name  string
		owner solana.PublicKey
		data  []byte
		want  error
	}{
		{"wrong owner", raydium.CPMMProgram, config, raydium.ErrWrongOwner},
		{"short", raydium.CLMMProgram, config[:116], raydium.ErrMalformedAccount},
		{"other discriminator", raydium.CLMMProgram, edit(config, func(d []byte) { d[0] ^= 1 }), raydium.ErrMalformedAccount},
	}
	for _, tt := range configTests {
		t.Run("config "+tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := raydium.DecodeCLMMConfig(tt.owner, tt.data); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestCLMMSwappable(t *testing.T) {
	t.Parallel()
	base, err := raydium.DecodeCLMMPool(raydium.CLMMProgram, loadAccount(t, "3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv").data)
	if err != nil {
		t.Fatal(err)
	}
	opens := time.Unix(int64(base.OpenTime), 0)
	tests := []struct {
		name string
		edit func(*raydium.CLMMPool)
		at   time.Time
		ok   bool
	}{
		{"after open", func(*raydium.CLMMPool) {}, opens.Add(time.Second), true},
		{"at open (program wants now > open_time)", func(*raydium.CLMMPool) {}, opens, false},
		{"positions disabled", func(p *raydium.CLMMPool) { p.Status = 0b0_1111 }, opens.Add(time.Second), true},
		{"limit orders disabled", func(p *raydium.CLMMPool) { p.Status = 0b10_0000 }, opens.Add(time.Second), true},
		{"swap disabled", func(p *raydium.CLMMPool) { p.Status = 0b1_0000 }, opens.Add(time.Second), false},
		{"same mints", func(p *raydium.CLMMPool) { p.Mint1 = p.Mint0 }, opens.Add(time.Second), false},
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

func TestCLMMSwapInstructionBytes(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("global:swap_v2"))
	if !bytes.Equal(sum[:8], []byte{0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62}) {
		t.Fatalf("anchor rule gives %x", sum[:8])
	}
	pool := key("BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU")
	extension := key("HSJS4BrYCZq9DmCNFNfctqq9o2sfTY5YQiy9n4W8djcB") // api-v3 exBitmapAccount
	tick1, tick2 := key("2CntbsRKrr4an5zekGb8WZyPyPzbXv9R2CErrRQSQVo2"), key("11111111111111111111111111111115")
	s := raydium.CLMMSwap{
		Payer: key("11111111111111111111111111111112"), AmmConfig: key("9iFER3bpjf1PTTCQCfTRu17EJgvsxo9pVyA9QWwEuX4x"),
		Pool: pool, InputTokenAccount: key("11111111111111111111111111111113"), OutputTokenAccount: key("11111111111111111111111111111114"),
		InputVault: key("4iJQGzZpys4N13rGXbsB3NqehPkrgrcmDUEcLw7D6GKL"), OutputVault: key("7NQjToK5NenQZvhPRc1nC1URuKJ4nuLxGPdTbpTiz9EN"),
		Observation: key("3YZE41e4GKm9Uqvx6H4QA1FdyxT15nEjUP8LbvdiDF2t"), InputVaultMint: usdc, OutputVaultMint: usdt,
		WithBitmapExtension: true, TickArrays: []solana.PublicKey{tick1, tick2},
		Amount: 1_000_000_000, OtherAmountThreshold: 997_890_718,
	}
	in, err := s.Instruction()
	if err != nil {
		t.Fatal(err)
	}
	// lib.rs swap_v2(amount u64, other_amount_threshold u64,
	// sqrt_price_limit_x64 u128, is_base_input bool), Borsh little-endian.
	want := []byte{
		0x2b, 0x04, 0xed, 0x0b, 0x1a, 0xc9, 0x1e, 0x62,
		0x00, 0xca, 0x9a, 0x3b, 0, 0, 0, 0, // 1_000_000_000
		0x9e, 0x9a, 0x7a, 0x3b, 0, 0, 0, 0, // 997_890_718
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		1,
	}
	if !bytes.Equal(in.Data, want) {
		t.Errorf("data %x\nwant %x", in.Data, want)
	}
	// swap_v2.rs SwapSingleV2 in order, then the remaining accounts.
	wantAccounts := []solana.AccountMeta{
		{PublicKey: s.Payer, IsSigner: true},
		{PublicKey: s.AmmConfig},
		{PublicKey: pool, IsWritable: true},
		{PublicKey: s.InputTokenAccount, IsWritable: true},
		{PublicKey: s.OutputTokenAccount, IsWritable: true},
		{PublicKey: s.InputVault, IsWritable: true},
		{PublicKey: s.OutputVault, IsWritable: true},
		{PublicKey: s.Observation, IsWritable: true},
		{PublicKey: solana.TokenProgramID},
		{PublicKey: key("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")},
		{PublicKey: key("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")},
		{PublicKey: usdc},
		{PublicKey: usdt},
		{PublicKey: extension, IsWritable: true},
		{PublicKey: tick1, IsWritable: true},
		{PublicKey: tick2, IsWritable: true},
	}
	if in.Program != raydium.CLMMProgram || !slices.Equal(in.Accounts, wantAccounts) {
		t.Errorf("program %s accounts\n got %v\nwant %v", in.Program, in.Accounts, wantAccounts)
	}
	got, err := raydium.DecodeCLMMSwap(in.Program, in.Keys(), in.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !equalCLMMSwap(got, s) {
		t.Errorf("round trip %+v, want %+v", got, s)
	}

	// Without the extension the tick arrays follow the mints directly.
	s.WithBitmapExtension = false
	in, err = s.Instruction()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(in.Accounts, slices.Concat(wantAccounts[:13], wantAccounts[14:])) {
		t.Errorf("accounts without extension: %v", in.Accounts)
	}
	if got, err := raydium.DecodeCLMMSwap(in.Program, in.Keys(), in.Data); err != nil || !equalCLMMSwap(got, s) {
		t.Errorf("round trip without extension: %+v, %v", got, err)
	}
}

func equalCLMMSwap(a, b raydium.CLMMSwap) bool { return reflect.DeepEqual(a, b) }

func TestCLMMSwapInstructionTickArrayCount(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, raydium.MaxTickArrays + 1} {
		s := raydium.CLMMSwap{TickArrays: make([]solana.PublicKey, n)}
		if _, err := s.Instruction(); !errors.Is(err, raydium.ErrMalformedInstruction) {
			t.Errorf("%d tick arrays: %v", n, err)
		}
	}
	arrays := make([]solana.PublicKey, raydium.MaxTickArrays)
	for i := range arrays {
		arrays[i] = solana.PublicKeyFromBytes(bytes.Repeat([]byte{byte(i + 1)}, 32))
	}
	in, err := raydium.CLMMSwap{TickArrays: arrays}.Instruction()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := raydium.DecodeCLMMSwap(in.Program, in.Keys(), in.Data); err != nil || len(got.TickArrays) != raydium.MaxTickArrays {
		t.Errorf("MaxTickArrays round trip: %d, %v", len(got.TickArrays), err)
	}
}

func TestCLMMPoolSwapUsesPoolAccounts(t *testing.T) {
	t.Parallel()
	a := loadAccount(t, "BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU")
	p, err := raydium.DecodeCLMMPool(a.owner, a.data)
	if err != nil {
		t.Fatal(err)
	}
	payer, in, out := key("11111111111111111111111111111112"), key("11111111111111111111111111111113"), key("11111111111111111111111111111114")
	arrays := []solana.PublicKey{key("2CntbsRKrr4an5zekGb8WZyPyPzbXv9R2CErrRQSQVo2")}
	usdcVault, usdtVault := key("4iJQGzZpys4N13rGXbsB3NqehPkrgrcmDUEcLw7D6GKL"), key("7NQjToK5NenQZvhPRc1nC1URuKJ4nuLxGPdTbpTiz9EN")
	common := raydium.CLMMSwap{
		Payer: payer, AmmConfig: key("9iFER3bpjf1PTTCQCfTRu17EJgvsxo9pVyA9QWwEuX4x"), Pool: a.key,
		InputTokenAccount: in, OutputTokenAccount: out, Observation: key("3YZE41e4GKm9Uqvx6H4QA1FdyxT15nEjUP8LbvdiDF2t"),
		WithBitmapExtension: true, TickArrays: arrays, Amount: 9, OtherAmountThreshold: 8,
	}
	sell := common
	sell.InputVault, sell.OutputVault, sell.InputVaultMint, sell.OutputVaultMint = usdcVault, usdtVault, usdc, usdt
	buy := common
	buy.InputVault, buy.OutputVault, buy.InputVaultMint, buy.OutputVaultMint = usdtVault, usdcVault, usdt, usdc
	for mint, want := range map[solana.PublicKey]raydium.CLMMSwap{usdc: sell, usdt: buy} {
		got, err := p.Swap(a.key, mint, payer, in, out, true, arrays, 9, 8)
		if err != nil || !equalCLMMSwap(got, want) {
			t.Errorf("%s: %+v, %v; want %+v", mint, got, err, want)
		}
	}
	if _, err := p.Swap(a.key, wsol, payer, in, out, true, arrays, 9, 8); !errors.Is(err, raydium.ErrPoolNotSwappable) {
		t.Errorf("other mint: %v", err)
	}
	// The swap keeps its own copy of the tick arrays.
	got, _ := p.Swap(a.key, usdc, payer, in, out, true, arrays, 9, 8)
	arrays[0] = payer
	if got.TickArrays[0] == payer {
		t.Error("Swap aliases the caller's tick array slice")
	}
}

func TestDecodeCLMMSwapRefuses(t *testing.T) {
	t.Parallel()
	pool := key("BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU")
	extension := raydium.CLMMBitmapExtensionAddress(pool)
	t1, t2 := key("11111111111111111111111111111115"), key("11111111111111111111111111111116")
	good, err := raydium.CLMMSwap{Pool: pool, WithBitmapExtension: true, TickArrays: []solana.PublicKey{t1, t2}, Amount: 1}.Instruction()
	if err != nil {
		t.Fatal(err)
	}
	keys := good.Keys()
	fixed := keys[:13]
	with := func(rest ...solana.PublicKey) []solana.PublicKey { return slices.Concat(fixed, rest) }
	replace := func(i int, k solana.PublicKey) []solana.PublicKey {
		out := slices.Clone(keys)
		out[i] = k
		return out
	}
	data := func(f func([]byte)) []byte {
		d := slices.Clone(good.Data)
		f(d)
		return d
	}
	swapV1 := sha256.Sum256([]byte("global:swap"))
	tests := []struct {
		name     string
		program  solana.PublicKey
		accounts []solana.PublicKey
		data     []byte
	}{
		{"other program", raydium.CPMMProgram, keys, good.Data},
		{"swap (v1) discriminator", raydium.CLMMProgram, keys, append(swapV1[:8:8], good.Data[8:]...)},
		{"trailing byte", raydium.CLMMProgram, keys, append(slices.Clone(good.Data), 0)},
		{"short data", raydium.CLMMProgram, keys, good.Data[:40]},
		{"price limit low byte", raydium.CLMMProgram, keys, data(func(d []byte) { d[24] = 1 })},
		{"price limit high byte", raydium.CLMMProgram, keys, data(func(d []byte) { d[39] = 1 })},
		{"exact output", raydium.CLMMProgram, keys, data(func(d []byte) { d[40] = 0 })},
		{"bool 2", raydium.CLMMProgram, keys, data(func(d []byte) { d[40] = 2 })},
		{"no tick arrays", raydium.CLMMProgram, fixed, good.Data},
		{"extension only", raydium.CLMMProgram, with(extension), good.Data},
		{"too many tick arrays", raydium.CLMMProgram, with(slices.Repeat([]solana.PublicKey{t1}, raydium.MaxTickArrays+1)...), good.Data},
		{"repeated tick array", raydium.CLMMProgram, with(extension, t1, t1), good.Data},
		{"extension after a tick array", raydium.CLMMProgram, with(t1, extension), good.Data},
		{"extension twice", raydium.CLMMProgram, with(extension, extension, t1), good.Data},
		{"token program", raydium.CLMMProgram, replace(8, solana.Token2022ProgramID), good.Data},
		{"token-2022 program", raydium.CLMMProgram, replace(9, solana.TokenProgramID), good.Data},
		{"memo v1", raydium.CLMMProgram, replace(10, key("Memo1UhkJRfHyvLMcVucJwxXeuD728EqVDDwQDxFMNo")), good.Data},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := raydium.DecodeCLMMSwap(tt.program, tt.accounts, tt.data); !errors.Is(err, raydium.ErrMalformedInstruction) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// Another pool's extension is a tick array as far as the decoder can
	// tell; the caller's comparison with the expected arrays refuses it.
	other := raydium.CLMMBitmapExtensionAddress(key("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"))
	got, err := raydium.DecodeCLMMSwap(raydium.CLMMProgram, with(other, t1), good.Data)
	if err != nil || got.WithBitmapExtension || !slices.Equal(got.TickArrays, []solana.PublicKey{other, t1}) {
		t.Errorf("foreign extension: %+v, %v", got, err)
	}
}
