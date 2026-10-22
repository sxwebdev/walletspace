package solana_test

import (
	"os"
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/rpcpool"
)

// liveSwapWallet is a public mainnet wallet that holds SOL, USDC and USDT in
// its associated token accounts. Quotes only simulate — without signatures —
// so nothing is signed or sent on its behalf.
const liveSwapWallet = "2ojv9BAiHUrvsm9gxDe7fJSzbNZSJcxZvf8dqmWGHG8S"

// TestRaydiumSwapQuotesLive quotes three swaps against mainnet through the
// Trade API and the public RPC: each is built from the pools' state and its
// final message simulated. About 25 RPC requests and 6 API requests.
func TestRaydiumSwapQuotesLive(t *testing.T) {
	if os.Getenv("WALLETSPACE_LIVE_SOLANA_TESTS") != "1" {
		t.Skip("set WALLETSPACE_LIVE_SOLANA_TESTS=1 to use the public Solana RPC and the Raydium API")
	}
	settings, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := solanachain.New(liveNetworks(t), rpcpool.New(settings))
	if err != nil {
		t.Fatal(err)
	}
	usdc := spl("solana-mainnet", usdcMint, "USDC", 6)
	usdt := spl("solana-mainnet", usdtMint, "USDT", 6)
	sol := native("solana-mainnet")
	for _, tt := range []struct {
		name    string
		in, out chain.Asset
		amount  string
	}{
		{"SOL for USDC", sol, usdc, "0.01"},
		{"USDC for USDT", usdc, usdt, "1"},
		{"USDT for SOL", usdt, sol, "1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q, err := adapter.QuoteSwap(t.Context(), "solana-mainnet", solanachain.SwapQuoteRequest{
				From: liveSwapWallet, In: tt.in, Out: tt.out, Amount: tt.amount, SlippageBps: 50,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%+v", q)
		})
	}
}
