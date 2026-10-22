package raydium_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

// The canonical AMM v4 authority as Raydium's API and explorers name it.
var ammV4Authority = key("5Q544fKrFoe6tsEbD7S8EmxGTJYAKtTVhAW5Q5pge4j1")

func TestDecodeAMMV4PoolMatchesIndependentSources(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2", // SOL/USDC
		"7TbGqz32RsuwXbXY7EyBCiAnMbJq1gm1wKmfjQjuwoyF", // USDT/USDC
	} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			a := loadAccount(t, id)
			p, err := raydium.DecodeAMMV4Pool(a.owner, a.data)
			if err != nil {
				t.Fatal(err)
			}
			api := poolKeys(t, id)
			checks := []struct {
				name      string
				got, want solana.PublicKey
			}{
				{"coin vault", p.CoinVault, key(api.Vault.A)},
				{"pc vault", p.PcVault, key(api.Vault.B)},
				{"coin mint", p.CoinMint, key(api.MintA.Address)},
				{"pc mint", p.PcMint, key(api.MintB.Address)},
				{"authority", p.Authority, key(api.Authority)},
				{"authority (canonical)", p.Authority, ammV4Authority},
				{"lp mint", p.LPMint, key(api.MintLp.Address)},
				{"open orders", p.OpenOrders, key(api.OpenOrders)},
				{"target orders", p.TargetOrders, key(api.TargetOrders)},
				{"market", p.Market, key(api.MarketID)},
				{"market program", p.MarketProgram, key(api.MarketProgramID)},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
				}
			}
			if p.CoinDecimals != uint64(api.MintA.Decimals) || p.PcDecimals != uint64(api.MintB.Decimals) {
				t.Errorf("decimals %d/%d, want %d/%d", p.CoinDecimals, p.PcDecimals, api.MintA.Decimals, api.MintB.Decimals)
			}
			if strconv.FormatUint(p.PoolOpenTime, 10) != api.OpenTime {
				t.Errorf("open time %d, want %s", p.PoolOpenTime, api.OpenTime)
			}
			// pool_info_api_v3.json: feeRate 0.0025 for both pools.
			if p.SwapFeeNumerator*10000 != 25*p.SwapFeeDenominator {
				t.Errorf("swap fee %d/%d, want 0.25%%", p.SwapFeeNumerator, p.SwapFeeDenominator)
			}
			// The vaults are token accounts of the pool's mints held by
			// the authority (read with the SPL token layout).
			for _, v := range []struct {
				vault, mint solana.PublicKey
			}{{p.CoinVault, p.CoinMint}, {p.PcVault, p.PcMint}} {
				mint, authority, _ := tokenAccount(t, v.vault.String())
				if mint != v.mint || authority != p.Authority {
					t.Errorf("vault %s: mint %s authority %s", v.vault, mint, authority)
				}
			}
			if err := p.Swappable(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)); err != nil {
				t.Errorf("a pool the API routes through is not swappable: %v", err)
			}
		})
	}
}

// ammV4Account builds pool data with the given fields set, on top of a real
// captured pool, at the offsets of state.rs AmmInfo counted by hand.
func ammV4Account(t *testing.T, edit func(data []byte)) []byte {
	t.Helper()
	data := slices.Clone(loadAccount(t, "58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2").data)
	edit(data)
	return data
}

func TestDecodeAMMV4PoolRefuses(t *testing.T) {
	t.Parallel()
	good := ammV4Account(t, func([]byte) {})
	tests := []struct {
		name  string
		owner solana.PublicKey
		data  []byte
		want  error
	}{
		{"wrong owner", raydium.CPMMProgram, good, raydium.ErrWrongOwner},
		{"short", raydium.AMMV4Program, good[:751], raydium.ErrMalformedAccount},
		{"long", raydium.AMMV4Program, append(slices.Clone(good), 0), raydium.ErrMalformedAccount},
		{"status uninitialized", raydium.AMMV4Program, ammV4Account(t, func(d []byte) { binary.LittleEndian.PutUint64(d[0:], 0) }), raydium.ErrMalformedAccount},
		{"status 8", raydium.AMMV4Program, ammV4Account(t, func(d []byte) { binary.LittleEndian.PutUint64(d[0:], 8) }), raydium.ErrMalformedAccount},
		{"nonce 256", raydium.AMMV4Program, ammV4Account(t, func(d []byte) { binary.LittleEndian.PutUint64(d[8:], 256) }), raydium.ErrMalformedAccount},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := raydium.DecodeAMMV4Pool(tt.owner, tt.data); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDecodeAMMV4PoolAuthorityFollowsNonce(t *testing.T) {
	t.Parallel()
	real, err := raydium.DecodeAMMV4Pool(raydium.AMMV4Program, ammV4Account(t, func([]byte) {}))
	if err != nil {
		t.Fatal(err)
	}
	// Another nonce gives another program address, or none.
	for nonce := range uint64(256) {
		if uint8(nonce) == real.Nonce {
			continue
		}
		p, err := raydium.DecodeAMMV4Pool(raydium.AMMV4Program, ammV4Account(t, func(d []byte) { binary.LittleEndian.PutUint64(d[8:], nonce) }))
		if err == nil && p.Authority == real.Authority {
			t.Fatalf("nonce %d gives the authority of nonce %d", nonce, real.Nonce)
		}
		if err != nil && !errors.Is(err, raydium.ErrMalformedAccount) {
			t.Fatalf("nonce %d: %v", nonce, err)
		}
	}
}

func TestAMMV4Swappable(t *testing.T) {
	t.Parallel()
	open := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name   string
		status uint64
		opens  uint64
		fee    [2]uint64
		same   bool
		ok     bool
	}{
		{name: "initialized", status: 1, fee: [2]uint64{25, 10000}, ok: true},
		{name: "disabled", status: 2, fee: [2]uint64{25, 10000}},
		{name: "withdraw only", status: 3, fee: [2]uint64{25, 10000}},
		{name: "liquidity only", status: 4, fee: [2]uint64{25, 10000}},
		{name: "orderbook only", status: 5, fee: [2]uint64{25, 10000}},
		{name: "swap only", status: 6, fee: [2]uint64{25, 10000}, ok: true},
		{name: "waiting, open", status: 7, opens: uint64(open.Unix()), fee: [2]uint64{25, 10000}, ok: true},
		{name: "waiting, not open", status: 7, opens: uint64(open.Unix()) + 1, fee: [2]uint64{25, 10000}},
		{name: "same mints", status: 6, fee: [2]uint64{25, 10000}, same: true},
		{name: "zero fee denominator", status: 6, fee: [2]uint64{0, 0}},
		{name: "fee of 100%", status: 6, fee: [2]uint64{10000, 10000}},
		{name: "zero fee", status: 6, fee: [2]uint64{0, 10000}, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, err := raydium.DecodeAMMV4Pool(raydium.AMMV4Program, ammV4Account(t, func(d []byte) {
				binary.LittleEndian.PutUint64(d[0:], tt.status)
				binary.LittleEndian.PutUint64(d[224:], tt.opens)
				binary.LittleEndian.PutUint64(d[176:], tt.fee[0])
				binary.LittleEndian.PutUint64(d[184:], tt.fee[1])
				if tt.same {
					copy(d[432:464], d[400:432])
				}
			}))
			if err != nil {
				t.Fatal(err)
			}
			err = p.Swappable(open)
			if tt.ok != (err == nil) || (err != nil && !errors.Is(err, raydium.ErrPoolNotSwappable)) {
				t.Fatalf("Swappable = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestAMMV4SwapInstructionBytes(t *testing.T) {
	t.Parallel()
	s := raydium.AMMV4Swap{
		Pool: key("58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2"), Authority: ammV4Authority,
		CoinVault: key("DQyrAcCrDXQ7NeoqGgDCZwBvWDcYmFCjSb9JtteuvPpz"), PcVault: key("HLmqeL62xR1QoZ1HKKbXRrdN1p3phKpxRMb2VVopvBBz"),
		UserSource: key("11111111111111111111111111111112"), UserDestination: key("11111111111111111111111111111113"),
		UserOwner: key("11111111111111111111111111111114"),
		AmountIn:  0x0102030405060708, MinAmountOut: 1_000_000,
	}
	in := s.Instruction()
	// instruction.rs AmmInstruction::pack for SwapBaseInV2: tag 16, then
	// amount_in and minimum_amount_out as u64 little-endian.
	wantData := []byte{16, 8, 7, 6, 5, 4, 3, 2, 1, 0x40, 0x42, 0x0f, 0, 0, 0, 0, 0}
	if !bytes.Equal(in.Data, wantData) {
		t.Errorf("data %x, want %x", in.Data, wantData)
	}
	if in.Program != raydium.AMMV4Program {
		t.Errorf("program %s", in.Program)
	}
	// instruction.rs swap_base_in_v2: the account metas in order.
	want := []solana.AccountMeta{
		{PublicKey: solana.TokenProgramID},
		{PublicKey: s.Pool, IsWritable: true},
		{PublicKey: s.Authority},
		{PublicKey: s.CoinVault, IsWritable: true},
		{PublicKey: s.PcVault, IsWritable: true},
		{PublicKey: s.UserSource, IsWritable: true},
		{PublicKey: s.UserDestination, IsWritable: true},
		{PublicKey: s.UserOwner, IsSigner: true},
	}
	if !slices.Equal(in.Accounts, want) {
		t.Errorf("accounts\n got %v\nwant %v", in.Accounts, want)
	}
	got, err := raydium.DecodeAMMV4Swap(in.Program, in.Keys(), in.Data)
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Errorf("round trip %+v, want %+v", got, s)
	}
}

func TestAMMV4PoolSwapUsesPoolAccounts(t *testing.T) {
	t.Parallel()
	a := loadAccount(t, "58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2")
	p, err := raydium.DecodeAMMV4Pool(a.owner, a.data)
	if err != nil {
		t.Fatal(err)
	}
	user, src, dst := key("11111111111111111111111111111114"), key("11111111111111111111111111111112"), key("11111111111111111111111111111113")
	for _, mint := range []solana.PublicKey{wsol, usdc} {
		s, err := p.Swap(a.key, mint, src, dst, user, 5, 4)
		if err != nil {
			t.Fatal(err)
		}
		want := raydium.AMMV4Swap{
			Pool: a.key, Authority: ammV4Authority,
			CoinVault: key("DQyrAcCrDXQ7NeoqGgDCZwBvWDcYmFCjSb9JtteuvPpz"), PcVault: key("HLmqeL62xR1QoZ1HKKbXRrdN1p3phKpxRMb2VVopvBBz"),
			UserSource: src, UserDestination: dst, UserOwner: user, AmountIn: 5, MinAmountOut: 4,
		}
		if s != want {
			t.Errorf("%s: %+v, want %+v", mint, s, want)
		}
	}
	if _, err := p.Swap(a.key, usdt, src, dst, user, 5, 4); !errors.Is(err, raydium.ErrPoolNotSwappable) {
		t.Errorf("swap of a mint the pool does not hold: %v", err)
	}
}

func TestDecodeAMMV4SwapRefuses(t *testing.T) {
	t.Parallel()
	good := raydium.AMMV4Swap{Pool: key("11111111111111111111111111111112"), AmountIn: 1, MinAmountOut: 1}.Instruction()
	keys := good.Keys()
	tests := []struct {
		name     string
		program  solana.PublicKey
		accounts []solana.PublicKey
		data     []byte
	}{
		{"other program", raydium.CPMMProgram, keys, good.Data},
		{"tag 9 (SwapBaseIn)", raydium.AMMV4Program, keys, append([]byte{9}, good.Data[1:]...)},
		{"tag 17 (SwapBaseOutV2)", raydium.AMMV4Program, keys, append([]byte{17}, good.Data[1:]...)},
		{"trailing byte", raydium.AMMV4Program, keys, append(slices.Clone(good.Data), 0)},
		{"short data", raydium.AMMV4Program, keys, good.Data[:16]},
		{"empty data", raydium.AMMV4Program, keys, nil},
		{"7 accounts", raydium.AMMV4Program, keys[:7], good.Data},
		{"9 accounts", raydium.AMMV4Program, append(slices.Clone(keys), keys[1]), good.Data},
		{"token-2022", raydium.AMMV4Program, append([]solana.PublicKey{solana.Token2022ProgramID}, keys[1:]...), good.Data},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := raydium.DecodeAMMV4Swap(tt.program, tt.accounts, tt.data); !errors.Is(err, raydium.ErrMalformedInstruction) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestAMMV4AmountOut(t *testing.T) {
	t.Parallel()
	p, err := raydium.DecodeAMMV4Pool(raydium.AMMV4Program, ammV4Account(t, func(d []byte) {
		binary.LittleEndian.PutUint64(d[176:], 25)            // swap fee 25/10000
		binary.LittleEndian.PutUint64(d[184:], 10000)         //
		binary.LittleEndian.PutUint64(d[192:], 500_000_000)   // need_take_pnl_coin
		binary.LittleEndian.PutUint64(d[200:], 1_000_000_000) // need_take_pnl_pc
	}))
	if err != nil {
		t.Fatal(err)
	}
	// By hand (processor.rs, math.rs): fee = ceil(1_000_001 × 25 / 10000) =
	// 2501; in = 997_500; reserves coin 1.5e9−0.5e9, pc 3e9−1e9.
	// coin→pc: 2e9 × 997_500 / (1e9 + 997_500) = 1_993_011.9… → 1_993_011.
	got, err := p.AmountOut(p.CoinMint, 1_000_001, 1_500_000_000, 3_000_000_000)
	if err != nil || got != 1_993_011 {
		t.Errorf("coin→pc = %d, %v; want 1993011", got, err)
	}
	// pc→coin: 1e9 × 997_500 / (2e9 + 997_500) = 498_501.2… → 498_501.
	got, err = p.AmountOut(p.PcMint, 1_000_001, 1_500_000_000, 3_000_000_000)
	if err != nil || got != 498_501 {
		t.Errorf("pc→coin = %d, %v; want 498501", got, err)
	}
	for name, call := range map[string]func() (uint64, error){
		"other mint":         func() (uint64, error) { return p.AmountOut(usdt, 1, 2e9, 2e9) },
		"vault below pnl":    func() (uint64, error) { return p.AmountOut(p.CoinMint, 1, 499_999_999, 2e9) },
		"pc vault below pnl": func() (uint64, error) { return p.AmountOut(p.CoinMint, 1, 6e8, 999_999_999) },
		"empty pool":         func() (uint64, error) { return p.AmountOut(p.CoinMint, 1, 5e8, 1e9) },
	} {
		if _, err := call(); !errors.Is(err, raydium.ErrPoolNotSwappable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	zero := p
	zero.SwapFeeDenominator = 0
	if _, err := zero.AmountOut(p.CoinMint, 1, 1000, 1000); !errors.Is(err, raydium.ErrPoolNotSwappable) {
		t.Errorf("zero denominator: %v", err)
	}
}
