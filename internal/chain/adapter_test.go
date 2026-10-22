package chain_test

import (
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/chaintest"
	"github.com/sxwebdev/walletspace/internal/network"
)

// There is no default adapter: a family nobody registered is refused, never
// served by whichever family used to sit in the `else`.
func TestAdaptersForServesOnlyRegisteredFamilies(t *testing.T) {
	t.Parallel()

	tron, evm := &chaintest.Adapter{Kind: "trc20"}, &chaintest.Adapter{Kind: "erc20"}
	adapters, err := chain.NewAdapters(map[network.Family]chain.Adapter{
		network.FamilyTron: tron, network.FamilyEVM: evm,
	})
	if err != nil {
		t.Fatalf("NewAdapters() error = %v", err)
	}
	tests := []struct {
		name    string
		family  network.Family
		want    chain.Adapter
		wantErr error
	}{
		{name: "tron", family: network.FamilyTron, want: tron},
		{name: "evm", family: network.FamilyEVM, want: evm},
		{name: "unknown family", family: "bogus", wantErr: chain.ErrUnsupportedFamily},
		{name: "empty family", family: "", wantErr: chain.ErrUnsupportedFamily},
		// Family names are compared exactly; a near miss is not the family.
		{name: "case differs", family: "TRON", wantErr: chain.ErrUnsupportedFamily},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := adapters.For(tt.family)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("For(%q) error = %v, want %v", tt.family, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("For(%q) = %v, want %v", tt.family, got, tt.want)
			}
		})
	}
}

func TestNewAdaptersRefusesIncompleteEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		adapters map[network.Family]chain.Adapter
	}{
		{name: "nil adapter", adapters: map[network.Family]chain.Adapter{network.FamilyTron: nil}},
		{name: "empty family", adapters: map[network.Family]chain.Adapter{"": &chaintest.Adapter{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got, err := chain.NewAdapters(tt.adapters); err == nil {
				t.Fatalf("NewAdapters() = %v, want an error", got)
			}
		})
	}
}

// The set is copied: changing the caller's map later does not change which
// families are served.
func TestNewAdaptersCopiesTheMap(t *testing.T) {
	t.Parallel()

	source := map[network.Family]chain.Adapter{network.FamilyTron: &chaintest.Adapter{}}
	adapters, err := chain.NewAdapters(source)
	if err != nil {
		t.Fatalf("NewAdapters() error = %v", err)
	}
	source["bogus"] = &chaintest.Adapter{}
	if _, err := adapters.For("bogus"); !errors.Is(err, chain.ErrUnsupportedFamily) {
		t.Errorf("For(bogus) after the source map changed: error = %v, want ErrUnsupportedFamily", err)
	}
}

func TestAdaptersAllIsOrderedByFamily(t *testing.T) {
	t.Parallel()

	tron, evm := &chaintest.Adapter{}, &chaintest.Adapter{}
	adapters, err := chain.NewAdapters(map[network.Family]chain.Adapter{
		network.FamilyTron: tron, network.FamilyEVM: evm,
	})
	if err != nil {
		t.Fatalf("NewAdapters() error = %v", err)
	}
	var families []network.Family
	var got []chain.Adapter
	for family, adapter := range adapters.All() {
		families = append(families, family)
		got = append(got, adapter)
	}
	if len(families) != 2 || families[0] != network.FamilyEVM || families[1] != network.FamilyTron ||
		got[0] != evm || got[1] != tron {
		t.Fatalf("All() = %v %v, want evm then tron", families, got)
	}

	// Stopping early stops the iteration.
	seen := 0
	for range adapters.All() {
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("All() yielded %d values after break, want 1", seen)
	}
}
