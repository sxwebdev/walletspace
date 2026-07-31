package network_test

import (
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
	if len(items) != 17 {
		t.Fatalf("len(List()) = %d, want 17", len(items))
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
