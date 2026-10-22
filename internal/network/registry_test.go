package network_test

import (
	"reflect"
	"testing"

	"github.com/sxwebdev/walletspace/internal/network"
)

func TestBuiltinRegistry(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}
	items := registry.List()
	if len(items) != 20 {
		t.Fatalf("len(List()) = %d, want 20", len(items))
	}
	seenChainIDs := make(map[string]string)
	for _, item := range items {
		if err := network.Validate(item); err != nil {
			t.Errorf("Validate(%s) error = %v", item.ID, err)
		}
		if item.Family != network.FamilyEVM {
			continue
		}
		if previous := seenChainIDs[item.ChainID]; previous != "" {
			t.Errorf("chain ID %s is shared by %s and %s", item.ChainID, previous, item.ID)
		}
		seenChainIDs[item.ChainID] = item.ID
	}
	base, err := registry.Get("base-mainnet")
	if err != nil {
		t.Fatalf("Get(base-mainnet) error = %v", err)
	}
	if base.ChainID != "8453" || base.Native.Symbol != "ETH" || base.Testnet {
		t.Errorf("base-mainnet = %+v", base)
	}
}

func TestPolygonAmoyDefaultRPC(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("Builtin() error = %v", err)
	}
	amoy, err := registry.Get("polygon-amoy")
	if err != nil {
		t.Fatalf("Get(polygon-amoy) error = %v", err)
	}
	if amoy.ChainID != "80002" {
		t.Fatalf("polygon-amoy chain ID = %q, want 80002", amoy.ChainID)
	}
	if len(amoy.RPCFallbacks) != 1 || amoy.RPCFallbacks[0] != "https://polygon-amoy-bor-rpc.publicnode.com" {
		t.Fatalf("polygon-amoy RPC fallbacks = %q, want PublicNode Amoy Bor RPC", amoy.RPCFallbacks)
	}
}

func TestWrappedNativeRegistryIsImmutable(t *testing.T) {
	t.Parallel()
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	original, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatal(err)
	}
	want := original.WrappedNative.Contract
	original.WrappedNative.Contract = "changed"
	for _, item := range registry.List() {
		if item.WrappedNative != nil {
			item.WrappedNative.Contract = "also changed"
		}
	}
	got, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatal(err)
	}
	if got.WrappedNative.Contract != want {
		t.Fatal("callers can mutate the canonical wrapper")
	}
	other, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	got, err = other.Get("ethereum-mainnet")
	if err != nil || got.WrappedNative.Contract != want {
		t.Fatal("mutation escaped into a new registry")
	}
}

// Tronscan and Etherscan spell a transaction page differently; a link built
// with the other's path opens nothing.
func TestExplorerTransactionLinks(t *testing.T) {
	t.Parallel()
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"tron-mainnet":     "https://tronscan.org/transaction/{tx}",
		"tron-nile":        "https://nile.tronscan.org/transaction/{tx}",
		"ethereum-mainnet": "https://etherscan.io/tx/{tx}",
		"solana-mainnet":   "https://solscan.io/tx/{tx}",
		"solana-devnet":    "https://solscan.io/tx/{tx}?cluster=devnet",
	} {
		item, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if item.Explorer.Tx != want {
			t.Errorf("%s transaction link = %s, want %s", id, item.Explorer.Tx, want)
		}
	}
}

func TestSolanaNetworks(t *testing.T) {
	t.Parallel()
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	// Swaps are routed by Raydium's Trade API, which serves mainnet only.
	solanaCapabilities := network.Capabilities{NativeTransfer: true, TokenTransfer: true, SolanaCloseAccounts: true}
	solanaMainnetCapabilities := solanaCapabilities
	solanaMainnetCapabilities.SolanaSwap = true
	want := []network.Network{
		{
			ID: "solana-mainnet", Name: "Solana Mainnet", ShortName: "Mainnet",
			Family: network.FamilySolana, ChainID: "mainnet",
			Native: network.NativeAsset{Symbol: "SOL", Decimals: 9}, Enabled: true,
			Explorer: network.Explorer{
				Address: "https://solscan.io/account/{address}",
				Tx:      "https://solscan.io/tx/{tx}",
				Block:   "https://solscan.io/block/{block}",
			},
			RPCFallbacks: []string{"https://api.mainnet.solana.com"},
			Capabilities: solanaMainnetCapabilities, PriceChain: "solana", NativePrice: "coingecko:solana",
		},
		{
			ID: "solana-devnet", Name: "Solana Devnet", ShortName: "Devnet",
			Family: network.FamilySolana, ChainID: "devnet",
			Native: network.NativeAsset{Symbol: "SOL", Decimals: 9}, Testnet: true, Enabled: true,
			Explorer: network.Explorer{
				Address: "https://solscan.io/account/{address}?cluster=devnet",
				Tx:      "https://solscan.io/tx/{tx}?cluster=devnet",
				Block:   "https://solscan.io/block/{block}?cluster=devnet",
			},
			RPCFallbacks: []string{"https://api.devnet.solana.com"},
			Capabilities: solanaCapabilities,
		},
	}
	for _, expected := range want {
		got, err := registry.Get(expected.ID)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", expected.ID, err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("%s =\n%+v\nwant\n%+v", expected.ID, got, expected)
		}
	}
}

func TestValidateSolanaCluster(t *testing.T) {
	t.Parallel()
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	devnet, err := registry.Get("solana-devnet")
	if err != nil {
		t.Fatal(err)
	}
	for chainID, valid := range map[string]bool{
		"mainnet": true, "devnet": true,
		"testnet": false, "nile": false, "1": false, "": false, "Devnet": false,
		"EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG": false,
	} {
		item := devnet
		item.ChainID = chainID
		if err := network.Validate(item); (err == nil) != valid {
			t.Errorf("Validate(solana %q) error = %v, want valid=%v", chainID, err, valid)
		}
	}
	// The cluster names are Solana's own: a Tron network may not borrow one.
	tron, err := registry.Get("tron-nile")
	if err != nil {
		t.Fatal(err)
	}
	tron.ChainID = "devnet"
	if err := network.Validate(tron); err == nil {
		t.Error("Validate(tron devnet) error = nil")
	}
	devnet.Family = "solana2"
	if err := network.Validate(devnet); err == nil {
		t.Error("Validate(unknown family) error = nil")
	}
}
