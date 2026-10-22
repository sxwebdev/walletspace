package httpapi

import (
	"testing"

	"github.com/sxwebdev/walletspace/internal/asset"
	"github.com/sxwebdev/walletspace/internal/network"
)

// Solana mints are base58 and case-sensitive, so the feed is asked for the
// mint exactly as written; a lower-cased mint would name no token at all.
func TestPriceIdentifierForSolana(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	item, err := registry.Get("solana-mainnet")
	if err != nil {
		t.Fatalf("Get(solana-mainnet) error = %v", err)
	}
	store, err := asset.New(t.TempDir())
	if err != nil {
		t.Fatalf("asset.New() error = %v", err)
	}
	want := map[string]string{
		"solana-mainnet:native": "coingecko:solana",
		"solana-mainnet:spl:EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "solana:EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		"solana-mainnet:spl:Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "solana:Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
	}
	listed := store.List(item)
	if len(listed) != len(want) {
		t.Fatalf("assets = %+v, want %d", listed, len(want))
	}
	for _, token := range listed {
		if got := priceIdentifier(item, token); got != want[token.ID] {
			t.Errorf("priceIdentifier(%s) = %q, want %q", token.ID, got, want[token.ID])
		}
	}
}
