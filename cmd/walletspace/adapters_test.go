package main

import (
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain/chaintest"
	"github.com/sxwebdev/walletspace/internal/network"
)

// Every built-in network reaches the adapter of its own family. A family left
// out of the set would leave its networks with no balances and no Doctor.
func TestEveryBuiltinNetworkHasItsFamilysAdapter(t *testing.T) {
	t.Parallel()

	evm, tron, solana := &chaintest.Adapter{Kind: "erc20"}, &chaintest.Adapter{Kind: "trc20"}, &chaintest.Adapter{Kind: "spl"}
	chains, err := familyAdapters(evm, tron, solana)
	if err != nil {
		t.Fatalf("familyAdapters() error = %v", err)
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	want := map[network.Family]string{network.FamilyEVM: "erc20", network.FamilyTron: "trc20", network.FamilySolana: "spl"}
	seen := make(map[network.Family]bool)
	for _, item := range registry.List() {
		adapter, err := chains.For(item.Family)
		if err != nil {
			t.Errorf("%s: %v", item.ID, err)
			continue
		}
		if got := adapter.TokenKind(); got != want[item.Family] {
			t.Errorf("%s reaches the %s adapter, want %s", item.ID, got, want[item.Family])
		}
		seen[item.Family] = true
	}
	if len(seen) != len(want) {
		t.Errorf("families with built-in networks = %v, want %d", seen, len(want))
	}
}
