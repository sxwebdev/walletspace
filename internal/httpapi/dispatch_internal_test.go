package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sxwebdev/walletspace/internal/asset"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/chaintest"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/network"
)

// dispatchPlatform is a Platform reduced to what the family dispatch needs,
// with recording adapters in place of the real ones.
func dispatchPlatform(t *testing.T) (*Platform, *chaintest.Adapter, *chaintest.Adapter) {
	t.Helper()
	settings, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	tron, evm := &chaintest.Adapter{Kind: "trc20"}, &chaintest.Adapter{Kind: "erc20"}
	chains, err := chain.NewAdapters(map[network.Family]chain.Adapter{
		network.FamilyTron: tron, network.FamilyEVM: evm,
	})
	if err != nil {
		t.Fatalf("chain.NewAdapters() error = %v", err)
	}
	return &Platform{
		settings: settings, chains: chains,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, tron, evm
}

// Every place that used to read `if evm { … } else { tron }`, driven with a
// Tron, an EVM and an unknown network. The unknown one has to come back as
// ErrUnsupportedFamily without either adapter having been called; the two
// known ones prove each site does reach exactly its own family's adapter, so
// the refusal is the dispatch's and not some earlier failure.
//
// The unknown network is a plain value: network.Validate refuses its family,
// so no registry could hand it out, and these sites take the network itself.
func TestFamilyDispatchRefusesAFamilyWithoutAnAdapter(t *testing.T) {
	t.Parallel()

	sites := []struct {
		name   string
		method string
		call   func(context.Context, *Platform, network.Network) error
	}{
		{
			name: "balance stream", method: "BalanceStream",
			call: func(ctx context.Context, p *Platform, item network.Network) error {
				results, err := p.chainBalanceStream(ctx, item, nil, nil, false)
				if err != nil {
					return err
				}
				for range results {
				}
				return nil
			},
		},
		{
			name: "estimate", method: "EstimateTransfer",
			call: func(ctx context.Context, p *Platform, item network.Network) error {
				_, err := p.chainEstimate(ctx, item, chain.TransferRequest{Amount: "1"})
				return err
			},
		},
		{
			name: "estimate max", method: "EstimateMaxTransfer",
			call: func(ctx context.Context, p *Platform, item network.Network) error {
				_, err := p.chainEstimateMax(ctx, item, chain.TransferRequest{Amount: "max"})
				return err
			},
		},
		{
			name: "send", method: "Send",
			call: func(ctx context.Context, p *Platform, item network.Network) error {
				_, err := p.chainSend(ctx, item, chain.TransferRequest{Amount: "1"}, nil)
				return err
			},
		},
		{
			name: "transaction", method: "Transaction",
			call: func(ctx context.Context, p *Platform, item network.Network) error {
				_, err := p.chainTransaction(ctx, item, "tx")
				return err
			},
		},
		{
			name: "token metadata", method: "TokenMetadata",
			call: func(ctx context.Context, p *Platform, item network.Network) error {
				_, err := p.tokenAsset(ctx, item, "contract")
				return err
			},
		},
		{
			name: "rpc verification", method: "VerifyEndpoint",
			call: func(ctx context.Context, p *Platform, item network.Network) error {
				return p.verifyRPCs(ctx, item, config.NetworkOverride{
					Endpoints: []config.Endpoint{{URL: "https://rpc.example"}},
				})
			},
		},
	}
	families := []struct {
		name    string
		family  network.Family
		served  func(tron, evm *chaintest.Adapter) *chaintest.Adapter
		wantErr error
	}{
		{name: "tron", family: network.FamilyTron, served: func(tron, _ *chaintest.Adapter) *chaintest.Adapter { return tron }},
		{name: "evm", family: network.FamilyEVM, served: func(_, evm *chaintest.Adapter) *chaintest.Adapter { return evm }},
		{name: "unknown", family: "bogus", wantErr: chain.ErrUnsupportedFamily},
	}
	for _, site := range sites {
		for _, family := range families {
			t.Run(site.name+"/"+family.name, func(t *testing.T) {
				t.Parallel()

				p, tron, evm := dispatchPlatform(t)
				item := network.Network{ID: "net-" + family.name, Family: family.family}
				err := site.call(t.Context(), p, item)
				if !errors.Is(err, family.wantErr) {
					t.Fatalf("error = %v, want %v", err, family.wantErr)
				}
				want := map[*chaintest.Adapter][]chaintest.Call{tron: nil, evm: nil}
				if family.served != nil {
					want[family.served(tron, evm)] = []chaintest.Call{{Method: site.method, NetworkID: item.ID}}
				}
				for adapter, name := range map[*chaintest.Adapter]string{tron: "tron", evm: "evm"} {
					if got := adapter.Calls(); !equalCalls(got, want[adapter]) {
						t.Errorf("%s adapter calls = %v, want %v", name, got, want[adapter])
					}
				}
			})
		}
	}
}

// A changed discovery policy invalidates each network in its own family's
// adapter; a network no adapter serves is skipped, not invalidated in Tron.
func TestInvalidateNetworksUsesOnlyTheOwnFamily(t *testing.T) {
	t.Parallel()

	p, tron, evm := dispatchPlatform(t)
	p.invalidateNetworks([]network.Network{
		{ID: "net-tron", Family: network.FamilyTron},
		{ID: "net-evm", Family: network.FamilyEVM},
		{ID: "net-bogus", Family: "bogus"},
	})
	if got, want := tron.Calls(), []chaintest.Call{{Method: "Invalidate", NetworkID: "net-tron"}}; !equalCalls(got, want) {
		t.Errorf("tron adapter calls = %v, want %v", got, want)
	}
	if got, want := evm.Calls(), []chaintest.Call{{Method: "Invalidate", NetworkID: "net-evm"}}; !equalCalls(got, want) {
		t.Errorf("evm adapter calls = %v, want %v", got, want)
	}
}

// Saving or dropping a network's settings invalidates it in every adapter, as
// it did before the adapters were keyed by family.
func TestInvalidateEverywhereReachesEveryAdapter(t *testing.T) {
	t.Parallel()

	p, tron, evm := dispatchPlatform(t)
	p.invalidateEverywhere("net-any")
	want := []chaintest.Call{{Method: "Invalidate", NetworkID: "net-any"}}
	if got := tron.Calls(); !equalCalls(got, want) {
		t.Errorf("tron adapter calls = %v, want %v", got, want)
	}
	if got := evm.Calls(); !equalCalls(got, want) {
		t.Errorf("evm adapter calls = %v, want %v", got, want)
	}
}

// The token's kind comes from the adapter that read its metadata.
func TestTokenAssetTakesTheKindFromItsFamily(t *testing.T) {
	t.Parallel()

	p, _, _ := dispatchPlatform(t)
	tests := []struct {
		family network.Family
		want   string
	}{
		{family: network.FamilyTron, want: "trc20"},
		{family: network.FamilyEVM, want: "erc20"},
	}
	for _, tt := range tests {
		token, err := p.tokenAsset(t.Context(), network.Network{ID: "net", Family: tt.family}, "C")
		if err != nil {
			t.Fatalf("tokenAsset(%s) error = %v", tt.family, err)
		}
		if token.Kind != tt.want || token.ID != asset.ID("net", tt.want, "C") || token.Symbol != "FAKE" ||
			token.Decimals != 6 || !token.Configured {
			t.Errorf("tokenAsset(%s) = %+v, want kind %s", tt.family, token, tt.want)
		}
	}
}

// A contract is lower-cased for the price feed only where the family's
// addresses are case-insensitive. A family the code does not know gets no
// price rather than a guess about its addresses.
func TestPriceIdentifierByFamily(t *testing.T) {
	t.Parallel()

	token := chain.Asset{Kind: "token", Contract: "AbCd"}
	tests := []struct {
		name   string
		family network.Family
		want   string
	}{
		{name: "evm lower-cases", family: network.FamilyEVM, want: "feed:abcd"},
		{name: "tron keeps case", family: network.FamilyTron, want: "feed:AbCd"},
		{name: "unknown family has no price", family: "bogus", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			item := network.Network{ID: "net", Family: tt.family, PriceChain: "feed"}
			if got := priceIdentifier(item, token); got != tt.want {
				t.Errorf("priceIdentifier() = %q, want %q", got, tt.want)
			}
		})
	}
}

func equalCalls(got, want []chaintest.Call) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
