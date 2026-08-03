package asset_test

import (
	"errors"
	"fmt"
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

// A token symbol is whatever the contract chose to return, so it is
// attacker-controlled text that reaches the DOM and a stored file. Escaping at
// the sink is what makes it safe to render; refusing the absurd cases here
// keeps a hostile contract from filling the asset file or hiding what it is.
func TestAddRejectsHostileMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		symbol   string
		contract string
	}{
		{name: "null byte", symbol: "TE\x00ST", contract: "0xABCDEF"},
		{name: "newline", symbol: "TEST\nEVIL", contract: "0xABCDEF"},
		{name: "bidi override", symbol: "TEST‮EVIL", contract: "0xABCDEF"},
		{name: "too long", symbol: strings.Repeat("A", 65), contract: "0xABCDEF"},
		{name: "invalid utf-8", symbol: "TEST\xff\xfe", contract: "0xABCDEF"},
		{name: "quote in the contract", symbol: "TEST", contract: `0xAB" onmouseover=alert(1) x="`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			store, err := asset.New(home)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			err = store.Add(chain.Asset{
				ID:        asset.ID("ethereum-sepolia", "erc20", tt.contract),
				NetworkID: "ethereum-sepolia", Kind: "erc20", Symbol: tt.symbol,
				Decimals: 6, Contract: tt.contract,
			})
			if err == nil {
				t.Fatal("Add() accepted hostile metadata")
			}
			if !errors.Is(err, asset.ErrInvalidMetadata) {
				t.Errorf("Add() error = %v, want ErrInvalidMetadata so the API answers 400", err)
			}
			if _, statErr := os.Stat(filepath.Join(home, "assets.json")); statErr == nil {
				t.Error("a rejected asset was still written to disk")
			}
		})
	}
}

// The identifier embeds the contract straight from the request body and is
// rendered into an HTML attribute, so it has to hold the line on its own rather
// than lean on the address validation the chain adapters happen to do first.
func TestValidID(t *testing.T) {
	t.Parallel()

	valid := []string{
		"ethereum-sepolia:erc20:0xabcdef",
		"tron-nile:trc20:TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
	}
	for _, id := range valid {
		if !asset.ValidID(id) {
			t.Errorf("ValidID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"", `eth:erc20:0x" onmouseover=x`, "eth:erc20:0x<script>", "eth:erc20:0x\n",
		"eth:erc20:" + strings.Repeat("a", 128),
	}
	for _, id := range invalid {
		if asset.ValidID(id) {
			t.Errorf("ValidID(%q) = true, want false", id)
		}
	}
}

// Markup in a symbol is stored verbatim on purpose. Escaping at the sink is
// what makes it safe to display, and a blocklist here would only obscure
// whether that escaping actually holds.
func TestAddKeepsMarkupVerbatimForTheSinkToEscape(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	store, err := asset.New(home)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	const hostile = `</span><img src=x onerror=alert(1)>`
	id := asset.ID("ethereum-sepolia", "erc20", "0xABCDEF")
	if err := store.Add(chain.Asset{
		ID: id, NetworkID: "ethereum-sepolia", Kind: "erc20", Symbol: hostile,
		Decimals: 6, Contract: "0xABCDEF",
	}); err != nil {
		t.Fatalf("Add() error = %v, want the symbol stored for the sink to escape", err)
	}

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}
	item, err := registry.Get("ethereum-sepolia")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	for _, stored := range store.List(item) {
		if stored.ID == id && stored.Symbol != hostile {
			t.Errorf("symbol = %q, want it stored unchanged", stored.Symbol)
		}
	}
}

// Every configured asset is one more contract call per account on every balance
// refresh, and the list is reloaded on start — so an unbounded list makes each
// refresh permanently more expensive.
func TestConfiguredAssetsAreCapped(t *testing.T) {
	t.Parallel()

	store, err := asset.New(t.TempDir())
	if err != nil {
		t.Fatalf("asset.New() error = %v", err)
	}
	var last chain.Asset
	for i := range 256 {
		token := chain.Asset{
			ID:        fmt.Sprintf("ethereum-mainnet:erc20:0x%040x", i),
			NetworkID: "ethereum-mainnet", Kind: "erc20",
			Contract: fmt.Sprintf("0x%040x", i), Symbol: "TKN", Decimals: 18,
		}
		if err := store.Add(token); err != nil {
			t.Fatalf("Add(%d) error = %v", i, err)
		}
		last = token
	}
	overflow := chain.Asset{
		ID:        "ethereum-mainnet:erc20:0xffffffffffffffffffffffffffffffffffffffff",
		NetworkID: "ethereum-mainnet", Kind: "erc20",
		Contract: "0xffffffffffffffffffffffffffffffffffffffff", Symbol: "TKN", Decimals: 18,
	}
	if err := store.Add(overflow); !errors.Is(err, asset.ErrQuotaExceeded) {
		t.Fatalf("Add() past the ceiling error = %v, want ErrQuotaExceeded", err)
	}
	// Updating one that is already stored is not adding one, so it still works
	// at the ceiling — otherwise refreshing metadata would become impossible.
	last.Symbol = "RENAMED"
	if err := store.Add(last); err != nil {
		t.Errorf("Add() replacing an existing asset at the ceiling error = %v", err)
	}
}
