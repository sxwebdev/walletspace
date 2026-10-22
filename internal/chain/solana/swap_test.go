package solana_test

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

const (
	lutCLMM  = "47Fi43gX1EHVN1LX1Nu73MDpmZ3yooB8fwtBuxufyzX7" // named by api-v3 for 2Qdh
	lutBZtg  = "8TM3cxoXfnNXU7XUjsAeyN8niu7dQMDf5ZB47nmrTcLD"
	lutAMM   = "8rG5WFRriQYz3SiYSjB2V7TVCqJiTfur23foeRkWLD67"
	lutCPMM  = "5SVTkysCYR59jvVcgfvFa7cCNY6KUa8RrcoH6FUt1oEi"
	tick2Qdh = "8Zo3x55Q81YL8Bev6EPULCNmoGNtwCT58d1KswzRTvbQ" // the Trade API's first tick array for 2Qdh selling SOL
)

type swapFixture struct {
	chain   *swapChain
	api     *fakeRaydium
	adapter *solanachain.Adapter
	user    *testSigner
}

func newSwapFixture(t *testing.T) swapFixture {
	t.Helper()
	c := newSwapChain(t)
	api := newFakeRaydium(t)
	adapter, _ := single(t, c.node)
	adapter.UseRaydiumQuoter(api.quoter(t))
	user := newTestSigner(9)
	c.set(user.address(), ledgerAccount{owner: systemProgram, lamports: 2_000_000_000})
	for _, table := range []string{lutCLMM, lutBZtg, lutAMM, lutCPMM} {
		c.pool(table)
	}
	return swapFixture{chain: c, api: api, adapter: adapter, user: user}
}

// solToUSDC prepares a SOL → USDC swap through the CLMM pool 2Qdh, quoted
// by the API as captured (0.01 SOL for 1.043581 USDC).
func (f swapFixture) solToUSDC() {
	f.chain.pool(clmmSOLUSDC, "9z9VTNvaTpJuwjn4LSnjHwZgUR9iGuy59BwXTNbxRF6s")
	f.chain.rate(clmmSOLUSDC, wsolMint, 1_043_581, 10_000_000)
	f.chain.tokens(f.user.address(), usdcMint, 0)
	f.api.answer(http.StatusOK, quoteBody(wsolMint, usdcMint, 10_000_000, 1_043_581, 50,
		hopFixture{pool: clmmSOLUSDC, in: wsolMint, out: usdcMint, remaining: []string{tick2Qdh}}))
}

func quoteRequest(f swapFixture, in, out chain.Asset, amount string, slippage uint16) solanachain.SwapQuoteRequest {
	return solanachain.SwapQuoteRequest{From: f.user.address(), In: in, Out: out, Amount: amount, SlippageBps: slippage}
}

func approve(q solanachain.SwapQuote) solanachain.SwapApproval {
	return solanachain.SwapApproval{
		ComputeUnitLimit: q.ComputeUnitLimit, ComputeUnitPriceMicroLamports: q.ComputeUnitPriceMicroLamports,
		TokenAccountsCreated: q.TokenAccountsCreated, TokenAccountRent: q.TokenAccountRent, Fee: q.Fee,
	}
}

func swapRequest(f swapFixture, in, out chain.Asset, amount string, slippage uint16, q solanachain.SwapQuote) solanachain.SwapRequest {
	return solanachain.SwapRequest{
		From: f.user.address(), In: in, Out: out, Amount: amount, SlippageBps: slippage,
		MinOut: q.MinOut, Route: q.Route, Approved: approve(q),
	}
}

var (
	solAsset  = native("solana-mainnet")
	usdcAsset = spl("solana-mainnet", usdcMint, "USDC", 6)
	usdtAsset = spl("solana-mainnet", usdtMint, "USDT", 6)
)

type programCall struct {
	program string
	data    []byte
}

// calls lists a transaction's instructions as program and data.
func calls(t *testing.T, raw []byte, c *swapChain) ([]programCall, *solana.Transaction, []solana.PublicKey) {
	t.Helper()
	c.mu.Lock()
	tx, keys := decodeTransaction(t, raw, c.accounts)
	c.mu.Unlock()
	var out []programCall
	for _, in := range tx.Message.Instructions {
		out = append(out, programCall{program: keys[in.ProgramIDIndex].String(), data: in.Data})
	}
	return out, tx, keys
}

func TestQuoteSwapSOLForUSDCThroughCLMM(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	// Output 10,000,000 × 1,043,581 / 10,000,000; minimum ⌊1,043,581 × 0.995⌋.
	// Compute: 120,000 measured + 5,000 for the close, a fifth more and
	// 1,000; price at the 1,000 µlamport floor: fee 5,000 + ⌈151,000 × 1,000
	// / 10⁶⌉ = 5,151 lamports. The USDC account exists; the wrapped-SOL
	// account is opened and closed in the swap, so no rent is charged.
	want := solanachain.SwapQuote{
		NetworkID: "solana-mainnet", AmountIn: "0.01", AmountOut: "1.043581", MinOut: "1.038363", SlippageBps: 50,
		Source: solanachain.SwapSourceAPI, QuotedOut: "1.043581", PriceImpactPct: "0.01", QuoteDeviationPct: "0.0000",
		Route:          solanachain.SwapRoute{Hops: []solanachain.SwapHop{{Pool: clmmSOLUSDC, Kind: "clmm", InputMint: wsolMint, OutputMint: usdcMint}}},
		MessageVersion: "legacy", FeeModel: "solana", Fee: "0.000005151", ComputeUnitLimit: 151_000,
		ComputeUnitPriceMicroLamports: 1000, NativeBalance: "2",
	}
	want.MessageSize = q.MessageSize
	if !equalQuote(q, want) {
		t.Fatalf("quote\n got %+v\nwant %+v", q, want)
	}
	if q.MessageSize <= 0 || q.MessageSize > 1232 {
		t.Errorf("message size %d", q.MessageSize)
	}
	quotes, keys := f.api.counts()
	if quotes != 1 || keys != 1 {
		t.Errorf("Trade API asked %d times, pool keys %d", quotes, keys)
	}
	// One read for the wallet, mints, pool, extension, accounts and tables;
	// a measuring simulation and the final one.
	if n := f.chain.node.count("getMultipleAccounts"); n != 1 {
		t.Errorf("%d getMultipleAccounts calls", n)
	}
	if n := f.chain.node.count("simulateTransaction"); n != 2 {
		t.Errorf("%d simulations", n)
	}
}

func equalQuote(a, b solanachain.SwapQuote) bool { return reflect.DeepEqual(a, b) }

func TestSwapSOLForUSDCSignsThePlannedTransaction(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	req := swapRequest(f, solAsset, usdcAsset, "0.01", 50, q)
	var height uint64
	var recorded string
	req.BeforeSigning = func(h uint64) error { height = h; return nil }
	req.BeforeBroadcast = func(id string) error { recorded = id; return nil }
	tx, err := f.adapter.Swap(t.Context(), "solana-mainnet", req, f.user)
	if err != nil {
		t.Fatal(err)
	}
	sent := f.chain.sentTransactions()
	if len(sent) != 1 {
		t.Fatalf("%d transactions sent", len(sent))
	}
	raw := sent[0]
	if raw[0] != 1 || !ed25519.Verify(f.user.PublicKey(), raw[65:], raw[1:65]) {
		t.Fatal("the signature does not verify over the broadcast message")
	}
	if tx.Hash != solana.SignatureFromBytes(raw[1:65]).String() || recorded != tx.Hash || height != testLastValid || tx.Status != "pending" {
		t.Fatalf("tx %+v, recorded %s, height %d", tx, recorded, height)
	}
	got, decoded, keys := calls(t, raw, f.chain)
	if decoded.Message.IsVersioned() || decoded.Message.RecentBlockhash.String() != testBlockhash {
		t.Errorf("version %v blockhash %s", decoded.Message.IsVersioned(), decoded.Message.RecentBlockhash)
	}
	programs := make([]string, len(got))
	for i, call := range got {
		programs[i] = call.program
	}
	wantPrograms := []string{swapComputeBudget, swapComputeBudget, ataProgram, systemProgram, tokenProgram, raydium.CLMMProgram.String(), tokenProgram}
	if !slices.Equal(programs, wantPrograms) {
		t.Fatalf("programs %v\nwant %v", programs, wantPrograms)
	}
	// The approved limit and price, the SOL sold, SyncNative, the swap at
	// the approved minimum, CloseAccount.
	if binary.LittleEndian.Uint32(got[0].data[1:]) != 151_000 || binary.LittleEndian.Uint64(got[1].data[1:]) != 1000 ||
		binary.LittleEndian.Uint64(got[3].data[4:]) != 10_000_000 || !slices.Equal(got[4].data, []byte{17}) ||
		!slices.Equal(got[6].data, []byte{9}) {
		t.Errorf("instructions %v", got)
	}
	swapIx := decoded.Message.Instructions[5]
	accounts := make([]solana.PublicKey, len(swapIx.Accounts))
	for i, index := range swapIx.Accounts {
		accounts[i] = keys[index]
	}
	s, err := raydium.DecodeCLMMSwap(raydium.CLMMProgram, accounts, swapIx.Data)
	if err != nil {
		t.Fatal(err)
	}
	if s.Amount != 10_000_000 || s.OtherAmountThreshold != 1_038_363 || s.Payer.String() != f.user.address() ||
		s.InputTokenAccount.String() != ata(f.user.address(), wsolMint) || s.OutputTokenAccount.String() != ata(f.user.address(), usdcMint) ||
		!s.WithBitmapExtension || len(s.TickArrays) != 3 || s.TickArrays[0].String() != tick2Qdh {
		t.Errorf("swap %+v", s)
	}
	if f.user.signs.Load() != 1 {
		t.Errorf("%d signatures", f.user.signs.Load())
	}
}

// firstTickArrays is the walk the CLMM program makes from a captured pool's
// current tick, n arrays long.
func firstTickArrays(t *testing.T, pool, extension, inputMint string, n int) []string {
	t.Helper()
	poolAccount, extAccount := fixtureData(t, pool), fixtureData(t, extension)
	p, err := raydium.DecodeCLMMPool(solana.MustPublicKeyFromBase58(poolAccount.owner), poolAccount.data)
	if err != nil {
		t.Fatal(err)
	}
	ext, err := raydium.DecodeCLMMBitmapExtension(solana.MustPublicKeyFromBase58(extAccount.owner), extAccount.data)
	if err != nil {
		t.Fatal(err)
	}
	zeroForOne, err := p.ZeroForOne(solana.MustPublicKeyFromBase58(inputMint))
	if err != nil {
		t.Fatal(err)
	}
	arrays, err := p.TickArrays(solana.MustPublicKeyFromBase58(pool), &ext, zeroForOne, n)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(arrays))
	for i, a := range arrays {
		out[i] = a.String()
	}
	return out
}

// usdtToSOL prepares a two-hop USDT → USDC (CLMM BZtg) → SOL (CLMM 2Qdh)
// swap of 50 USDT. The wallet has no USDC account yet.
func (f swapFixture) usdtToSOL(t *testing.T) {
	f.chain.pool(clmmUSDCUSDT, "HSJS4BrYCZq9DmCNFNfctqq9o2sfTY5YQiy9n4W8djcB")
	f.chain.pool(clmmSOLUSDC, "9z9VTNvaTpJuwjn4LSnjHwZgUR9iGuy59BwXTNbxRF6s")
	f.chain.rate(clmmUSDCUSDT, usdtMint, 1_000_893_399, 1_000_000_000)
	f.chain.rate(clmmSOLUSDC, usdcMint, 239_461_095, 25_000_000)
	f.chain.tokens(f.user.address(), usdtMint, 100_000_000)
	f.api.answer(http.StatusOK, quoteBody(usdtMint, wsolMint, 50_000_000, 479_000_000, 100,
		hopFixture{
			pool: clmmUSDCUSDT, in: usdtMint, out: usdcMint,
			remaining: firstTickArrays(t, clmmUSDCUSDT, "HSJS4BrYCZq9DmCNFNfctqq9o2sfTY5YQiy9n4W8djcB", usdtMint, 1),
		},
		hopFixture{
			pool: clmmSOLUSDC, in: usdcMint, out: wsolMint,
			remaining: firstTickArrays(t, clmmSOLUSDC, "9z9VTNvaTpJuwjn4LSnjHwZgUR9iGuy59BwXTNbxRF6s", usdcMint, 1),
		},
	))
}

// The two-hop route does not fit a legacy transaction; it is built as v0
// with the pools' lookup tables, and what the second hop sells is the first
// hop's output less the slippage.
func TestSwapUSDTForSOLInTwoHopsWithLookupTables(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.usdtToSOL(t)
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdtAsset, solAsset, "50", 100))
	if err != nil {
		t.Fatal(err)
	}
	// Hop 1: 50,000,000 × 1.000893399 = 50,044,669 USDC; the second hop
	// sells ⌊× 0.99⌋ = 49,544,222, which pays ⌊× 239,461,095 / 25,000,000⌋
	// = 474,556,546 lamports; minimum ⌊× 0.99⌋ = 469,810,980.
	if q.AmountOut != "0.474556546" || q.MinOut != "0.46981098" || q.MessageVersion != "v0" || q.MessageSize > 1232 ||
		q.IntermediateMint != usdcMint || q.IntermediateLeftover != "0.500447" || q.TokenAccountsCreated != 1 ||
		q.TokenAccountRent != "0.00148844" || len(q.Route.Hops) != 2 || q.QuoteDeviationPct != "" ||
		!slices.Equal(q.Route.LookupTables, []string{lutBZtg, lutCLMM}) {
		t.Fatalf("quote %+v", q)
	}
	tx, err := f.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(f, usdtAsset, solAsset, "50", 100, q), f.user)
	if err != nil {
		t.Fatal(err)
	}
	sent := f.chain.sentTransactions()
	if len(sent) != 1 || !ed25519.Verify(f.user.PublicKey(), sent[0][65:], sent[0][1:65]) || tx.Hash != solana.SignatureFromBytes(sent[0][1:65]).String() {
		t.Fatal("the swap was not broadcast with a valid signature")
	}
	got, decoded, _ := calls(t, sent[0], f.chain)
	if !decoded.Message.IsVersioned() || len(decoded.Message.AddressTableLookups) == 0 {
		t.Fatal("the two-hop swap is not a v0 message with lookups")
	}
	var programs []string
	for _, call := range got {
		programs = append(programs, call.program)
	}
	want := []string{
		swapComputeBudget, swapComputeBudget, ataProgram, ataProgram,
		raydium.CLMMProgram.String(), raydium.CLMMProgram.String(), tokenProgram,
	}
	if !slices.Equal(programs, want) {
		t.Fatalf("programs %v\nwant %v", programs, want)
	}
}

// localOut is what a captured AMM v4 or CPMM pool's curve pays for amount
// of inputMint, with its captured vault balances.
func localOut(t *testing.T, pool, inputMint string, amount uint64) uint64 {
	t.Helper()
	account := fixtureData(t, pool)
	owner := solana.MustPublicKeyFromBase58(account.owner)
	in := solana.MustPublicKeyFromBase58(inputMint)
	switch raydium.KindOf(owner) {
	case raydium.KindAMMV4:
		p, err := raydium.DecodeAMMV4Pool(owner, account.data)
		if err != nil {
			t.Fatal(err)
		}
		out, err := p.AmountOut(in, amount, tokenAmount(fixtureData(t, p.CoinVault.String()).data), tokenAmount(fixtureData(t, p.PcVault.String()).data))
		if err != nil {
			t.Fatal(err)
		}
		return out
	default:
		p, err := raydium.DecodeCPMMPool(owner, account.data)
		if err != nil {
			t.Fatal(err)
		}
		c := fixtureData(t, p.AmmConfig.String())
		cfg, err := raydium.DecodeCPMMConfig(solana.MustPublicKeyFromBase58(c.owner), c.data)
		if err != nil {
			t.Fatal(err)
		}
		out, err := p.AmountOut(cfg, in, amount, tokenAmount(fixtureData(t, p.Token0Vault.String()).data), tokenAmount(fixtureData(t, p.Token1Vault.String()).data))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
}

func TestSwapUSDCForUSDTThroughAMMV4AndCPMM(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, pool, kind string
		extras           []string
		program          solana.PublicKey
	}{
		{"amm v4", ammUSDTUSDC, "amm-v4", []string{"Enb9jGaKzgDBfEbbUN3Ytx2ZLoZuBhBpjVX6DULiRmvu", "HyyZpz1JUZjsfyiVSt3qz6E9PkwnBcyhUg4zKGthMNeH"}, raydium.AMMV4Program},
		{"cpmm", cpmmUSDCUSDT, "cpmm", []string{"C7Cx2pMLtjybS3mDKSfsBj4zQ3PRZGkKt7RCYTTbCSx2", "79i6TxYg9GCC5ngcAL4i5jT5vtb8Dbg7Fimx6ixQNayM", "5YfaG4st68wWCVnsP473zhYg9C8Ff26F3pVAZ22DFCPh"}, raydium.CPMMProgram},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			f.chain.pool(tt.pool, tt.extras...)
			const amount = 1_000_000 // 1 USDC: the CPMM pool is shallow
			out := localOut(t, tt.pool, usdcMint, amount)
			f.chain.rate(tt.pool, usdcMint, int64(out), amount)
			f.chain.tokens(f.user.address(), usdcMint, 5_000_000)
			f.api.answer(http.StatusOK, quoteBody(usdcMint, usdtMint, amount, out, 30, hopFixture{pool: tt.pool, in: usdcMint, out: usdtMint}))
			q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdcAsset, usdtAsset, "1", 30))
			if err != nil {
				t.Fatal(err)
			}
			// The wallet has no USDT account: it is opened, and its rent is
			// part of the fee.
			if q.AmountOut != amountString(out, 6) || q.MinOut != amountString(out*9970/10000, 6) || q.Route.Hops[0].Kind != tt.kind ||
				q.TokenAccountsCreated != 1 || q.MessageVersion != "legacy" || q.QuoteDeviationPct != "0.0000" {
				t.Fatalf("quote %+v", q)
			}
			if _, err := f.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(f, usdcAsset, usdtAsset, "1", 30, q), f.user); err != nil {
				t.Fatal(err)
			}
			sent := f.chain.sentTransactions()
			if len(sent) != 1 {
				t.Fatalf("%d sent", len(sent))
			}
			got, _, _ := calls(t, sent[0], f.chain)
			var programs []string
			for _, call := range got {
				programs = append(programs, call.program)
			}
			if !slices.Equal(programs, []string{swapComputeBudget, swapComputeBudget, ataProgram, tt.program.String()}) {
				t.Fatalf("programs %v", programs)
			}
		})
	}
}

func amountString(units uint64, decimals int) string {
	scale := uint64(1)
	for range decimals {
		scale *= 10
	}
	whole, frac := units/scale, units%scale
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	text := strconv.FormatUint(whole, 10) + "." + leftPad(strconv.FormatUint(frac, 10), decimals)
	for text[len(text)-1] == '0' {
		text = text[:len(text)-1]
	}
	return text
}

// When the Trade API fails or limits the wallet, the pools are found on
// chain and the swap is built the same way.
func TestQuoteSwapFallsBackToDiscovery(t *testing.T) {
	t.Parallel()
	exotic := key(90)
	tests := []struct {
		name   string
		status int
		header http.Header
		body   []byte
		quotes int // Trade API requests over two quotes
	}{
		{"server error", http.StatusBadGateway, nil, []byte(`{"error":"down"}`), 2},
		{"rate limited", http.StatusTooManyRequests, http.Header{"Retry-After": {"60"}}, []byte(`{"error":"down"}`), 1},
		{"no route", http.StatusOK, nil, []byte(`{"id":"x","success":false,"msg":"ROUTE_NOT_FOUND"}`), 2},
		// Two hops through a token other than SOL, USDC or USDT would leave
		// some of it, and an account for it, in the wallet.
		{"two hops through an unlisted token", http.StatusOK, nil, quoteBody(wsolMint, usdcMint, 10_000_000, 1_043_581, 50,
			hopFixture{pool: clmmSOLUSDC, in: wsolMint, out: exotic},
			hopFixture{pool: clmmUSDCUSDT, in: exotic, out: usdcMint}), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			f.solToUSDC()
			f.chain.pool(clmmSOLUSDC, "E2BcoCeJLTa27mAXDA4xwEq3pBUcyH6XXEHYk4KvKYTv", "4d35yC7C8zhCDec7JbPptL9SEb4NUddKHxURgmvD8hfo")
			f.api.mu.Lock()
			f.api.header = tt.header
			f.api.mu.Unlock()
			f.api.answer(tt.status, tt.body)
			for range 2 {
				q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
				if err != nil {
					t.Fatal(err)
				}
				if q.Source != solanachain.SwapSourceDiscovery || q.QuotedOut != "" || q.AmountOut != "1.043581" ||
					len(q.Route.Hops) != 1 || q.Route.Hops[0].Pool != clmmSOLUSDC {
					t.Fatalf("quote %+v", q)
				}
			}
			if quotes, _ := f.api.counts(); quotes != tt.quotes {
				t.Errorf("Trade API asked %d times, want %d", quotes, tt.quotes)
			}
			// Four getProgramAccounts per discovery: AMM v4 in both mint
			// orders, CPMM and CLMM sorted.
			if n := f.chain.node.count("getProgramAccounts"); n != 8 {
				t.Errorf("%d getProgramAccounts calls", n)
			}
		})
	}
}

// Routes and pools the chain does not back are refused at the quote. A
// refusal caused by the API's route (its pools, tick arrays, what it pays)
// sends the quote to the pools on chain first (review decision B1); here
// there is no other pool, so the answer is "no pool" with the route's reason.
func TestQuoteSwapRefuses(t *testing.T) {
	t.Parallel()
	other := key(77)
	texts := map[string]string{
		"pool owned by another program":                               "not to Raydium",
		"tick arrays not the walk":                                    "tick arrays do not match",
		"pool does not trade the hop's mints":                         "does not trade",
		"swaps disabled on the pool":                                  "disables swaps",
		"Token-2022 mint":                                             "Token-2022",
		"slippage above 5%":                                           "slippage",
		"not enough to sell":                                          "main token account holds",
		"the pool pays less than the minimum in the final simulation": "less than the minimum",
	}
	tests := []struct {
		name  string
		setup func(f swapFixture)
		req   func(f swapFixture) solanachain.SwapQuoteRequest
		want  error
	}{
		{"pool owned by another program", func(f swapFixture) {
			f.solToUSDC()
			a := fixtureData(t, clmmSOLUSDC)
			a.owner = other
			f.chain.set(clmmSOLUSDC, a)
		}, nil, chain.ErrInvalidRequest},
		{"tick arrays not the walk", func(f swapFixture) {
			f.solToUSDC()
			f.api.answer(http.StatusOK, quoteBody(wsolMint, usdcMint, 10_000_000, 1_043_581, 50,
				hopFixture{pool: clmmSOLUSDC, in: wsolMint, out: usdcMint, remaining: []string{key(78)}}))
		}, nil, chain.ErrInvalidRequest},
		{"pool does not trade the hop's mints", func(f swapFixture) {
			f.solToUSDC()
			f.api.answer(http.StatusOK, quoteBody(wsolMint, usdtMint, 10_000_000, 1_043_581, 50,
				hopFixture{pool: clmmSOLUSDC, in: wsolMint, out: usdtMint, remaining: []string{tick2Qdh}}))
		}, func(f swapFixture) solanachain.SwapQuoteRequest {
			return quoteRequest(f, solAsset, usdtAsset, "0.01", 50)
		}, chain.ErrInvalidRequest},
		{"swaps disabled on the pool", func(f swapFixture) {
			f.solToUSDC()
			a := fixtureData(t, clmmSOLUSDC)
			a.data[389] |= 1 << 4
			f.chain.set(clmmSOLUSDC, a)
		}, nil, chain.ErrInvalidRequest},
		{"Token-2022 mint", func(f swapFixture) {
			f.solToUSDC()
			f.chain.set(usdcMint, ledgerAccount{owner: token2022, lamports: 1, data: mintData(6, true)})
		}, nil, solanachain.ErrToken2022},
		{"slippage above 5%", func(f swapFixture) { f.solToUSDC() }, func(f swapFixture) solanachain.SwapQuoteRequest {
			return quoteRequest(f, solAsset, usdcAsset, "0.01", 501)
		}, chain.ErrInvalidRequest},
		{"not enough to sell", func(f swapFixture) {
			f.chain.pool(clmmSOLUSDC, "9z9VTNvaTpJuwjn4LSnjHwZgUR9iGuy59BwXTNbxRF6s")
			f.chain.tokens(f.user.address(), usdcMint, 999_999)
			f.api.answer(http.StatusOK, quoteBody(usdcMint, wsolMint, 1_000_000, 9_578_443, 50,
				hopFixture{
					pool: clmmSOLUSDC, in: usdcMint, out: wsolMint,
					remaining: firstTickArrays(t, clmmSOLUSDC, "9z9VTNvaTpJuwjn4LSnjHwZgUR9iGuy59BwXTNbxRF6s", usdcMint, 1),
				}))
		}, func(f swapFixture) solanachain.SwapQuoteRequest {
			return quoteRequest(f, usdcAsset, solAsset, "1", 50)
		}, chain.ErrInvalidRequest},
		{"the pool pays less than the minimum in the final simulation", func(f swapFixture) {
			f.solToUSDC()
			f.chain.finalErr = instructionError(5, 6018)
		}, nil, chain.ErrInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			tt.setup(f)
			req := quoteRequest(f, solAsset, usdcAsset, "0.01", 50)
			if tt.req != nil {
				req = tt.req(f)
			}
			_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", req)
			if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), texts[tt.name]) {
				t.Fatalf("err = %v, want %v saying %q", err, tt.want, texts[tt.name])
			}
		})
	}
}

// A message that is not the planned swap is never signed.
func TestSwapRefusesAMessageOtherThanThePlan(t *testing.T) {
	t.Parallel()
	stranger := solana.MustPublicKeyFromBase58(key(55))
	clmmIndex := func(instructions []raydium.Instruction) int {
		for i, in := range instructions {
			if in.Program == raydium.CLMMProgram {
				return i
			}
		}
		t.Fatal("no CLMM swap")
		return -1
	}
	type edit = func([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice)
	tests := []struct {
		name   string
		twoHop bool
		edit   edit
	}{
		{"another vault", false, func(in []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) {
			in[clmmIndex(in)].Accounts[5].PublicKey = stranger
			return in, tables
		}},
		{"another destination", false, func(in []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) {
			in[clmmIndex(in)].Accounts[4].PublicKey = solana.MustPublicKeyFromBase58(ata(key(56), usdcMint))
			return in, tables
		}},
		{"an extra instruction", false, func(in []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) {
			payer := in[clmmIndex(in)].Accounts[0].PublicKey
			return append(in, raydium.Instruction{
				Program:  solana.SystemProgramID,
				Accounts: []solana.AccountMeta{{PublicKey: payer, IsWritable: true, IsSigner: true}, {PublicKey: stranger, IsWritable: true}},
				Data:     binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), 1),
			}), tables
		}},
		{"a lower minimum", false, func(in []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) {
			data := in[clmmIndex(in)].Data
			binary.LittleEndian.PutUint64(data[16:], 1)
			return in, tables
		}},
		{"a config made writable", false, func(in []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) {
			in[clmmIndex(in)].Accounts[1].IsWritable = true
			return in, tables
		}},
		{"a lookup entry that is another account on chain", true, func(in []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) {
			table := tables[solana.MustPublicKeyFromBase58(lutBZtg)]
			table[0] = solana.MustPublicKeyFromBase58(clmmUSDCUSDT)
			return in, tables
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			in, out, amount, slippage := solAsset, usdcAsset, "0.01", uint16(50)
			if tt.twoHop {
				f.usdtToSOL(t)
				in, out, amount, slippage = usdtAsset, solAsset, "50", 100
			} else {
				f.solToUSDC()
			}
			q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, in, out, amount, slippage))
			if err != nil {
				t.Fatal(err)
			}
			solanachain.TamperWithSwap(f.adapter, tt.edit)
			_, err = f.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(f, in, out, amount, slippage, q), f.user)
			if !errors.Is(err, solanachain.ErrSwapMismatch) {
				t.Fatalf("err = %v, want a mismatch", err)
			}
			if f.user.signs.Load() != 0 || len(f.chain.sentTransactions()) != 0 {
				t.Fatal("a tampered swap was signed or sent")
			}
			// The untampered builder signs the same request.
			solanachain.TamperWithSwap(f.adapter, func(in []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) {
				return in, tables
			})
			if _, err := f.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(f, in, out, amount, slippage, q), f.user); err != nil {
				t.Fatalf("the plain swap: %v", err)
			}
		})
	}
}

// What changed on chain since the quote is refused before anything is
// signed: the price, the fee, the accounts.
func TestSwapRefusesWhatChangedSinceTheQuote(t *testing.T) {
	t.Parallel()
	texts := map[string]string{
		"the pool pays less than the minimum":                  "below the approved minimum",
		"the minimum is more than 5% below the current output": "more than 5%",
		"the priority price rose":                              "priority price",
		"the swap needs more compute":                          "now needs 247000 compute units",
		"the receiving account was closed":                     "opens 1 token accounts",
		"the pool stopped swaps":                               "disables swaps",
		"the final simulation fails":                           "less than the minimum",
		"the route names a pool of another program":            "not to Raydium",
		"the route names another kind":                         "the route says cpmm",
		"the fee does not add up":                              "does not add up",
		"a slippage above 5%":                                  "slippage",
	}
	tests := []struct {
		name   string
		change func(f swapFixture, req *solanachain.SwapRequest)
		want   error
	}{
		{"the pool pays less than the minimum", func(f swapFixture, _ *solanachain.SwapRequest) {
			f.chain.rate(clmmSOLUSDC, wsolMint, 1_038_000, 10_000_000)
		}, chain.ErrStateChanged},
		{"the minimum is more than 5% below the current output", func(f swapFixture, req *solanachain.SwapRequest) {
			req.MinOut = "0.9914" // ⌊1,043,581 × 0.95⌋ = 991,401 is the floor
		}, chain.ErrStateChanged},
		{"the priority price rose", func(f swapFixture, _ *solanachain.SwapRequest) {
			f.chain.mu.Lock()
			f.chain.fees = []uint64{5000}
			f.chain.mu.Unlock()
		}, chain.ErrFeeChanged},
		{"the swap needs more compute", func(f swapFixture, _ *solanachain.SwapRequest) {
			f.chain.mu.Lock()
			f.chain.units = 200_000
			f.chain.mu.Unlock()
		}, chain.ErrFeeChanged},
		{"the receiving account was closed", func(f swapFixture, _ *solanachain.SwapRequest) {
			f.chain.mu.Lock()
			delete(f.chain.accounts, ata(f.user.address(), usdcMint))
			f.chain.mu.Unlock()
		}, chain.ErrFeeChanged},
		{"the pool stopped swaps", func(f swapFixture, _ *solanachain.SwapRequest) {
			a := fixtureData(t, clmmSOLUSDC)
			a.data[389] |= 1 << 4
			f.chain.set(clmmSOLUSDC, a)
		}, chain.ErrStateChanged},
		{"the final simulation fails", func(f swapFixture, _ *solanachain.SwapRequest) {
			f.chain.mu.Lock()
			f.chain.finalErr = instructionError(5, 6018)
			f.chain.mu.Unlock()
		}, chain.ErrStateChanged},
		{"the route names a pool of another program", func(f swapFixture, req *solanachain.SwapRequest) {
			req.Route.Hops[0].Pool = ata(f.user.address(), usdcMint)
		}, chain.ErrInvalidRequest},
		{"the route names another kind", func(f swapFixture, req *solanachain.SwapRequest) {
			req.Route.Hops[0].Kind = "cpmm"
		}, chain.ErrInvalidRequest},
		{"the fee does not add up", func(f swapFixture, req *solanachain.SwapRequest) {
			req.Approved.Fee = "0.000005"
		}, chain.ErrInvalidRequest},
		{"a slippage above 5%", func(f swapFixture, req *solanachain.SwapRequest) {
			req.SlippageBps = 600
		}, chain.ErrInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			f.solToUSDC()
			q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
			if err != nil {
				t.Fatal(err)
			}
			req := swapRequest(f, solAsset, usdcAsset, "0.01", 50, q)
			tt.change(f, &req)
			_, err = f.adapter.Swap(t.Context(), "solana-mainnet", req, f.user)
			if !errors.Is(err, tt.want) || !strings.Contains(err.Error(), texts[tt.name]) {
				t.Fatalf("err = %v, want %v saying %q", err, tt.want, texts[tt.name])
			}
			if f.user.signs.Load() != 0 || len(f.chain.sentTransactions()) != 0 {
				t.Fatal("signed or sent")
			}
		})
	}
	// At exactly the 5% floor the minimum is accepted.
	f := newSwapFixture(t)
	f.solToUSDC()
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	req := swapRequest(f, solAsset, usdcAsset, "0.01", 50, q)
	req.MinOut = "0.991401"
	if _, err := f.adapter.Swap(t.Context(), "solana-mainnet", req, f.user); err != nil {
		t.Fatalf("minimum at the floor: %v", err)
	}
}

func TestSwapBroadcastOutcomes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		reply  reply
		want   error
		status string
	}{
		{"lost answer", reply{status: http.StatusGatewayTimeout, raw: "timeout"}, chain.ErrBroadcastUnknown, "broadcast_unknown"},
		{"refused", reply{err: &rpcError{Code: -32002, Message: "Transaction simulation failed"}}, chain.ErrInvalidRequest, "failed"},
		{"another signature", reply{result: solana.SignatureFromBytes(make([]byte, 64)).String()}, chain.ErrBroadcastUnknown, "broadcast_unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			f.solToUSDC()
			q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
			if err != nil {
				t.Fatal(err)
			}
			f.chain.mu.Lock()
			f.chain.sendReply = &tt.reply
			f.chain.mu.Unlock()
			tx, err := f.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(f, solAsset, usdcAsset, "0.01", 50, q), f.user)
			sent := f.chain.sentTransactions()
			if !errors.Is(err, tt.want) || len(sent) != 1 || tx.Status != tt.status || tx.Hash != solana.SignatureFromBytes(sent[0][1:65]).String() {
				t.Fatalf("tx %+v, err %v", tx, err)
			}
		})
	}
}

func TestSwapRefusesAnotherSigner(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	req := swapRequest(f, solAsset, usdcAsset, "0.01", 50, q)
	for name, key := range map[string]chain.Key{"another key": newTestSigner(10), "a digest signer": digestOnly{}} {
		if _, err := f.adapter.Swap(t.Context(), "solana-mainnet", req, key); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(f.chain.sentTransactions()) != 0 {
		t.Fatal("sent")
	}
}

// The approved compute limit and price are what get signed, even when the
// swap needs less; and the expiry is recorded so the status can tell a
// transaction that can no longer land.
func TestSwapSignsTheApprovedFeeAndRecordsTheExpiry(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	req := swapRequest(f, solAsset, usdcAsset, "0.01", 50, q)
	// 5,000 + ⌈200,000 × 2,000 / 10⁶⌉ = 5,400.
	req.Approved.ComputeUnitLimit, req.Approved.ComputeUnitPriceMicroLamports, req.Approved.Fee = 200_000, 2000, "0.0000054"
	tx, err := f.adapter.Swap(t.Context(), "solana-mainnet", req, f.user)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := calls(t, f.chain.sentTransactions()[0], f.chain)
	if binary.LittleEndian.Uint32(got[0].data[1:]) != 200_000 || binary.LittleEndian.Uint64(got[1].data[1:]) != 2000 {
		t.Fatalf("compute budget %v %v", got[0].data, got[1].data)
	}
	status, err := f.adapter.Transaction(t.Context(), "solana-mainnet", tx.Hash)
	if err != nil || status.Status != "pending" {
		t.Fatalf("status %+v, %v", status, err)
	}
	f.chain.mu.Lock()
	f.chain.blockHeight = testLastValid + 1000
	f.chain.mu.Unlock()
	status, err = f.adapter.Transaction(t.Context(), "solana-mainnet", tx.Hash)
	if err != nil || status.Status != "failed" {
		t.Fatalf("status past the expiry %+v, %v", status, err)
	}
}
