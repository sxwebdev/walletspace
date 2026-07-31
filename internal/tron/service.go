// Package tron wraps the gotron client with the few operations the UI needs:
// reading TRX and TRC20 balances, and sending either asset.
package tron

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/address"
	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/config"
	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"
)

const (
	// maxTokenDecimals mirrors the cap the gotron amount constructors enforce:
	// the number of decimal digits in the largest ABI uint256.
	maxTokenDecimals = 78
	// balanceTTL is how long a fetched balance is considered fresh.
	balanceTTL = 15 * time.Second
	// balanceRetention keeps the last known value available for immediate
	// stale-while-revalidate rendering across page reloads.
	balanceRetention = 5 * time.Minute
	// estimateTTL covers one pass through the send dialog. Fees move with
	// chain parameters, which change far more slowly than that.
	estimateTTL = 60 * time.Second
	// Public TronGrid endpoints allow ~3 requests per second per IP and each
	// address costs two calls, so without an API key we stay deliberately slow.
	balanceConcurrencyPublic = 2
	balanceConcurrencyKeyed  = 8
)

// Asset identifies which currency an operation applies to.
type Asset string

const (
	AssetTRX  Asset = "trx"
	AssetUSDT Asset = "usdt"
)

// Balance is the pair of balances shown for one address.
type Balance struct {
	TRX       decimal.Decimal
	USDT      decimal.Decimal
	Activated bool
}

// BalanceResult is one completed address read. It lets HTTP callers render
// large portfolios progressively instead of waiting for the slowest address.
type BalanceResult struct {
	Address string
	Balance Balance
	Err     error
	Stale   bool
}

// Estimate is what a transfer will cost the sender, on top of the amount.
type Estimate struct {
	// Fee is the total cost in TRX, including Activation.
	Fee decimal.Decimal
	// Activation is the part of Fee charged for creating the recipient's
	// account on chain. Zero when the recipient already exists.
	Activation decimal.Decimal
	// bandwidthFee is the part of Fee burned for bandwidth. It stays internal:
	// callers show the total, while Spendable needs the itemised charge to know
	// whether the probed amount needs per-byte padding.
	bandwidthFee decimal.Decimal
}

// TokenInfo describes the TRC20 contract configured as USDT.
type TokenInfo struct {
	Contract string
	Symbol   string
	Decimals int32
}

func (s *Service) Health(ctx context.Context) error {
	_, err := retry(ctx, s.nodes, func() (*api.BlockExtention, error) {
		return s.client.GetLastBlock(ctx)
	})
	return err
}

// TokenMetadata validates a TRC20 contract by reading its on-chain metadata.
func (s *Service) TokenMetadata(ctx context.Context, contract string) (string, uint8, error) {
	if err := address.Validate(contract); err != nil {
		return "", 0, fmt.Errorf("%w: invalid TRC20 contract: %s", ErrInvalidRequest, err)
	}
	symbol, err := s.client.TRC20GetSymbol(ctx, contract)
	if err != nil {
		return "", 0, fmt.Errorf("read TRC20 symbol: %w", err)
	}
	decimals, err := s.client.TRC20GetDecimals(ctx, contract)
	if err != nil {
		return "", 0, fmt.Errorf("read TRC20 decimals: %w", err)
	}
	if !decimals.IsUint64() || decimals.Uint64() > 78 {
		return "", 0, errors.New("TRC20 decimals are out of range")
	}
	return symbol, uint8(decimals.Uint64()), nil
}

func (s *Service) TokenBalance(
	ctx context.Context,
	holder, contract string,
	decimals uint8,
) (decimal.Decimal, error) {
	if err := address.Validate(holder); err != nil {
		return decimal.Zero, fmt.Errorf("%w: invalid holder address", ErrInvalidRequest)
	}
	if err := address.Validate(contract); err != nil {
		return decimal.Zero, fmt.Errorf("%w: invalid TRC20 contract", ErrInvalidRequest)
	}
	value, err := retry(ctx, s.nodes, func() (client.TokenAmount, error) {
		return s.client.TRC20ContractBalance(ctx, holder, contract)
	})
	if err != nil {
		return decimal.Zero, err
	}
	return value.Decimal(int32(decimals)), nil
}

func (s *Service) EstimateToken(
	ctx context.Context,
	from, to, contract string,
	decimals uint8,
	amount decimal.Decimal,
) (Estimate, error) {
	tokens, err := tokenAmount(amount, int32(decimals))
	if err != nil {
		return Estimate{}, err
	}
	result, err := retry(ctx, s.nodes, func() (*client.EstimateTransferResult, error) {
		return s.client.EstimateTRC20Transfer(ctx, from, to, contract, tokens)
	})
	if err != nil {
		return Estimate{}, s.chainError("estimate TRC20 transfer", err)
	}
	return Estimate{
		Fee:          result.Fee.TRX(),
		Activation:   (result.Charges.AccountCreation + result.Charges.UnstakedCreation).TRX(),
		bandwidthFee: result.Charges.Bandwidth.TRX(),
	}, nil
}

func (s *Service) SendTokenWithSigner(
	ctx context.Context,
	from, to, contract string,
	decimals uint8,
	amount decimal.Decimal,
	signer chain.Signer,
) (string, error) {
	if signer == nil || signer.Family() != chain.FamilyTron {
		return "", errors.New("Tron signer is required")
	}
	if err := address.Validate(to); err != nil {
		return "", fmt.Errorf("%w: invalid recipient address", ErrInvalidRequest)
	}
	if err := address.Validate(contract); err != nil {
		return "", fmt.Errorf("%w: invalid TRC20 contract", ErrInvalidRequest)
	}
	tokens, err := tokenAmount(amount, int32(decimals))
	if err != nil {
		return "", err
	}
	tx, err := retry(ctx, s.nodes, func() (*api.TransactionExtention, error) {
		return s.client.TRC20Send(ctx, from, to, contract, tokens, s.feeLimit)
	})
	if err != nil {
		return "", s.chainError("create TRC20 transfer", err)
	}
	txID, err := s.submitWithSigner(ctx, tx, signer)
	if err != nil {
		return "", err
	}
	s.invalidate(from, to)
	return txID, nil
}

// Service talks to the Tron network.
type Service struct {
	client       *client.Client
	log          *slog.Logger
	token        TokenInfo
	feeLimit     client.SUN
	nodes        int // number of configured nodes, used as the retry budget
	workers      int // parallel balance fetches
	cache        *ttlcache.Cache[string, Balance]
	balanceTimes sync.Map // address -> time.Time of the last successful fetch
	estimates    *ttlcache.Cache[string, Estimate]
	// balanceSlots is shared by every HTTP request. A per-request errgroup limit
	// would let two overlapping refreshes double the load on public TronGrid.
	balanceSlots chan struct{}
	balanceGroup singleflight.Group
	// fetch reads one address from chain. It is a field so the caching and
	// fan-out logic in Balances can be exercised without a live node.
	fetch func(ctx context.Context, addr string) (Balance, error)
}

// retry runs call up to attempts times. Each attempt is routed to the next
// node, so a single dead endpoint does not fail the whole operation.
func retry[T any](ctx context.Context, attempts int, call func() (T, error)) (T, error) {
	var (
		zero T
		err  error
	)

	if attempts < 1 {
		attempts = 1
	}

	for range attempts {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}

		var result T
		if result, err = call(); err == nil {
			return result, nil
		}
	}

	return zero, err
}

// New builds a client from the configuration and reads the token metadata
// once, so balances can be scaled without an extra call per request.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Service, error) {
	nodes, err := cfg.ParseNodes()
	if err != nil {
		return nil, err
	}

	nodeCfgs := make([]client.NodeConfig, 0, len(nodes))
	for _, n := range nodes {
		nc := client.NodeConfig{
			Protocol: client.ProtocolGRPC,
			Address:  n.Address,
			UseTLS:   n.TLS,
			Tier:     n.Tier,
		}
		if n.HTTP {
			nc.Protocol = client.ProtocolHTTP
			nc.UseTLS = false
		}
		if cfg.APIKey != "" {
			nc.Headers = map[string]string{"TRON-PRO-API-KEY": cfg.APIKey}
		}

		nodeCfgs = append(nodeCfgs, nc)
	}

	c, err := client.New(client.Config{
		Nodes:      nodeCfgs,
		Network:    client.Network(cfg.Network),
		Blockchain: "tron",
		Health:     client.HealthConfig{Logger: healthLogger{log}},
	})
	if err != nil {
		return nil, fmt.Errorf("create tron client: %w", err)
	}

	workers := balanceConcurrencyPublic
	if cfg.APIKey != "" {
		workers = balanceConcurrencyKeyed
	}

	feeLimit, err := client.FromTRX(decimal.NewFromInt(cfg.FeeLimitTRX))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("fee limit of %d TRX: %w", cfg.FeeLimitTRX, err)
	}

	s := &Service{
		client:       c,
		log:          log,
		feeLimit:     feeLimit,
		nodes:        len(nodeCfgs),
		workers:      workers,
		cache:        newBalanceCache(balanceRetention),
		balanceSlots: make(chan struct{}, workers),
		estimates: ttlcache.New(
			ttlcache.WithTTL[string, Estimate](estimateTTL),
			ttlcache.WithDisableTouchOnHit[string, Estimate](),
		),
		token: TokenInfo{Contract: cfg.USDTContract},
	}
	s.fetch = s.balance

	if err := s.loadTokenInfo(ctx); err != nil {
		c.Close()
		return nil, err
	}

	// Started only once the Service is going to be returned. Starting earlier
	// would leak the janitor goroutine on the error path above, with no handle
	// left to stop it — and Stop() is a no-op until Start has actually run, so
	// a caller could not clean it up either.
	go s.cache.Start()
	go s.estimates.Start()

	return s, nil
}

// newBalanceCache builds the balance cache.
//
// WithDisableTouchOnHit is essential: by default ttlcache extends an item's
// expiry every time it is read, so a UI that polls faster than the TTL would
// keep an address alive forever and never see a fresh balance.
func newBalanceCache(ttl time.Duration) *ttlcache.Cache[string, Balance] {
	return ttlcache.New(
		ttlcache.WithTTL[string, Balance](ttl),
		ttlcache.WithDisableTouchOnHit[string, Balance](),
	)
}

// Close stops the cache janitor and releases the underlying transports.
func (s *Service) Close() error {
	s.cache.Stop()
	s.estimates.Stop()

	return s.client.Close()
}

// Token returns the metadata of the configured TRC20 contract.
func (s *Service) Token() TokenInfo { return s.token }

func (s *Service) loadTokenInfo(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// At start-up the health checker has not probed anything yet, so the first
	// node picked may well be a dead one. Retry: every attempt is routed to the
	// next node in the tier.
	decimals, err := retry(ctx, s.nodes, func() (*big.Int, error) {
		return s.client.TRC20GetDecimals(ctx, s.token.Contract)
	})
	if err != nil {
		return fmt.Errorf("read decimals of %s: %w", s.token.Contract, err)
	}

	scale, err := validateDecimals(decimals)
	if err != nil {
		return fmt.Errorf("contract %s on %s: %w — check that USDT_CONTRACT points at a TRC20 token",
			s.token.Contract, s.client.GetNetwork(), err)
	}
	s.token.Decimals = scale

	symbol, err := retry(ctx, s.nodes, func() (string, error) {
		return s.client.TRC20GetSymbol(ctx, s.token.Contract)
	})
	if err != nil {
		return fmt.Errorf("read symbol of %s: %w", s.token.Contract, err)
	}
	s.token.Symbol = symbol

	s.log.Info("token contract resolved",
		"contract", s.token.Contract,
		"symbol", s.token.Symbol,
		"decimals", s.token.Decimals,
	)

	return nil
}

// BalanceStream emits cached balances first, then fresh reads as each address
// completes. All concurrent streams share the same RPC budget.
func (s *Service) BalanceStream(ctx context.Context, addresses []string, refresh bool) <-chan BalanceResult {
	results := make(chan BalanceResult)

	go func() {
		defer close(results)

		pending := make([]string, 0, len(addresses))
		if refresh {
			pending = append(pending, addresses...)
		} else {
			for _, addr := range addresses {
				if item := s.cache.Get(addr); item != nil {
					fetchedAt, _ := s.balanceTimes.Load(addr)
					at, _ := fetchedAt.(time.Time)
					stale := at.IsZero() || time.Since(at) > balanceTTL

					select {
					case results <- BalanceResult{
						Address: addr,
						Balance: item.Value(),
						Stale:   stale,
					}:
					case <-ctx.Done():
						return
					}
					if !stale {
						continue
					}
				}

				pending = append(pending, addr)
			}
		}

		var wg sync.WaitGroup
		wg.Add(len(pending))
		for _, addr := range pending {
			go func() {
				defer wg.Done()

				b, err := s.fetchShared(ctx, addr)
				if err == nil {
					// Only fresh successful reads restart the TTL.
					s.cache.Set(addr, b, ttlcache.DefaultTTL)
					s.balanceTimes.Store(addr, time.Now())
				}

				select {
				case results <- BalanceResult{Address: addr, Balance: b, Err: err}:
				case <-ctx.Done():
				}
			}()
		}
		wg.Wait()
	}()

	return results
}

// fetchShared deduplicates the same address across overlapping refreshes and
// applies the service-wide concurrency limit before touching a node.
func (s *Service) fetchShared(ctx context.Context, addr string) (Balance, error) {
	ch := s.balanceGroup.DoChan(addr, func() (any, error) {
		select {
		case s.balanceSlots <- struct{}{}:
			defer func() { <-s.balanceSlots }()
		case <-ctx.Done():
			return Balance{}, ctx.Err()
		}

		return s.fetch(ctx, addr)
	})

	select {
	case result := <-ch:
		if result.Val == nil {
			return Balance{}, result.Err
		}
		return result.Val.(Balance), result.Err
	case <-ctx.Done():
		return Balance{}, ctx.Err()
	}
}

// Balances is the aggregate form used by calculations and the JSON endpoint.
// It consumes the same progressive stream, preserving the existing API.
func (s *Service) Balances(ctx context.Context, addresses []string, refresh bool) (map[string]Balance, map[string]error) {
	out := make(map[string]Balance, len(addresses))
	errs := make(map[string]error)

	for result := range s.BalanceStream(ctx, addresses, refresh) {
		if result.Err != nil {
			errs[result.Address] = result.Err
			continue
		}
		out[result.Address] = result.Balance
	}

	return out, errs
}

// invalidate drops the cached balances of the given addresses, so the next
// read goes back to the chain. Both sides of a transfer have moved.
func (s *Service) invalidate(addresses ...string) {
	for _, addr := range addresses {
		s.cache.Delete(addr)
		s.balanceTimes.Delete(addr)
	}

	// A transfer can activate the recipient, which makes every later transfer
	// to them about 1 TRX cheaper. The cache is a handful of entries, so
	// clearing it wholesale is simpler than tracking which ones the recipient
	// appears in.
	s.estimates.DeleteAll()
}

// accountBalance carries the TRX balance together with whether the account
// exists on chain at all.
type accountBalance struct {
	trx       client.SUN
	activated bool
}

func (s *Service) balance(ctx context.Context, addr string) (Balance, error) {
	// An address that has never received anything simply does not exist on
	// chain yet — that is a zero balance of an unactivated account, not a
	// failure, so it must not be retried against the other nodes.
	account, err := retry(ctx, s.nodes, func() (accountBalance, error) {
		v, err := s.client.GetAccountBalance(ctx, addr)
		if errors.Is(err, client.ErrAccountNotFound) {
			return accountBalance{}, nil
		}
		if err != nil {
			return accountBalance{}, err
		}

		return accountBalance{trx: v, activated: true}, nil
	})
	if err != nil {
		return Balance{}, fmt.Errorf("trx balance: %w", err)
	}

	tokens, err := retry(ctx, s.nodes, func() (client.TokenAmount, error) {
		return s.client.TRC20ContractBalance(ctx, addr, s.token.Contract)
	})
	if err != nil {
		return Balance{}, fmt.Errorf("%s balance: %w", s.token.Symbol, err)
	}

	return Balance{
		TRX:       account.trx.TRX(),
		USDT:      tokens.Decimal(s.token.Decimals),
		Activated: account.activated,
	}, nil
}

// probeAmount is the amount used to price a transfer whose size is not known
// yet. The fee depends on the transaction's byte length, not on the value, so
// one SUN prices the same transfer as any other amount — up to the few bytes
// the amount's varint encoding differs by, which feeSlack covers.
var probeAmount = decimal.New(1, -6)

// feeSlack pads paid bandwidth to absorb the varint width difference between
// the probe and the real amount: an int64 is at most 10 bytes, and bandwidth is
// charged per byte. It must not be reserved when either bandwidth pool covers
// the transaction, because Tron uses one whole pool or the other and never
// combines free and staked bandwidth.
var feeSlack = decimal.New(16, -3)

// Estimate reports what a transfer would cost without broadcasting anything.
//
// Sending TRX to a recipient that does not exist on chain yet also pays for
// creating their account, which is why the fee cannot be assumed constant and
// why "send everything" has to leave room for it.
func (s *Service) Estimate(ctx context.Context, from, to string, asset Asset, amount decimal.Decimal) (Estimate, error) {
	if err := address.Validate(to); err != nil {
		return Estimate{}, fmt.Errorf("%w: invalid recipient address: %s", ErrInvalidRequest, err)
	}

	if amount.LessThanOrEqual(decimal.Zero) {
		return Estimate{}, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidRequest)
	}

	// Pricing a transfer costs five or six node calls, and the UI asks again on
	// every pause in typing. The fee is driven by the transaction's size and by
	// whether the recipient exists, not by the amount, so one answer serves the
	// whole dialog — which also keeps a keyless public endpoint within its few
	// requests per second.
	cacheKey := from + "|" + to + "|" + string(asset)
	if item := s.estimates.Get(cacheKey); item != nil {
		return item.Value(), nil
	}

	var (
		res *client.EstimateTransferResult
		err error
	)

	switch asset {
	case AssetTRX:
		sun, convErr := trxAmount(amount)
		if convErr != nil {
			return Estimate{}, convErr
		}

		res, err = retry(ctx, s.nodes, func() (*client.EstimateTransferResult, error) {
			return s.client.EstimateTRXTransfer(ctx, from, to, sun)
		})

	case AssetUSDT:
		tokens, convErr := tokenAmount(amount, s.token.Decimals)
		if convErr != nil {
			return Estimate{}, convErr
		}

		res, err = retry(ctx, s.nodes, func() (*client.EstimateTransferResult, error) {
			return s.client.EstimateTRC20Transfer(ctx, from, to, s.token.Contract, tokens)
		})

	default:
		return Estimate{}, fmt.Errorf("%w: unknown asset %q", ErrInvalidRequest, asset)
	}
	if err != nil {
		return Estimate{}, s.chainError("estimate transfer", err)
	}

	// Fee is what actually leaves the account: gotron nets the transfer against
	// the sender's own bandwidth and energy, and itemises the account-creation
	// charges that the free allowance is not allowed to pay for.
	est := Estimate{
		Fee:          res.Fee.TRX(),
		Activation:   (res.Charges.AccountCreation + res.Charges.UnstakedCreation).TRX(),
		bandwidthFee: res.Charges.Bandwidth.TRX(),
	}
	s.estimates.Set(cacheKey, est, ttlcache.DefaultTTL)

	return est, nil
}

// Spendable returns the largest amount that can still be sent after the fee,
// together with the estimate it is based on.
//
// The fee is priced on a probe transfer rather than on the whole balance:
// estimating builds a real transaction, and the node refuses to build one whose
// value the account cannot cover — which is exactly the case "send everything"
// runs into when the recipient has to be created first.
//
// The bandwidth padding applies only when the estimate actually burns TRX for
// bandwidth. A zero fee means one of the sender's bandwidth pools covers the
// whole transaction, so "send everything" must not leave an invented fee
// behind. Activation is a flat system-contract charge and needs no per-byte
// padding either.
func (s *Service) Spendable(ctx context.Context, from, to string, asset Asset) (decimal.Decimal, Estimate, error) {
	balances, errs := s.Balances(ctx, []string{from}, false)
	if err, ok := errs[from]; ok {
		return decimal.Zero, Estimate{}, fmt.Errorf("read balance of %s: %w", from, err)
	}

	balance, ok := balances[from]
	if !ok {
		return decimal.Zero, Estimate{}, fmt.Errorf("no balance for %s", from)
	}

	// A token transfer is paid for in TRX, so the whole token balance can go.
	if asset == AssetUSDT {
		if balance.USDT.LessThanOrEqual(decimal.Zero) {
			return decimal.Zero, Estimate{}, fmt.Errorf("%w: %s holds no %s", ErrInvalidRequest, from, s.token.Symbol)
		}

		est, err := s.Estimate(ctx, from, to, asset, balance.USDT)
		if err != nil {
			return decimal.Zero, Estimate{}, err
		}

		return balance.USDT, est, nil
	}

	if balance.TRX.LessThanOrEqual(probeAmount) {
		return decimal.Zero, Estimate{}, fmt.Errorf("%w: %s holds %s TRX, too little to send anything", ErrInvalidRequest, from, balance.TRX)
	}

	est, err := s.Estimate(ctx, from, to, asset, probeAmount)
	if err != nil {
		return decimal.Zero, Estimate{}, err
	}

	spendable := balance.TRX.Sub(est.Fee)
	if est.bandwidthFee.IsPositive() {
		spendable = spendable.Sub(feeSlack)
	}
	if spendable.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, est, fmt.Errorf(
			"%w: %s holds %s TRX, which does not cover the fee of about %s TRX",
			ErrInvalidRequest, from, balance.TRX, est.Fee,
		)
	}

	return spendable, est, nil
}

// Shortfall is how much TRX the sender is missing to complete a transfer at the
// given cost. Zero when the balance covers it.
//
// A token transfer is paid for in TRX, so an account holding plenty of USDT and
// no TRX cannot move any of it — and the node only says so after the
// transaction has been built, signed and broadcast, as a bare BANDWITH_ERROR.
// This is the same fact, in the dialog, before any of that happens.
//
// It reports rather than forbids: the estimate is the node's own figure, but
// the balance may move between the estimate and the send, and refusing a
// transfer the chain would have accepted is worse than letting the chain
// answer.
func (s *Service) Shortfall(ctx context.Context, from string, asset Asset, amount decimal.Decimal, est Estimate) (decimal.Decimal, error) {
	// Sending TRX spends the amount as well as the fee; sending a token spends
	// only the fee, and whether the token balance covers the amount is a
	// separate question the caller already sees.
	need := est.Fee
	if asset == AssetTRX {
		need = need.Add(amount)
	}

	return s.missingTRX(ctx, from, need)
}

// missingTRX is how much TRX the account is short of need, zero when it is not.
func (s *Service) missingTRX(ctx context.Context, from string, need decimal.Decimal) (decimal.Decimal, error) {
	balances, errs := s.Balances(ctx, []string{from}, false)
	if err, ok := errs[from]; ok {
		return decimal.Zero, fmt.Errorf("read balance of %s: %w", from, err)
	}

	balance, ok := balances[from]
	if !ok {
		return decimal.Zero, fmt.Errorf("no balance for %s", from)
	}

	if missing := need.Sub(balance.TRX); missing.GreaterThan(decimal.Zero) {
		return missing, nil
	}

	return decimal.Zero, nil
}

// chainError classifies a node's refusal to build a transaction, wherever it
// surfaces — the estimate, the send and the staking paths.
//
// A contract validation error is the node saying the request itself is wrong:
// nothing staked, an account that does not exist, more than is delegated. It is
// reported as a bad request so the API answers 400 rather than 502, and the
// node's own wording is kept — it names the actual condition, and every attempt
// to summarise it here has been less specific than the original.
//
// A broadcast rejection is classified the same way, by the node's response
// code. Anything else is treated as an upstream problem and keeps its stage
// prefix.
func (s *Service) chainError(stage string, err error) error {
	if refusal, ok := errors.AsType[*client.ContractValidateError](err); ok {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, refusal)
	}

	// A call the VM ran and refused says nothing about the node: the contract
	// reverted, or the constructor did. It arrives from a simulation, so it
	// costs nothing and there is nothing to retry — the same code will revert
	// on every node.
	if errors.Is(err, client.ErrContractCallFailed) {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}

	if rejection, ok := errors.AsType[*client.BroadcastError](err); ok {
		if reason := rejectionReason(rejection.Code); reason != "" {
			return fmt.Errorf("%w: %s (%w)", ErrInvalidRequest, reason, rejection)
		}
	}

	return fmt.Errorf("%s: %w", stage, err)
}

// rejectionReason explains the broadcast codes that are about the transaction
// the caller asked for rather than about the node. An empty string means the
// rejection is not the caller's to fix — a busy node, too few peers, a chain
// still catching up — and it stays an upstream failure.
//
// The code is what is matched, not the message: a node routinely rejects a
// transaction with an empty message and the code as the only diagnostic, which
// is why gotron surfaces it as a field.
func rejectionReason(code api.ReturnResponseCode) string {
	switch code {
	case api.Return_BANDWITH_ERROR:
		// The one an account with tokens but no TRX runs into: a TRC20
		// transfer is paid for in bandwidth and energy, and burning TRX is the
		// only fallback when neither is staked.
		return "the account has neither the bandwidth and energy this transaction needs nor the TRX to pay for them"
	case api.Return_CONTRACT_VALIDATE_ERROR:
		return "the chain refused the transaction"
	case api.Return_CONTRACT_EXE_ERROR:
		return "the contract call failed"
	case api.Return_TOO_BIG_TRANSACTION_ERROR:
		return "the transaction is too large"
	default:
		return ""
	}
}

// submit signs a transaction a node built and broadcasts it, returning the txid.
//
// The broadcast is deliberately not retried: an attempt that reached the
// network may well have been accepted, and sending the same transaction to the
// next node could double it.
func (s *Service) submit(ctx context.Context, tx *api.TransactionExtention, key *ecdsa.PrivateKey) (string, error) {
	if err := s.client.SignTransaction(tx.GetTransaction(), key); err != nil {
		return "", fmt.Errorf("sign transaction: %w", err)
	}

	// A rejection arrives as *client.BroadcastError carrying the node's response
	// code, which is what says whether the transaction or the node is at fault.
	if _, err := s.client.BroadcastTransaction(ctx, tx.GetTransaction()); err != nil {
		return "", s.chainError("broadcast transaction", err)
	}

	return hex.EncodeToString(tx.GetTxid()), nil
}

func (s *Service) submitWithSigner(ctx context.Context, tx *api.TransactionExtention, signer chain.Signer) (string, error) {
	rawData, err := proto.Marshal(tx.GetTransaction().GetRawData())
	if err != nil {
		return "", fmt.Errorf("encode transaction for signing: %w", err)
	}
	digest := sha256.Sum256(rawData)
	signature, err := signer.SignDigest(ctx, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign transaction: %w", err)
	}
	defer clear(signature)
	if len(signature) != 65 {
		return "", errors.New("sign transaction: secp256k1 signature must be 65 bytes")
	}
	tx.GetTransaction().Signature = append(tx.GetTransaction().Signature, append([]byte(nil), signature...))
	if _, err := s.client.BroadcastTransaction(ctx, tx.GetTransaction()); err != nil {
		return "", s.chainError("broadcast transaction", err)
	}
	return hex.EncodeToString(tx.GetTxid()), nil
}

func (s *Service) buildTransfer(ctx context.Context, from, to string, asset Asset, amount decimal.Decimal) (*api.TransactionExtention, error) {
	if err := address.Validate(to); err != nil {
		return nil, fmt.Errorf("%w: invalid recipient address: %s", ErrInvalidRequest, err)
	}

	if amount.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidRequest)
	}

	var (
		tx  *api.TransactionExtention
		err error
	)

	switch asset {
	case AssetTRX:
		sun, convErr := trxAmount(amount)
		if convErr != nil {
			return nil, convErr
		}

		// Building a transaction is a read-only call on the node, so it is safe
		// to route past an unhealthy endpoint. Only the broadcast below must
		// not be retried.
		tx, err = retry(ctx, s.nodes, func() (*api.TransactionExtention, error) {
			return s.client.CreateTransferTransaction(ctx, from, to, sun)
		})
		if err != nil {
			err = s.chainError("create TRX transfer", err)
		}

	case AssetUSDT:
		tokens, convErr := tokenAmount(amount, s.token.Decimals)
		if convErr != nil {
			return nil, convErr
		}

		tx, err = retry(ctx, s.nodes, func() (*api.TransactionExtention, error) {
			return s.client.TRC20Send(ctx, from, to, s.token.Contract, tokens, s.feeLimit)
		})
		if err != nil {
			err = s.chainError("create "+s.token.Symbol+" transfer", err)
		}

	default:
		return nil, fmt.Errorf("%w: unknown asset %q", ErrInvalidRequest, asset)
	}
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// Send transfers TRX or the configured TRC20 token and returns the txid.
func (s *Service) Send(ctx context.Context, from, to string, asset Asset, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	tx, err := s.buildTransfer(ctx, from, to, asset, amount)
	if err != nil {
		return "", err
	}

	txid, err := s.submit(ctx, tx, key)
	if err != nil {
		return "", err
	}

	s.invalidate(from, to)

	return txid, nil
}

// SendWithSigner is the key-isolated equivalent used by the multichain adapter.
func (s *Service) SendWithSigner(
	ctx context.Context,
	from, to string,
	asset Asset,
	amount decimal.Decimal,
	signer chain.Signer,
) (string, error) {
	if signer == nil || signer.Family() != chain.FamilyTron {
		return "", errors.New("Tron signer is required")
	}
	tx, err := s.buildTransfer(ctx, from, to, asset, amount)
	if err != nil {
		return "", err
	}
	txid, err := s.submitWithSigner(ctx, tx, signer)
	if err != nil {
		return "", err
	}
	s.invalidate(from, to)
	return txid, nil
}

// TransactionConfirmed reports whether a broadcast transaction has appeared
// in a block. A missing receipt is the normal pending state, not an error.
func (s *Service) TransactionConfirmed(ctx context.Context, txid string) (bool, error) {
	if len(txid) != 64 {
		return false, fmt.Errorf("%w: transaction id must contain 64 hex characters", ErrInvalidRequest)
	}
	if _, err := hex.DecodeString(txid); err != nil {
		return false, fmt.Errorf("%w: invalid transaction id: %s", ErrInvalidRequest, err)
	}

	_, err := s.client.GetTransactionInfoByHash(ctx, txid)
	if errors.Is(err, client.ErrTransactionInfoNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read transaction receipt: %w", err)
	}

	return true, nil
}

// healthLogger bridges the client's health checker to slog.
type healthLogger struct{ log *slog.Logger }

func (l healthLogger) Infof(format string, args ...any) {
	l.log.Info(fmt.Sprintf(format, args...))
}
