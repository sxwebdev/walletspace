package solana

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
	"github.com/sxwebdev/walletspace/internal/network"
)

// swapDeps is what swaps need beyond the rest of the adapter's state: the
// Raydium quoter — one per adapter, which is one per process, since Raydium
// limits quotes per IP — and a test hook on the message that gets signed.
type swapDeps struct {
	mu sync.Mutex
	// quoter was built over the network's client under the settings of
	// version; injected, when set, replaces it for good.
	quoter   *raydium.Quoter
	network  string
	version  uint64
	injected *raydium.Quoter
	// buildFinal serializes a swap's final message; a test replaces it to
	// prove that a message other than the planned one is never signed.
	buildFinal func(*swapPlan, solana.Hash) ([]byte, error)
}

func (d *swapDeps) finalBuilder() func(*swapPlan, solana.Hash) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buildFinal
}

// UseRaydiumQuoter replaces the quoter swaps ask for routes; without it the
// adapter builds one on first use.
func (a *Adapter) UseRaydiumQuoter(quoter *raydium.Quoter) {
	a.swaps.mu.Lock()
	defer a.swaps.mu.Unlock()
	a.swaps.injected = quoter
}

// raydiumQuoter is the adapter's quoter, built over the guarded client the
// endpoint provider hands out for the network: the resolver's client dials
// through its own address policy (hostpolicy.PublicIP, with the user's
// allow-insecure-RPC setting), never through http.DefaultClient. That
// setting is read when the client is built, so the quoter is built again
// when the network's settings change (Invalidate) — and only then, to keep
// its pacing and any Retry-After ban.
func (a *Adapter) raydiumQuoter(item network.Network) (*raydium.Quoter, error) {
	a.mu.Lock()
	version := a.version[item.ID]
	a.mu.Unlock()
	a.swaps.mu.Lock()
	defer a.swaps.mu.Unlock()
	if a.swaps.injected != nil {
		return a.swaps.injected, nil
	}
	if a.swaps.quoter == nil || a.swaps.network != item.ID || a.swaps.version != version {
		client := a.endpoints.HTTPClient(item)
		if client == nil || client.Transport == nil {
			return nil, errors.New("a guarded HTTP client is required for the Raydium API")
		}
		quoter, err := raydium.NewQuoter(client, "")
		if err != nil {
			return nil, err // not reachable: the default URLs parse
		}
		a.swaps.quoter, a.swaps.network, a.swaps.version = quoter, item.ID, version
	}
	return a.swaps.quoter, nil
}

// discoveryRPC is raydium.RPC over the network's verified, paced nodes.
type discoveryRPC struct{ nodes *nodes }

func (d discoveryRPC) ProgramAccountKeys(ctx context.Context, q raydium.ProgramAccountsQuery) ([]solana.PublicKey, error) {
	filters := []rpc.RPCFilter{{DataSize: q.DataSize}}
	for _, m := range q.Memcmp {
		filters = append(filters, rpc.RPCFilter{Memcmp: &rpc.RPCFilterMemcmp{Offset: m.Offset, Bytes: m.Bytes}})
	}
	zero := uint64(0)
	var result rpc.GetProgramAccountsResult
	err := d.nodes.do(ctx, "getProgramAccounts", func(client *rpc.Client) error {
		var callErr error
		result, callErr = client.GetProgramAccountsWithOpts(ctx, q.Program, &rpc.GetProgramAccountsOpts{
			Encoding: solana.EncodingBase64, Commitment: readCommitment, Filters: filters,
			DataSlice: &rpc.DataSlice{Offset: &zero, Length: &zero},
		})
		return callErr
	})
	if err != nil {
		return nil, err
	}
	keys := make([]solana.PublicKey, 0, len(result))
	for _, item := range result {
		if item == nil || item.Account == nil || item.Account.Owner != q.Program {
			return nil, fmt.Errorf("getProgramAccounts answered an account of another program")
		}
		keys = append(keys, item.Pubkey)
	}
	return keys, nil
}

func (d discoveryRPC) MultipleAccounts(ctx context.Context, keys []solana.PublicKey) ([]*raydium.AccountData, error) {
	accounts, err := readAccounts(ctx, d.nodes, keys)
	if err != nil {
		return nil, err
	}
	out := make([]*raydium.AccountData, len(accounts))
	for i, account := range accounts {
		if account != nil {
			out[i] = &raydium.AccountData{Owner: account.Owner, Lamports: account.Lamports, Data: account.Data.GetBinary()}
		}
	}
	return out, nil
}
