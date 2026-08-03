package asset_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/sxwebdev/walletspace/internal/asset"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/network"
)

func TestCustomAssetsPersistWithoutReplacingBuiltIns(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	store, err := asset.New(home)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}
	item, err := registry.Get("ethereum-sepolia")
	if err != nil {
		t.Fatalf("Get(ethereum-sepolia) error = %v", err)
	}
	custom := chain.Asset{
		ID:        asset.ID(item.ID, "erc20", "0xABCDEF"),
		NetworkID: item.ID, Kind: "erc20", Symbol: "TEST",
		Decimals: 6, Contract: "0xABCDEF",
	}
	if err := store.Add(custom); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(home, "assets.json"))
	if err != nil {
		t.Fatalf("Stat(assets.json) error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("assets.json mode = %o, want 600", info.Mode().Perm())
	}

	reopened, err := asset.New(home)
	if err != nil {
		t.Fatalf("New(reopen) error = %v", err)
	}
	list := reopened.List(item)
	if len(list) != 2 {
		t.Fatalf("assets = %+v, want native and custom", list)
	}
	var found bool
	for _, item := range list {
		if item.ID == custom.ID {
			found = item.Configured
		}
	}
	if !found {
		t.Errorf("custom asset = %+v, want configured", list)
	}
	if err := reopened.Delete(custom.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if got := len(reopened.List(item)); got != 1 {
		t.Fatalf("assets after delete = %d, want native only", got)
	}
}

func TestBuiltinMainnetAssets(t *testing.T) {
	t.Parallel()

	store, err := asset.New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}
	expectedCounts := map[string]int{
		"tron-mainnet":      2,
		"tron-nile":         2,
		"bsc-mainnet":       14,
		"ethereum-mainnet":  11,
		"polygon-mainnet":   8,
		"optimism-mainnet":  4,
		"base-mainnet":      5,
		"arbitrum-mainnet":  6,
		"avalanche-mainnet": 3,
		"robinhood-mainnet": 2,
	}
	for networkID, expectedCount := range expectedCounts {
		item, err := registry.Get(networkID)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", networkID, err)
		}
		items := store.List(item)
		if len(items) != expectedCount {
			t.Errorf("List(%s) count = %d, want %d", networkID, len(items), expectedCount)
		}
		if len(items) == 0 || items[0].Kind != "native" {
			t.Errorf("List(%s) does not put the native asset first: %+v", networkID, items)
		}
		seen := make(map[string]struct{}, len(items))
		for _, token := range items {
			if token.Name == "" || token.Symbol == "" || token.Decimals == 0 {
				t.Errorf("List(%s) has incomplete metadata: %+v", networkID, token)
			}
			if _, exists := seen[token.ID]; exists {
				t.Errorf("List(%s) has duplicate id %q", networkID, token.ID)
			}
			seen[token.ID] = struct{}{}
			if token.Kind == "erc20" && !common.IsHexAddress(token.Contract) {
				t.Errorf("List(%s) has invalid ERC20 contract %q", networkID, token.Contract)
			}
			if token.Configured {
				t.Errorf("List(%s) marks built-in %s as configured", networkID, token.Symbol)
			}
		}
	}

	robinhood, err := registry.Get("robinhood-mainnet")
	if err != nil {
		t.Fatalf("Get(robinhood-mainnet) error = %v", err)
	}
	var found bool
	for _, token := range store.List(robinhood) {
		if strings.EqualFold(token.Contract, "0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73") {
			found = token.Name == "Robinhood Wrapped ETH" && token.Symbol == "WETH" && token.Decimals == 18
		}
	}
	if !found {
		t.Error("Robinhood WETH is missing or has unexpected metadata")
	}
}

func TestCustomAssetDoesNotDuplicateNewBuiltin(t *testing.T) {
	t.Parallel()

	store, err := asset.New(t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}
	item, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatalf("Get(ethereum-mainnet) error = %v", err)
	}
	contract := "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	if err := store.Add(chain.Asset{
		ID: asset.ID(item.ID, "erc20", contract), NetworkID: item.ID, Kind: "erc20",
		Name: "USD Coin", Symbol: "USDC", Decimals: 6, Contract: contract,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	items := store.List(item)
	if len(items) != 11 {
		t.Fatalf("List() count = %d, want built-ins without duplicate", len(items))
	}
	var usdcCount int
	for _, token := range items {
		if token.Symbol == "USDC" {
			usdcCount++
		}
	}
	if usdcCount != 1 {
		t.Errorf("USDC count = %d, want 1", usdcCount)
	}
}
