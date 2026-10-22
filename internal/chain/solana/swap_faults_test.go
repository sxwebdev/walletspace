package solana_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
	"github.com/sxwebdev/walletspace/internal/network"
)

const (
	ext2Qdh  = "9z9VTNvaTpJuwjn4LSnjHwZgUR9iGuy59BwXTNbxRF6s"
	extBZtg  = "HSJS4BrYCZq9DmCNFNfctqq9o2sfTY5YQiy9n4W8djcB"
	cpmmCfg  = "C7Cx2pMLtjybS3mDKSfsBj4zQ3PRZGkKt7RCYTTbCSx2"
	ammCoinV = "Enb9jGaKzgDBfEbbUN3Ytx2ZLoZuBhBpjVX6DULiRmvu"
	ammPcV   = "HyyZpz1JUZjsfyiVSt3qz6E9PkwnBcyhUg4zKGthMNeH"
)

// usdcToSOL prepares a USDC → SOL swap of 1 USDC through 2Qdh.
func (f swapFixture) usdcToSOL(t *testing.T) {
	f.chain.pool(clmmSOLUSDC, ext2Qdh)
	f.chain.rate(clmmSOLUSDC, usdcMint, 9_578_443, 1_000_000)
	f.chain.tokens(f.user.address(), usdcMint, 5_000_000)
	f.api.answer(http.StatusOK, quoteBody(usdcMint, wsolMint, 1_000_000, 9_578_443, 50,
		hopFixture{pool: clmmSOLUSDC, in: usdcMint, out: wsolMint, remaining: firstTickArrays(t, clmmSOLUSDC, ext2Qdh, usdcMint, 1)}))
}

// usdcToUSDT prepares a USDC → USDT swap of 1 USDC through a pool of kind.
func (f swapFixture) usdcToUSDT(t *testing.T, pool string, extras ...string) {
	f.chain.pool(pool, extras...)
	out := localOut(t, pool, usdcMint, 1_000_000)
	f.chain.rate(pool, usdcMint, int64(out), 1_000_000)
	f.chain.tokens(f.user.address(), usdcMint, 5_000_000)
	f.api.answer(http.StatusOK, quoteBody(usdcMint, usdtMint, 1_000_000, out, 30, hopFixture{pool: pool, in: usdcMint, out: usdtMint}))
}

func TestQuoteSwapWithAWrappedSOLAccount(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.usdcToSOL(t)
	// 5 SOL already wrapped: the account is reused, not opened, and its
	// balance is not counted as output.
	f.chain.set(ata(f.user.address(), wsolMint), ledgerAccount{
		owner: tokenProgram, lamports: rentExemptToken + 5_000_000_000,
		data: tokenAccountData(wsolMint, f.user.address(), 5_000_000_000, 1),
	})
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdcAsset, solAsset, "1", 50))
	if err != nil {
		t.Fatal(err)
	}
	if q.AmountOut != "0.009578443" || q.TokenAccountsCreated != 0 || q.Fee != "0.000005151" || q.UnwrapsWrappedSOL != "5" {
		t.Fatalf("quote %+v", q)
	}
	if _, err := f.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(f, usdcAsset, solAsset, "1", 50, q), f.user); err != nil {
		t.Fatal(err)
	}
	got, _, _ := calls(t, f.chain.sentTransactions()[0], f.chain)
	var programs []string
	for _, call := range got {
		programs = append(programs, call.program)
	}
	// No account is opened; the wrapped SOL is closed into the wallet.
	if want := []string{swapComputeBudget, swapComputeBudget, raydium.CLMMProgram.String(), tokenProgram}; strings.Join(programs, ",") != strings.Join(want, ",") {
		t.Fatalf("programs %v", programs)
	}
}

// Every read and simulation a quote depends on that fails fails the quote.
func TestQuoteSwapRPCFaults(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		method string
		call   int
	}{
		{"getSlot", 1},
		{"getBlockTime", 1},
		{"getMultipleAccounts", 1},
		{"getMinimumBalanceForRentExemption", 1},
		{"getRecentPrioritizationFees", 1},
		{"simulateTransaction", 1},
		{"simulateTransaction", 2},
	} {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			f.solToUSDC()
			f.chain.fault(tt.method, tt.call)
			_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
			if err == nil || !strings.Contains(err.Error(), "injected fault") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// Every read, simulation and broadcast step of a swap that fails stops it
// before anything is signed, or reports what happened after.
func TestSwapRPCFaults(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		method string
		call   int // counted after the quote
	}{
		{"getSlot", 1},
		{"getMultipleAccounts", 1},
		{"getRecentPrioritizationFees", 1},
		{"simulateTransaction", 1},
		{"getLatestBlockhash", 1},
		{"simulateTransaction", 2},
	} {
		t.Run(tt.method+"/"+string(rune('0'+tt.call)), func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			f.solToUSDC()
			q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
			if err != nil {
				t.Fatal(err)
			}
			f.chain.mu.Lock()
			f.chain.faults[tt.method] = f.chain.calls[tt.method] + tt.call
			f.chain.mu.Unlock()
			_, err = f.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(f, solAsset, usdcAsset, "0.01", 50, q), f.user)
			if err == nil || !strings.Contains(err.Error(), "injected fault") || f.user.signs.Load() != 0 {
				t.Fatalf("err = %v, %d signatures", err, f.user.signs.Load())
			}
		})
	}
}

// What the chain holds decides; anything that does not fit is refused.
func TestQuoteSwapRefusesChainState(t *testing.T) {
	t.Parallel()
	user := newTestSigner(9).address()
	tests := []struct {
		name    string
		setup   func(t *testing.T, f swapFixture)
		in, out chain.Asset
		amount  string
		want    string
	}{
		{"no SOL", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.set(user, ledgerAccount{owner: systemProgram})
		}, solAsset, usdcAsset, "0.01", "no SOL"},
		{"the mint has other decimals", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.set(usdcMint, ledgerAccount{owner: tokenProgram, lamports: 1, data: mintData(9, true)})
		}, solAsset, usdcAsset, "0.01", "decimals"},
		{"a Token-2022 intermediate", func(t *testing.T, f swapFixture) {
			f.usdtToSOL(t)
			f.chain.set(usdcMint, ledgerAccount{owner: token2022, lamports: 1, data: mintData(6, true)})
		}, usdtAsset, solAsset, "50", "intermediate"},
		{"no source account", func(t *testing.T, f swapFixture) {
			f.usdcToSOL(t)
			f.chain.mu.Lock()
			delete(f.chain.accounts, ata(user, usdcMint))
			f.chain.mu.Unlock()
		}, usdcAsset, solAsset, "1", "no USDC"},
		{"a frozen source", func(t *testing.T, f swapFixture) {
			f.usdcToSOL(t)
			f.chain.set(ata(user, usdcMint), ledgerAccount{owner: tokenProgram, lamports: rentExemptToken, data: tokenAccountData(usdcMint, user, 5_000_000, 2)})
		}, usdcAsset, solAsset, "1", "frozen"},
		{"a wrapped SOL account of another owner", func(t *testing.T, f swapFixture) {
			f.usdcToSOL(t)
			f.chain.set(ata(user, wsolMint), ledgerAccount{owner: tokenProgram, lamports: rentExemptToken, data: tokenAccountData(wsolMint, key(3), 0, 1)})
		}, usdcAsset, solAsset, "1", "wrapped SOL"},
		{"an uninitialized receiving account", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.set(ata(user, usdcMint), ledgerAccount{owner: tokenProgram, lamports: rentExemptToken, data: tokenAccountData(usdcMint, user, 0, 0)})
		}, solAsset, usdcAsset, "0.01", "receiving"},
		{"an uninitialized intermediate account", func(t *testing.T, f swapFixture) {
			f.usdtToSOL(t)
			f.chain.set(ata(user, usdcMint), ledgerAccount{owner: tokenProgram, lamports: rentExemptToken, data: tokenAccountData(usdcMint, user, 0, 0)})
		}, usdtAsset, solAsset, "50", "intermediate"},
		{"the extension of another pool", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.set(ext2Qdh, fixtureData(t, extBZtg))
		}, solAsset, usdcAsset, "0.01", "belongs to pool"},
		{"the extension of another pool, without the API's tick arrays", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.set(ext2Qdh, fixtureData(t, extBZtg))
			f.api.answer(http.StatusOK, quoteBody(wsolMint, usdcMint, 10_000_000, 1_043_581, 50, hopFixture{pool: clmmSOLUSDC, in: wsolMint, out: usdcMint}))
		}, solAsset, usdcAsset, "0.01", "belongs to pool"},
		{"a malformed extension", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			a := fixtureData(t, ext2Qdh)
			a.data = a.data[:100]
			f.chain.set(ext2Qdh, a)
		}, solAsset, usdcAsset, "0.01", "bitmap extension"},
		{"a pool that does not exist", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.mu.Lock()
			delete(f.chain.accounts, clmmSOLUSDC)
			f.chain.mu.Unlock()
		}, solAsset, usdcAsset, "0.01", "does not exist"},
		{"a malformed CLMM pool", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			a := fixtureData(t, clmmSOLUSDC)
			a.data = a.data[:1000]
			f.chain.set(clmmSOLUSDC, a)
		}, solAsset, usdcAsset, "0.01", "malformed"},
		{"a disabled AMM v4 pool", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, ammUSDTUSDC, ammCoinV, ammPcV)
			a := fixtureData(t, ammUSDTUSDC)
			a.data[0] = 2
			f.chain.set(ammUSDTUSDC, a)
		}, usdcAsset, usdtAsset, "1", "does not allow"},
		{"a malformed AMM v4 pool", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, ammUSDTUSDC, ammCoinV, ammPcV)
			a := fixtureData(t, ammUSDTUSDC)
			a.data = a.data[:700]
			f.chain.set(ammUSDTUSDC, a)
		}, usdcAsset, usdtAsset, "1", "malformed"},
		{"a CPMM pool with swaps disabled", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, cpmmUSDCUSDT, cpmmCfg, "79i6TxYg9GCC5ngcAL4i5jT5vtb8Dbg7Fimx6ixQNayM", "5YfaG4st68wWCVnsP473zhYg9C8Ff26F3pVAZ22DFCPh")
			a := fixtureData(t, cpmmUSDCUSDT)
			a.data[329] = 4
			f.chain.set(cpmmUSDCUSDT, a)
		}, usdcAsset, usdtAsset, "1", "does not allow"},
		{"a malformed CPMM pool", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, cpmmUSDCUSDT, cpmmCfg, "79i6TxYg9GCC5ngcAL4i5jT5vtb8Dbg7Fimx6ixQNayM", "5YfaG4st68wWCVnsP473zhYg9C8Ff26F3pVAZ22DFCPh")
			a := fixtureData(t, cpmmUSDCUSDT)
			a.data[0] ^= 1
			f.chain.set(cpmmUSDCUSDT, a)
		}, usdcAsset, usdtAsset, "1", "malformed"},
		{"a vault that is not a token account", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, ammUSDTUSDC, ammCoinV)
		}, usdcAsset, usdtAsset, "1", "not a token account)"},
		{"no liquidity in the direction", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			a := fixtureData(t, clmmSOLUSDC)
			clear(a.data[904:1032])
			f.chain.set(clmmSOLUSDC, a)
			e := fixtureData(t, ext2Qdh)
			clear(e.data[40:])
			f.chain.set(ext2Qdh, e)
			f.api.answer(http.StatusOK, quoteBody(wsolMint, usdcMint, 10_000_000, 1_043_581, 50, hopFixture{pool: clmmSOLUSDC, in: wsolMint, out: usdcMint}))
		}, solAsset, usdcAsset, "0.01", "no liquidity"},
		{"the node shows no payout", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.simAccounts = func([]string) []any {
				return []any{account(tokenProgram, rentExemptToken, tokenAccountData(usdcMint, user, 0, 1))}
			}
		}, solAsset, usdcAsset, "0.01", "would pay nothing"},
		{"the node shows no payout of the first pool", func(t *testing.T, f swapFixture) {
			f.usdtToSOL(t)
			f.chain.simAccounts = func([]string) []any {
				return []any{account(tokenProgram, rentExemptToken, tokenAccountData(usdcMint, user, 0, 1))}
			}
		}, usdtAsset, solAsset, "50", "first pool would pay nothing"},
		{"the first pool pays too little to go on", func(t *testing.T, f swapFixture) {
			f.usdtToSOL(t)
			f.chain.rate(clmmUSDCUSDT, usdtMint, 1, 50_000_000)
		}, usdtAsset, solAsset, "50", "too little"},
		{"the first hop fails", func(t *testing.T, f swapFixture) {
			f.usdtToSOL(t)
			f.chain.tokens(f.user.address(), usdtMint, 100_000_000)
			f.chain.mu.Lock()
			f.chain.simErr = "AccountInUse"
			f.chain.mu.Unlock()
		}, usdtAsset, solAsset, "50", "AccountInUse"},
		{"the minimum rounds to zero", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.rate(clmmSOLUSDC, wsolMint, 1, 10_000_000)
		}, solAsset, usdcAsset, "0.01", "too small"},
		{"not enough SOL for the swap", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			// The measure (no priority fee) passes, the swap at the offered
			// price does not: 5,151 + 1,488,440 + 10,000,000 = 11,493,591.
			f.chain.set(user, ledgerAccount{owner: systemProgram, lamports: 11_493_500})
		}, solAsset, usdcAsset, "0.01", "does not cover"},
		{"the node reports no compute", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.simUnits = 0
		}, solAsset, usdcAsset, "0.01", "compute"},
		{"the node answers other accounts", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.simAccounts = func([]string) []any { return []any{} }
		}, solAsset, usdcAsset, "0.01", "accounts for"},
		{"the final simulation pays below the minimum", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			calls := 0
			f.chain.simAccounts = func(addresses []string) []any {
				calls++
				amount := uint64(1_043_581)
				if calls > 1 {
					amount = 1_000_000
				}
				return []any{account(tokenProgram, rentExemptToken, tokenAccountData(usdcMint, user, amount, 1))}
			}
		}, solAsset, usdcAsset, "0.01", "less than the minimum"},
		{"the final simulation's account is another's", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			calls := 0
			f.chain.simAccounts = func(addresses []string) []any {
				calls++
				owner := user
				if calls > 1 {
					owner = key(3)
				}
				return []any{account(tokenProgram, rentExemptToken, tokenAccountData(usdcMint, owner, 1_043_581, 1))}
			}
		}, solAsset, usdcAsset, "0.01", "not the wallet's"},
		{"the final simulation's balance went down", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.tokens(user, usdcMint, 500)
			calls := 0
			f.chain.simAccounts = func(addresses []string) []any {
				calls++
				amount := uint64(1_044_081)
				if calls > 1 {
					amount = 100
				}
				return []any{account(tokenProgram, rentExemptToken, tokenAccountData(usdcMint, user, amount, 1))}
			}
		}, solAsset, usdcAsset, "0.01", "less than the minimum"},
		{"the final message needs more compute twice", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.finalUnits = 1_450_000
		}, solAsset, usdcAsset, "0.01", "compute units)"}, // the route's reason, after looking for other pools
		{"the raised limit is more than the SOL covers", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.finalUnits = 400_000
			f.chain.set(user, ledgerAccount{owner: systemProgram, lamports: 10_000_000 + rentExemptToken + 5_000 + 152})
		}, solAsset, usdcAsset, "0.01", "does not cover"},
		{"the raised limit's simulation fails", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.finalUnits = 400_000
			f.chain.fault("simulateTransaction", 3)
		}, solAsset, usdcAsset, "0.01", "injected fault"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			tt.setup(t, f)
			slippage := uint16(50)
			if tt.in == usdtAsset {
				slippage = 100
			}
			if tt.out == usdtAsset {
				slippage = 30
			}
			_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, tt.in, tt.out, tt.amount, slippage))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to say %q", err, tt.want)
			}
		})
	}
}

// A final message that needs more compute than the measure gets its limit
// raised once.
func TestQuoteSwapRaisesTheLimitOnce(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	f.chain.finalUnits = 160_000
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	// The final message runs out of its 151,000 units; the limit is raised
	// once to 151,000 + 30,200 + 1,000, which the 160,000 it needs fit.
	if q.ComputeUnitLimit != 182_200 {
		t.Fatalf("limit %d", q.ComputeUnitLimit)
	}
}

func TestQuoteSwapRefusesTheRequest(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	other := spl("solana-devnet", usdcMint, "USDC", 6)
	for name, tt := range map[string]struct {
		network string
		req     solanachain.SwapQuoteRequest
	}{
		"devnet":             {"solana-devnet", quoteRequest(f, native("solana-devnet"), spl("solana-devnet", usdcMint, "USDC", 6), "1", 50)},
		"a bad sender":       {"solana-mainnet", solanachain.SwapQuoteRequest{From: "x", In: solAsset, Out: usdcAsset, Amount: "1", SlippageBps: 50}},
		"no slippage":        {"solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "1", 0)},
		"another network":    {"solana-mainnet", quoteRequest(f, solAsset, other, "1", 50)},
		"input elsewhere":    {"solana-mainnet", quoteRequest(f, other, usdcAsset, "1", 50)},
		"the same asset":     {"solana-mainnet", quoteRequest(f, usdcAsset, usdcAsset, "1", 50)},
		"a bad amount":       {"solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "-1", 50)},
		"an unknown network": {"solana-nowhere", quoteRequest(f, solAsset, usdcAsset, "1", 50)},
	} {
		if _, err := f.adapter.QuoteSwap(t.Context(), tt.network, tt.req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A context that ends while the Trade API is asked is not a reason to
	// look for pools on chain.
	ctx, cancel := context.WithCancel(t.Context())
	f.api.mu.Lock()
	f.api.status = http.StatusBadGateway
	f.api.mu.Unlock()
	cancel()
	if _, err := f.adapter.QuoteSwap(ctx, "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50)); err == nil {
		t.Fatal("a cancelled quote succeeded")
	}
}

// Discovery: a failing node fails the quote; candidates that do not work are
// tried in turn and the first reason is kept.
func TestQuoteSwapDiscoveryFailures(t *testing.T) {
	t.Parallel()
	t.Run("getProgramAccounts fails", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t)
		f.solToUSDC()
		f.api.answer(http.StatusBadGateway, nil)
		f.chain.fault("getProgramAccounts", 1)
		if _, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50)); err == nil ||
			!strings.Contains(err.Error(), "find Raydium pools") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no pool", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t)
		f.solToUSDC()
		f.api.answer(http.StatusBadGateway, nil)
		if _, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50)); err == nil ||
			!strings.Contains(err.Error(), "no Raydium pool") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("every candidate fails", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t)
		f.solToUSDC()
		f.chain.pool(clmmSOLUSDC, "E2BcoCeJLTa27mAXDA4xwEq3pBUcyH6XXEHYk4KvKYTv", "4d35yC7C8zhCDec7JbPptL9SEb4NUddKHxURgmvD8hfo")
		f.chain.pool("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv", "4NFvUKqknMpoe6CWTzK758B8ojVLzURL5pC6MtiaJ8TQ",
			"4ct7br2vTPzfdmY3S5HLtTxcGSBfn6pnw98hsS6v359A", "5it83u57VRrVgc51oNV19TTmAJuffPx5GtGwQr7gQNUo")
		f.api.answer(http.StatusBadGateway, nil)
		f.chain.rate(clmmSOLUSDC, wsolMint, 0, 1)
		f.chain.rate("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv", wsolMint, 0, 1)
		_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
		if err == nil || !strings.Contains(err.Error(), "less than the minimum") {
			t.Fatalf("err = %v", err)
		}
		// Both pools were tried.
		if n := f.chain.node.count("simulateTransaction"); n != 2 {
			t.Fatalf("%d simulations", n)
		}
	})
}

func TestSwapRefusesTheRequest(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	good := swapRequest(f, solAsset, usdcAsset, "0.01", 50, q)
	failing := errors.New("disk full")
	for name, change := range map[string]func(*solanachain.SwapRequest){
		"a bad route":        func(r *solanachain.SwapRequest) { r.Route.Hops = nil },
		"a bad amount":       func(r *solanachain.SwapRequest) { r.Amount = "x" },
		"a bad minimum":      func(r *solanachain.SwapRequest) { r.MinOut = "0" },
		"devnet":             func(r *solanachain.SwapRequest) { r.SlippageBps = 0 },
		"assets swapped":     func(r *solanachain.SwapRequest) { r.In, r.Out = r.Out, r.In },
		"the same asset":     func(r *solanachain.SwapRequest) { r.Out = r.In },
		"an asset elsewhere": func(r *solanachain.SwapRequest) { r.Out = spl("solana-devnet", usdcMint, "USDC", 6) },
		"input elsewhere":    func(r *solanachain.SwapRequest) { r.In = native("solana-devnet") },
		"no expiry record":   func(r *solanachain.SwapRequest) { r.BeforeSigning = func(uint64) error { return failing } },
		"no broadcast record": func(r *solanachain.SwapRequest) {
			r.BeforeBroadcast = func(string) error { return failing }
		},
	} {
		req := good
		req.Route.Hops = append([]solanachain.SwapHop(nil), good.Route.Hops...)
		change(&req)
		if _, err := f.adapter.Swap(t.Context(), "solana-mainnet", req, f.user); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(f.chain.sentTransactions()) != 0 {
		t.Fatal("sent")
	}
	// A signer whose signature does not verify is not broadcast.
	corrupt := newTestSigner(9)
	corrupt.corrupt = true
	if _, err := f.adapter.Swap(t.Context(), "solana-mainnet", good, corrupt); err == nil || len(f.chain.sentTransactions()) != 0 {
		t.Fatalf("a bad signature: %v", err)
	}
	// A swap on devnet is refused before anything is read.
	if _, err := f.adapter.Swap(t.Context(), "solana-devnet", good, f.user); err == nil {
		t.Fatal("devnet: accepted")
	}
	// A lookup table the route names that is gone is a change of state.
	two := newSwapFixture(t)
	two.usdtToSOL(t)
	q2, err := two.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(two, usdtAsset, solAsset, "50", 100))
	if err != nil {
		t.Fatal(err)
	}
	table := fixtureData(t, lutBZtg)
	table.data[4] = 0 // deactivated
	two.chain.set(lutBZtg, table)
	if _, err := two.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(two, usdtAsset, solAsset, "50", 100, q2), two.user); !errors.Is(err, chain.ErrStateChanged) {
		t.Fatalf("deactivated table: %v", err)
	}
	two.chain.mu.Lock()
	delete(two.chain.accounts, lutBZtg)
	delete(two.chain.accounts, lutCLMM)
	two.chain.mu.Unlock()
	if _, err := two.adapter.Swap(t.Context(), "solana-mainnet", swapRequest(two, usdtAsset, solAsset, "50", 100, q2), two.user); !errors.Is(err, chain.ErrStateChanged) {
		t.Fatalf("missing table: %v", err)
	}
	// Without its tables the two-hop swap does not fit a transaction.
	if _, err := two.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(two, usdtAsset, solAsset, "50", 100)); err == nil ||
		!strings.Contains(err.Error(), "more than one transaction holds") {
		t.Fatalf("too large: %v", err)
	}
}

// The Raydium API is reached through the guarded client the endpoint
// provider hands out — the same one, with the same address policy, as the
// network's RPC — and the quoter is built once.
func TestTheDefaultQuoterUsesTheProvidersClient(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.chain.pool(clmmSOLUSDC, ext2Qdh, "E2BcoCeJLTa27mAXDA4xwEq3pBUcyH6XXEHYk4KvKYTv", "4d35yC7C8zhCDec7JbPptL9SEb4NUddKHxURgmvD8hfo")
	f.chain.rate(clmmSOLUSDC, wsolMint, 1_043_581, 10_000_000)
	f.chain.tokens(f.user.address(), usdcMint, 0)
	nodeClient := f.chain.node.server.Client()
	var mu sync.Mutex
	var hosts []string
	guarded := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "transaction-v1.raydium.io" {
			mu.Lock()
			hosts = append(hosts, r.URL.Host)
			mu.Unlock()
			return nil, errors.New("refused by the address policy")
		}
		return nodeClient.Transport.RoundTrip(r)
	})}
	adapter := newAdapter(t, &provider{endpoints: []string{f.chain.node.server.URL}, client: guarded})
	q, err := adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if q.Source != solanachain.SwapSourceDiscovery || len(hosts) != 1 {
		t.Fatalf("source %s, Trade API requests through the provider's client %v", q.Source, hosts)
	}
	first, err := solanachain.RaydiumQuoter(adapter, mainnet())
	if err != nil {
		t.Fatal(err)
	}
	second, err := solanachain.RaydiumQuoter(adapter, mainnet())
	if err != nil || first != second {
		t.Fatalf("quoters %p %p, %v", first, second, err)
	}
	// A provider without a guarded client gets no quoter.
	bare := newAdapter(t, &provider{endpoints: []string{f.chain.node.server.URL}, client: &http.Client{}})
	if _, err := solanachain.RaydiumQuoter(bare, mainnet()); err == nil {
		t.Fatal("a quoter over an unguarded client")
	}
}

type lockedSigner struct{ *testSigner }

func (lockedSigner) SignMessage(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("the key is locked")
}

type tronFamily struct{ *testSigner }

func (tronFamily) Family() chain.Family { return chain.FamilyTron }

// More odd answers and edges of the quote.
func TestQuoteSwapOddAnswers(t *testing.T) {
	t.Parallel()
	user := newTestSigner(9).address()
	for _, tt := range []struct {
		name   string
		setup  func(t *testing.T, f swapFixture)
		in     chain.Asset
		out    chain.Asset
		amount string
		want   string
	}{
		{"a head block without time", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.blockTimeNull = true
		}, solAsset, usdcAsset, "0.01", "no time"},
		{"a simulation without a result", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.simNull = true
		}, solAsset, usdcAsset, "0.01", "no result"},
		{"the first vault is not a token account", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, ammUSDTUSDC, ammPcV)
		}, usdcAsset, usdtAsset, "1", "not a token account)"},
		{"the vaults cannot be read", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, ammUSDTUSDC, ammCoinV, ammPcV)
			f.chain.fault("getMultipleAccounts", 2)
		}, usdcAsset, usdtAsset, "1", "injected fault"},
		{"the first hop's simulation fails", func(t *testing.T, f swapFixture) {
			f.usdtToSOL(t)
			f.chain.fault("simulateTransaction", 1)
		}, usdtAsset, solAsset, "50", "injected fault"},
		{"the first hop pays into another's account", func(t *testing.T, f swapFixture) {
			f.usdtToSOL(t)
			f.chain.simAccounts = func([]string) []any {
				return []any{account(tokenProgram, rentExemptToken, tokenAccountData(usdcMint, key(3), 5, 1))}
			}
		}, usdtAsset, solAsset, "50", "not the wallet's"},
		{"the swap pays into another's account", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.simAccounts = func([]string) []any {
				return []any{account(tokenProgram, rentExemptToken, tokenAccountData(usdcMint, key(3), 5, 1))}
			}
		}, solAsset, usdcAsset, "0.01", "not the wallet's"},
		{"the current array needs the missing extension", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			a := fixtureData(t, clmmSOLUSDC)
			binary.LittleEndian.PutUint32(a.data[269:], 400_000) // tick 400,000: beyond the pool's own bitmap
			f.chain.set(clmmSOLUSDC, a)
			f.chain.mu.Lock()
			delete(f.chain.accounts, ext2Qdh)
			f.chain.mu.Unlock()
			f.api.answer(http.StatusOK, quoteBody(wsolMint, usdcMint, 10_000_000, 1_043_581, 50, hopFixture{pool: clmmSOLUSDC, in: wsolMint, out: usdcMint}))
		}, solAsset, usdcAsset, "0.01", "extension is needed"},
		{"discovery reads fail", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.chain.pool(clmmSOLUSDC, "E2BcoCeJLTa27mAXDA4xwEq3pBUcyH6XXEHYk4KvKYTv", "4d35yC7C8zhCDec7JbPptL9SEb4NUddKHxURgmvD8hfo")
			f.api.answer(http.StatusBadGateway, nil)
			f.chain.fault("getMultipleAccounts", 1)
		}, solAsset, usdcAsset, "0.01", "injected fault"},
		{"discovery answered by another program", func(t *testing.T, f swapFixture) {
			f.solToUSDC()
			f.api.answer(http.StatusBadGateway, nil)
			f.chain.gpaOwner = systemProgram
		}, solAsset, usdcAsset, "0.01", "another program"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			_ = user
			tt.setup(t, f)
			slippage := uint16(50)
			if tt.in == usdtAsset {
				slippage = 100
			}
			if tt.out == usdtAsset {
				slippage = 30
			}
			_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, tt.in, tt.out, tt.amount, slippage))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to say %q", err, tt.want)
			}
		})
	}
}

func TestQuoteSwapDropsAnUnusableTable(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.usdtToSOL(t)
	table := fixtureData(t, lutCLMM)
	table.data[4] = 0 // deactivated
	f.chain.set(lutCLMM, table)
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdtAsset, solAsset, "50", 100))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(q.Route.LookupTables, ",") != lutBZtg || q.MessageVersion != "v0" {
		t.Fatalf("tables %v version %s", q.Route.LookupTables, q.MessageVersion)
	}
}

func TestQuoteSwapOnDevnetIsRefused(t *testing.T) {
	t.Parallel()
	n := newNode(t, devnetGenesis)
	adapter, _ := single(t, n)
	f := newSwapFixture(t)
	_, err := adapter.QuoteSwap(t.Context(), "solana-devnet", quoteRequest(f, native("solana-devnet"), spl("solana-devnet", usdcMint, "USDC", 6), "1", 50))
	if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "mainnet only") {
		t.Fatalf("err = %v", err)
	}
}

// A quote whose caller leaves stops instead of searching on.
func TestQuoteSwapStopsWhenTheCallerLeaves(t *testing.T) {
	t.Parallel()
	t.Run("during the Trade API request", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t)
		f.solToUSDC()
		ctx, cancel := context.WithCancel(t.Context())
		quoter, err := raydium.NewQuoter(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			cancel()
			return nil, context.Canceled
		})}, "https://quote.test")
		if err != nil {
			t.Fatal(err)
		}
		f.adapter.UseRaydiumQuoter(quoter)
		if _, err := f.adapter.QuoteSwap(ctx, "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50)); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
		if n := f.chain.node.count("getProgramAccounts"); n != 0 {
			t.Fatalf("searched on chain %d times", n)
		}
	})
	t.Run("while a discovered pool is tried", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t)
		f.solToUSDC()
		f.chain.pool(clmmSOLUSDC, "E2BcoCeJLTa27mAXDA4xwEq3pBUcyH6XXEHYk4KvKYTv", "4d35yC7C8zhCDec7JbPptL9SEb4NUddKHxURgmvD8hfo")
		f.api.answer(http.StatusBadGateway, nil)
		ctx, cancel := context.WithCancel(t.Context())
		f.chain.simAccounts = func([]string) []any {
			cancel()
			return []any{}
		}
		if _, err := f.adapter.QuoteSwap(ctx, "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50)); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// More odd answers and edges of the swap.
func TestSwapOddAnswers(t *testing.T) {
	t.Parallel()
	user := newTestSigner(9).address()
	tests := []struct {
		name   string
		change func(f swapFixture, req *solanachain.SwapRequest)
		key    func(f swapFixture) chain.Key
		want   string
	}{
		{"a blockhash without an expiry", func(f swapFixture, _ *solanachain.SwapRequest) {
			f.chain.mu.Lock()
			defer f.chain.mu.Unlock()
			f.chain.blockhashZero = true
		}, nil, "no blockhash"},
		{"the final message needs more compute than approved", func(f swapFixture, _ *solanachain.SwapRequest) {
			f.chain.mu.Lock()
			defer f.chain.mu.Unlock()
			f.chain.finalUnits = 160_000
		}, nil, "fee estimate changed"},
		{"the SOL no longer covers the swap", func(f swapFixture, _ *solanachain.SwapRequest) {
			f.chain.set(user, ledgerAccount{owner: systemProgram, lamports: 11_493_500})
		}, nil, "does not cover"},
		{"a lower rent was approved", func(f swapFixture, req *solanachain.SwapRequest) {
			f.chain.mu.Lock()
			delete(f.chain.accounts, ata(user, usdcMint))
			f.chain.mu.Unlock()
			req.Approved.TokenAccountsCreated = 1
			req.Approved.TokenAccountRent = "0.001"
			req.Approved.Fee = "0.001005151"
		}, nil, "costs"},
		{"a builder that fails", func(f swapFixture, _ *solanachain.SwapRequest) {
			solanachain.TamperWithSwap(f.adapter, func(_ []raydium.Instruction, tables map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) {
				return nil, tables
			})
		}, nil, "build"},
		{"a key of another family", nil, func(f swapFixture) chain.Key { return tronFamily{f.user} }, "not a Solana key"},
		{"a key that cannot sign", nil, func(f swapFixture) chain.Key { return lockedSigner{f.user} }, "locked"},
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
			if tt.change != nil {
				tt.change(f, &req)
			}
			var key chain.Key = f.user
			if tt.key != nil {
				key = tt.key(f)
			}
			_, err = f.adapter.Swap(t.Context(), "solana-mainnet", req, key)
			if err == nil || !strings.Contains(err.Error(), tt.want) || len(f.chain.sentTransactions()) != 0 {
				t.Fatalf("err = %v, want it to say %q", err, tt.want)
			}
		})
	}
}

// When the Trade API answers with a route that cannot be used — its pools,
// tables, size or quoted output do not hold up on chain — the quote looks for
// the pools on chain and says why the API's route was dropped (review
// decision B1, refining O11: the fallback is for "no usable route from the
// API", not only "API down").
func TestQuoteSwapFallsBackWhenTheAPIRouteIsUnusable(t *testing.T) {
	t.Parallel()
	ammVaults := []string{ammCoinV, ammPcV}
	cpmmVaults := []string{"79i6TxYg9GCC5ngcAL4i5jT5vtb8Dbg7Fimx6ixQNayM", "5YfaG4st68wWCVnsP473zhYg9C8Ff26F3pVAZ22DFCPh"}
	for _, tt := range []struct {
		name   string
		setup  func(t *testing.T, f swapFixture)
		reason string
	}{
		{"a quote far off the pool's curve", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, ammUSDTUSDC, ammVaults...)
			out := localOut(t, ammUSDTUSDC, usdcMint, 1_000_000)
			f.api.answer(http.StatusOK, quoteBody(usdcMint, usdtMint, 1_000_000, out+out/100, 30, hopFixture{pool: ammUSDTUSDC, in: usdcMint, out: usdtMint}))
		}, "the pool's curve"},
		{"tick arrays for an AMM v4 pool", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, ammUSDTUSDC, ammVaults...)
			out := localOut(t, ammUSDTUSDC, usdcMint, 1_000_000)
			f.api.answer(http.StatusOK, quoteBody(usdcMint, usdtMint, 1_000_000, out, 30,
				hopFixture{pool: ammUSDTUSDC, in: usdcMint, out: usdtMint, remaining: []string{key(5)}}))
		}, "tick arrays for a amm-v4 pool"},
		{"the CPMM config is gone", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, cpmmUSDCUSDT, cpmmVaults...)
		}, "config does not exist"},
		{"the CPMM config is malformed", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, cpmmUSDCUSDT, cpmmVaults...)
			f.chain.set(cpmmCfg, ledgerAccount{owner: raydium.CPMMProgram.String(), lamports: 1, data: make([]byte, 236)})
		}, "malformed"},
		{"a pool the curve says is empty", func(t *testing.T, f swapFixture) {
			f.usdcToUSDT(t, ammUSDTUSDC, ammVaults...)
			f.chain.set(ammPcV, setTokenAmount(fixtureData(t, ammPcV), 0)) // the USDC side
		}, "does not allow"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			tt.setup(t, f)
			q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdcAsset, usdtAsset, "1", 30))
			if err != nil {
				t.Fatal(err)
			}
			if q.Source != solanachain.SwapSourceDiscovery || !strings.Contains(q.APIRouteRejected, tt.reason) {
				t.Fatalf("source %s, API route rejected for %q, want %q", q.Source, q.APIRouteRejected, tt.reason)
			}
		})
	}
}

// A client route through a token other than SOL, USDC or USDT is refused at
// swap time too (review A1), before anything is read or signed.
func TestSwapRefusesARouteThroughAnotherToken(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err != nil {
		t.Fatal(err)
	}
	req := swapRequest(f, solAsset, usdcAsset, "0.01", 50, q)
	exotic := key(91)
	req.Route = solanachain.SwapRoute{Hops: []solanachain.SwapHop{
		{Pool: clmmSOLUSDC, Kind: "clmm", InputMint: wsolMint, OutputMint: exotic},
		{Pool: clmmUSDCUSDT, Kind: "clmm", InputMint: exotic, OutputMint: usdcMint},
	}}
	reads := f.chain.node.count("getMultipleAccounts")
	_, err = f.adapter.Swap(t.Context(), "solana-mainnet", req, f.user)
	if !errors.Is(err, chain.ErrInvalidRequest) || !strings.Contains(err.Error(), "SOL, USDC or USDT") {
		t.Fatalf("err = %v", err)
	}
	if f.user.signs.Load() != 0 || len(f.chain.sentTransactions()) != 0 || f.chain.node.count("getMultipleAccounts") != reads {
		t.Fatal("the route was read, signed or sent")
	}
}

// Refusals of the wallet's own do not send the quote looking for other
// pools (review B1): they would follow it to any route.
func TestQuoteSwapDoesNotFallBackForTheWalletsOwnRefusals(t *testing.T) {
	t.Parallel()
	user := newTestSigner(9).address()
	for _, tt := range []struct {
		name    string
		setup   func(f swapFixture)
		in, out chain.Asset
		amount  string
		want    string
	}{
		{"a Token-2022 mint", func(f swapFixture) {
			f.solToUSDC()
			f.chain.set(usdcMint, ledgerAccount{owner: token2022, lamports: 1, data: mintData(6, true)})
		}, solAsset, usdcAsset, "0.01", "Token-2022"},
		{"not enough to sell", func(f swapFixture) {
			f.usdcToSOL(t)
			f.chain.tokens(user, usdcMint, 10)
		}, usdcAsset, solAsset, "1", "main token account holds"},
		{"not enough SOL", func(f swapFixture) {
			f.solToUSDC()
			// One lamport short of 5,151 + 1,488,440 + 10,000,000.
			f.chain.set(user, ledgerAccount{owner: systemProgram, lamports: 11_493_590})
		}, solAsset, usdcAsset, "0.01", "does not cover"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			tt.setup(f)
			_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, tt.in, tt.out, tt.amount, 50))
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "no Raydium pool") {
				t.Fatalf("err = %v", err)
			}
			if n := f.chain.node.count("getProgramAccounts"); n != 0 {
				t.Fatalf("looked for pools %d times", n)
			}
		})
	}
}

// A route that needs its lookup tables, when the pool-keys API fails, falls
// back to the pools on chain and the reason names the tables (review B1).
func TestQuoteSwapNamesTheMissingLookupTables(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.usdtToSOL(t)
	f.api.mu.Lock()
	f.api.keysStatus = http.StatusBadGateway
	f.api.mu.Unlock()
	_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdtAsset, solAsset, "50", 100))
	// No USDT/SOL pool is on the fake chain for discovery.
	if err == nil || !strings.Contains(err.Error(), "more than one transaction holds") || !strings.Contains(err.Error(), "lookup tables") {
		t.Fatalf("err = %v", err)
	}
	// With a pool to find, the quote is the pool's and says why.
	f.chain.pool("3nMFwZXwY1s1M5s8vYAHqd4wGs4iSxXE4LRoUMMYqEgF", "2ncinnTcJxbZ1nUHavBVJ3Ap3R4CE7p2LJ6Jtpd1vLzd",
		"AbcuyoPeYnddzFoFQudsiFka8qd6tTwvLgxwtpTKTpKC", "2n6fxuD6PA5NYgEnXXYMh2iWD1JBJ1LGf76kFJAayZmX")
	f.chain.rate("3nMFwZXwY1s1M5s8vYAHqd4wGs4iSxXE4LRoUMMYqEgF", usdtMint, 9_569, 1_000_000)
	q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdtAsset, solAsset, "50", 100))
	if err != nil {
		t.Fatal(err)
	}
	if q.Source != solanachain.SwapSourceDiscovery || !strings.Contains(q.APIRouteRejected, "lookup tables") {
		t.Fatalf("quote %+v", q)
	}
}

// The quoter follows the network's settings: after Invalidate it is built
// again over the provider's new client (review B2), and not before.
func TestTheQuoterIsRebuiltWhenTheSettingsChange(t *testing.T) {
	t.Parallel()
	n := newNode(t, mainnetGenesis)
	var built int
	p := &countingProvider{provider: provider{endpoints: []string{n.server.URL}, client: n.server.Client()}, built: &built}
	adapter, err := solanachain.New(networks(), p)
	if err != nil {
		t.Fatal(err)
	}
	first, err := solanachain.RaydiumQuoter(adapter, mainnet())
	if err != nil {
		t.Fatal(err)
	}
	again, err := solanachain.RaydiumQuoter(adapter, mainnet())
	if err != nil || again != first || built != 1 {
		t.Fatalf("quoter rebuilt without a settings change: %d clients", built)
	}
	adapter.Invalidate("solana-mainnet")
	rebuilt, err := solanachain.RaydiumQuoter(adapter, mainnet())
	if err != nil || rebuilt == first || built != 2 {
		t.Fatalf("quoter not rebuilt after Invalidate: %d clients", built)
	}
	// A quoter given to the adapter stays.
	given, _ := raydium.NewQuoter(n.server.Client(), "https://quote.test")
	adapter.UseRaydiumQuoter(given)
	adapter.Invalidate("solana-mainnet")
	if got, err := solanachain.RaydiumQuoter(adapter, mainnet()); err != nil || got != given {
		t.Fatal("the given quoter was replaced")
	}
}

// countingProvider counts the clients it hands out.
type countingProvider struct {
	provider
	built *int
}

func (p *countingProvider) HTTPClient(item network.Network) *http.Client {
	*p.built++
	return &http.Client{Transport: p.provider.client.Transport}
}

// In discovery too, a refusal of the wallet's own ends the search at the
// first candidate.
func TestDiscoveryStopsAtTheWalletsOwnRefusal(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	f.chain.pool(clmmSOLUSDC, "E2BcoCeJLTa27mAXDA4xwEq3pBUcyH6XXEHYk4KvKYTv", "4d35yC7C8zhCDec7JbPptL9SEb4NUddKHxURgmvD8hfo")
	f.chain.pool("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv", "4NFvUKqknMpoe6CWTzK758B8ojVLzURL5pC6MtiaJ8TQ",
		"4ct7br2vTPzfdmY3S5HLtTxcGSBfn6pnw98hsS6v359A", "5it83u57VRrVgc51oNV19TTmAJuffPx5GtGwQr7gQNUo")
	f.chain.rate("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv", wsolMint, 1_043_581, 10_000_000)
	f.api.answer(http.StatusBadGateway, nil)
	f.chain.set(f.user.address(), ledgerAccount{owner: systemProgram, lamports: 11_493_500})
	_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("err = %v", err)
	}
	// One candidate measured (one simulation), not both.
	if n := f.chain.node.count("simulateTransaction"); n != 1 {
		t.Fatalf("%d simulations", n)
	}
}

// What only the API's route needs from the wallet's SOL — the rent of an
// account for its intermediate token, here wrapped SOL — is the route's
// problem, not the wallet's: with 0.001 SOL a USDC → SOL → USDT route cannot
// open its wrapped-SOL account, and the direct USDC/USDT pool, which needs
// only the fee, is quoted instead (review round 2, item 1).
func TestQuoteSwapFallsBackWhenOnlyTheRouteNeedsMoreSOL(t *testing.T) {
	t.Parallel()
	const sol3nMF = "3nMFwZXwY1s1M5s8vYAHqd4wGs4iSxXE4LRoUMMYqEgF"
	for _, tt := range []struct {
		name     string
		lamports uint64
		reason   string
	}{
		// The measuring simulation already cannot open the account.
		{"the account cannot be opened", 1_000_000, "network would reject"},
		// The measure (no priority fee) can; the swap at the offered price
		// cannot: 5,000 + 1,488,440 + 100 < 5,000 + ⌈limit × 1,000 / 10⁶⌉ + 1,488,440.
		{"the fee and the account's rent are not covered", 5_000 + rentExemptToken + 100, "does not cover"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newSwapFixture(t)
			f.chain.pool(clmmSOLUSDC, ext2Qdh)
			f.chain.pool(sol3nMF, "2ncinnTcJxbZ1nUHavBVJ3Ap3R4CE7p2LJ6Jtpd1vLzd")
			f.chain.rate(clmmSOLUSDC, usdcMint, 9_578_443, 1_000_000)
			f.chain.rate(sol3nMF, wsolMint, 104_000_000, 1_000_000_000)
			f.usdcToUSDT(t, ammUSDTUSDC, ammCoinV, ammPcV) // the direct pool, for discovery
			f.chain.tokens(f.user.address(), usdtMint, 0)
			f.chain.set(f.user.address(), ledgerAccount{owner: systemProgram, lamports: tt.lamports})
			f.api.answer(http.StatusOK, quoteBody(usdcMint, usdtMint, 1_000_000, 996_000, 30,
				hopFixture{pool: clmmSOLUSDC, in: usdcMint, out: wsolMint, remaining: firstTickArrays(t, clmmSOLUSDC, ext2Qdh, usdcMint, 1)},
				hopFixture{pool: sol3nMF, in: wsolMint, out: usdtMint, remaining: firstTickArrays(t, sol3nMF, "2ncinnTcJxbZ1nUHavBVJ3Ap3R4CE7p2LJ6Jtpd1vLzd", wsolMint, 1)},
			))
			q, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdcAsset, usdtAsset, "1", 30))
			if err != nil {
				t.Fatal(err)
			}
			if q.Source != solanachain.SwapSourceDiscovery || q.Route.Hops[0].Pool != ammUSDTUSDC || !strings.Contains(q.APIRouteRejected, tt.reason) {
				t.Fatalf("quote %+v", q)
			}
		})
	}
}

// A frozen intermediate account is the route's, not the wallet's.
func TestAFrozenIntermediateAccountIsTheRoutes(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.usdtToSOL(t)
	f.chain.set(ata(f.user.address(), usdcMint), ledgerAccount{
		owner: tokenProgram, lamports: rentExemptToken,
		data: tokenAccountData(usdcMint, f.user.address(), 0, 2),
	})
	_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdtAsset, solAsset, "50", 100))
	// No USDT/SOL pool is on the fake chain: the answer is "no pool" with the
	// route's reason, after looking.
	if err == nil || !strings.Contains(err.Error(), "no Raydium pool") || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("err = %v", err)
	}
}

// When every pool found on chain fails too, the API's reason is kept, and a
// route the hop rules refuse does not cost a pool-keys request (round 2,
// items 2 and 3).
func TestTheAPIsReasonOutlivesTheCandidatesAndCostsNoPoolKeys(t *testing.T) {
	t.Parallel()
	f := newSwapFixture(t)
	f.solToUSDC()
	f.chain.pool(clmmSOLUSDC, "E2BcoCeJLTa27mAXDA4xwEq3pBUcyH6XXEHYk4KvKYTv", "4d35yC7C8zhCDec7JbPptL9SEb4NUddKHxURgmvD8hfo")
	f.chain.rate(clmmSOLUSDC, wsolMint, 0, 1)
	exotic := key(92)
	f.api.answer(http.StatusOK, quoteBody(wsolMint, usdcMint, 10_000_000, 1_043_581, 50,
		hopFixture{pool: clmmSOLUSDC, in: wsolMint, out: exotic},
		hopFixture{pool: clmmUSDCUSDT, in: exotic, out: usdcMint}))
	_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, solAsset, usdcAsset, "0.01", 50))
	if err == nil || !strings.Contains(err.Error(), "less than the minimum") || !strings.Contains(err.Error(), "(the Trade API:") ||
		!strings.Contains(err.Error(), "SOL, USDC or USDT") {
		t.Fatalf("err = %v", err)
	}
	if _, keys := f.api.counts(); keys != 0 {
		t.Fatalf("%d pool-keys requests for a route the hop rules refuse", keys)
	}
}

// The receiving account is the wallet's own on any route: a two-hop route
// that cannot open it for want of SOL is refused as the wallet's, without
// looking for other pools.
func TestTheReceivingAccountIsTheWalletsOnATwoHopRoute(t *testing.T) {
	t.Parallel()
	const sol3nMF = "3nMFwZXwY1s1M5s8vYAHqd4wGs4iSxXE4LRoUMMYqEgF"
	f := newSwapFixture(t)
	f.chain.pool(clmmSOLUSDC, ext2Qdh)
	f.chain.pool(sol3nMF, "2ncinnTcJxbZ1nUHavBVJ3Ap3R4CE7p2LJ6Jtpd1vLzd")
	f.chain.rate(clmmSOLUSDC, usdcMint, 9_578_443, 1_000_000)
	f.chain.rate(sol3nMF, wsolMint, 104_000_000, 1_000_000_000)
	f.usdcToUSDT(t, ammUSDTUSDC, ammCoinV, ammPcV)
	// The intermediate wrapped-SOL account exists; the USDT account does
	// not, and 0.001 SOL does not pay its rent.
	f.chain.set(ata(f.user.address(), wsolMint), ledgerAccount{
		owner: tokenProgram, lamports: rentExemptToken,
		data: tokenAccountData(wsolMint, f.user.address(), 0, 1),
	})
	f.chain.set(f.user.address(), ledgerAccount{owner: systemProgram, lamports: 1_000_000})
	f.api.answer(http.StatusOK, quoteBody(usdcMint, usdtMint, 1_000_000, 996_000, 30,
		hopFixture{pool: clmmSOLUSDC, in: usdcMint, out: wsolMint, remaining: firstTickArrays(t, clmmSOLUSDC, ext2Qdh, usdcMint, 1)},
		hopFixture{pool: sol3nMF, in: wsolMint, out: usdtMint, remaining: firstTickArrays(t, sol3nMF, "2ncinnTcJxbZ1nUHavBVJ3Ap3R4CE7p2LJ6Jtpd1vLzd", wsolMint, 1)},
	))
	_, err := f.adapter.QuoteSwap(t.Context(), "solana-mainnet", quoteRequest(f, usdcAsset, usdtAsset, "1", 30))
	if err == nil || strings.Contains(err.Error(), "no Raydium pool") {
		t.Fatalf("err = %v", err)
	}
	if n := f.chain.node.count("getProgramAccounts"); n != 0 {
		t.Fatalf("looked for pools %d times", n)
	}
}
