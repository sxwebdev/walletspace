package solana

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

func testKey(seed byte) solana.PublicKey {
	return solana.PublicKeyFromBytes(bytes.Repeat([]byte{seed}, 32))
}

// testSwap is a plan-free set of instructions shaped like a swap: a compute
// budget, an AMM v4 swap and a SOL transfer, with a table that holds the
// pool's accounts.
func testSwap() (solana.PublicKey, []raydium.Instruction, []any, map[solana.PublicKey]solana.PublicKeySlice) {
	payer := testKey(1)
	s := raydium.AMMV4Swap{
		Pool: testKey(10), Authority: testKey(11), CoinVault: testKey(12), PcVault: testKey(13),
		UserSource: testKey(14), UserDestination: testKey(15), UserOwner: payer, AmountIn: 7, MinAmountOut: 6,
	}
	instructions := []raydium.Instruction{
		{Program: solana.ComputeBudget, Data: binary.LittleEndian.AppendUint32([]byte{2}, 1000)},
		s.Instruction(),
		{
			Program:  solana.SystemProgramID,
			Accounts: []solana.AccountMeta{{PublicKey: payer, IsWritable: true, IsSigner: true}, {PublicKey: testKey(16), IsWritable: true}},
			Data:     binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), 1),
		},
	}
	tables := map[solana.PublicKey]solana.PublicKeySlice{
		testKey(50): {testKey(99), testKey(10), testKey(11), testKey(12), testKey(13), testKey(14), testKey(15)},
	}
	return payer, instructions, []any{s}, tables
}

func contents(tables map[solana.PublicKey]solana.PublicKeySlice) map[solana.PublicKey][]solana.PublicKey {
	out := make(map[solana.PublicKey][]solana.PublicKey, len(tables))
	for k, v := range tables {
		out[k] = slices.Clone(v)
	}
	return out
}

// v0 forces a v0 message by giving the builder a payload too large for a
// legacy one; it returns the instructions without the padding as well.
func buildV0(t *testing.T, payer solana.PublicKey, blockhash solana.Hash, instructions []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) []byte {
	t.Helper()
	padded := append(slices.Clone(instructions), raydium.Instruction{Program: solana.MemoProgramID, Data: bytes.Repeat([]byte{'x'}, 900)})
	message, versioned, err := buildMessage(payer, blockhash, padded, tables)
	if err != nil || !versioned {
		t.Fatalf("v0 build: %v, versioned %v", err, versioned)
	}
	return message
}

func TestVerifySwapMessageAcceptsThePlan(t *testing.T) {
	t.Parallel()
	payer, instructions, swaps, tables := testSwap()
	blockhash := solana.Hash(testKey(77))
	legacy, versioned, err := buildMessage(payer, blockhash, instructions, tables)
	if err != nil || versioned {
		t.Fatalf("legacy build: %v %v", err, versioned)
	}
	if err := verifySwapMessage(legacy, payer, blockhash, instructions, swaps, nil); err != nil {
		t.Fatalf("legacy: %v", err)
	}
	padded := append(slices.Clone(instructions), raydium.Instruction{Program: solana.MemoProgramID, Data: bytes.Repeat([]byte{'x'}, 900)})
	v0 := buildV0(t, payer, blockhash, instructions, tables)
	if err := verifySwapMessage(v0, payer, blockhash, padded, swaps, contents(tables)); err != nil {
		t.Fatalf("v0: %v", err)
	}
	decoded, err := decodeSwapMessage(v0, contents(tables))
	if err != nil || !decoded.versioned || len(decoded.lookups) != 1 || decoded.static == len(decoded.keys) {
		t.Fatalf("v0 decoded %+v, %v", decoded, err)
	}
}

func TestDecodeSwapMessageRefusesMalformed(t *testing.T) {
	t.Parallel()
	payer, instructions, _, tables := testSwap()
	blockhash := solana.Hash(testKey(77))
	legacy, _, err := buildMessage(payer, blockhash, instructions, nil)
	if err != nil {
		t.Fatal(err)
	}
	v0 := buildV0(t, payer, blockhash, instructions, tables)
	for name, message := range map[string][]byte{"legacy": legacy, "v0": v0} {
		// Every truncation is refused, and so is a byte more.
		for n := range len(message) {
			if _, err := decodeSwapMessage(message[:n], contents(tables)); err == nil {
				t.Fatalf("%s cut at %d of %d decoded", name, n, len(message))
			}
		}
		if _, err := decodeSwapMessage(append(slices.Clone(message), 0), contents(tables)); !errors.Is(err, errMalformedMessage) {
			t.Fatalf("%s with a trailing byte: %v", name, err)
		}
	}
	edit := func(message []byte, f func([]byte)) []byte {
		out := slices.Clone(message)
		f(out)
		return out
	}
	other := contents(tables)
	other[testKey(50)] = other[testKey(50)][:2]
	tests := []struct {
		name    string
		message []byte
		tables  map[solana.PublicKey][]solana.PublicKey
	}{
		{"version 1", edit(v0, func(m []byte) { m[0] = 0x81 }), contents(tables)},
		{"more signers than keys", edit(legacy, func(m []byte) { m[0] = 200 }), nil},
		{"read-only signers beyond the signers", edit(legacy, func(m []byte) { m[1] = 2 }), nil},
		{"read-only accounts beyond the keys", edit(legacy, func(m []byte) { m[2] = 200 }), nil},
		{"a table not in the route", v0, nil},
		{"an index past the table's end", v0, other},
		{"a compact length of four bytes", append(slices.Clone(legacy[:3]), 0x80, 0x80, 0x80, 0x80), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeSwapMessage(tt.message, tt.tables); err == nil {
				t.Fatal("decoded")
			}
		})
	}
	// An instruction whose program is loaded from a table, or names an
	// account past the list, is refused.
	decoded, err := decodeSwapMessage(v0, contents(tables))
	if err != nil {
		t.Fatal(err)
	}
	programAt := 3 + 1 + 32*decoded.static + 32 + 1 + 1 // version, header, keys, blockhash, count
	if _, err := decodeSwapMessage(edit(v0, func(m []byte) { m[programAt] = byte(decoded.static) }), contents(tables)); err == nil {
		t.Fatal("a program from a table decoded")
	}
	first := legacyInstructionStart(legacy)
	if _, err := decodeSwapMessage(edit(legacy, func(m []byte) { m[first] = 250 }), nil); err == nil {
		t.Fatal("a program index past the keys decoded")
	}
	// The AMM swap's first account index set past the keys.
	ammAccounts := first + 1 + 1 + 1 + 5 + 1 // compute budget: program, 0 accounts, data length, 5 bytes; then program, count
	if _, err := decodeSwapMessage(edit(legacy, func(m []byte) { m[ammAccounts+1] = 250 }), nil); err == nil {
		t.Fatal("an account index past the keys decoded")
	}
}

// legacyInstructionStart is the offset of a legacy message's first
// instruction: header, key count, keys, blockhash, instruction count.
func legacyInstructionStart(message []byte) int {
	keys := int(message[3])
	return 3 + 1 + 32*keys + 32 + 1
}

func TestVerifySwapMessageRefuses(t *testing.T) {
	t.Parallel()
	payer, instructions, swaps, _ := testSwap()
	blockhash := solana.Hash(testKey(77))
	build := func(in []raydium.Instruction) []byte {
		message, _, err := buildMessage(payer, blockhash, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	good := build(instructions)
	clone := func() []raydium.Instruction {
		out := make([]raydium.Instruction, len(instructions))
		for i, in := range instructions {
			out[i] = raydium.Instruction{Program: in.Program, Accounts: slices.Clone(in.Accounts), Data: slices.Clone(in.Data)}
		}
		return out
	}
	changed := func(f func([]raydium.Instruction) []raydium.Instruction) []raydium.Instruction { return f(clone()) }
	otherAMM := raydium.AMMV4Swap{Pool: testKey(30), UserOwner: payer, AmountIn: 7, MinAmountOut: 6}
	tests := []struct {
		name      string
		message   []byte
		payer     solana.PublicKey
		blockhash solana.Hash
		want      []raydium.Instruction
		swaps     []any
	}{
		{"not decodable", good[:10], payer, blockhash, instructions, swaps},
		{"another payer", good, testKey(2), blockhash, instructions, swaps},
		{"two signers", build(changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[2].Accounts[1].IsSigner = true
			return in
		})), payer, blockhash, instructions, swaps},
		{"another blockhash", good, payer, solana.Hash(testKey(78)), instructions, swaps},
		{"fewer instructions", good, payer, blockhash, instructions[:2], swaps},
		{"another program", good, payer, blockhash, changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[2].Program = solana.TokenProgramID
			return in
		}), swaps},
		{"other data", good, payer, blockhash, changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[2].Data[4] = 9
			return in
		}), swaps},
		{"more accounts", good, payer, blockhash, changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[2].Accounts = append(in[2].Accounts, solana.AccountMeta{PublicKey: testKey(17)})
			return in
		}), swaps},
		{"another account", good, payer, blockhash, changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[2].Accounts[1].PublicKey = testKey(17)
			return in
		}), swaps},
		{"a second signer expected", good, payer, blockhash, changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[2].Accounts[1].IsSigner = true
			return in
		}), swaps},
		{"a swap the plan lacks", good, payer, blockhash, instructions, nil},
		{"a swap more in the plan", good, payer, blockhash, instructions, []any{swaps[0], swaps[0]}},
		{"another swap", good, payer, blockhash, instructions, []any{otherAMM}},
		{"a swap that does not decode", build(changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[1].Data = append(in[1].Data, 0)
			return in
		})), payer, blockhash, changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[1].Data = append(in[1].Data, 0)
			return in
		}), swaps},
		{
			"an unused account", build(append(clone(), raydium.Instruction{Program: solana.MemoProgramID, Accounts: []solana.AccountMeta{{PublicKey: testKey(40)}}})),
			payer, blockhash, append(clone(), raydium.Instruction{Program: solana.MemoProgramID, Accounts: []solana.AccountMeta{{PublicKey: testKey(41)}}}), swaps,
		},
		{"a writable flag", build(changed(func(in []raydium.Instruction) []raydium.Instruction {
			in[1].Accounts[2].IsWritable = true
			return in
		})), payer, blockhash, instructions, swaps},
		{"an account listed twice", duplicateKey(good), payer, blockhash, instructions, swaps},
		{"two accounts in each other's place", build(append(clone(), raydium.Instruction{
			Program: solana.MemoProgramID, Accounts: []solana.AccountMeta{{PublicKey: testKey(41)}, {PublicKey: testKey(40)}},
		})), payer, blockhash, append(clone(), raydium.Instruction{
			Program: solana.MemoProgramID, Accounts: []solana.AccountMeta{{PublicKey: testKey(40)}, {PublicKey: testKey(41)}},
		}), swaps},
	}
	texts := map[string]string{"an account listed twice": "listed twice", "two accounts in each other's place": "account 0 is"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := verifySwapMessage(tt.message, tt.payer, tt.blockhash, tt.want, tt.swaps, nil)
			if !errors.Is(err, ErrSwapMismatch) || !bytes.Contains([]byte(err.Error()), []byte(texts[tt.name])) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// A CPMM or CLMM swap is compared with its own decoder.
	cpmm := raydium.CPMMSwap{Payer: payer, InputTokenProgram: solana.TokenProgramID, OutputTokenProgram: solana.TokenProgramID, AmountIn: 1}
	clmm := raydium.CLMMSwap{Payer: payer, Pool: testKey(60), TickArrays: []solana.PublicKey{testKey(61)}, Amount: 1}
	clmmIx, err := clmm.Instruction()
	if err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct {
		in   raydium.Instruction
		swap any
	}{"cpmm": {cpmm.Instruction(), cpmm}, "clmm": {clmmIx, clmm}} {
		message := build([]raydium.Instruction{tt.in})
		if err := verifySwapMessage(message, payer, blockhash, []raydium.Instruction{tt.in}, []any{tt.swap}, nil); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// duplicateKey writes the second key over the third in a legacy message.
func duplicateKey(message []byte) []byte {
	out := slices.Clone(message)
	copy(out[4+64:4+96], out[4+32:4+64])
	return out
}

func TestDecodeLookupTable(t *testing.T) {
	t.Parallel()
	table := func(kind uint32, deactivation uint64, addresses int) []byte {
		data := make([]byte, lookupTableMetaSize+32*addresses)
		binary.LittleEndian.PutUint32(data, kind)
		binary.LittleEndian.PutUint64(data[4:], deactivation)
		for i := range addresses {
			copy(data[lookupTableMetaSize+32*i:], bytes.Repeat([]byte{byte(i + 1)}, 32))
		}
		return data
	}
	got, err := decodeLookupTable(addressLookupTableProgram, table(1, math.MaxUint64, 2))
	if err != nil || len(got) != 2 || got[1] != testKey(2) {
		t.Fatalf("table %v, %v", got, err)
	}
	for name, tt := range map[string]struct {
		owner solana.PublicKey
		data  []byte
	}{
		"another owner":   {solana.SystemProgramID, table(1, math.MaxUint64, 1)},
		"short":           {addressLookupTableProgram, make([]byte, 55)},
		"a partial entry": {addressLookupTableProgram, append(table(1, math.MaxUint64, 1), 0)},
		"uninitialized":   {addressLookupTableProgram, table(0, math.MaxUint64, 1)},
		"deactivated":     {addressLookupTableProgram, table(1, 400_000_000, 1)},
	} {
		if _, err := decodeLookupTable(tt.owner, tt.data); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestParseSwapApproval(t *testing.T) {
	t.Parallel()
	good := SwapApproval{ComputeUnitLimit: 151_000, ComputeUnitPriceMicroLamports: 1000, Fee: "0.000005151"}
	if got, err := parseSwapApproval(good); err != nil || got.fee != 5151 || got.limit != 151_000 {
		t.Fatalf("parse %+v, %v", got, err)
	}
	withRent := SwapApproval{ComputeUnitLimit: 151_000, ComputeUnitPriceMicroLamports: 1000, TokenAccountsCreated: 1, TokenAccountRent: "0.00148844", Fee: "0.001493591"}
	if got, err := parseSwapApproval(withRent); err != nil || got.rent != 1_488_440 || got.creates != 1 {
		t.Fatalf("parse %+v, %v", got, err)
	}
	change := func(f func(*SwapApproval)) SwapApproval {
		a := good
		f(&a)
		return a
	}
	for name, a := range map[string]SwapApproval{
		"zero limit":           change(func(a *SwapApproval) { a.ComputeUnitLimit = 0 }),
		"limit above max":      change(func(a *SwapApproval) { a.ComputeUnitLimit = maxComputeUnitLimit + 1 }),
		"price above max":      change(func(a *SwapApproval) { a.ComputeUnitPriceMicroLamports = maxComputeUnitPrice + 1 }),
		"negative creates":     change(func(a *SwapApproval) { a.TokenAccountsCreated = -1 }),
		"three creates":        change(func(a *SwapApproval) { a.TokenAccountsCreated = 3 }),
		"creates without rent": change(func(a *SwapApproval) { a.TokenAccountsCreated = 1 }),
		"rent without creates": change(func(a *SwapApproval) { a.TokenAccountRent = "0.001" }),
		"rent not an amount":   change(func(a *SwapApproval) { a.TokenAccountRent = "x" }),
		"fee not an amount":    change(func(a *SwapApproval) { a.Fee = "x" }),
		"fee off by one":       change(func(a *SwapApproval) { a.Fee = "0.000005152" }),
	} {
		if _, err := parseSwapApproval(a); !errors.Is(err, chain.ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := parseSwapApproval(change(func(a *SwapApproval) { a.TokenAccountRent = "0" })); err != nil {
		t.Errorf("a zero rent without creates: %v", err)
	}
}

func TestParseSwapRoute(t *testing.T) {
	t.Parallel()
	hop := SwapHop{Pool: testKey(1).String(), Kind: "clmm", InputMint: testKey(2).String(), OutputMint: testKey(3).String()}
	good := SwapRoute{Hops: []SwapHop{hop}, LookupTables: []string{testKey(4).String()}}
	hops, tables, err := parseSwapRoute(good)
	if err != nil || len(hops) != 1 || hops[0].kind != raydium.KindCLMM || len(tables) != 1 {
		t.Fatalf("parse %v %v %v", hops, tables, err)
	}
	bad := func(f func(*SwapRoute)) SwapRoute {
		r := SwapRoute{Hops: []SwapHop{hop}, LookupTables: slices.Clone(good.LookupTables)}
		f(&r)
		return r
	}
	for name, r := range map[string]SwapRoute{
		"no hops":      bad(func(r *SwapRoute) { r.Hops = nil }),
		"three hops":   bad(func(r *SwapRoute) { r.Hops = []SwapHop{hop, hop, hop} }),
		"unknown kind": bad(func(r *SwapRoute) { r.Hops[0].Kind = "orca" }),
		"bad pool":     bad(func(r *SwapRoute) { r.Hops[0].Pool = "x" }),
		"bad mint":     bad(func(r *SwapRoute) { r.Hops[0].OutputMint = "" }),
		"three tables": bad(func(r *SwapRoute) {
			r.LookupTables = []string{testKey(4).String(), testKey(5).String(), testKey(6).String()}
		}),
		"bad table":     bad(func(r *SwapRoute) { r.LookupTables = []string{"x"} }),
		"a table twice": bad(func(r *SwapRoute) { r.LookupTables = []string{testKey(4).String(), testKey(4).String()} }),
	} {
		if _, _, err := parseSwapRoute(r); !errors.Is(err, chain.ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckHops(t *testing.T) {
	t.Parallel()
	// b is an allowed intermediate (USDC); x is not.
	a, b, c, x := testKey(1), solana.MustPublicKeyFromBase58("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"), testKey(3), testKey(4)
	hop := func(pool byte, in, out solana.PublicKey) hopSpec {
		return hopSpec{pool: testKey(pool), inMint: in, outMint: out}
	}
	for _, mid := range []string{"So11111111111111111111111111111111111111112", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"} {
		m := solana.MustPublicKeyFromBase58(mid)
		if err := checkHops([]hopSpec{hop(9, a, m), hop(8, m, c)}, a, c); err != nil {
			t.Fatalf("two hops through %s: %v", mid, err)
		}
	}
	for name, hops := range map[string][]hopSpec{
		"none":                  nil,
		"three":                 {hop(9, a, b), hop(8, b, c), hop(7, c, a)},
		"not from the input":    {hop(9, b, c)},
		"to itself":             {hop(9, a, a)},
		"not connected":         {hop(9, a, b), hop(8, a, c)},
		"not to the output":     {hop(9, a, b)},
		"the same pool twice":   {hop(9, a, b), hop(9, b, c)},
		"through another token": {hop(9, a, x), hop(8, x, c)},
	} {
		out := c
		if name == "to itself" {
			out = a
		}
		if err := checkHops(hops, a, out); !errors.Is(err, chain.ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSwapMint(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		asset chain.Asset
		ok    bool
	}{
		"SOL":             {chain.Asset{NetworkID: "n", Kind: "native", Decimals: 9}, true},
		"SOL with 8":      {chain.Asset{NetworkID: "n", Kind: "native", Decimals: 8}, false},
		"USDC":            {chain.Asset{NetworkID: "n", Kind: "spl", Contract: testKey(5).String()}, true},
		"bad mint":        {chain.Asset{NetworkID: "n", Kind: "spl", Contract: "x"}, false},
		"wrapped SOL":     {chain.Asset{NetworkID: "n", Kind: "spl", Contract: solana.WrappedSol.String()}, false},
		"another kind":    {chain.Asset{NetworkID: "n", Kind: "erc20"}, false},
		"another network": {chain.Asset{NetworkID: "m", Kind: "native", Decimals: 9}, false},
	} {
		if _, _, err := swapMint(tt.asset, "n"); (err == nil) != tt.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSlippageFailure(t *testing.T) {
	t.Parallel()
	payer, instructions, _, _ := testSwap()
	message, _, err := buildMessage(payer, solana.Hash{}, instructions, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &swapPlan{user: payer}
	failure := func(index any, code any) any {
		return map[string]any{"InstructionError": []any{index, map[string]any{"Custom": code}}}
	}
	for name, tt := range map[string]struct {
		failure any
		want    bool
	}{
		"AMM v4 slippage":             {failure(1.0, 30.0), true},
		"another AMM v4 error":        {failure(1.0, 31.0), false},
		"an error of another program": {failure(2.0, 30.0), false},
		"a string":                    {"AccountNotFound", false},
		"another map":                 {map[string]any{"InsufficientFundsForRent": map[string]any{}}, false},
		"short pair":                  {map[string]any{"InstructionError": []any{1.0}}, false},
		"not custom":                  {map[string]any{"InstructionError": []any{1.0, "InvalidAccountData"}}, false},
		"no code":                     {failure(1.0, "x"), false},
		"bad index":                   {failure("x", 30.0), false},
		"index past the message":      {failure(9.0, 30.0), false},
	} {
		if got := p.slippageFailure(tt.failure, message); got != tt.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if p.slippageFailure(failure(1.0, 30.0), message[:5]) {
		t.Error("an undecodable message")
	}
	err = p.explainSwap(&rpc.SimulateTransactionResult{Err: failure(1.0, 30.0)}, message)
	if !errors.Is(err, chain.ErrInvalidRequest) || !errors.Is(err, errStateDependent) {
		t.Errorf("explain: %v", err)
	}
	if !errors.Is(err, errRoute) {
		t.Errorf("a pool's slippage refusal is not the route's: %v", err)
	}
	// Another refusal by the pool's program is the route's; a refusal by the
	// wallet's own transfer is not.
	if err := p.explainSwap(&rpc.SimulateTransactionResult{Err: failure(1.0, 31.0)}, message); !errors.Is(err, errRoute) {
		t.Errorf("a pool's other refusal: %v", err)
	}
	if err := p.explainSwap(&rpc.SimulateTransactionResult{Err: failure(2.0, 1.0)}, message); errors.Is(err, errRoute) || !errors.Is(err, errStateDependent) {
		t.Errorf("the transfer's refusal: %v", err)
	}
	err = p.explainSwap(&rpc.SimulateTransactionResult{Err: "InsufficientFundsForFee"}, message)
	if !errors.Is(err, errStateDependent) || errors.Is(err, errRoute) {
		t.Errorf("explain fee: %v", err)
	}
}

func TestTokenAfter(t *testing.T) {
	t.Parallel()
	mint, owner := testKey(1), testKey(2)
	data := make([]byte, tokenAccountSize)
	copy(data, mint[:])
	copy(data[32:], owner[:])
	binary.LittleEndian.PutUint64(data[64:], 42)
	account := func(owner solana.PublicKey, data []byte) *rpc.Account {
		return &rpc.Account{Owner: owner, Data: rpc.DataBytesOrJSONFromBytes(data)}
	}
	result := &rpc.SimulateTransactionResult{Accounts: []*rpc.Account{account(solana.TokenProgramID, data)}}
	if got, err := tokenAfter(result, 0, mint, owner); err != nil || got != 42 {
		t.Fatalf("tokenAfter = %d, %v", got, err)
	}
	for name, a := range map[string]*rpc.Account{
		"missing":       nil,
		"another owner": account(solana.Token2022ProgramID, data),
		"short":         account(solana.TokenProgramID, data[:100]),
	} {
		if _, err := tokenAfter(&rpc.SimulateTransactionResult{Accounts: []*rpc.Account{a}}, 0, mint, owner); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if _, err := tokenAfter(result, 0, testKey(3), owner); err == nil {
		t.Error("another mint: read")
	}
	if _, err := tokenAfter(result, 0, mint, testKey(3)); err == nil {
		t.Error("another owner of the tokens: read")
	}
}

// A read-only entry past the end of its table is refused, after the
// writable ones resolved.
func TestDecodeSwapMessageReadonlyLookupPastTheEnd(t *testing.T) {
	t.Parallel()
	payer, instructions, _, _ := testSwap()
	// The authority, the only read-only lookup, is the table's last entry.
	tables := map[solana.PublicKey]solana.PublicKeySlice{
		testKey(50): {testKey(99), testKey(10), testKey(12), testKey(13), testKey(14), testKey(15), testKey(11)},
	}
	v0 := buildV0(t, payer, solana.Hash{}, instructions, tables)
	short := contents(tables)
	short[testKey(50)] = short[testKey(50)][:6]
	if _, err := decodeSwapMessage(v0, short); err == nil || !bytes.Contains([]byte(err.Error()), []byte("no entry 6")) {
		t.Fatalf("err = %v", err)
	}
}

// An account the message lists that no instruction uses is refused.
func TestVerifySwapMessageRefusesAnUnusedAccount(t *testing.T) {
	t.Parallel()
	payer, instructions, swaps, _ := testSwap()
	message, _, err := buildMessage(payer, solana.Hash{}, instructions, nil)
	if err != nil {
		t.Fatal(err)
	}
	// One more read-only key at the end of the static list.
	count := int(message[3])
	extra := slices.Concat(message[:3], []byte{byte(count + 1)}, message[4:4+32*count], testKey(88).Bytes(), message[4+32*count:])
	extra[2]++
	err = verifySwapMessage(extra, payer, solana.Hash{}, instructions, swaps, nil)
	if !errors.Is(err, ErrSwapMismatch) || !bytes.Contains([]byte(err.Error()), []byte("not used")) {
		t.Fatalf("err = %v", err)
	}
}

func TestComputeExhausted(t *testing.T) {
	t.Parallel()
	failure := func(detail any) any { return map[string]any{"InstructionError": []any{4.0, detail}} }
	exceeded := []string{"Program X consumed 100 of 100 compute units", "Program X failed: exceeded CUs meter at BPF instruction #7"}
	for name, tt := range map[string]struct {
		failure any
		logs    []string
		want    bool
	}{
		"a program out of compute":        {failure("ProgramFailedToComplete"), exceeded, true},
		"a builtin out of compute":        {failure("ComputationalBudgetExceeded"), nil, true},
		"a program that failed otherwise": {failure("ProgramFailedToComplete"), []string{"Program X failed: Access violation"}, false},
		"another instruction error":       {failure(map[string]any{"Custom": 1.0}), exceeded, false},
		"a transaction error":             {"InsufficientFundsForFee", exceeded, false},
		"another map":                     {map[string]any{"InsufficientFundsForRent": map[string]any{}}, exceeded, false},
		"a short pair":                    {map[string]any{"InstructionError": []any{4.0}}, exceeded, false},
	} {
		if got := computeExhausted(tt.failure, tt.logs); got != tt.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
