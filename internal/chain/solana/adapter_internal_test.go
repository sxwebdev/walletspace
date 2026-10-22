package solana

import (
	"context"
	"net/http"
	"testing"

	"github.com/sxwebdev/walletspace/internal/network"
)

type countingProvider struct{ calls int }

func (p *countingProvider) Endpoints(context.Context, network.Network) ([]string, error) {
	p.calls++
	return nil, nil
}

func (p *countingProvider) HTTPClient(network.Network) *http.Client { return nil }

type oneNetwork struct{}

func (oneNetwork) Get(string) (network.Network, error) { return network.Network{}, nil }

// A caller can find the cache empty, then start its own build just after an
// earlier build stored its result. That build must take the stored result
// instead of checking every endpoint again.
func TestInstallTakesWhatAnEarlierBuildStored(t *testing.T) {
	t.Parallel()

	p := &countingProvider{}
	adapter, err := New(oneNetwork{}, p)
	if err != nil {
		t.Fatal(err)
	}
	stored := &nodes{}
	adapter.clients["solana-mainnet"] = stored
	got, err := adapter.install(t.Context(), network.Network{ID: "solana-mainnet"}, "", 0)
	if err != nil || got != stored {
		t.Errorf("install() = %p, %v, want the stored client %p", got, err, stored)
	}
	if p.calls != 0 {
		t.Errorf("endpoint lists = %d, want no second build", p.calls)
	}
}
