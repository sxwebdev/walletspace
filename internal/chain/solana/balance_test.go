package solana_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
)

// balanceNode serves SOL balances and token accounts from fixed tables.
func balanceNode(t *testing.T, lamports map[string]uint64, tokens map[string][]map[string]any) *node {
	t.Helper()
	n := newNode(t, mainnetGenesis)
	n.handle("getMultipleAccounts", func(params []json.RawMessage) reply {
		var opts struct {
			Commitment string `json:"commitment"`
			DataSlice  *struct {
				Length int `json:"length"`
			} `json:"dataSlice"`
		}
		if len(params) < 2 || json.Unmarshal(params[1], &opts) != nil || opts.Commitment != "confirmed" ||
			opts.DataSlice == nil || opts.DataSlice.Length != 0 {
			t.Errorf("getMultipleAccounts options = %s, want confirmed and no data", params)
		}
		var value []any
		for _, address := range requestedKeys(t, params) {
			if amount, ok := lamports[address]; ok {
				value = append(value, account(systemProgram, amount, nil))
			} else {
				value = append(value, nil)
			}
		}
		return reply{result: withContext(value)}
	})
	n.handle("getTokenAccountsByOwner", func(params []json.RawMessage) reply {
		var filter struct {
			ProgramID string `json:"programId"`
		}
		var opts struct {
			Encoding   string `json:"encoding"`
			Commitment string `json:"commitment"`
			DataSlice  struct {
				Offset int `json:"offset"`
				Length int `json:"length"`
			} `json:"dataSlice"`
		}
		if len(params) != 3 || json.Unmarshal(params[1], &filter) != nil || json.Unmarshal(params[2], &opts) != nil ||
			filter.ProgramID != tokenProgram || opts.Encoding != "base64" || opts.Commitment != "confirmed" ||
			opts.DataSlice.Offset != 0 || opts.DataSlice.Length != 72 {
			t.Errorf("getTokenAccountsByOwner params = %s", params)
		}
		owner := requestedOwner(t, params)
		value := tokens[owner]
		if value == nil {
			value = []map[string]any{}
		}
		return reply{result: withContext(value)}
	})
	return n
}

func TestBalanceStreamReadsSOLAndSumsEveryTokenAccountOfAMint(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t,
			map[string]uint64{ownerA: 1_500_000_000},
			map[string][]map[string]any{
				ownerA: {
					tokenAccount(usdcMint, ownerA, 1_000_000),
					// A second account of the same mint, outside the ATA.
					tokenAccount(usdcMint, ownerA, 250_000),
					tokenAccount(usdtMint, ownerA, 7),
					// A mint the wallet does not list is read and ignored.
					tokenAccount(key(9), ownerA, 99),
				},
			},
		)
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}, {AccountID: "b", Address: ownerB}}
		assets := []chain.Asset{
			native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6), spl("solana-mainnet", usdtMint, "USDT", 6),
		}
		got := byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false)))
		want := map[string]string{
			"a|solana-mainnet:native": "1.5", "a|solana-mainnet:spl:" + usdcMint: "1.25",
			"a|solana-mainnet:spl:" + usdtMint: "0.000007",
			// No account on chain and no token accounts: zero, not an error.
			"b|solana-mainnet:native": "0", "b|solana-mainnet:spl:" + usdcMint: "0", "b|solana-mainnet:spl:" + usdtMint: "0",
		}
		if len(got) != len(want) {
			t.Errorf("balances = %+v, want %d", got, len(want))
		}
		for k, amount := range want {
			if b := got[k]; b.Amount != amount || b.Error != "" || b.Stale {
				t.Errorf("balance %s = %+v, want amount %s", k, b, amount)
			}
		}
		if got := n.count("getMultipleAccounts"); got != 1 {
			t.Errorf("getMultipleAccounts calls = %d, want 1 for every holder", got)
		}
		if got := n.count("getTokenAccountsByOwner"); got != 2 {
			t.Errorf("getTokenAccountsByOwner calls = %d, want one per holder", got)
		}
	})
}

func TestBalanceStreamWithoutTokensMakesNoTokenCalls(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 5}, nil)
		adapter, _ := single(t, n)
		got := collect(adapter.BalanceStream(t.Context(), "solana-mainnet",
			[]chain.AccountAddress{{AccountID: "a", Address: ownerA}}, []chain.Asset{native("solana-mainnet")}, false))
		if len(got) != 1 || got[0].Amount != "0.000000005" {
			t.Errorf("balances = %+v", got)
		}
		if got := n.count("getTokenAccountsByOwner"); got != 0 {
			t.Errorf("getTokenAccountsByOwner calls = %d, want 0", got)
		}
	})
}

// getMultipleAccounts takes at most a hundred addresses.
func TestBalanceStreamSplitsSOLReadsIntoHundreds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		var sizes []int
		n.handle("getMultipleAccounts", func(params []json.RawMessage) reply {
			keys := requestedKeys(t, params)
			sizes = append(sizes, len(keys))
			value := make([]any, len(keys))
			for i := range keys {
				value[i] = account(systemProgram, 1, nil)
			}
			return reply{result: withContext(value)}
		})
		adapter, _ := single(t, n)
		var accounts []chain.AccountAddress
		for i := range 150 {
			accounts = append(accounts, chain.AccountAddress{AccountID: fmt.Sprint(i), Address: key(byte(i + 10))})
		}
		got := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, []chain.Asset{native("solana-mainnet")}, false))
		if len(got) != 150 {
			t.Fatalf("balances = %d, want 150", len(got))
		}
		for _, b := range got {
			if b.Amount != "0.000000001" {
				t.Errorf("balance = %+v", b)
			}
		}
		if !slices.Equal(sizes, []int{100, 50}) {
			t.Errorf("getMultipleAccounts sizes = %v, want [100 50]", sizes)
		}
	})
}

func TestBalanceStreamCachesLikeTron(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		amount := uint64(1_000_000_000)
		n := balanceNode(t, nil, nil)
		n.handle("getMultipleAccounts", func(params []json.RawMessage) reply {
			return reply{result: withContext([]any{account(systemProgram, amount, nil)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}
		read := func(refresh bool) []chain.Balance {
			return collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, refresh))
		}

		read(false)
		calls := n.total()
		amount = 2_000_000_000

		// Within the TTL the cache answers and nothing is asked.
		if got := read(false); len(got) != 2 || got[0].Amount != "1" || got[0].Stale {
			t.Errorf("cached read = %+v, want the fresh cached value", got)
		}
		if got := n.total(); got != calls {
			t.Errorf("RPC calls during a fresh cached read = %d, want 0", got-calls)
		}

		// refresh reads past the cache.
		if got := read(true); len(got) != 2 || got[0].Amount != "2" || got[0].Stale {
			t.Errorf("refreshed read = %+v, want the new value only", got)
		}
		calls = n.total()
		amount = 3_000_000_000

		// Past the TTL the cached value comes first, marked stale, then the
		// new one.
		time.Sleep(16 * time.Second)
		got := read(false)
		if len(got) != 4 {
			t.Fatalf("stale read = %+v, want cached then fresh", got)
		}
		if got[0].Amount != "2" || !got[0].Stale || got[2].Amount != "3" || got[2].Stale {
			t.Errorf("stale read = %+v, want stale 2 then fresh 3", got)
		}
		if n.total() == calls {
			t.Error("a stale cached value was not read again")
		}

		// Past the retention the cached value is gone.
		time.Sleep(6 * time.Minute)
		if got := read(false); len(got) != 2 || got[0].Stale {
			t.Errorf("read after retention = %+v, want a fresh read only", got)
		}

		// Invalidate drops the cache with the client.
		adapter.Invalidate("solana-mainnet")
		calls = n.count("getMultipleAccounts")
		if got := read(false); len(got) != 2 {
			t.Errorf("read after invalidate = %+v", got)
		}
		if got := n.count("getGenesisHash"); got != 2 {
			t.Errorf("getGenesisHash calls = %d, want the client rebuilt", got)
		}
		if n.count("getMultipleAccounts") == calls {
			t.Error("Invalidate kept the cached balances")
		}
	})
}

// A cached read without tokens cannot answer a request for token balances.
func TestBalanceStreamDoesNotAnswerTokensFromANativeOnlyRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, map[string][]map[string]any{
			ownerA: {tokenAccount(usdcMint, ownerA, 5)},
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, []chain.Asset{native("solana-mainnet")}, false))
		got := byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts,
			[]chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}, false)))
		if b := got["a|solana-mainnet:spl:"+usdcMint]; b.Amount != "0.000005" {
			t.Errorf("USDC = %+v, want it read", b)
		}
	})
}

// One holder's failed token read marks only that holder's token balances; its
// SOL balance and the other holders are still answered, and the failed read is
// not cached.
func TestBalanceStreamReportsPartialFailuresPerHolder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1_000_000_000, ownerB: 2_000_000_000}, map[string][]map[string]any{
			ownerA: {tokenAccount(usdcMint, ownerA, 1_000_000)},
		})
		failB := true
		n.handle("getTokenAccountsByOwner", func(params []json.RawMessage) reply {
			if owner := requestedOwner(t, params); owner == ownerB && failB {
				return reply{status: http.StatusInternalServerError, raw: "upstream down"}
			} else if owner == ownerA {
				return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, 1_000_000)})}
			}
			return reply{result: withContext([]any{})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}, {AccountID: "b", Address: ownerB}}
		assets := []chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}
		got := byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false)))
		if b := got["a|solana-mainnet:native"]; b.Amount != "1" || b.Error != "" {
			t.Errorf("A SOL = %+v", b)
		}
		if b := got["a|solana-mainnet:spl:"+usdcMint]; b.Amount != "1" || b.Error != "" {
			t.Errorf("A USDC = %+v", b)
		}
		if b := got["b|solana-mainnet:native"]; b.Amount != "2" || b.Error != "" {
			t.Errorf("B SOL = %+v, want it answered despite the token failure", b)
		}
		if b := got["b|solana-mainnet:spl:"+usdcMint]; b.Amount != "" || !strings.Contains(b.Error, "SPL token balances") {
			t.Errorf("B USDC = %+v, want a token read error", b)
		}

		// B was not cached: the next read asks again and succeeds.
		failB = false
		calls := n.count("getTokenAccountsByOwner")
		got = byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false)))
		if b := got["b|solana-mainnet:spl:"+usdcMint]; b.Amount != "0" || b.Error != "" {
			t.Errorf("B USDC after recovery = %+v", b)
		}
		if got := n.count("getTokenAccountsByOwner") - calls; got != 1 {
			t.Errorf("token reads after recovery = %d, want B only", got)
		}
	})
}

func TestBalanceStreamReportsAFailedSOLReadOnSOLOnly(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, map[string][]map[string]any{ownerA: {tokenAccount(usdcMint, ownerA, 3)}})
		down := true
		n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
			if down {
				return reply{err: &rpcError{Code: -32005, Message: "Node is unhealthy"}}
			}
			return reply{result: withContext([]any{account(systemProgram, 4_000_000_000, nil)})}
		})
		adapter, _ := single(t, n)
		read := func() map[string]chain.Balance {
			return byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet",
				[]chain.AccountAddress{{AccountID: "a", Address: ownerA}},
				[]chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}, false)))
		}
		got := read()
		if b := got["a|solana-mainnet:native"]; !strings.Contains(b.Error, "SOL balance") || b.Amount != "" {
			t.Errorf("SOL = %+v, want an error", b)
		}
		if b := got["a|solana-mainnet:spl:"+usdcMint]; b.Amount != "0.000003" {
			t.Errorf("USDC = %+v, want it answered", b)
		}
		// The incomplete read was not cached: the next one asks again rather
		// than answering zero SOL from memory.
		down = false
		if b := read()["a|solana-mainnet:native"]; b.Amount != "4" || b.Error != "" {
			t.Errorf("SOL after recovery = %+v, want 4", b)
		}
	})
}

// A node that answers with something the filter excluded, or with a shape
// that cannot be a token account, is not summed from.
func TestBalanceStreamRefusesMalformedTokenAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		answer  reply
		wantErr string
	}{
		{name: "another program", answer: reply{result: withContext([]any{map[string]any{
			"pubkey": key(70), "account": account(token2022, 1, tokenSlice(usdcMint, ownerA, 1)),
		}})}, wantErr: "another program"},
		{name: "wrong slice", answer: reply{result: withContext([]any{map[string]any{
			"pubkey": key(70), "account": account(tokenProgram, 1, make([]byte, 165)),
		}})}, wantErr: "165 bytes"},
		{name: "another owner", answer: reply{result: withContext([]any{tokenAccount(usdcMint, ownerB, 1)})}, wantErr: "another owner"},
		{name: "empty entry", answer: reply{result: withContext([]any{nil})}, wantErr: "empty token account"},
		{name: "null result", answer: reply{result: nil}, wantErr: "no result"},
		{name: "not json", answer: reply{raw: "{nope"}, wantErr: "SPL token balances"},
		{name: "wrong types", answer: reply{result: withContext("lots")}, wantErr: "SPL token balances"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
				n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply { return tt.answer })
				adapter, _ := single(t, n)
				got := byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet",
					[]chain.AccountAddress{{AccountID: "a", Address: ownerA}},
					[]chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}, false)))
				if b := got["a|solana-mainnet:spl:"+usdcMint]; !strings.Contains(b.Error, tt.wantErr) || b.Amount != "" {
					t.Errorf("USDC = %+v, want an error mentioning %q", b, tt.wantErr)
				}
				if b := got["a|solana-mainnet:native"]; b.Amount != "0.000000001" {
					t.Errorf("SOL = %+v, want it answered", b)
				}
			})
		})
	}
}

func TestBalanceStreamRefusesAMiscountedSOLAnswer(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
			return reply{result: withContext([]any{account(systemProgram, 1, nil), account(systemProgram, 2, nil)})}
		})
		adapter, _ := single(t, n)
		got := collect(adapter.BalanceStream(t.Context(), "solana-mainnet",
			[]chain.AccountAddress{{AccountID: "a", Address: ownerA}}, []chain.Asset{native("solana-mainnet")}, false))
		if len(got) != 1 || !strings.Contains(got[0].Error, "answered 2 accounts for 1") {
			t.Errorf("balances = %+v, want a miscount error", got)
		}
	})
}

func TestBalanceStreamRefusesBadHoldersAndAssetsIndividually(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{
			{AccountID: "a", Address: ownerA},
			{AccountID: "evm", Address: "0x0000000000000000000000000000000000000001"},
		}
		assets := []chain.Asset{
			native("solana-mainnet"),
			{ID: "x:erc20:0x1", NetworkID: "solana-mainnet", Kind: "erc20", Contract: "0x1"},
			spl("solana-mainnet", "not-a-mint", "BAD", 6),
			native("solana-devnet"),
		}
		got := byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false)))
		if b := got["a|solana-mainnet:native"]; b.Amount != "0.000000001" {
			t.Errorf("A SOL = %+v", b)
		}
		for k, wantErr := range map[string]string{
			"a|x:erc20:0x1":                   "unsupported Solana asset kind",
			"a|solana-mainnet:spl:not-a-mint": "invalid SPL mint",
			"a|solana-devnet:native":          "another network",
			"evm|solana-mainnet:native":       "invalid Solana address",
		} {
			if b := got[k]; !strings.Contains(b.Error, wantErr) {
				t.Errorf("%s = %+v, want %q", k, b, wantErr)
			}
		}
		if len(got) != 8 {
			t.Errorf("balances = %d, want every holder × asset", len(got))
		}
	})
}

func TestBalanceStreamOnANetworkItCannotServeAnswersEveryBalanceWithAnError(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}, {AccountID: "b", Address: ownerB}}
		for networkID, wantErr := range map[string]string{
			"tron-mainnet": "not Solana", "solana-odd": "unknown Solana cluster", "nope": "unknown network",
		} {
			got := collect(adapter.BalanceStream(t.Context(), networkID, accounts, []chain.Asset{native(networkID)}, false))
			if len(got) != 2 {
				t.Errorf("%s: balances = %+v, want one per holder", networkID, got)
			}
			for _, b := range got {
				if !strings.Contains(b.Error, wantErr) {
					t.Errorf("%s: balance = %+v, want %q", networkID, b, wantErr)
				}
			}
		}
		if got := n.total(); got != 0 {
			t.Errorf("RPC calls = %d, want none", got)
		}
	})
}

// An endpoint URL can carry a provider key in its path or query, and a balance
// error goes straight to the browser.
func TestBalanceErrorsDoNotCarryTheEndpointURL(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
			return reply{status: http.StatusBadGateway, raw: "<html>bad gateway</html>"}
		})
		endpoint := n.server.URL + "/v2/secret-key?api-key=secret"
		adapter := newAdapter(t, &provider{endpoints: []string{endpoint}, client: n.server.Client()})
		got := collect(adapter.BalanceStream(t.Context(), "solana-mainnet",
			[]chain.AccountAddress{{AccountID: "a", Address: ownerA}}, []chain.Asset{native("solana-mainnet")}, false))
		if len(got) != 1 || got[0].Error == "" {
			t.Fatalf("balances = %+v, want an error", got)
		}
		if strings.Contains(got[0].Error, "secret") {
			t.Errorf("error = %q, leaks the endpoint credential", got[0].Error)
		}
	})
}

func TestBalanceStreamStopsWhenTheCallerLeaves(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1, ownerB: 1}, nil)
		adapter, _ := single(t, n)
		ctx, cancel := context.WithCancel(t.Context())
		ch := adapter.BalanceStream(ctx, "solana-mainnet",
			[]chain.AccountAddress{{AccountID: "a", Address: ownerA}, {AccountID: "b", Address: ownerB}},
			[]chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}, false)
		<-ch
		cancel()
		for range ch {
		}
		// Shared token reads run on for other streams; let them end, then
		// synctest fails the test if anything is still blocked.
		time.Sleep(time.Minute)
	})
}

// Whatever the stream is doing when the caller leaves — refusing a network,
// refusing a holder, answering from the cache, reading SOL or tokens — it stops
// and closes. synctest fails the test if a goroutine is left blocked.
func TestBalanceStreamStopsAtEveryStage(t *testing.T) {
	t.Parallel()

	accounts := []chain.AccountAddress{
		{AccountID: "bad", Address: "nope"},
		{AccountID: "bad2", Address: "nope2"},
		{AccountID: "a", Address: ownerA},
		{AccountID: "b", Address: ownerB},
	}
	tests := []struct {
		name      string
		networkID string
		holders   []chain.AccountAddress
		tokens    bool
		warm      bool
	}{
		{name: "network refused", networkID: "tron-mainnet", holders: accounts},
		{name: "holder refused", networkID: "solana-mainnet", holders: accounts},
		{name: "cached", networkID: "solana-mainnet", holders: accounts[2:], warm: true},
		{name: "SOL only", networkID: "solana-mainnet", holders: accounts[2:]},
		{name: "tokens", networkID: "solana-mainnet", holders: accounts[2:], tokens: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				n := balanceNode(t, map[string]uint64{ownerA: 1, ownerB: 2}, nil)
				adapter, _ := single(t, n)
				assets := []chain.Asset{native("solana-mainnet"), native("solana-mainnet")}
				if tt.tokens {
					assets = []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6), spl("solana-mainnet", usdtMint, "USDT", 6)}
				}
				if tt.warm {
					collect(adapter.BalanceStream(t.Context(), tt.networkID, tt.holders, assets, false))
				}
				ctx, cancel := context.WithCancel(t.Context())
				ch := adapter.BalanceStream(ctx, tt.networkID, tt.holders, assets, false)
				<-ch
				cancel()
				synctest.Wait()
				for range ch {
				}
				// Shared token reads run on for other streams.
				time.Sleep(time.Minute)
			})
		})
	}
}

// A stream that leaves while waiting for a token-read slot gives it up.
func TestBalanceStreamLeavesTheSlotQueue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			time.Sleep(time.Minute)
			return reply{result: withContext([]any{})}
		})
		adapter, _ := single(t, n)
		var accounts []chain.AccountAddress
		for i := range 6 {
			accounts = append(accounts, chain.AccountAddress{AccountID: fmt.Sprint(i), Address: key(byte(i + 10))})
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		got := collect(adapter.BalanceStream(ctx, "solana-mainnet", accounts, []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}, false))
		// The stream closes; anything it still delivers on the way out is
		// the caller's own deadline, never an amount.
		for _, b := range got {
			if b.Amount != "" || !strings.Contains(b.Error, "deadline") {
				t.Errorf("balance after the caller left = %+v", b)
			}
		}
		// The two reads already sent may finish; the four still waiting for
		// a slot never reach the node.
		time.Sleep(5 * time.Minute)
		if got := n.count("getTokenAccountsByOwner"); got != 2 {
			t.Errorf("token reads that reached the node = %d, want the 2 already sent", got)
		}
	})
}

// A stream that starts after the endpoints were replaced reads from the new
// ones; a read still in flight on the old ones is shown stale and does not end
// up in the cache.
func TestReplacedEndpointsAreNeitherJoinedNorCached(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		old := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		old.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			time.Sleep(5 * time.Second)
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, 1_000_000)})}
		})
		replacement := balanceNode(t, map[string]uint64{ownerA: 1}, map[string][]map[string]any{
			ownerA: {tokenAccount(usdcMint, ownerA, 2_000_000)},
		})
		mux := http.NewServeMux()
		mux.HandleFunc("/old", old.serve)
		mux.HandleFunc("/new", replacement.serve)
		p := &provider{endpoints: []string{"http://rpc.test/old"}, client: newMuxClient(t, mux)}
		adapter := newAdapter(t, p)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}
		read := func() []chain.Balance {
			return collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		}

		first := make(chan []chain.Balance)
		go func() { first <- read() }()
		time.Sleep(time.Second)
		p.setEndpoints("http://rpc.test/new")
		adapter.Invalidate("solana-mainnet")

		second := read()
		if len(second) != 1 || second[0].Amount != "2" || second[0].Stale {
			t.Errorf("stream after the change = %+v, want 2 from the new endpoint", second)
		}
		if got := replacement.count("getTokenAccountsByOwner"); got != 1 {
			t.Errorf("token reads on the new endpoint = %d, want 1", got)
		}
		stale := <-first
		if len(stale) != 1 || stale[0].Amount != "1" || !stale[0].Stale {
			t.Errorf("stream on the old endpoint = %+v, want 1 marked stale", stale)
		}
		third := read()
		if len(third) != 1 || third[0].Amount != "2" {
			t.Errorf("cached read = %+v, want the new endpoint's 2, not the old one's", third)
		}
		if got := replacement.count("getTokenAccountsByOwner"); got != 1 {
			t.Errorf("token reads on the new endpoint = %d, want the third read from the cache", got)
		}
	})
}

// A token read shared by two streams belongs to neither: the stream that
// started it leaving does not fail the other.
func TestASharedTokenReadOutlivesTheStreamThatStartedIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			time.Sleep(5 * time.Second)
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, 1_000_000)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}

		first, leave := context.WithCancel(t.Context())
		firstDone := make(chan []chain.Balance)
		go func() { firstDone <- collect(adapter.BalanceStream(first, "solana-mainnet", accounts, assets, false)) }()
		time.Sleep(time.Second)
		secondDone := make(chan []chain.Balance)
		go func() {
			secondDone <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		}()
		time.Sleep(time.Second)
		leave()
		for _, b := range <-firstDone {
			if b.Amount != "" {
				t.Errorf("the stream that left answered %+v", b)
			}
		}
		second := <-secondDone
		if len(second) != 1 || second[0].Amount != "1" || second[0].Error != "" {
			t.Errorf("second stream = %+v, want USDC 1", second)
		}
		if got := n.count("getTokenAccountsByOwner"); got != 1 {
			t.Errorf("token reads = %d, want one shared read", got)
		}
	})
}

// The same while the shared read is still waiting for a slot: the stream that
// started it leaving does not take it away from the stream still waiting.
func TestAQueuedSharedTokenReadOutlivesTheStreamThatStartedIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		n.handle("getTokenAccountsByOwner", func(params []json.RawMessage) reply {
			time.Sleep(5 * time.Second)
			if requestedOwner(t, params) == ownerA {
				return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, 1_000_000)})}
			}
			return reply{result: withContext([]any{})}
		})
		adapter, _ := single(t, n)
		assets := []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}
		stream := func(ctx context.Context, holders ...string) chan []chain.Balance {
			var accounts []chain.AccountAddress
			for _, holder := range holders {
				accounts = append(accounts, chain.AccountAddress{AccountID: holder, Address: holder})
			}
			done := make(chan []chain.Balance, 1)
			go func() { done <- collect(adapter.BalanceStream(ctx, "solana-mainnet", accounts, assets, false)) }()
			return done
		}

		// Two other holders take both slots for five seconds.
		busy := stream(t.Context(), key(30), key(31))
		time.Sleep(time.Second)
		first, leave := context.WithCancel(t.Context())
		firstDone := stream(first, ownerA)
		time.Sleep(time.Second)
		secondDone := stream(t.Context(), ownerA)
		time.Sleep(time.Second)
		leave()
		<-firstDone
		second := <-secondDone
		if len(second) != 1 || second[0].Amount != "1" || second[0].Error != "" {
			t.Errorf("second stream = %+v, want USDC 1", second)
		}
		<-busy
		if got := n.count("getTokenAccountsByOwner"); got != 3 {
			t.Errorf("token reads = %d, want 3 (two busy, one shared)", got)
		}
	})
}

// A refresh asks about now — typically just after a transfer confirmed — so
// it does not take the answer of a read that was already on its way.
func TestARefreshDoesNotJoinARequestAlreadySent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		var mu sync.Mutex
		held := uint64(1_000_000)
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			mu.Lock()
			amount := held
			mu.Unlock()
			time.Sleep(5 * time.Second)
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, amount)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}
		before := make(chan []chain.Balance)
		go func() {
			before <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		}()
		time.Sleep(time.Second)
		mu.Lock()
		held = 3_000_000
		mu.Unlock()
		refreshed := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, true))
		if len(refreshed) != 1 || refreshed[0].Amount != "3" {
			t.Errorf("refresh = %+v, want the new 3", refreshed)
		}
		<-before
		if got := n.count("getTokenAccountsByOwner"); got != 2 {
			t.Errorf("token reads = %d, want the refresh's own", got)
		}
		if cached := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false)); len(cached) != 1 || cached[0].Amount != "3" {
			t.Errorf("cached = %+v, want 3", cached)
		}
	})
}

// A refresh does join a read that has not been sent yet: it will answer
// from after the refresh was asked for.
func TestARefreshJoinsARequestNotYetSent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		n.handle("getTokenAccountsByOwner", func(params []json.RawMessage) reply {
			time.Sleep(5 * time.Second)
			return reply{result: withContext([]any{})}
		})
		adapter, _ := single(t, n)
		assets := []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}
		stream := func(refresh bool, holders ...string) chan []chain.Balance {
			var accounts []chain.AccountAddress
			for _, holder := range holders {
				accounts = append(accounts, chain.AccountAddress{AccountID: holder, Address: holder})
			}
			done := make(chan []chain.Balance, 1)
			go func() {
				done <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, refresh))
			}()
			return done
		}
		busy := stream(false, key(30), key(31))
		time.Sleep(time.Second)
		queued := stream(false, ownerA)
		time.Sleep(time.Second)
		refreshed := stream(true, ownerA)
		<-busy
		<-queued
		<-refreshed
		if got := n.count("getTokenAccountsByOwner"); got != 3 {
			t.Errorf("token reads = %d, want the refresh to share the queued one", got)
		}
	})
}

// On a public node a read waiting for its paced turn is not yet sent: when
// every stream waiting on it has left, it never reaches the node.
func TestAPacedReadStopsWhenEveryoneLeft(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			time.Sleep(time.Second)
			return reply{result: withContext([]any{})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}, {AccountID: "b", Address: ownerB}}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		collect(adapter.BalanceStream(ctx, "solana-mainnet", accounts, []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}, false))
		time.Sleep(time.Minute)
		if got := n.count("getTokenAccountsByOwner"); got != 1 {
			t.Errorf("token reads that reached the node = %d, want only the one sent before the caller left", got)
		}
	})
}

// An invalidation that lands between a stream's check and its cache write
// still keeps the read out of the cache.
func TestAnInvalidationDuringTheCacheWriteWins(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1_000_000_000}, nil)
		adapter, _ := single(t, n)
		once := sync.Once{}
		solanachain.BeforeCachePut(adapter, func(uint64) {
			once.Do(func() { adapter.Invalidate("solana-mainnet") })
		})
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{native("solana-mainnet")}
		first := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		if len(first) != 1 || !first[0].Stale {
			t.Errorf("first = %+v, want it marked stale", first)
		}
		calls := n.count("getMultipleAccounts")
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		if n.count("getMultipleAccounts") == calls {
			t.Error("a read from before the invalidation was answered from the cache")
		}
	})
}

// A stream that arrives after the last waiter left, while the cancelled read
// has not yet removed itself, starts its own read instead of joining it.
func TestAStreamAfterEveryoneLeftDoesNotJoinTheCancelledRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, map[string][]map[string]any{
			ownerA: {tokenAccount(usdcMint, ownerA, 1_000_000)},
		})
		adapter, _ := single(t, n)
		release := make(chan struct{})
		var once sync.Once
		solanachain.WhenAFlightEnds(adapter, func() { once.Do(func() { <-release }) })
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}

		// Both slots busy, so the first read waits for one.
		n.handle("getTokenAccountsByOwner", func(params []json.RawMessage) reply {
			if requestedOwner(t, params) != ownerA {
				time.Sleep(5 * time.Second)
				return reply{result: withContext([]any{})}
			}
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, 1_000_000)})}
		})
		busyAccounts := []chain.AccountAddress{{AccountID: "x", Address: key(30)}, {AccountID: "y", Address: key(31)}}
		busy := make(chan struct{})
		go func() {
			collect(adapter.BalanceStream(t.Context(), "solana-mainnet", busyAccounts, assets, false))
			close(busy)
		}()
		time.Sleep(time.Second)
		ctx, cancel := context.WithCancel(t.Context())
		left := make(chan struct{})
		go func() {
			collect(adapter.BalanceStream(ctx, "solana-mainnet", accounts, assets, false))
			close(left)
		}()
		time.Sleep(time.Second)
		cancel()
		<-left
		// The cancelled read is held where it ends, before it leaves the group.
		synctest.Wait()
		later := make(chan []chain.Balance)
		go func() {
			later <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		}()
		time.Sleep(time.Second)
		close(release)
		got := <-later
		if len(got) != 1 || got[0].Amount != "1" {
			t.Errorf("later stream = %+v, want USDC 1", got)
		}
		<-busy
	})
}

// Reads finish in any order. A read asked for before another must not replace
// it in the cache by finishing last: here the stream that read 1 SOL is held
// until the refresh that read 2 SOL afterwards has cached its answer. The
// older stream then shows the cached value, not its own.
func TestAnOlderReadFinishingLastDoesNotReplaceANewerOne(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		var mu sync.Mutex
		lamports := uint64(1_000_000_000)
		n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
			mu.Lock()
			defer mu.Unlock()
			return reply{result: withContext([]any{account(systemProgram, lamports, nil)})}
		})
		n.handle("getTokenAccountsByOwner", func(params []json.RawMessage) reply {
			if requestedOwner(t, params) != ownerA {
				time.Sleep(5 * time.Second)
			}
			return reply{result: withContext([]any{})}
		})
		adapter, _ := single(t, n)
		refreshed := make(chan struct{})
		solanachain.BeforeCachePut(adapter, func(written uint64) {
			if written == 1_000_000_000 {
				<-refreshed
			}
		})
		assets := []chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}
		holderA := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}

		// Both slots busy, so the first stream's token read waits for one.
		busy := make(chan struct{})
		go func() {
			collect(adapter.BalanceStream(t.Context(), "solana-mainnet",
				[]chain.AccountAddress{{AccountID: "x", Address: key(30)}, {AccountID: "y", Address: key(31)}}, assets, false))
			close(busy)
		}()
		time.Sleep(time.Second)
		first := make(chan []chain.Balance)
		go func() { first <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", holderA, assets, false)) }()
		time.Sleep(time.Second)
		// A transfer confirmed; the refresh reads the new SOL balance and
		// joins the token read that has not been sent yet.
		mu.Lock()
		lamports = 2_000_000_000
		mu.Unlock()
		second := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", holderA, assets, true))
		close(refreshed)
		older := <-first
		if len(second) != 2 || second[0].Amount != "2" || second[0].Stale {
			t.Errorf("refresh = %+v, want 2 SOL", second)
		}
		// The older stream shows what is cached, the newer 2 SOL, so a screen
		// that keeps the last line per asset does not end on the older one.
		want := map[string]string{"a|solana-mainnet:native": "2", "a|solana-mainnet:spl:" + usdcMint: "0"}
		lines := byKey(t, older)
		if len(lines) != len(want) {
			t.Errorf("older read = %+v, want one line per asset", older)
		}
		for id, amount := range want {
			if b, ok := lines[id]; !ok || b.Amount != amount || b.Stale || b.Error != "" {
				t.Errorf("older %s = %+v, want the cached %s, not stale", id, b, amount)
			}
		}
		cached := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", holderA, assets, false))
		if len(cached) != 2 || cached[0].Amount != "2" {
			t.Errorf("cached = %+v, want the newer 2 SOL", cached)
		}
		<-busy
	})
}

// The same on the token side: an older token request that answers after a
// refresh's own does not replace it, and its stream shows the cached value.
func TestAnOlderTokenAnswerFinishingLastDoesNotReplaceANewerOne(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		var mu sync.Mutex
		held, calls := uint64(1_000_000), 0
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			mu.Lock()
			amount := held
			calls++
			slow := calls == 1
			mu.Unlock()
			if slow {
				time.Sleep(5 * time.Second)
			}
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, amount)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}
		first := make(chan []chain.Balance)
		go func() {
			first <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		}()
		time.Sleep(time.Second)
		mu.Lock()
		held = 3_000_000
		mu.Unlock()
		refreshed := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, true))
		if len(refreshed) != 1 || refreshed[0].Amount != "3" {
			t.Errorf("refresh = %+v, want 3", refreshed)
		}
		if older := <-first; len(older) != 1 || older[0].Amount != "3" || older[0].Stale {
			t.Errorf("older read = %+v, want the cached 3", older)
		}
		cached := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		if len(cached) != 1 || cached[0].Amount != "3" {
			t.Errorf("cached = %+v, want the newer 3", cached)
		}
	})
}

// A holding is as old as its oldest part. A stream that reads SOL now but
// joins a token request sent earlier holds tokens from then; it must not pass
// for newer than a read made in between.
func TestAHoldingIsAsOldAsItsOldestPart(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		var mu sync.Mutex
		held, calls := uint64(1_000_000), 0
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			mu.Lock()
			amount := held
			calls++
			slow := calls == 1
			mu.Unlock()
			if slow {
				time.Sleep(5 * time.Second)
			}
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, amount)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		tokens := []chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}
		started := make(chan []chain.Balance, 1)
		// t=0: a token request is sent and answers five seconds later with 1.
		go func() {
			started <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, tokens, false))
		}()
		time.Sleep(time.Second)
		// t=1: a SOL-only refresh is cached.
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, []chain.Asset{native("solana-mainnet")}, true))
		time.Sleep(time.Second)
		mu.Lock()
		held = 3_000_000
		mu.Unlock()
		// t=2: a stream that needs tokens reads SOL now and joins the token
		// request sent at t=0.
		joined := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, tokens, false))
		<-started
		lines := byKey(t, joined)
		if len(lines) != 2 {
			t.Errorf("joined read = %+v, want a line for SOL and one for USDC", joined)
		}
		for _, id := range []string{"a|solana-mainnet:native", "a|solana-mainnet:spl:" + usdcMint} {
			if b, ok := lines[id]; !ok || !b.Stale {
				t.Errorf("joined %s = %+v, want it present and stale: its tokens are from before the cached read", id, b)
			}
		}
		time.Sleep(time.Second)
		later := byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, tokens, false)))
		if b := later["a|solana-mainnet:spl:"+usdcMint]; b.Amount != "3" {
			t.Errorf("later read = %+v, want 3 read afresh, not 1 from the cache", b)
		}
	})
}

// Only a complete read is cached: after a read whose token part failed, even
// a SOL-only stream asks the node again rather than answering from it.
func TestAnIncompleteReadIsNotCached(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			return reply{err: &rpcError{Code: -32005, Message: "unhealthy"}}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts,
			[]chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}, false))
		calls := n.count("getMultipleAccounts")
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, []chain.Asset{native("solana-mainnet")}, false))
		if n.count("getMultipleAccounts") == calls {
			t.Error("an incomplete read was answered from the cache")
		}
	})
}

// Freshness counts from when a holding was cached, not from when its SOL was
// asked for: a holder whose token read waited twenty seconds is fresh the
// moment it arrives, and the next stream is answered from the cache.
func TestAHoldingIsFreshWhenWritten(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			time.Sleep(20 * time.Second)
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, 1_000_000)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		calls := n.total()
		again := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		if len(again) != 2 || again[0].Stale || again[1].Stale {
			t.Errorf("read right after the write = %+v, want it fresh", again)
		}
		if n.total() != calls {
			t.Error("a holding written a moment ago was read again")
		}
		// Retention counts from the write too: 4 min 50 s after it — 5 min
		// 10 s after the SOL request — the cached value is still shown first,
		// marked stale, before the new read.
		time.Sleep(4*time.Minute + 50*time.Second)
		if got := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false)); len(got) != 4 || !got[0].Stale {
			t.Errorf("read 4 min 50 s after the write = %+v, want the cached value marked stale, then the new read", got)
		}
	})
}

// Two tabs joining one token request already sent hold the same token time —
// older than either SOL read — so the tie is broken by the SOL read: the tab
// that read SOL later is the newer, whichever finishes last.
func TestATieIsBrokenByTheSOLRead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		var mu sync.Mutex
		lamports := uint64(500_000_000)
		n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
			mu.Lock()
			defer mu.Unlock()
			return reply{result: withContext([]any{account(systemProgram, lamports, nil)})}
		})
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			time.Sleep(5 * time.Second)
			return reply{result: withContext([]any{})}
		})
		adapter, _ := single(t, n)
		secondDone := make(chan struct{})
		solanachain.BeforeCachePut(adapter, func(written uint64) {
			if written == 1_000_000_000 {
				<-secondDone
			}
		})
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}
		stream := func() chan []chain.Balance {
			done := make(chan []chain.Balance, 1)
			go func() { done <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false)) }()
			return done
		}
		setLamports := func(value uint64) {
			mu.Lock()
			lamports = value
			mu.Unlock()
		}
		// A third stream sends the token request; both tabs read SOL after
		// it left and join it.
		starter := stream()
		time.Sleep(time.Second)
		setLamports(1_000_000_000)
		first := stream()
		time.Sleep(time.Second)
		setLamports(2_000_000_000)
		second := <-stream()
		close(secondDone)
		<-first
		<-starter
		if len(second) != 2 || second[0].Amount != "2" {
			t.Errorf("second tab = %+v, want 2 SOL", second)
		}
		cached := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		if len(cached) != 2 || cached[0].Amount != "2" {
			t.Errorf("cached = %+v, want the later SOL read, 2", cached)
		}
		if got := n.count("getTokenAccountsByOwner"); got != 1 {
			t.Errorf("token reads = %d, want the one all three joined", got)
		}
	})
}

// The cached value an older read shows instead of its own is marked stale
// when it is itself older than the TTL by then.
func TestTheCachedValueShownInsteadIsStaleWhenOld(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, map[string]uint64{ownerA: 1}, nil)
		var mu sync.Mutex
		held, calls := uint64(1_000_000), 0
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			mu.Lock()
			amount := held
			calls++
			slow := calls == 1
			mu.Unlock()
			if slow {
				time.Sleep(25 * time.Second)
			}
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, amount)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}
		first := make(chan []chain.Balance)
		go func() {
			first <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		}()
		time.Sleep(time.Second)
		mu.Lock()
		held = 3_000_000
		mu.Unlock()
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, true))
		// The older request answers 24 s after the refresh was cached.
		if older := <-first; len(older) != 1 || older[0].Amount != "3" || !older[0].Stale {
			t.Errorf("older read = %+v, want the cached 3 marked stale", older)
		}
	})
}

// A read that partly fails after a newer one was cached shows the cached
// values, not its older SOL next to an error: here the main pass's token
// request was sent before a transfer confirmed and fails after the refresh.
func TestAPartialFailureShowsTheNewerCachedValues(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, nil)
		var mu sync.Mutex
		lamports, held, calls := uint64(1_000_000_000), uint64(1_000_000), 0
		n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
			mu.Lock()
			defer mu.Unlock()
			return reply{result: withContext([]any{account(systemProgram, lamports, nil)})}
		})
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			mu.Lock()
			amount := held
			calls++
			first := calls == 1
			mu.Unlock()
			if first {
				time.Sleep(5 * time.Second)
				return reply{err: &rpcError{Code: 429, Message: "Too many requests"}}
			}
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, amount)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}
		main := make(chan []chain.Balance)
		go func() { main <- collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false)) }()
		time.Sleep(time.Second)
		mu.Lock()
		lamports, held = 2_000_000_000, 3_000_000
		mu.Unlock()
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, true))
		got := byKey(t, <-main)
		want := map[string]string{"a|solana-mainnet:native": "2", "a|solana-mainnet:spl:" + usdcMint: "3"}
		if len(got) != len(want) {
			t.Errorf("main pass = %+v, want one line per asset", got)
		}
		for id, amount := range want {
			if b := got[id]; b.Amount != amount || b.Error != "" || b.Stale {
				t.Errorf("main pass %s = %+v, want the cached %s", id, b, amount)
			}
		}
	})
}

// Without a newer cached read, a partial failure is shown as it is: the SOL
// that was read and an error for the tokens — also when an older read is
// cached.
func TestAPartialFailureWithoutANewerCacheIsShownAsItIs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := balanceNode(t, nil, map[string][]map[string]any{ownerA: {tokenAccount(usdcMint, ownerA, 1_000_000)}})
		var mu sync.Mutex
		lamports := uint64(1_000_000_000)
		n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
			mu.Lock()
			defer mu.Unlock()
			return reply{result: withContext([]any{account(systemProgram, lamports, nil)})}
		})
		adapter, _ := single(t, n)
		accounts := []chain.AccountAddress{{AccountID: "a", Address: ownerA}}
		assets := []chain.Asset{native("solana-mainnet"), spl("solana-mainnet", usdcMint, "USDC", 6)}
		// An older complete read is cached.
		collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, false))
		mu.Lock()
		lamports = 2_000_000_000
		mu.Unlock()
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			return reply{err: &rpcError{Code: 429, Message: "Too many requests"}}
		})
		got := byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, true)))
		if b := got["a|solana-mainnet:native"]; b.Amount != "2" || b.Stale {
			t.Errorf("SOL = %+v, want the 2 just read, not the older cached 1", b)
		}
		if b := got["a|solana-mainnet:spl:"+usdcMint]; !strings.Contains(b.Error, "Too many requests") {
			t.Errorf("USDC = %+v, want the error", b)
		}

		// The other way round: SOL fails and the tokens are read. The read is
		// dated by its tokens, newer than the cached one, so SOL stays an
		// error rather than the older cached value.
		n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
			return reply{err: &rpcError{Code: 429, Message: "Too many requests"}}
		})
		n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
			return reply{result: withContext([]any{tokenAccount(usdcMint, ownerA, 5_000_000)})}
		})
		time.Sleep(time.Second)
		got = byKey(t, collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts, assets, true)))
		if b := got["a|solana-mainnet:native"]; !strings.Contains(b.Error, "Too many requests") {
			t.Errorf("SOL = %+v, want the error, not the older cached value", b)
		}
		if b := got["a|solana-mainnet:spl:"+usdcMint]; b.Amount != "5" {
			t.Errorf("USDC = %+v, want the 5 just read", b)
		}
	})
}
