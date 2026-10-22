package raydium_test

import (
	"encoding/base64"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

func decodePool(t *testing.T, id string) raydium.CLMMPool {
	t.Helper()
	a := loadAccount(t, id)
	p, err := raydium.DecodeCLMMPool(a.owner, a.data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func decodeExtension(t *testing.T, id string) *raydium.CLMMBitmapExtension {
	t.Helper()
	a := loadAccount(t, id)
	e, err := raydium.DecodeCLMMBitmapExtension(a.owner, a.data)
	if err != nil {
		t.Fatal(err)
	}
	return &e
}

// The Trade API's remainingAccounts in the captured quotes are tick arrays
// of the quoted pool (the chain says so: owner, header and PDA), and they are
// the arrays the program walks first from the pool's current tick.
func TestQuotedTickArraysAreTheWalk(t *testing.T) {
	t.Parallel()
	tests := []struct {
		quote, pool, extension, array string
		input                         solana.PublicKey
	}{
		{"usdc_usdt.json", "BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU", "HSJS4BrYCZq9DmCNFNfctqq9o2sfTY5YQiy9n4W8djcB", "2CntbsRKrr4an5zekGb8WZyPyPzbXv9R2CErrRQSQVo2", usdc},
		{"usdt_sol.json", "3nMFwZXwY1s1M5s8vYAHqd4wGs4iSxXE4LRoUMMYqEgF", "2ncinnTcJxbZ1nUHavBVJ3Ap3R4CE7p2LJ6Jtpd1vLzd", "D4iaph1swuDtgemsMZjd5doVCdoVfj4BjuicQX11xw2d", usdt},
		{"sol_usdc_small.json", "2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv", "9z9VTNvaTpJuwjn4LSnjHwZgUR9iGuy59BwXTNbxRF6s", "8Zo3x55Q81YL8Bev6EPULCNmoGNtwCT58d1KswzRTvbQ", wsol},
	}
	for _, tt := range tests {
		t.Run(tt.quote, func(t *testing.T) {
			t.Parallel()
			route := replayQuote(t, tt.quote)
			if len(route.Hops) == 0 || route.Hops[0].Pool != key(tt.pool) || !slices.Equal(route.Hops[0].RemainingAccounts, []solana.PublicKey{key(tt.array)}) {
				t.Fatalf("quote does not route %s through %s: %+v", tt.array, tt.pool, route.Hops)
			}
			a := loadAccount(t, tt.array)
			header, err := raydium.DecodeCLMMTickArray(a.owner, a.data)
			if err != nil {
				t.Fatal(err)
			}
			if header.Pool != key(tt.pool) || header.InitializedTickCount == 0 {
				t.Fatalf("tick array header %+v", header)
			}
			if pda := raydium.CLMMTickArrayAddress(key(tt.pool), header.StartTickIndex); pda != a.key {
				t.Fatalf("PDA of start %d = %s, want %s", header.StartTickIndex, pda, a.key)
			}
			p := decodePool(t, tt.pool)
			ext := decodeExtension(t, tt.extension)
			zeroForOne, err := p.ZeroForOne(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.CheckTickArrays(key(tt.pool), ext, zeroForOne, route.Hops[0].RemainingAccounts); err != nil {
				t.Fatal(err)
			}
			// The same array in the other direction is the first array
			// only if it holds the current tick.
			starts, err := p.TickArrayStarts(ext, !zeroForOne, 1)
			if err != nil {
				t.Fatal(err)
			}
			holdsCurrent := header.StartTickIndex <= p.TickCurrent && p.TickCurrent < header.StartTickIndex+60*int32(p.TickSpacing)
			if (starts[0] == header.StartTickIndex) != holdsCurrent {
				t.Errorf("reverse walk starts at %d, array %d, current tick %d", starts[0], header.StartTickIndex, p.TickCurrent)
			}
		})
	}
}

// tickArraySnapshot is testdata/tickarrays_<pool>.json: the pool and its
// bitmap extension, and every tick array of the pool the node returned for
// getProgramAccounts, with its start index and initialized tick count.
type tickArraySnapshot struct {
	Pool      string `json:"pool"`
	Extension string `json:"extension"`
	Accounts  map[string]struct {
		Owner string `json:"owner"`
		Data  string `json:"data"`
	} `json:"accounts"`
	TickArrays map[string]struct {
		Start                int32 `json:"start"`
		InitializedTickCount uint8 `json:"initializedTickCount"`
	} `json:"tickArrays"`
}

// The pool's bitmap and extension, read the way TickArrayStarts reads them,
// name exactly the tick arrays the chain holds with initialized ticks — on a
// pool whose arrays reach into the extension on both sides.
func TestBitmapMatchesTheChain(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv", "3nMFwZXwY1s1M5s8vYAHqd4wGs4iSxXE4LRoUMMYqEgF"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			var snap tickArraySnapshot
			readJSON(t, "tickarrays_"+id+".json", &snap)
			raw := func(k string) (solana.PublicKey, []byte) {
				a, ok := snap.Accounts[k]
				if !ok {
					t.Fatalf("snapshot lacks %s", k)
				}
				data, err := base64.StdEncoding.DecodeString(a.Data)
				if err != nil {
					t.Fatal(err)
				}
				return key(a.Owner), data
			}
			p, err := raydium.DecodeCLMMPool(raw(snap.Pool))
			if err != nil {
				t.Fatal(err)
			}
			ext, err := raydium.DecodeCLMMBitmapExtension(raw(snap.Extension))
			if err != nil {
				t.Fatal(err)
			}
			pool := key(snap.Pool)
			var chain []int32
			for k, a := range snap.TickArrays {
				// The node says account k is the array at a.Start: the
				// PDA seeds must give k back.
				if pda := raydium.CLMMTickArrayAddress(pool, a.Start); pda != key(k) {
					t.Fatalf("PDA of %d = %s, the chain has %s", a.Start, pda, k)
				}
				if a.InitializedTickCount > 0 {
					chain = append(chain, a.Start)
				}
			}
			down, err := p.TickArrayStarts(&ext, true, math.MaxInt)
			if err != nil {
				t.Fatal(err)
			}
			up, err := p.TickArrayStarts(&ext, false, math.MaxInt)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.IsSortedFunc(down, func(a, b int32) int { return int(b - a) }) || !slices.IsSorted(up) {
				t.Fatal("walks are not monotonic")
			}
			walked := slices.Compact(slices.Sorted(slices.Values(slices.Concat(down, up))))
			slices.Sort(chain)
			if !slices.Equal(walked, chain) {
				t.Fatalf("bitmap gives %d arrays, chain has %d initialized\n bitmap %v\n chain  %v", len(walked), len(chain), walked, chain)
			}
			beyond := 0
			for _, s := range chain {
				if s < -30720 || s >= 30720 {
					beyond++
				}
			}
			if beyond == 0 {
				t.Error("the snapshot does not exercise the extension")
			}
			// Without the extension the walk stops at the pool's own
			// bitmap boundary.
			if _, err := p.TickArrayStarts(nil, true, math.MaxInt); !errors.Is(err, raydium.ErrNeedBitmapExtension) {
				t.Errorf("walk without extension: %v", err)
			}
		})
	}
}

// pool builds a CLMM pool with the given spacing, current tick and set
// bits in its own bitmap (bit = start/(60·spacing) + 512, little-endian
// U1024: libraries/tick_array_bit_map.rs check_current_tick_array_is_initialized).
func syntheticPool(spacing uint16, tick int32, starts ...int32) raydium.CLMMPool {
	p := raydium.CLMMPool{TickSpacing: spacing, TickCurrent: tick}
	for _, s := range starts {
		bit := s/(60*int32(spacing)) + 512
		p.TickArrayBitmap[bit/64] |= 1 << (bit % 64)
	}
	return p
}

func TestTickArrayStarts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		pool       raydium.CLMMPool
		noExt      bool
		zeroForOne bool
		n          int
		want       []int32
		err        error
	}{
		{name: "current array first, down", pool: syntheticPool(1, 70, -120, 0, 60, 180), zeroForOne: true, n: 5, want: []int32{60, 0, -120}},
		{name: "current array first, up", pool: syntheticPool(1, 70, -120, 0, 60, 180), n: 5, want: []int32{60, 180}},
		{name: "n bounds the walk", pool: syntheticPool(1, 70, -120, 0, 60, 180), zeroForOne: true, n: 2, want: []int32{60, 0}},
		{name: "current array empty, down", pool: syntheticPool(1, 130, -120, 0, 60, 180), zeroForOne: true, n: 5, want: []int32{60, 0, -120}},
		{name: "current array empty, up", pool: syntheticPool(1, 130, -120, 0, 60, 180), n: 5, want: []int32{180}},
		{name: "negative tick rounds down", pool: syntheticPool(1, -1, -60, 0), zeroForOne: false, n: 5, want: []int32{-60, 0}},
		{name: "tick on an array start", pool: syntheticPool(1, -60, -60, -120), zeroForOne: true, n: 5, want: []int32{-60, -120}},
		{name: "spacing 10", pool: syntheticPool(10, 1234, 600, 1200, 1800), zeroForOne: true, n: 5, want: []int32{1200, 600}},
		{name: "nothing initialized", pool: syntheticPool(60, 0), zeroForOne: true, n: 5},
		{name: "spacing 60 never needs the extension", pool: syntheticPool(60, -443636, -446400), n: 5, want: []int32{-446400}},
		{name: "walk reaches the pool bitmap's edge", pool: syntheticPool(1, -30690, -30720), noExt: true, zeroForOne: true, n: 5, err: raydium.ErrNeedBitmapExtension},
		{name: "fewer than n before the edge", pool: syntheticPool(1, 70, -120, 0, 60, 180), noExt: true, n: 5, err: raydium.ErrNeedBitmapExtension},
		{name: "n reached before the edge", pool: syntheticPool(1, 70, -120, 0, 60, 180), noExt: true, zeroForOne: true, n: 3, want: []int32{60, 0, -120}},
		{name: "current array in the extension", pool: syntheticPool(1, 30725), noExt: true, n: 1, err: raydium.ErrNeedBitmapExtension},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// An empty extension lets a walk run to MIN_TICK/MAX_TICK.
			ext := &raydium.CLMMBitmapExtension{}
			if tt.noExt {
				ext = nil
			}
			got, err := tt.pool.TickArrayStarts(ext, tt.zeroForOne, tt.n)
			if !errors.Is(err, tt.err) || !slices.Equal(got, tt.want) {
				t.Fatalf("TickArrayStarts = %v, %v; want %v, %v", got, err, tt.want, tt.err)
			}
		})
	}
}

// The extension's bit layout, worked out by hand from
// tickarray_bitmap_extension.rs for tick spacing 1 (512 arrays of 60 ticks
// = 30720 ticks per bitmap): positive bitmap k covers [30720(k+1),
// 30720(k+2)) with bit (start − 30720(k+1))/60; negative bitmap k covers
// [−30720(k+2), −30720(k+1)) with bit 512 − (|start| mod 30720)/60, or 0 at
// a multiple of 30720.
func TestTickArrayStartsInTheExtension(t *testing.T) {
	t.Parallel()
	var ext raydium.CLMMBitmapExtension
	set := func(b *[8]uint64, bit int) { b[bit/64] |= 1 << (bit % 64) }
	set(&ext.Positive[0], 0)    // 30720
	set(&ext.Positive[0], 1)    // 30780
	set(&ext.Positive[1], 511)  // 61440 + 511·60 = 92100
	set(&ext.Positive[13], 0)   // 30720·14 = 430080
	set(&ext.Negative[0], 511)  // −30780
	set(&ext.Negative[0], 0)    // −61440
	set(&ext.Negative[13], 400) // −30720·15 + 400·60 = −436800
	tests := []struct {
		name       string
		pool       raydium.CLMMPool
		zeroForOne bool
		want       []int32
	}{
		{"up from the pool's bitmap", syntheticPool(1, 30000, 30660), false, []int32{30660, 30720, 30780, 92100, 430080}},
		{"down from the pool's bitmap", syntheticPool(1, -30000, -30720), true, []int32{-30720, -30780, -61440, -436800}},
		{"down from inside the extension, across the pool's bitmap", syntheticPool(1, 30779), true, []int32{30720, -30780, -61440, -436800}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.pool.TickArrayStarts(&ext, tt.zeroForOne, 10)
			if err != nil || !slices.Equal(got, tt.want) {
				t.Fatalf("TickArrayStarts = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
	// The walk ends at the last array that can hold MIN_TICK or MAX_TICK
	// even when the extension has bits beyond.
	var far raydium.CLMMBitmapExtension
	set(&far.Positive[13], 511) // 30720·14 + 511·60 = 460740 > MAX_TICK
	if got, err := syntheticPool(1, 443000).TickArrayStarts(&far, false, 10); err != nil || len(got) != 0 {
		t.Errorf("beyond MAX_TICK: %v, %v", got, err)
	}
}

func TestCheckTickArrays(t *testing.T) {
	t.Parallel()
	pool := key("BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU")
	p := syntheticPool(1, 70, -120, 0, 60, 180)
	addr := func(start int32) solana.PublicKey { return raydium.CLMMTickArrayAddress(pool, start) }
	if err := p.CheckTickArrays(pool, nil, true, []solana.PublicKey{addr(60), addr(0), addr(-120)}); err != nil {
		t.Fatalf("the walk itself: %v", err)
	}
	if err := p.CheckTickArrays(pool, nil, true, []solana.PublicKey{addr(60)}); err != nil {
		t.Fatalf("a prefix of the walk: %v", err)
	}
	ownExt := &raydium.CLMMBitmapExtension{Pool: pool}
	if err := p.CheckTickArrays(pool, ownExt, false, []solana.PublicKey{addr(60), addr(180)}); err != nil {
		t.Fatalf("with the pool's extension: %v", err)
	}
	tests := []struct {
		name  string
		ext   *raydium.CLMMBitmapExtension
		down  bool
		given []solana.PublicKey
		want  error
	}{
		{"none", nil, true, nil, raydium.ErrTickArrays},
		{"too many", nil, true, slices.Repeat([]solana.PublicKey{addr(60)}, raydium.MaxTickArrays+1), raydium.ErrTickArrays},
		{"skips the current array", nil, true, []solana.PublicKey{addr(0), addr(-120)}, raydium.ErrTickArrays},
		{"wrong order", nil, true, []solana.PublicKey{addr(60), addr(-120), addr(0)}, raydium.ErrTickArrays},
		{"wrong direction", nil, true, []solana.PublicKey{addr(60), addr(180)}, raydium.ErrTickArrays},
		{"an uninitialized array", nil, true, []solana.PublicKey{addr(60), addr(-60)}, raydium.ErrTickArrays},
		{"another pool's array", nil, true, []solana.PublicKey{raydium.CLMMTickArrayAddress(key("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"), 60)}, raydium.ErrTickArrays},
		{"more than the walk has", ownExt, false, []solana.PublicKey{addr(60), addr(180), addr(240)}, raydium.ErrTickArrays},
		{"another pool's extension", &raydium.CLMMBitmapExtension{Pool: key("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv")}, true, []solana.PublicKey{addr(60)}, raydium.ErrTickArrays},
		{"extension needed", nil, true, slices.Repeat([]solana.PublicKey{addr(60)}, 4), raydium.ErrNeedBitmapExtension},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := p.CheckTickArrays(pool, tt.ext, tt.down, tt.given); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if err := (raydium.CLMMPool{}).CheckTickArrays(pool, nil, true, []solana.PublicKey{addr(0)}); !errors.Is(err, raydium.ErrMalformedAccount) {
		t.Errorf("zero tick spacing: %v", err)
	}
}

func TestDecodeTickAccountsRefuse(t *testing.T) {
	t.Parallel()
	array := loadAccount(t, "2CntbsRKrr4an5zekGb8WZyPyPzbXv9R2CErrRQSQVo2").data
	ext := loadAccount(t, "4NFvUKqknMpoe6CWTzK758B8ojVLzURL5pC6MtiaJ8TQ").data
	if _, err := raydium.DecodeCLMMTickArray(raydium.CPMMProgram, array); !errors.Is(err, raydium.ErrWrongOwner) {
		t.Errorf("tick array owner: %v", err)
	}
	if _, err := raydium.DecodeCLMMTickArray(raydium.CLMMProgram, array[:10239]); !errors.Is(err, raydium.ErrMalformedAccount) {
		t.Errorf("tick array size: %v", err)
	}
	bad := slices.Clone(array)
	bad[0] ^= 1
	if _, err := raydium.DecodeCLMMTickArray(raydium.CLMMProgram, bad); !errors.Is(err, raydium.ErrMalformedAccount) {
		t.Errorf("tick array discriminator: %v", err)
	}
	if _, err := raydium.DecodeCLMMBitmapExtension(raydium.AMMV4Program, ext); !errors.Is(err, raydium.ErrWrongOwner) {
		t.Errorf("extension owner: %v", err)
	}
	if _, err := raydium.DecodeCLMMBitmapExtension(raydium.CLMMProgram, append(slices.Clone(ext), 0)); !errors.Is(err, raydium.ErrMalformedAccount) {
		t.Errorf("extension size: %v", err)
	}
	bad = slices.Clone(ext)
	bad[5] ^= 1
	if _, err := raydium.DecodeCLMMBitmapExtension(raydium.CLMMProgram, bad); !errors.Is(err, raydium.ErrMalformedAccount) {
		t.Errorf("extension discriminator: %v", err)
	}
	e := decodeExtension(t, "4NFvUKqknMpoe6CWTzK758B8ojVLzURL5pC6MtiaJ8TQ")
	if e.Pool != key("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv") {
		t.Errorf("extension pool %s", e.Pool)
	}
}
