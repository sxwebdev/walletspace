package raydium_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

// gpaCapture is testdata/gpa_sol_usdc.json: four getProgramAccounts
// requests sent to a public mainnet node and the keys it returned.
type gpaCapture map[string]struct {
	Program  string   `json:"program"`
	DataSize uint64   `json:"dataSize"`
	Memcmp   [][]any  `json:"memcmp"`
	Keys     []string `json:"keys"`
}

// fakeRPC answers from the captures. It records every call.
type fakeRPC struct {
	mu       sync.Mutex
	queries  []raydium.ProgramAccountsQuery
	batches  [][]solana.PublicKey
	gpa      map[string][]solana.PublicKey // by query fingerprint
	accounts map[solana.PublicKey]*raydium.AccountData
	gpaErr   error
	gmaErr   error
	gmaFrom  int // fail from this MultipleAccounts call on (1-based); 0: from the first
	short    bool
}

func fingerprint(program solana.PublicKey, size uint64, filters []raydium.Memcmp) string {
	var b bytes.Buffer
	b.Write(program[:])
	b.Write(binary.LittleEndian.AppendUint64(nil, size))
	for _, f := range filters {
		b.Write(binary.LittleEndian.AppendUint64(nil, f.Offset))
		b.Write(f.Bytes)
	}
	return b.String()
}

func (f *fakeRPC) ProgramAccountKeys(_ context.Context, q raydium.ProgramAccountsQuery) ([]solana.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.gpaErr != nil {
		return nil, f.gpaErr
	}
	return f.gpa[fingerprint(q.Program, q.DataSize, q.Memcmp)], nil
}

func (f *fakeRPC) MultipleAccounts(_ context.Context, keys []solana.PublicKey) ([]*raydium.AccountData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, slices.Clone(keys))
	if f.gmaErr != nil && len(f.batches) >= f.gmaFrom {
		return nil, f.gmaErr
	}
	out := make([]*raydium.AccountData, len(keys))
	for i, k := range keys {
		if a, ok := f.accounts[k]; ok {
			c := *a
			c.Data = slices.Clone(a.Data)
			out[i] = &c
		}
	}
	if f.short {
		out = out[:len(out)-1]
	}
	return out, nil
}

func newFakeRPC(t *testing.T) *fakeRPC {
	t.Helper()
	var capture gpaCapture
	readJSON(t, "gpa_sol_usdc.json", &capture)
	f := &fakeRPC{gpa: map[string][]solana.PublicKey{}, accounts: map[solana.PublicKey]*raydium.AccountData{}}
	for name, q := range capture {
		if name == "_meta" {
			continue
		}
		var filters []raydium.Memcmp
		for _, m := range q.Memcmp {
			filters = append(filters, raydium.Memcmp{Offset: uint64(m[0].(float64)), Bytes: key(m[1].(string)).Bytes()})
		}
		var keys []solana.PublicKey
		for _, k := range q.Keys {
			keys = append(keys, key(k))
		}
		f.gpa[fingerprint(key(q.Program), q.DataSize, filters)] = keys
	}
	var snap struct {
		Accounts map[string]*struct {
			Owner    string `json:"owner"`
			Lamports uint64 `json:"lamports"`
			Data     string `json:"data"`
		} `json:"accounts"`
	}
	readJSON(t, "discovery_sol_usdc_accounts.json", &snap)
	for k, a := range snap.Accounts {
		if a == nil {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(a.Data)
		if err != nil {
			t.Fatal(err)
		}
		f.accounts[key(k)] = &raydium.AccountData{Owner: key(a.Owner), Lamports: a.Lamports, Data: data}
	}
	return f
}

var discoveryTime = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func TestDiscoverOnCapturedChain(t *testing.T) {
	t.Parallel()
	for _, dir := range []struct{ in, out solana.PublicKey }{{wsol, usdc}, {usdc, wsol}} {
		t.Run(dir.in.String()[:4]+"→"+dir.out.String()[:4], func(t *testing.T) {
			t.Parallel()
			rpc := newFakeRPC(t)
			got, err := raydium.Discover(t.Context(), rpc, dir.in, dir.out, discoveryTime)
			if err != nil {
				t.Fatal(err)
			}
			// Every query is one the public node answered in the capture:
			// AMM v4 both orders, CPMM and CLMM sorted (SOL < USDC as bytes).
			if len(rpc.queries) != 4 {
				t.Fatalf("%d getProgramAccounts calls, want 4", len(rpc.queries))
			}
			for _, q := range rpc.queries {
				if _, ok := rpc.gpa[fingerprint(q.Program, q.DataSize, q.Memcmp)]; !ok {
					t.Errorf("query %+v is not one of the captured ones", q)
				}
			}
			// 51 pools in one read, then their vaults.
			if len(rpc.batches) < 2 || len(rpc.batches[0]) != 51 {
				t.Fatalf("batches %d, first of %d keys", len(rpc.batches), len(rpc.batches[0]))
			}
			for _, b := range rpc.batches {
				if len(b) > raydium.MaxAccountsPerRequest {
					t.Errorf("a batch of %d keys", len(b))
				}
			}
			if len(got) < 10 {
				t.Fatalf("%d candidates", len(got))
			}
			// The deepest pool for both directions is the AMM v4 pool
			// api-v3 lists with by far the largest TVL.
			if got[0].Pool != key("58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2") || got[0].Kind != raydium.KindAMMV4 || got[0].AMMV4 == nil {
				t.Errorf("first candidate %s (%s)", got[0].Pool, got[0].Kind)
			}
			seen := map[solana.PublicKey]bool{}
			for i, c := range got {
				if seen[c.Pool] {
					t.Errorf("pool %s listed twice", c.Pool)
				}
				seen[c.Pool] = true
				if i > 0 && c.OutputReserve > got[i-1].OutputReserve {
					t.Errorf("candidate %d has more output than %d", i, i-1)
				}
				// Reserves are what the vault token accounts hold, read
				// with the SPL token layout from the snapshot.
				for _, v := range []struct {
					vault   solana.PublicKey
					mint    solana.PublicKey
					reserve uint64
				}{{c.InputVault, dir.in, c.InputReserve}, {c.OutputVault, dir.out, c.OutputReserve}} {
					a := rpc.accounts[v.vault]
					if a == nil || !bytes.Equal(a.Data[:32], v.mint[:]) || binary.LittleEndian.Uint64(a.Data[64:]) != v.reserve {
						t.Errorf("pool %s vault %s: reserve %d", c.Pool, v.vault, v.reserve)
					}
				}
				set := 0
				for _, p := range []bool{c.AMMV4 != nil, c.CPMM != nil, c.CLMM != nil} {
					if p {
						set++
					}
				}
				if set != 1 || raydium.KindOf(rpc.accounts[c.Pool].Owner) != c.Kind {
					t.Errorf("pool %s: kind %s with %d decoded pools", c.Pool, c.Kind, set)
				}
			}
			// The CLMM pools the Trade API routed through are found too.
			for _, id := range []string{"3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv", "2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv", "8sLbNZoA1cfnvMJLPfp98ZLAnFSYCFApfJKMbiXNLwxj", "47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4"} {
				if !seen[key(id)] {
					t.Errorf("pool %s not found", id)
				}
			}
		})
	}
}

func TestDiscoverLeavesOutBadPools(t *testing.T) {
	t.Parallel()
	pool := key("58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2")
	coinVault := key("DQyrAcCrDXQ7NeoqGgDCZwBvWDcYmFCjSb9JtteuvPpz")
	pcVault := key("HLmqeL62xR1QoZ1HKKbXRrdN1p3phKpxRMb2VVopvBBz")
	tests := []struct {
		name string
		edit func(f *fakeRPC)
	}{
		{"pool disabled", func(f *fakeRPC) { binary.LittleEndian.PutUint64(f.accounts[pool].Data[0:], 2) }},
		{"pool missing", func(f *fakeRPC) { delete(f.accounts, pool) }},
		{"pool owned by another program", func(f *fakeRPC) { f.accounts[pool].Owner = solana.SystemProgramID }},
		{"pool undecodable", func(f *fakeRPC) { f.accounts[pool].Data = f.accounts[pool].Data[:700] }},
		{"pool for another pair", func(f *fakeRPC) { copy(f.accounts[pool].Data[432:464], usdt[:]) }},
		{"vault missing", func(f *fakeRPC) { delete(f.accounts, coinVault) }},
		{"vault not a token account", func(f *fakeRPC) { f.accounts[pcVault].Owner = solana.Token2022ProgramID }},
		{"vault of another mint", func(f *fakeRPC) { copy(f.accounts[pcVault].Data[:32], usdt[:]) }},
		{"vault held by someone else", func(f *fakeRPC) { copy(f.accounts[coinVault].Data[32:64], usdt[:]) }},
		{"vault truncated", func(f *fakeRPC) { f.accounts[coinVault].Data = f.accounts[coinVault].Data[:100] }},
		{"output vault empty", func(f *fakeRPC) { binary.LittleEndian.PutUint64(f.accounts[pcVault].Data[64:], 0) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rpc := newFakeRPC(t)
			tt.edit(rpc)
			got, err := raydium.Discover(t.Context(), rpc, wsol, usdc, discoveryTime)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range got {
				if c.Pool == pool {
					t.Fatalf("%s: the pool is still a candidate", tt.name)
				}
			}
			if len(got) < 10 {
				t.Fatalf("one bad pool cost %d others", 50-len(got))
			}
		})
	}
	// A CPMM or CLMM pool that the node returned for the pair but that
	// trades something else is left out.
	for _, id := range []string{"47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4", "3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv"} {
		rpc := newFakeRPC(t)
		data := rpc.accounts[key(id)].Data
		if len(data) == 637 {
			copy(data[200:232], usdt[:]) // CPMM token_1_mint
		} else {
			copy(data[105:137], usdt[:]) // CLMM token_mint_1
		}
		got, err := raydium.Discover(t.Context(), rpc, wsol, usdc, discoveryTime)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range got {
			if c.Pool == key(id) {
				t.Errorf("pool %s for another pair is a candidate", id)
			}
		}
	}
	// Each kind's own swap check applies: a CPMM pool not open yet and a
	// CLMM pool with swaps disabled are left out.
	rpc := newFakeRPC(t)
	cpmm, clmm := key("47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4"), key("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv")
	binary.LittleEndian.PutUint64(rpc.accounts[cpmm].Data[373:], uint64(discoveryTime.Unix())+1)
	rpc.accounts[clmm].Data[389] |= 1 << 4
	got, err := raydium.Discover(t.Context(), rpc, wsol, usdc, discoveryTime)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.Pool == cpmm || c.Pool == clmm {
			t.Errorf("pool %s is still a candidate", c.Pool)
		}
	}
}

// Pools with the same output reserve are ordered by key, so the ranking
// does not depend on the order the node listed them in.
func TestDiscoverTieBreak(t *testing.T) {
	t.Parallel()
	rpc := newFakeRPC(t)
	pools := []solana.PublicKey{key("47hq28mcL7q5GhBg7epyGF2dnuJd4MKFt8QhT7CzYUp4"), key("3ucNos4NbumPLZNWztqGHNFFgkHeRMBQAVemeeomsUxv")}
	vaults := []solana.PublicKey{key("BArWLNarRzS7N1GCwBjNgVnC4866c8oJNRqhb5axf6y4"), key("5it83u57VRrVgc51oNV19TTmAJuffPx5GtGwQr7gQNUo")} // their USDC vaults
	const deep = 1 << 60
	for _, v := range vaults {
		binary.LittleEndian.PutUint64(rpc.accounts[v].Data[64:], deep)
	}
	got, err := raydium.Discover(t.Context(), rpc, wsol, usdc, discoveryTime)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(pools, func(a, b solana.PublicKey) int { return bytes.Compare(a[:], b[:]) })
	if got[0].Pool != pools[0] || got[1].Pool != pools[1] || got[0].OutputReserve != deep || got[1].OutputReserve != deep {
		t.Fatalf("first two: %s %s, want %s %s", got[0].Pool, got[1].Pool, pools[0], pools[1])
	}
}

func TestDiscoverErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	tests := []struct {
		name string
		edit func(f *fakeRPC)
		in   solana.PublicKey
		want error
	}{
		{"getProgramAccounts fails", func(f *fakeRPC) { f.gpaErr = boom }, wsol, boom},
		{"getMultipleAccounts fails", func(f *fakeRPC) { f.gmaErr = boom }, wsol, boom},
		{"reading the vaults fails", func(f *fakeRPC) { f.gmaErr, f.gmaFrom = boom, 2 }, wsol, boom},
		{"short answer", func(f *fakeRPC) { f.short = true }, wsol, nil},
		{"no pools", func(f *fakeRPC) { f.gpa = nil }, wsol, raydium.ErrNoPool},
		{"no usable pools", func(f *fakeRPC) { f.accounts = nil }, wsol, raydium.ErrNoPool},
		{"same mints", func(*fakeRPC) {}, usdc, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rpc := newFakeRPC(t)
			tt.edit(rpc)
			_, err := raydium.Discover(t.Context(), rpc, tt.in, usdc, discoveryTime)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDiscoverReadsInBatches(t *testing.T) {
	t.Parallel()
	rpc := newFakeRPC(t)
	// 150 more keys that do not exist on chain, in front of the real ones.
	for fp, keys := range rpc.gpa {
		var extra []solana.PublicKey
		for i := range 150 {
			extra = append(extra, solana.PublicKeyFromBytes(slices.Repeat([]byte{byte(i), 0xee}, 16)))
		}
		rpc.gpa[fp] = append(extra, keys...)
		break
	}
	got, err := raydium.Discover(t.Context(), rpc, wsol, usdc, discoveryTime)
	if err != nil {
		t.Fatal(err)
	}
	want, err := raydium.Discover(t.Context(), newFakeRPC(t), wsol, usdc, discoveryTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d candidates with padding, %d without", len(got), len(want))
	}
	for i := range got {
		if got[i].Pool != want[i].Pool || got[i].OutputReserve != want[i].OutputReserve {
			t.Fatalf("candidate %d differs", i)
		}
	}
	// 150 + 51 pools: 100, 100 and 1 keys.
	if len(rpc.batches) < 3 || len(rpc.batches[0]) != 100 || len(rpc.batches[1]) != 100 || len(rpc.batches[2]) != 1 {
		t.Fatalf("pool batches of %d, %d and %d keys", len(rpc.batches[0]), len(rpc.batches[1]), len(rpc.batches[2]))
	}
}
