package asset_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sxwebdev/walletspace/internal/asset"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/network"
)

const (
	usdcMint       = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	usdtMint       = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	devnetUSDCMint = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	// A mint the wallet does not list: the wrapped SOL mint.
	otherMint = "So11111111111111111111111111111111111111112"
)

func solanaNetwork(id string) network.Network {
	return network.Network{ID: id, Family: "solana", Native: network.NativeAsset{Symbol: "SOL", Decimals: 9}}
}

func TestBuiltinSolanaAssets(t *testing.T) {
	t.Parallel()

	store, err := asset.New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	want := map[string][]chain.Asset{
		"solana-mainnet": {
			{ID: "solana-mainnet:native", NetworkID: "solana-mainnet", Kind: "native", Name: "Solana", Symbol: "SOL", Decimals: 9},
			// Ordered by name: Tether USD, then USD Coin.
			{ID: "solana-mainnet:spl:" + usdtMint, NetworkID: "solana-mainnet", Kind: "spl", Name: "Tether USD", Symbol: "USDT", Decimals: 6, Contract: usdtMint},
			{ID: "solana-mainnet:spl:" + usdcMint, NetworkID: "solana-mainnet", Kind: "spl", Name: "USD Coin", Symbol: "USDC", Decimals: 6, Contract: usdcMint},
		},
		"solana-devnet": {
			{ID: "solana-devnet:native", NetworkID: "solana-devnet", Kind: "native", Name: "Solana", Symbol: "SOL", Decimals: 9},
			{ID: "solana-devnet:spl:" + devnetUSDCMint, NetworkID: "solana-devnet", Kind: "spl", Name: "USD Coin", Symbol: "USDC", Decimals: 6, Contract: devnetUSDCMint},
		},
	}
	for networkID, assets := range want {
		got := store.List(solanaNetwork(networkID))
		if len(got) != len(assets) {
			t.Fatalf("List(%s) = %+v, want %d assets", networkID, got, len(assets))
		}
		for i := range assets {
			if got[i] != assets[i] {
				t.Errorf("List(%s)[%d] = %+v, want %+v", networkID, i, got[i], assets[i])
			}
			if !asset.ValidID(got[i].ID) {
				t.Errorf("built-in id %q is not a valid id", got[i].ID)
			}
		}
	}
}

// Base58 is case-sensitive: a mint in another case is another key, so the
// identifier keeps the mint exactly as written.
func TestSPLIdentifiersKeepTheMintsCase(t *testing.T) {
	t.Parallel()

	if got, want := asset.ID("solana-mainnet", "spl", usdcMint), "solana-mainnet:spl:"+usdcMint; got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
	if asset.ID("solana-mainnet", "spl", "AbC") == asset.ID("solana-mainnet", "spl", "abc") {
		t.Error("two mints differing in case got one identifier")
	}
}

func TestAddSPLAsset(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	store, err := asset.New(home)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	token := chain.Asset{
		ID: asset.ID("solana-mainnet", "spl", otherMint), NetworkID: "solana-mainnet", Kind: "spl",
		Symbol: "wSOL", Decimals: 9, Contract: otherMint,
	}
	if err := store.Add(token); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	reopened, err := asset.New(home)
	if err != nil {
		t.Fatalf("New(reopen) error = %v", err)
	}
	var found *chain.Asset
	for _, item := range reopened.List(solanaNetwork("solana-mainnet")) {
		if item.ID == token.ID {
			found = &item
		}
	}
	if found == nil || found.Contract != otherMint || !found.Configured || found.Name != "wSOL" {
		t.Errorf("stored asset = %+v, want the mint kept as written and configured", found)
	}
}

func TestAddRefusesAnInvalidMint(t *testing.T) {
	t.Parallel()

	for name, mint := range map[string]string{
		"evm address":     "0xdac17f958d2ee523a2206206994597c13d831ec7",
		"tron address":    "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		"too short":       "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyT",
		"too long":        usdcMint + "1",
		"leading one":     "1" + usdcMint,
		"not base58":      "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt10",
		"padded":          " " + usdcMint,
		"trailing line":   usdcMint + "\n",
		"signature sized": "2Ana1pUpv2ZbMVkwF5FXapYeBEjdxDatLn7nvJkhgTSXbs59SyZSx866bXirPgj8QQVB57uxHJBG1YFvkRbFj4T",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			store, err := asset.New(home)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = store.Add(chain.Asset{
				ID: asset.ID("solana-mainnet", "spl", mint), NetworkID: "solana-mainnet", Kind: "spl",
				Symbol: "BAD", Decimals: 6, Contract: mint,
			})
			if !errors.Is(err, asset.ErrInvalidMetadata) {
				t.Errorf("Add(%q) error = %v, want ErrInvalidMetadata", mint, err)
			}
			if _, statErr := os.Stat(filepath.Join(home, "assets.json")); statErr == nil {
				t.Error("a refused asset was written to disk")
			}
		})
	}
}

func TestAddStillRefusesOtherKinds(t *testing.T) {
	t.Parallel()

	store, err := asset.New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = store.Add(chain.Asset{
		ID: "solana-mainnet:spl2022:" + usdcMint, NetworkID: "solana-mainnet", Kind: "spl2022",
		Symbol: "X", Decimals: 6, Contract: usdcMint,
	})
	if err == nil {
		t.Error("Add() accepted an unknown kind")
	}
}
