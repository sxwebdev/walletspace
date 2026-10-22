package raydium

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gagliardetto/solana-go"
)

// MaxAccountsPerRequest is the most keys one MultipleAccounts call carries;
// getMultipleAccounts accepts 100.
const MaxAccountsPerRequest = 100

// SPL token account layout (spl-token state.rs Account): mint at 0, owner
// (the authority) at 32, amount at 64, 165 bytes.
const (
	tokenAccountSize        = 165
	tokenAccountOwnerOffset = 32
	tokenAccountAmount      = 64
)

// Memcmp is a getProgramAccounts memcmp filter.
type Memcmp struct {
	Offset uint64
	Bytes  []byte
}

// ProgramAccountsQuery is a getProgramAccounts request for the keys only:
// the implementation asks for a dataSlice of length 0, since the pools are
// read afterwards in one batch.
type ProgramAccountsQuery struct {
	Program  solana.PublicKey
	DataSize uint64
	Memcmp   []Memcmp
}

// AccountData is an account as getMultipleAccounts returns it.
type AccountData struct {
	Owner    solana.PublicKey
	Lamports uint64
	Data     []byte
}

// RPC is what discovery needs from a node. The caller's implementation
// paces the calls; public nodes allow getProgramAccounts about ten times in
// ten seconds.
type RPC interface {
	// ProgramAccountKeys returns the keys of the program's accounts that
	// match the query.
	ProgramAccountKeys(ctx context.Context, q ProgramAccountsQuery) ([]solana.PublicKey, error)
	// MultipleAccounts returns the accounts in the order of keys, nil for a
	// missing one; it is never given more than MaxAccountsPerRequest keys.
	MultipleAccounts(ctx context.Context, keys []solana.PublicKey) ([]*AccountData, error)
}

// Candidate is a pool that trades the two mints and allows a swap now, with
// the reserves it showed.
type Candidate struct {
	Pool        solana.PublicKey
	Kind        Kind
	InputVault  solana.PublicKey
	OutputVault solana.PublicKey
	// InputReserve and OutputReserve are the vault balances.
	InputReserve  uint64
	OutputReserve uint64
	// Exactly one of the pools is set, by Kind.
	AMMV4 *AMMV4Pool
	CPMM  *CPMMPool
	CLMM  *CLMMPool
}

// ErrNoPool means discovery found no pool for the pair that allows a swap.
var ErrNoPool = errors.New("raydium: no pool for the pair")

// Discover finds the pools of the three programs trading inputMint against
// outputMint without the Trade API, and ranks them by the balance of their
// output vault, deepest first.
//
// It asks getProgramAccounts four times — AMM v4 in both mint orders, CPMM
// and CLMM in the sorted order their programs require (cp-swap
// initialize.rs and clmm create_pool.rs: token_mint_0 < token_mint_1) — then
// reads the pools, then their vaults, MaxAccountsPerRequest keys a call.
// Pools that do not decode, do not allow a swap at now, or whose vaults are
// not token accounts of the right mint held by the pool's authority, are left
// out.
func Discover(ctx context.Context, rpc RPC, inputMint, outputMint solana.PublicKey, now time.Time) ([]Candidate, error) {
	if inputMint == outputMint {
		return nil, fmt.Errorf("raydium: cannot swap %s for itself", inputMint)
	}
	low, high := inputMint, outputMint
	if bytes.Compare(low[:], high[:]) > 0 {
		low, high = high, low
	}
	pair := func(program solana.PublicKey, size, first, second uint64, a, b solana.PublicKey) ProgramAccountsQuery {
		return ProgramAccountsQuery{Program: program, DataSize: size, Memcmp: []Memcmp{
			{Offset: first, Bytes: a.Bytes()}, {Offset: second, Bytes: b.Bytes()},
		}}
	}
	queries := []ProgramAccountsQuery{
		pair(AMMV4Program, ammV4Size, ammV4CoinMintOffset, ammV4PcMintOffset, inputMint, outputMint),
		pair(AMMV4Program, ammV4Size, ammV4CoinMintOffset, ammV4PcMintOffset, outputMint, inputMint),
		pair(CPMMProgram, cpmmPoolSize, cpmmToken0MintOffset, cpmmToken1MintOffset, low, high),
		pair(CLMMProgram, clmmPoolSize, clmmMint0Offset, clmmMint1Offset, low, high),
	}
	var keys []solana.PublicKey
	seen := map[solana.PublicKey]bool{}
	for _, q := range queries {
		found, err := rpc.ProgramAccountKeys(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("raydium: finding %s pools: %w", KindOf(q.Program), err)
		}
		for _, key := range found {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	pools, err := fetch(ctx, rpc, keys)
	if err != nil {
		return nil, err
	}
	var candidates []Candidate
	var vaults []solana.PublicKey
	for i, account := range pools {
		if account == nil {
			continue
		}
		c, ok := candidate(keys[i], account, inputMint, outputMint, now)
		if !ok {
			continue
		}
		candidates = append(candidates, c)
		vaults = append(vaults, c.InputVault, c.OutputVault)
	}
	balances, err := fetch(ctx, rpc, vaults)
	if err != nil {
		return nil, err
	}
	ranked := candidates[:0]
	for i, c := range candidates {
		authority := c.Pool // CLMM vaults are held by the pool itself.
		switch c.Kind {
		case KindAMMV4:
			authority = c.AMMV4.Authority
		case KindCPMM:
			authority = CPMMAuthority
		}
		var ok bool
		if c.InputReserve, ok = vaultBalance(balances[2*i], inputMint, authority); !ok {
			continue
		}
		if c.OutputReserve, ok = vaultBalance(balances[2*i+1], outputMint, authority); !ok || c.OutputReserve == 0 {
			continue
		}
		ranked = append(ranked, c)
	}
	if len(ranked) == 0 {
		return nil, ErrNoPool
	}
	slices.SortStableFunc(ranked, func(a, b Candidate) int {
		if c := cmp.Compare(b.OutputReserve, a.OutputReserve); c != 0 {
			return c
		}
		return bytes.Compare(a.Pool[:], b.Pool[:])
	})
	return ranked, nil
}

// fetch reads accounts MaxAccountsPerRequest at a time and checks the node
// answered for each key.
func fetch(ctx context.Context, rpc RPC, keys []solana.PublicKey) ([]*AccountData, error) {
	out := make([]*AccountData, 0, len(keys))
	for chunk := range slices.Chunk(keys, MaxAccountsPerRequest) {
		accounts, err := rpc.MultipleAccounts(ctx, chunk)
		if err != nil {
			return nil, fmt.Errorf("raydium: reading accounts: %w", err)
		}
		if len(accounts) != len(chunk) {
			return nil, fmt.Errorf("raydium: asked for %d accounts, got %d", len(chunk), len(accounts))
		}
		out = append(out, accounts...)
	}
	return out, nil
}

// candidate decodes a pool by its owner and checks it trades the pair and
// allows a swap now.
func candidate(key solana.PublicKey, account *AccountData, inputMint, outputMint solana.PublicKey, now time.Time) (Candidate, bool) {
	c := Candidate{Pool: key, Kind: KindOf(account.Owner)}
	switch c.Kind {
	case KindAMMV4:
		p, err := DecodeAMMV4Pool(account.Owner, account.Data)
		if err != nil || p.Swappable(now) != nil {
			return Candidate{}, false
		}
		switch {
		case p.CoinMint == inputMint && p.PcMint == outputMint:
			c.InputVault, c.OutputVault = p.CoinVault, p.PcVault
		case p.PcMint == inputMint && p.CoinMint == outputMint:
			c.InputVault, c.OutputVault = p.PcVault, p.CoinVault
		default:
			return Candidate{}, false
		}
		c.AMMV4 = &p
	case KindCPMM:
		p, err := DecodeCPMMPool(account.Owner, account.Data)
		if err != nil || p.Swappable(now) != nil {
			return Candidate{}, false
		}
		switch {
		case p.Token0Mint == inputMint && p.Token1Mint == outputMint:
			c.InputVault, c.OutputVault = p.Token0Vault, p.Token1Vault
		case p.Token1Mint == inputMint && p.Token0Mint == outputMint:
			c.InputVault, c.OutputVault = p.Token1Vault, p.Token0Vault
		default:
			return Candidate{}, false
		}
		c.CPMM = &p
	case KindCLMM:
		p, err := DecodeCLMMPool(account.Owner, account.Data)
		if err != nil || p.Swappable(now) != nil {
			return Candidate{}, false
		}
		switch {
		case p.Mint0 == inputMint && p.Mint1 == outputMint:
			c.InputVault, c.OutputVault = p.Vault0, p.Vault1
		case p.Mint1 == inputMint && p.Mint0 == outputMint:
			c.InputVault, c.OutputVault = p.Vault1, p.Vault0
		default:
			return Candidate{}, false
		}
		c.CLMM = &p
	default:
		return Candidate{}, false
	}
	return c, true
}

// vaultBalance reads a vault: a classic SPL token account of the mint, held
// by the pool's authority.
func vaultBalance(account *AccountData, mint, authority solana.PublicKey) (uint64, bool) {
	if account == nil || account.Owner != solana.TokenProgramID || len(account.Data) != tokenAccountSize {
		return 0, false
	}
	l := layout(account.Data)
	if l.key(0) != mint || l.key(tokenAccountOwnerOffset) != authority {
		return 0, false
	}
	return binary.LittleEndian.Uint64(account.Data[tokenAccountAmount:]), true
}
