package solana

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/config"
)

const (
	// balanceTTL is how long a read balance counts as fresh, the same as Tron.
	balanceTTL = 15 * time.Second
	// balanceRetention keeps the last known value for immediate
	// stale-while-revalidate rendering, the same as Tron.
	balanceRetention = 5 * time.Minute
	// maxAccountsPerCall is getMultipleAccounts' own ceiling.
	maxAccountsPerCall = 100
	// tokenAccountSlice is the prefix of an SPL token account that holds the
	// mint (0–32), the owner (32–64) and the amount (64–72). Asking for only
	// this much keeps a listing of a spam-heavy wallet small.
	tokenAccountSlice = 72
	// tokenReadTimeout bounds each request of a shared token read once it is
	// sent; waiting for a slot or a paced turn is not part of it. It is the
	// guarded HTTP client's own request timeout.
	tokenReadTimeout = 30 * time.Second
	// readCommitment is what balances are read at: voted on by a
	// supermajority, so a balance shown is not one a fork can take back.
	readCommitment = rpc.CommitmentConfirmed
)

// holding is what one address holds: lamports and, when tokens were read,
// the raw sum per mint of every classic SPL token account it owns.
type holding struct {
	lamports uint64
	tokens   map[solana.PublicKey]*big.Int
	// at is when the oldest part of the holding was asked for: the start of
	// the earliest request whose answer it contains. solAt is when its SOL
	// part was. Together they order reads: the one asked for later wins.
	at    time.Time
	solAt time.Time
	// written is when it was cached. Freshness and retention count from
	// here: a holding whose token read waited in the queue is not old the
	// moment it arrives.
	written time.Time
}

// newer reports whether h was asked for after other: its oldest part later,
// or on a tie, its SOL part.
func (h holding) newer(other holding) bool {
	return h.at.After(other.at) || (h.at.Equal(other.at) && h.solAt.After(other.solAt))
}

// balanceCache remembers each address's last complete read per network.
type balanceCache struct {
	mu      sync.Mutex
	items   map[string]holding
	flights map[string]*tokenFlight
	// generations counts each network's invalidations. A read records the
	// generation it started under and is cached only if it is still current,
	// checked in the same critical section as the write.
	generations map[string]uint64
}

func newBalanceCache() *balanceCache {
	return &balanceCache{
		items: make(map[string]holding), flights: make(map[string]*tokenFlight),
		generations: make(map[string]uint64),
	}
}

func (c *balanceCache) generation(networkID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generations[networkID]
}

func cacheKey(networkID string, owner solana.PublicKey) string {
	return networkID + "\x00" + owner.String()
}

func (c *balanceCache) get(networkID string, owner solana.PublicKey) (holding, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey(networkID, owner)
	item, ok := c.items[key]
	if ok && time.Since(item.written) > balanceRetention {
		delete(c.items, key)
		return holding{}, false
	}
	return item, ok
}

// put caches a read made under generation and reports what the cache holds
// for the owner afterwards. Reads finish in any order, and the last one to
// finish is not the newest when an older request was slower: a read asked
// for before the cached one is not written, and the cached one is returned
// with written false. After an invalidation nothing is written and ok is
// false.
func (c *balanceCache) put(
	networkID string, generation uint64, owner solana.PublicKey, item holding,
) (held holding, written, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generations[networkID] != generation {
		return holding{}, false, false
	}
	key := cacheKey(networkID, owner)
	if existing, found := c.items[key]; found && existing.newer(item) {
		return existing, false, true
	}
	item.written = time.Now()
	c.items[key] = item
	return item, true, true
}

func (c *balanceCache) dropNetwork(networkID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generations[networkID]++
	prefix := networkID + "\x00"
	for key := range c.items {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(c.items, key)
		}
	}
}

// BalanceStream reads SOL and SPL balances of every holder.
//
// Cached reads younger than balanceTTL are answered from memory; older ones
// are sent first marked stale and then read again, as the Tron adapter does.
// refresh skips the cache.
//
// The SOL balances of all holders come from one getMultipleAccounts call per
// hundred addresses. Token balances come from one getTokenAccountsByOwner call
// per holder, restricted to the classic token program and sliced to the
// mint/owner/amount prefix: an SPL balance is the sum of every token account
// of that mint the holder owns, not only its associated token account, so
// tokens held in another account are not invisible. Token-2022 accounts are
// not read: those mints cannot be added.
//
// A failure is reported per holder and per kind: a failed SOL read marks the
// SOL balances of the holders in that call, a failed token read marks that
// holder's token balances, and everything else is still answered.
func (a *Adapter) BalanceStream(
	ctx context.Context, networkID string, accounts []chain.AccountAddress, assets []chain.Asset, refresh bool,
) <-chan chain.Balance {
	out := make(chan chain.Balance)
	go func() {
		defer close(out)
		send := func(balance chain.Balance) bool {
			select {
			case out <- balance:
				return true
			case <-ctx.Done():
				return false
			}
		}
		failAll := func(holders []chain.AccountAddress, err error) bool {
			message := config.RedactError(err).Error()
			for _, holder := range holders {
				for _, asset := range assets {
					if !send(chain.Balance{AccountID: holder.AccountID, AssetID: asset.ID, Error: message}) {
						return false
					}
				}
			}
			return true
		}
		// Taken before the client: an invalidation after this point, however
		// it interleaves with the reads, keeps them out of the cache.
		generation := a.balances.generation(networkID)
		_, nodes, err := a.client(ctx, networkID)
		if err != nil {
			failAll(accounts, err)
			return
		}
		needTokens := false
		for _, asset := range assets {
			if asset.Kind == "spl" {
				needTokens = true
			}
		}
		// emit sends a holder's balances; SOL and tokens may come from reads
		// of different ages, so each has its own stale flag.
		emit := func(holder chain.AccountAddress, item holding, staleSOL, staleTokens bool, nativeErr, tokenErr error) bool {
			for _, asset := range assets {
				balance := chain.Balance{AccountID: holder.AccountID, AssetID: asset.ID, Stale: staleSOL}
				if asset.Kind == "spl" {
					balance.Stale = staleTokens
				}
				amount, err := assetAmount(networkID, asset, item, nativeErr, tokenErr)
				if err != nil {
					balance.Error = config.RedactError(err).Error()
				} else {
					balance.Amount = amount
				}
				if !send(balance) {
					return false
				}
			}
			return true
		}

		// Holders are read per address: two accounts with one address cost
		// one read.
		byOwner := make(map[solana.PublicKey][]chain.AccountAddress)
		var owners []solana.PublicKey
		for _, holder := range accounts {
			owner, err := solana.PublicKeyFromBase58(holder.Address)
			if err != nil {
				if !failAll([]chain.AccountAddress{holder}, fmt.Errorf(
					"%w: invalid Solana address", chain.ErrInvalidRequest,
				)) {
					return
				}
				continue
			}
			if _, seen := byOwner[owner]; !seen {
				owners = append(owners, owner)
			}
			byOwner[owner] = append(byOwner[owner], holder)
		}

		pending := make([]solana.PublicKey, 0, len(owners))
		for _, owner := range owners {
			if !refresh {
				if item, ok := a.balances.get(networkID, owner); ok && (item.tokens != nil || !needTokens) {
					stale := time.Since(item.written) > balanceTTL
					for _, holder := range byOwner[owner] {
						if !emit(holder, item, stale, stale, nil, nil) {
							return
						}
					}
					if !stale {
						continue
					}
				}
			}
			pending = append(pending, owner)
		}
		if len(pending) == 0 {
			return
		}

		lamports, nativeAt, nativeErrs := a.readLamports(ctx, nodes, pending)
		finish := func(owner solana.PublicKey, tokens map[solana.PublicKey]*big.Int, tokensAt time.Time, tokenErr error) bool {
			nativeErr := nativeErrs[owner]
			// Dated by the parts that were read: a failed SOL read has no time.
			at := nativeAt[owner]
			if tokens != nil && (at.IsZero() || tokensAt.Before(at)) {
				at = tokensAt
			}
			item := holding{lamports: lamports[owner], tokens: tokens, at: at, solAt: nativeAt[owner]}
			// A read made under settings that have since been replaced is
			// shown as stale and not cached, as a client built for replaced
			// settings is not kept: the next read goes to the current ones.
			stale := a.balances.generation(networkID) != generation
			staleTokens := stale
			if (nativeErr != nil || tokenErr != nil) && !stale {
				// Part of this read failed. If a read asked for later is
				// cached, show its values for SOL and for what failed here,
				// so the screen does not end on an older SOL balance next to
				// an error; only what the cache cannot supply stays an error.
				if held, ok := a.balances.get(networkID, owner); ok && held.newer(item) {
					cachedStale := time.Since(held.written) > balanceTTL
					item.lamports, stale, nativeErr = held.lamports, cachedStale, nil
					if tokenErr != nil && held.tokens != nil {
						item.tokens, staleTokens, tokenErr = held.tokens, cachedStale, nil
					}
				}
			}
			if nativeErr == nil && tokenErr == nil && !stale && !staleTokens {
				if a.beforeCachePut != nil {
					a.beforeCachePut(item.lamports)
				}
				// Only a complete read restarts the TTL.
				held, written, ok := a.balances.put(networkID, generation, owner, item)
				switch {
				case !ok:
					stale = true
				case !written && (held.tokens != nil || tokens == nil):
					// A newer read is cached: show that, not this older one,
					// so the screen does not end on the older value.
					item, stale = held, time.Since(held.written) > balanceTTL
				case !written:
					// The newer read has no tokens to show; this one is shown
					// for what it is.
					stale = true
				}
				staleTokens = stale
			}
			for _, holder := range byOwner[owner] {
				if !emit(holder, item, stale, staleTokens, nativeErr, tokenErr) {
					return false
				}
			}
			return true
		}
		if !needTokens {
			for _, owner := range pending {
				if !finish(owner, nil, time.Time{}, nil) {
					return
				}
			}
			return
		}

		type tokenResult struct {
			owner  solana.PublicKey
			tokens map[solana.PublicKey]*big.Int
			at     time.Time
			err    error
		}
		results := make(chan tokenResult)
		var wg sync.WaitGroup
		for _, owner := range pending {
			wg.Go(func() {
				tokens, at, err := a.readTokensShared(ctx, nodes, networkID, owner, refresh)
				select {
				case results <- tokenResult{owner: owner, tokens: tokens, at: at, err: err}:
				case <-ctx.Done():
				}
			})
		}
		go func() {
			wg.Wait()
			close(results)
		}()
		for result := range results {
			if !finish(result.owner, result.tokens, result.at, result.err) {
				// Drain so the readers can exit.
				for range results {
				}
				return
			}
		}
	}()
	return out
}

// assetAmount renders one asset's balance from a holding.
func assetAmount(networkID string, asset chain.Asset, item holding, nativeErr, tokenErr error) (string, error) {
	if asset.NetworkID != networkID {
		return "", fmt.Errorf("%w: asset belongs to another network", chain.ErrInvalidRequest)
	}
	switch asset.Kind {
	case "native":
		if nativeErr != nil {
			return "", nativeErr
		}
		return decimal.NewFromBigInt(new(big.Int).SetUint64(item.lamports), -int32(asset.Decimals)).String(), nil
	case "spl":
		mint, err := solana.PublicKeyFromBase58(asset.Contract)
		if err != nil {
			return "", fmt.Errorf("%w: invalid SPL mint", chain.ErrInvalidRequest)
		}
		if tokenErr != nil {
			return "", tokenErr
		}
		units := item.tokens[mint]
		if units == nil {
			units = new(big.Int)
		}
		return decimal.NewFromBigInt(units, -int32(asset.Decimals)).String(), nil
	default:
		return "", fmt.Errorf("%w: unsupported Solana asset kind %q", chain.ErrInvalidRequest, asset.Kind)
	}
}

// readLamports reads the SOL balance of every owner, a hundred per call, and
// when each was asked for. An owner whose call failed has its error in the
// third map; an address with no account on chain holds zero.
func (a *Adapter) readLamports(
	ctx context.Context, nodes *nodes, owners []solana.PublicKey,
) (map[solana.PublicKey]uint64, map[solana.PublicKey]time.Time, map[solana.PublicKey]error) {
	lamports := make(map[solana.PublicKey]uint64, len(owners))
	asked := make(map[solana.PublicKey]time.Time, len(owners))
	failures := make(map[solana.PublicKey]error)
	for start := 0; start < len(owners); start += maxAccountsPerCall {
		chunk := owners[start:min(start+maxAccountsPerCall, len(owners))]
		var result *rpc.GetMultipleAccountsResult
		var at time.Time
		err := nodes.do(ctx, "getMultipleAccounts", func(client *rpc.Client) error {
			at = time.Now()
			var callErr error
			result, callErr = client.GetMultipleAccountsWithOpts(ctx, chunk, &rpc.GetMultipleAccountsOpts{
				Encoding: solana.EncodingBase64, Commitment: readCommitment,
				DataSlice: &rpc.DataSlice{Offset: new(uint64(0)), Length: new(uint64(0))},
			})
			if callErr == nil && len(result.Value) != len(chunk) {
				callErr = fmt.Errorf("answered %d accounts for %d addresses", len(result.Value), len(chunk))
			}
			return callErr
		})
		if err != nil {
			err = fmt.Errorf("read SOL balance: %w", err)
			for _, owner := range chunk {
				failures[owner] = err
			}
			continue
		}
		for i, owner := range chunk {
			asked[owner] = at
			if account := result.Value[i]; account != nil {
				lamports[owner] = account.Lamports
			}
		}
	}
	return lamports, asked, failures
}

// tokenFlight is one token read shared by the streams waiting on it.
type tokenFlight struct {
	done    chan struct{}
	tokens  map[solana.PublicKey]*big.Int
	err     error
	waiters int
	cancel  context.CancelFunc
	// sent is set, under the group's mutex, just before a request leaves;
	// sentAt is when the last one left, which is how old its answer is.
	sent   bool
	sentAt time.Time
}

// readTokensShared deduplicates the same holder across overlapping streams of
// the same verified endpoint set, and takes a balance slot before touching a
// node.
//
// The read belongs to no single stream: one stream leaving does not end it
// for the others. When the last one leaves, a read not yet sent stops — while
// it waits for a slot, for its paced turn, or before moving on to another
// node — and never reaches a node; one already sent may finish. Its result is
// not cached: a holding is SOL and tokens read together, and this read has no
// SOL balance to go with it.
//
// The flight is keyed by the endpoint set as well as the holder, so a stream
// that starts after the settings changed never joins a read sent to the
// endpoints the user just replaced. A refresh joins a flight only before its
// request has left: one already on its way may answer from before what the
// refresh is asking about, such as a transfer that has just confirmed.
func (a *Adapter) readTokensShared(
	ctx context.Context, nodes *nodes, networkID string, owner solana.PublicKey, refresh bool,
) (map[solana.PublicKey]*big.Int, time.Time, error) {
	key := fmt.Sprintf("%s#%p", cacheKey(networkID, owner), nodes)
	flights := a.balances
	flights.mu.Lock()
	flight := flights.flights[key]
	if flight == nil || (refresh && flight.sent) {
		flightCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		started := &tokenFlight{done: make(chan struct{}), cancel: cancel}
		flight = started
		flights.flights[key] = started
		markSent := func() {
			flights.mu.Lock()
			started.sent, started.sentAt = true, time.Now()
			flights.mu.Unlock()
		}
		go func() {
			defer cancel()
			tokens, err := readTokensInSlot(flightCtx, nodes, owner, markSent)
			if a.flightEnded != nil {
				a.flightEnded()
			}
			flights.mu.Lock()
			if flights.flights[key] == started {
				delete(flights.flights, key)
			}
			started.tokens, started.err = tokens, err
			flights.mu.Unlock()
			close(started.done)
		}()
	}
	flight.waiters++
	flights.mu.Unlock()
	select {
	case <-flight.done:
		return flight.tokens, flight.sentAt, flight.err
	case <-ctx.Done():
		flights.mu.Lock()
		flight.waiters--
		if flight.waiters == 0 {
			// Nobody is waiting any more: a read not yet sent is not worth
			// sending.
			flight.cancel()
			// Removed now, in the same critical section as the cancel: the
			// flight's own goroutine removes it only once it has noticed, and
			// a stream arriving in between would join a cancelled read and be
			// answered with its cancellation.
			if flights.flights[key] == flight {
				delete(flights.flights, key)
			}
		}
		flights.mu.Unlock()
		return nil, time.Time{}, ctx.Err()
	}
}

// readTokensInSlot waits for a balance slot unless ctx ends first, then reads.
// Waiting — for the slot, for the paced turn, before another node — stops when
// ctx does; the request itself, once sent, runs under its own timeout.
func readTokensInSlot(
	ctx context.Context, nodes *nodes, owner solana.PublicKey, sent func(),
) (map[solana.PublicKey]*big.Int, error) {
	release, err := nodes.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return readTokens(ctx, nodes, owner, func() (context.Context, context.CancelFunc) {
		sent()
		return context.WithTimeout(context.WithoutCancel(ctx), tokenReadTimeout)
	})
}

// readTokens sums the owner's classic SPL token accounts per mint. request,
// when set, gives the context each request is sent under; without it the
// request runs under ctx.
func readTokens(
	ctx context.Context, nodes *nodes, owner solana.PublicKey, request func() (context.Context, context.CancelFunc),
) (map[solana.PublicKey]*big.Int, error) {
	var result *rpc.GetTokenAccountsResult
	err := nodes.do(ctx, "getTokenAccountsByOwner", func(client *rpc.Client) error {
		callCtx, cancel := ctx, context.CancelFunc(func() {})
		if request != nil {
			callCtx, cancel = request()
		}
		defer cancel()
		var callErr error
		result, callErr = client.GetTokenAccountsByOwner(callCtx, owner,
			&rpc.GetTokenAccountsConfig{ProgramId: new(solana.TokenProgramID)},
			&rpc.GetTokenAccountsOpts{
				Encoding: solana.EncodingBase64, Commitment: readCommitment,
				DataSlice: &rpc.DataSlice{Offset: new(uint64(0)), Length: new(uint64(tokenAccountSlice))},
			},
		)
		if callErr == nil && result == nil {
			callErr = errors.New("answer has no result")
		}
		return callErr
	})
	if err != nil {
		return nil, fmt.Errorf("read SPL token balances: %w", err)
	}
	tokens := make(map[solana.PublicKey]*big.Int)
	for _, entry := range result.Value {
		if entry == nil {
			return nil, errors.New("read SPL token balances: answer lists an empty token account")
		}
		// The filter asked the node for these; a node that answers with
		// anything else is not to be summed from.
		if entry.Account.Owner != solana.TokenProgramID {
			return nil, errors.New("read SPL token balances: answer lists an account of another program")
		}
		data := entry.Account.Data.GetBinary()
		if len(data) != tokenAccountSlice {
			return nil, fmt.Errorf("read SPL token balances: token account data is %d bytes, want %d", len(data), tokenAccountSlice)
		}
		if solana.PublicKeyFromBytes(data[32:64]) != owner {
			return nil, errors.New("read SPL token balances: answer lists another owner's token account")
		}
		mint := solana.PublicKeyFromBytes(data[0:32])
		sum := tokens[mint]
		if sum == nil {
			sum = new(big.Int)
			tokens[mint] = sum
		}
		sum.Add(sum, new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[64:72])))
	}
	return tokens, nil
}
