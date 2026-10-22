package solana_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/rpcpool"
)

// liveNetworks are the built-in Solana networks.
func liveNetworks(t *testing.T) registry {
	t.Helper()
	builtin, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	out := registry{}
	for _, item := range builtin.List() {
		if item.Family == solanachain.Family {
			out[item.ID] = item
		}
	}
	return out
}

// TestPublicSolanaLive reads both public clusters through the real resolver:
// identity, health, a balance, token metadata and a status lookup. About a
// dozen requests per cluster, well inside the public limits.
func TestPublicSolanaLive(t *testing.T) {
	if os.Getenv("WALLETSPACE_LIVE_SOLANA_TESTS") != "1" {
		t.Skip("set WALLETSPACE_LIVE_SOLANA_TESTS=1 to use the public Solana RPC")
	}

	settings, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	nets := liveNetworks(t)
	adapter, err := solanachain.New(nets, rpcpool.New(settings))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	resolver := rpcpool.New(settings)

	for _, networkID := range []string{"solana-mainnet", "solana-devnet"} {
		t.Run(networkID, func(t *testing.T) {
			item := nets[networkID]
			endpoints, err := resolver.Endpoints(t.Context(), item)
			if err != nil || len(endpoints) == 0 {
				t.Fatalf("Endpoints() = %v, %v", endpoints, err)
			}
			if err := adapter.ProbeEndpoint(t.Context(), item, endpoints[0], nil); err != nil {
				t.Fatalf("ProbeEndpoint(%s) error = %v", endpoints[0], err)
			}

			// The metadata program's account always holds lamports. (The token
			// program itself cannot be the holder: devnet refuses token
			// listings keyed by it.)
			holder := chain.AccountAddress{AccountID: "metadata-program", Address: metaplex}
			assets := []chain.Asset{native(networkID), spl(networkID, usdcMint, "USDC", 6)}
			for _, balance := range collect(adapter.BalanceStream(t.Context(), networkID, []chain.AccountAddress{holder}, assets, true)) {
				if balance.Error != "" {
					t.Errorf("balance %s error = %s", balance.AssetID, balance.Error)
					continue
				}
				amount, err := decimal.NewFromString(balance.Amount)
				if err != nil || amount.IsNegative() || (balance.AssetID == networkID+":native" && !amount.IsPositive()) {
					t.Errorf("balance %s = %q", balance.AssetID, balance.Amount)
				}
				t.Logf("balance %s = %s", balance.AssetID, balance.Amount)
			}

			// A signature nobody sent is pending, not an error.
			if got, err := adapter.Transaction(t.Context(), networkID, signature); err != nil || got.Status != "pending" {
				t.Errorf("Transaction(unknown) = %+v, %v", got, err)
			}
		})
	}

	t.Run("mainnet metadata", func(t *testing.T) {
		for mint, want := range map[string]string{usdcMint: "USDC", usdtMint: "USDT"} {
			symbol, decimals, err := adapter.TokenMetadata(t.Context(), "solana-mainnet", mint)
			if err != nil || symbol != want || decimals != 6 {
				t.Errorf("TokenMetadata(%s) = %q, %d, %v, want %s, 6", mint, symbol, decimals, err, want)
			}
		}
		// PayPal USD is a Token-2022 mint: refusing it proves the program id.
		const pyusd = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"
		if _, _, err := adapter.TokenMetadata(t.Context(), "solana-mainnet", pyusd); !errors.Is(err, solanachain.ErrToken2022) {
			t.Errorf("TokenMetadata(PYUSD) error = %v, want ErrToken2022", err)
		}
	})

	t.Run("devnet USDC", func(t *testing.T) {
		const devnetUSDC = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
		symbol, decimals, err := adapter.TokenMetadata(t.Context(), "solana-devnet", devnetUSDC)
		if errors.Is(err, solanachain.ErrNoSymbol) {
			t.Logf("devnet USDC has no metadata; decimals cannot be read through TokenMetadata")
			return
		}
		if err != nil || decimals != 6 {
			t.Errorf("TokenMetadata(devnet USDC) = %q, %d, %v, want 6 decimals", symbol, decimals, err)
		}
		t.Logf("devnet USDC symbol %q", symbol)
	})
}

// TestDevnetSendLive sends SOL on devnet between two keys made here, funded
// by the public faucet. It is skipped when the faucet refuses, which it often
// does.
func TestDevnetSendLive(t *testing.T) {
	if os.Getenv("WALLETSPACE_LIVE_SOLANA_TESTS") != "1" {
		t.Skip("set WALLETSPACE_LIVE_SOLANA_TESTS=1 to use the public Solana RPC")
	}

	settings, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	nets := liveNetworks(t)
	resolver := rpcpool.New(settings)
	adapter, err := solanachain.New(nets, resolver)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	devnet := nets["solana-devnet"]
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	sender := &testSigner{key: ed25519.NewKeyFromSeed(seed), family: chain.FamilySolana}
	recipient := newTestSigner(byte(seed[0] | 1))

	// The faucet, through the guarded client.
	client := rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(devnet.RPCFallbacks[0], &jsonrpc.RPCClientOpts{
		HTTPClient: resolver.HTTPClient(devnet),
	}))
	if _, err := client.RequestAirdrop(t.Context(), solana.MustPublicKeyFromBase58(sender.address()), 100_000_000, rpc.CommitmentConfirmed); err != nil {
		t.Skipf("the devnet faucet refused: %v", err)
	}
	native := chain.Asset{ID: "solana-devnet:native", NetworkID: "solana-devnet", Kind: "native", Symbol: "SOL", Decimals: 9}
	req := chain.TransferRequest{From: sender.address(), To: recipient.address(), Asset: native, Amount: "0.01"}
	var estimate chain.TransferEstimate
	deadline := time.Now().Add(60 * time.Second)
	for {
		estimate, err = adapter.EstimateTransfer(t.Context(), "solana-devnet", req)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if err != nil {
		t.Fatalf("EstimateTransfer() error = %v (airdrop not visible?)", err)
	}
	t.Logf("estimate %+v", estimate)
	req.Approved = &chain.FeeApproval{
		FeeModel: estimate.FeeModel, ComputeUnitLimit: estimate.ComputeUnitLimit,
		ComputeUnitPriceMicroLamports: estimate.ComputeUnitPriceMicroLamports, Fee: estimate.Fee,
	}
	sent, err := adapter.Send(t.Context(), "solana-devnet", req, sender)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	t.Logf("sent %s", sent.Hash)
	for time.Now().Before(deadline.Add(60 * time.Second)) {
		status, err := adapter.Transaction(t.Context(), "solana-devnet", sent.Hash)
		if err == nil && status.Status != "pending" {
			if status.Status != "confirmed" {
				t.Fatalf("status = %s, want confirmed", status.Status)
			}
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatal("the transfer was not confirmed within two minutes")
}

// TestMainnetEstimateLive estimates a SOL and a USDC transfer from a
// well-known mainnet wallet: the simulation runs the message this wallet
// builds against the real runtime, and nothing is signed.
func TestMainnetEstimateLive(t *testing.T) {
	if os.Getenv("WALLETSPACE_LIVE_SOLANA_TESTS") != "1" {
		t.Skip("set WALLETSPACE_LIVE_SOLANA_TESTS=1 to use the public Solana RPC")
	}

	settings, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	adapter, err := solanachain.New(liveNetworks(t), rpcpool.New(settings))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	const wallet = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	recipient := newTestSigner(42).address()
	for _, req := range []chain.TransferRequest{
		{From: wallet, To: recipient, Asset: native("solana-mainnet"), Amount: "0.001"},
		{From: wallet, To: recipient, Asset: spl("solana-mainnet", usdcMint, "USDC", 6), Amount: "0.01"},
	} {
		estimate, err := adapter.EstimateTransfer(t.Context(), "solana-mainnet", req)
		if err != nil {
			t.Errorf("EstimateTransfer(%s) error = %v", req.Asset.Symbol, err)
			continue
		}
		t.Logf("%s estimate %+v", req.Asset.Symbol, estimate)
	}
	// The maximum: the simulation sends everything but the signature fee and
	// leaves the wallet at zero, as the real sweep would.
	estimate, err := adapter.EstimateMaxTransfer(t.Context(), "solana-mainnet",
		chain.TransferRequest{From: wallet, To: recipient, Asset: native("solana-mainnet"), Amount: "max"})
	if err != nil {
		t.Errorf("EstimateMaxTransfer(SOL) error = %v", err)
		return
	}
	t.Logf("SOL max estimate %+v", estimate)
}

// TestMainnetCloseEstimateLive lists a well-known wallet's empty token
// accounts and estimates closing some of them: read-only, simulated, nothing
// signed.
func TestMainnetCloseEstimateLive(t *testing.T) {
	if os.Getenv("WALLETSPACE_LIVE_SOLANA_TESTS") != "1" {
		t.Skip("set WALLETSPACE_LIVE_SOLANA_TESTS=1 to use the public Solana RPC")
	}

	settings, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	adapter, err := solanachain.New(liveNetworks(t), rpcpool.New(settings))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	wallet := os.Getenv("WALLETSPACE_LIVE_SOLANA_CLOSE_WALLET")
	if wallet == "" {
		wallet = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	}
	listed, err := adapter.EmptyTokenAccounts(t.Context(), "solana-mainnet", wallet)
	if err != nil {
		t.Fatalf("EmptyTokenAccounts() error = %v", err)
	}
	t.Logf("%d empty token accounts", len(listed))
	if len(listed) == 0 {
		t.Skip("the wallet has no empty token accounts to estimate")
	}
	var addresses []string
	for _, account := range listed[:min(len(listed), solanachain.MaxCloseAccounts)] {
		addresses = append(addresses, account.Address)
	}
	estimate, err := adapter.EstimateClose(t.Context(), "solana-mainnet", solanachain.CloseRequest{Owner: wallet, Accounts: addresses})
	if err != nil {
		t.Fatalf("EstimateClose(%d) error = %v", len(addresses), err)
	}
	t.Logf("close %d: returned %s, fee %s, limit %d, price %d", len(estimate.Accounts), estimate.Returned, estimate.Fee,
		estimate.ComputeUnitLimit, estimate.ComputeUnitPriceMicroLamports)
}
