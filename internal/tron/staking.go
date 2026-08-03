package tron

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/address"
	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/walletspace/internal/chain"
	"golang.org/x/sync/errgroup"
)

// Resource is one of the two things TRX can be staked for.
type Resource string

const (
	ResourceBandwidth Resource = "bandwidth"
	ResourceEnergy    Resource = "energy"
)

// toClient maps the wire value onto the SDK enum. It is the only place an
// unknown resource is rejected, and it does so as a bad request rather than as
// an upstream failure.
func (r Resource) toClient() (client.ResourceType, error) {
	switch r {
	case ResourceBandwidth:
		return client.ResourceTypeBandwidth, nil
	case ResourceEnergy:
		return client.ResourceTypeEnergy, nil
	default:
		return 0, fmt.Errorf("%w: unknown resource %q", ErrInvalidRequest, r)
	}
}

// fromClient maps the SDK enum back for display. Anything else is reported as
// bandwidth rather than dropped, so an unexpected entry still shows its amount.
func resourceFromClient(r client.ResourceType) Resource {
	if r == client.ResourceTypeEnergy {
		return ResourceEnergy
	}

	return ResourceBandwidth
}

// Pool is how much of a resource is still spendable out of the account's
// limit. Both figures are resource units — bandwidth points or energy — not TRX.
type Pool struct {
	Available decimal.Decimal
	Total     decimal.Decimal
}

// Unstake is one in-flight unfreeze: Amount becomes withdrawable at ExpireAt.
type Unstake struct {
	Resource Resource
	Amount   decimal.Decimal
	ExpireAt time.Time
}

// Delegation is resource lent to another account, measured by the TRX staked
// behind it.
type Delegation struct {
	To       string
	Resource Resource
	Amount   decimal.Decimal
	// LockedUntil is set only for a delegation made with a lock, which the
	// chain refuses to reclaim until it passes. This service never creates one,
	// but the account may hold delegations made elsewhere.
	LockedUntil time.Time
}

// Resources is an account's whole staking position. Every TRX figure is in TRX.
type Resources struct {
	Bandwidth Pool
	Energy    Pool

	StakedBandwidth decimal.Decimal
	StakedEnergy    decimal.Decimal
	Unstaking       decimal.Decimal
	WithdrawableNow decimal.Decimal

	// *PerTRX is what one staked TRX currently yields, so the UI can say how
	// much resource an amount buys without asking a node on every keystroke.
	// The rate moves with the network-wide staked total, which is why it is
	// read alongside the position rather than hard-coded.
	BandwidthPerTRX decimal.Decimal
	EnergyPerTRX    decimal.Decimal

	// CanDelegate* is the node's own answer for how much stake may still be
	// lent out, and it is far below the staked total whenever the account has
	// been spending: resource already consumed cannot be delegated away, and
	// neither can what is lent out already.
	//
	// It is shown, never enforced. The figure falls as the day's usage grows,
	// so a request the caller sends anyway is the node's to judge — but leaving
	// it out means the dialog offers the whole stake and the chain answers with
	// "delegateBalance must be less than or equal to available
	// FreezeBandwidthV2 balance", which names no number at all.
	CanDelegateBandwidth decimal.Decimal
	CanDelegateEnergy    decimal.Decimal

	// UnstakeSlots is how many more unstakes may be started before the pending
	// ones have to be withdrawn or cancelled. The chain caps that queue, and
	// hitting the cap is an opaque refusal otherwise.
	UnstakeSlots int64

	Pending     []Unstake
	Delegations []Delegation
}

// oneTRX is the unit the resource rates are quoted per.
var oneTRX = client.MustFromTRX(decimal.NewFromInt(1))

// Resources reads an account's staking position.
//
// It is asked for one wallet at a time, when the dialog is opened. The six
// reads run in parallel under the same worker limit that keeps the balance
// fan-out inside a public node's rate limit — but the delegation read is not
// one request: the SDK walks the receiver index and fetches each receiver in
// turn, so an account lending to many others costs one request per receiver on
// top. That is the slow path here.
//
// A failure in any of them fails the whole call. Filling the missing part
// with zeros would be worse than saying nothing: "Staked: 0" is a statement
// about the account, and a read that did not happen must not make it.
//
// Nothing here is a limit the caller should enforce. How much may be staked or
// delegated is the node's judgement, and only the node has every input to it;
// these figures are for showing, not for gating a request on.
func (s *Service) Resources(ctx context.Context, addr string) (Resources, error) {
	if err := address.Validate(addr); err != nil {
		return Resources{}, fmt.Errorf("%w: invalid address: %s", ErrInvalidRequest, err)
	}

	var (
		account *api.AccountResourceMessage
		stake   *client.StakeInfo
		lent    []client.Delegation
		slots   int64
		canBW   client.SUN
		canEN   client.SUN
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.workers)

	g.Go(func() error {
		// Wrapped like the rest: an account that does not exist is an empty
		// position, not a failure. The proto getters read a nil message as
		// zeros, so nothing below needs a second nil check.
		v, err := retry(gctx, s.nodes, emptyIfMissing(func() (*api.AccountResourceMessage, error) {
			return s.client.GetAccountResource(gctx, addr)
		}))
		if err != nil {
			return fmt.Errorf("read resources: %w", err)
		}
		account = v

		return nil
	})

	g.Go(func() error {
		v, err := retry(gctx, s.nodes, emptyIfMissing(func() (*client.StakeInfo, error) {
			return s.client.GetStakeInfo(gctx, addr)
		}))
		if err != nil {
			return fmt.Errorf("read stake: %w", err)
		}
		stake = v

		return nil
	})

	g.Go(func() error {
		v, err := retry(gctx, s.nodes, emptyIfMissing(func() ([]client.Delegation, error) {
			return s.client.GetDelegatedResourcesV2(gctx, addr)
		}))
		if err != nil {
			return fmt.Errorf("read delegations: %w", err)
		}
		lent = v

		return nil
	})

	g.Go(func() error {
		v, err := retry(gctx, s.nodes, emptyIfMissing(func() (int64, error) {
			return s.client.GetAvailableUnstakeCount(gctx, addr)
		}))
		if err != nil {
			return fmt.Errorf("read unstake slots: %w", err)
		}
		slots = v

		return nil
	})

	g.Go(func() error {
		v, err := s.delegatable(gctx, addr, client.ResourceTypeBandwidth)
		canBW = v

		return err
	})

	g.Go(func() error {
		v, err := s.delegatable(gctx, addr, client.ResourceTypeEnergy)
		canEN = v

		return err
	})

	if err := g.Wait(); err != nil {
		return Resources{}, err
	}

	// An address that has never been funded exists nowhere on chain; every call
	// above then reports an empty position rather than a failure.
	if stake == nil {
		stake = &client.StakeInfo{}
	}

	out := Resources{
		Bandwidth: Pool{
			Available: s.client.AvailableBandwidth(account),
			Total:     s.client.TotalBandwidthLimit(account),
		},
		Energy: Pool{
			Available: s.client.AvailableEnergy(account),
			Total:     s.client.TotalEnergyLimit(account),
		},
		StakedBandwidth: stake.StakedBandwidth.TRX(),
		StakedEnergy:    stake.StakedEnergy.TRX(),
		Unstaking:       stake.UnstakingTotal.TRX(),
		WithdrawableNow: stake.WithdrawableNow.TRX(),
		// The network totals come from the same message as the pools, so the
		// rate costs no extra call. TotalEnergyLimit here is the network-wide
		// figure, not the account's own — it matches the chain parameter
		// getTotalEnergyCurrentLimit that gotron uses for the same conversion.
		BandwidthPerTRX: s.client.ConvertStakedToBandwidth(
			account.GetTotalNetWeight(), account.GetTotalNetLimit(), oneTRX),
		EnergyPerTRX: s.client.ConvertStakedToEnergy(
			account.GetTotalEnergyLimit(), account.GetTotalEnergyWeight(), oneTRX),
		CanDelegateBandwidth: canBW.TRX(),
		CanDelegateEnergy:    canEN.TRX(),
		UnstakeSlots:         slots,
	}

	for _, item := range stake.PendingUnstakes {
		out.Pending = append(out.Pending, Unstake{
			Resource: resourceFromClient(item.Resource),
			Amount:   item.Amount.TRX(),
			ExpireAt: item.ExpireTime,
		})
	}

	out.Delegations = splitDelegations(lent)

	return out, nil
}

// delegatable asks the node how much stake may still be lent out for one
// resource. An account that does not exist yet can lend nothing, which is an
// answer rather than a failure.
func (s *Service) delegatable(ctx context.Context, addr string, resource client.ResourceType) (client.SUN, error) {
	max, err := retry(ctx, s.nodes, emptyIfMissing(func() (client.SUN, error) {
		return s.client.GetCanDelegatedMaxSize(ctx, addr, resource)
	}))
	if err != nil {
		return 0, fmt.Errorf("read delegatable %s: %w", resourceFromClient(resource), err)
	}

	return max, nil
}

// splitDelegations turns the SDK's per-receiver records, which carry both
// resources at once, into one row per receiver and resource.
//
// Rows for the same pair are added together. The node can return several
// records for one receiver, and this list is read twice: once to show what is
// lent out, and once to work out how much a reclaim can take back. Leaving them
// apart would show two half amounts and reclaim only whichever came first.
func splitDelegations(lent []client.Delegation) []Delegation {
	type pair struct {
		to       string
		resource Resource
	}

	var (
		order  []pair
		staked = make(map[pair]client.SUN, len(lent)*2)
		locks  = make(map[pair]time.Time, len(lent)*2)
	)

	add := func(to string, resource Resource, amount client.SUN, lock time.Time) {
		// The SDK already drops records with nothing delegated, but one half of
		// a record is normally empty and would become a row of 0 TRX.
		if amount <= 0 {
			return
		}

		key := pair{to: to, resource: resource}
		if _, seen := staked[key]; !seen {
			order = append(order, key)
		}

		staked[key] += amount

		// A merged row is reclaimable only once its latest lock has passed.
		if lock.After(locks[key]) {
			locks[key] = lock
		}
	}

	for _, d := range lent {
		add(d.To, ResourceBandwidth, d.Bandwidth, d.BandwidthExpiresAt)
		add(d.To, ResourceEnergy, d.Energy, d.EnergyExpiresAt)
	}

	out := make([]Delegation, 0, len(order))
	for _, key := range order {
		out = append(out, Delegation{
			To:          key.to,
			Resource:    key.resource,
			Amount:      staked[key].TRX(),
			LockedUntil: locks[key],
		})
	}

	return out
}

// emptyIfMissing turns "this address does not exist on chain yet" into a zero
// result. A wallet that has never been funded has nothing staked and nothing
// delegated — that is an answer, not a failure, and retrying it against every
// other node would only cost requests.
func emptyIfMissing[T any](call func() (T, error)) func() (T, error) {
	return func() (T, error) {
		v, err := call()
		if errors.Is(err, client.ErrAccountNotFound) {
			var zero T
			return zero, nil
		}

		return v, err
	}
}

// stakeAmount validates a TRX amount for a staking operation.
//
// The check belongs here rather than in the SDK because the answer has to reach
// the caller as a bad request: the SDK's own refusal is indistinguishable from
// a node failure at the HTTP layer, and it would first be retried against every
// node.
func stakeAmount(amount decimal.Decimal) (client.SUN, error) {
	if amount.LessThanOrEqual(decimal.Zero) {
		return 0, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidRequest)
	}

	return trxAmount(amount)
}

// ValidateCounterparty reports whether a delegation between these two accounts
// is one the chain would take, without spending a round-trip — or a key
// derivation — to find out.
//
// It exists so the HTTP layer can refuse a delegation before it reaches the
// mnemonic. The operations validate again through the same code, so a caller
// that skips this loses nothing but the ordering.
func ValidateCounterparty(from, to string) error { return counterparty(from, to) }

// counterparty validates both sides of a delegation.
//
// The sender is checked here rather than being left to stakeOp: converting the
// amount into TRX takes a node call, and it happens first. A malformed sender
// would otherwise be spent on a round-trip per node and come back as an
// upstream failure instead of a bad request.
func counterparty(from, to string) error {
	if err := address.Validate(from); err != nil {
		return fmt.Errorf("%w: invalid sender address: %s", ErrInvalidRequest, err)
	}

	if err := address.Validate(to); err != nil {
		return fmt.Errorf("%w: invalid receiver address: %s", ErrInvalidRequest, err)
	}

	if from == to {
		return fmt.Errorf("%w: an account cannot delegate to itself", ErrInvalidRequest)
	}

	return nil
}

// Stake freezes TRX for bandwidth or energy and returns the txid.
func (s *Service) Stake(ctx context.Context, from string, resource Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}

	sun, err := stakeAmount(amount)
	if err != nil {
		return "", err
	}

	return s.stakeOp(ctx, from, key, func() (*api.TransactionExtention, error) {
		return s.client.Stake(ctx, from, kind, sun)
	})
}

// Unstake starts the unfreeze period for staked TRX and returns the txid. The
// TRX is not back until WithdrawUnstaked is called once that period expires.
func (s *Service) Unstake(ctx context.Context, from string, resource Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}

	sun, err := stakeAmount(amount)
	if err != nil {
		return "", err
	}

	return s.stakeOp(ctx, from, key, func() (*api.TransactionExtention, error) {
		return s.client.Unstake(ctx, from, kind, sun)
	})
}

// stakeBehind converts an amount of bandwidth or energy into the TRX that has
// to be frozen to yield it.
//
// Delegation is denominated in TRX on chain, but nobody thinks in "the TRX
// behind 10 000 energy" — they think in the energy. The conversion belongs on
// this side because the rate is a chain figure and the result is money: doing
// it in the browser would put an amount through a float on its way to a
// signature.
//
// It rounds up, so the receiver is never left just short of what was asked for.
func (s *Service) stakeBehind(ctx context.Context, addr string, resource Resource, units decimal.Decimal) (client.SUN, error) {
	if units.LessThanOrEqual(decimal.Zero) {
		return 0, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidRequest)
	}

	account, err := retry(ctx, s.nodes, func() (*api.AccountResourceMessage, error) {
		return s.client.GetAccountResource(ctx, addr)
	})
	if err != nil {
		return 0, fmt.Errorf("read resource rates: %w", err)
	}

	var sun client.SUN

	switch resource {
	case ResourceBandwidth:
		sun = s.client.ConvertBandwidthToStaked(
			account.GetTotalNetWeight(), account.GetTotalNetLimit(), units)
	case ResourceEnergy:
		sun = s.client.ConvertEnergyToStaked(
			account.GetTotalEnergyLimit(), account.GetTotalEnergyWeight(), units)
	default:
		return 0, fmt.Errorf("%w: unknown resource %q", ErrInvalidRequest, resource)
	}

	// Zero means the node reported no network total to price against, or the
	// request rounds to nothing. Either way the chain would refuse it with a
	// message about a balance the user never named.
	if sun <= 0 {
		return 0, fmt.Errorf("%w: %s %s is too little to move", ErrInvalidRequest, units, resource)
	}

	return sun, nil
}

// Delegate lends staked resource to another account and returns the txid.
//
// units is an amount of the resource itself — bandwidth points or energy — not
// TRX; see stakeBehind.
//
// The delegation is deliberately never locked: an unlocked one can be reclaimed
// at any time, so nothing done from this UI can strand the stake.
func (s *Service) Delegate(ctx context.Context, from, to string, resource Resource, units decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	if err := counterparty(from, to); err != nil {
		return "", err
	}

	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}

	sun, err := s.stakeBehind(ctx, from, resource, units)
	if err != nil {
		return "", err
	}

	return s.stakeOp(ctx, from, key, func() (*api.TransactionExtention, error) {
		return s.client.DelegateResource(ctx, from, to, kind, sun, false, 0)
	})
}

// Reclaim takes delegated resource back from the receiver and returns the txid.
// units is an amount of the resource, as for Delegate.
func (s *Service) Reclaim(ctx context.Context, from, to string, resource Resource, units decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	if err := counterparty(from, to); err != nil {
		return "", err
	}

	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}

	sun, err := s.stakeBehind(ctx, from, resource, units)
	if err != nil {
		return "", err
	}

	return s.stakeOp(ctx, from, key, func() (*api.TransactionExtention, error) {
		return s.client.ReclaimResource(ctx, from, to, kind, sun)
	})
}

// reclaimable adds up what may be taken back from one receiver for one
// resource.
//
// A delegation still under lock is refused outright rather than skipped: asking
// the node for it costs a round-trip and comes back naming neither the lock nor
// its expiry, and silently reclaiming only the unlocked part would take back
// less than the caller asked for without saying so.
func reclaimable(rows []Delegation, to string, resource Resource, now time.Time) (decimal.Decimal, error) {
	staked := decimal.Zero

	for _, d := range rows {
		if d.To != to || d.Resource != resource {
			continue
		}

		if !d.LockedUntil.IsZero() && d.LockedUntil.After(now) {
			return decimal.Zero, fmt.Errorf("%w: the %s delegated to %s is locked until %s",
				ErrInvalidRequest, resource, to, d.LockedUntil.Format(time.RFC3339))
		}

		staked = staked.Add(d.Amount)
	}

	if staked.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("%w: nothing is delegated to %s for %s", ErrInvalidRequest, to, resource)
	}

	return staked, nil
}

// ReclaimAll takes back everything delegated to one receiver for one resource.
//
// It exists because rounding makes "all of it" unreachable through Reclaim: the
// resource figure shown is the TRX amount converted at the current rate, and
// converting it back rounds up past what was actually delegated — which the
// chain refuses. Here the staked amount goes back verbatim.
func (s *Service) ReclaimAll(ctx context.Context, from, to string, resource Resource, key *ecdsa.PrivateKey) (string, error) {
	if err := counterparty(from, to); err != nil {
		return "", err
	}

	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}

	lent, err := retry(ctx, s.nodes, emptyIfMissing(func() ([]client.Delegation, error) {
		return s.client.GetDelegatedResourcesV2(ctx, from)
	}))
	if err != nil {
		return "", fmt.Errorf("read delegations: %w", err)
	}

	staked, err := reclaimable(splitDelegations(lent), to, resource, time.Now())
	if err != nil {
		return "", err
	}

	// The figure came from SUN, so converting it back is exact.
	sun, err := trxAmount(staked)
	if err != nil {
		return "", err
	}

	return s.stakeOp(ctx, from, key, func() (*api.TransactionExtention, error) {
		return s.client.ReclaimResource(ctx, from, to, kind, sun)
	})
}

// WithdrawUnstaked moves every expired unstake back into the spendable balance
// and returns the txid. Without it unstaked TRX stays out of reach for good.
func (s *Service) WithdrawUnstaked(ctx context.Context, from string, key *ecdsa.PrivateKey) (string, error) {
	return s.stakeOp(ctx, from, key, func() (*api.TransactionExtention, error) {
		return s.client.WithdrawUnstaked(ctx, from)
	})
}

// CancelUnstakes calls off every pending unstake, putting the TRX back into
// stake, and returns the txid. Entries that already expired are withdrawn.
func (s *Service) CancelUnstakes(ctx context.Context, from string, key *ecdsa.PrivateKey) (string, error) {
	return s.stakeOp(ctx, from, key, func() (*api.TransactionExtention, error) {
		return s.client.CancelAllUnstakes(ctx, from)
	})
}

func (s *Service) StakeWithSigner(
	ctx context.Context, from string, resource Resource, amount decimal.Decimal, signer chain.Signer,
) (string, error) {
	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}
	sun, err := stakeAmount(amount)
	if err != nil {
		return "", err
	}
	return s.stakeOpSigner(ctx, from, signer, func() (*api.TransactionExtention, error) {
		return s.client.Stake(ctx, from, kind, sun)
	})
}

func (s *Service) UnstakeWithSigner(
	ctx context.Context, from string, resource Resource, amount decimal.Decimal, signer chain.Signer,
) (string, error) {
	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}
	sun, err := stakeAmount(amount)
	if err != nil {
		return "", err
	}
	return s.stakeOpSigner(ctx, from, signer, func() (*api.TransactionExtention, error) {
		return s.client.Unstake(ctx, from, kind, sun)
	})
}

func (s *Service) DelegateWithSigner(
	ctx context.Context,
	from, to string,
	resource Resource,
	units decimal.Decimal,
	signer chain.Signer,
) (string, error) {
	if err := counterparty(from, to); err != nil {
		return "", err
	}
	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}
	sun, err := s.stakeBehind(ctx, from, resource, units)
	if err != nil {
		return "", err
	}
	return s.stakeOpSigner(ctx, from, signer, func() (*api.TransactionExtention, error) {
		return s.client.DelegateResource(ctx, from, to, kind, sun, false, 0)
	})
}

func (s *Service) ReclaimWithSigner(
	ctx context.Context,
	from, to string,
	resource Resource,
	units decimal.Decimal,
	signer chain.Signer,
) (string, error) {
	if err := counterparty(from, to); err != nil {
		return "", err
	}
	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}
	sun, err := s.stakeBehind(ctx, from, resource, units)
	if err != nil {
		return "", err
	}
	return s.stakeOpSigner(ctx, from, signer, func() (*api.TransactionExtention, error) {
		return s.client.ReclaimResource(ctx, from, to, kind, sun)
	})
}

func (s *Service) ReclaimAllWithSigner(
	ctx context.Context,
	from, to string,
	resource Resource,
	signer chain.Signer,
) (string, error) {
	if err := counterparty(from, to); err != nil {
		return "", err
	}
	kind, err := resource.toClient()
	if err != nil {
		return "", err
	}
	lent, err := retry(ctx, s.nodes, emptyIfMissing(func() ([]client.Delegation, error) {
		return s.client.GetDelegatedResourcesV2(ctx, from)
	}))
	if err != nil {
		return "", fmt.Errorf("read delegations: %w", err)
	}
	staked, err := reclaimable(splitDelegations(lent), to, resource, time.Now())
	if err != nil {
		return "", err
	}
	sun, err := trxAmount(staked)
	if err != nil {
		return "", err
	}
	return s.stakeOpSigner(ctx, from, signer, func() (*api.TransactionExtention, error) {
		return s.client.ReclaimResource(ctx, from, to, kind, sun)
	})
}

func (s *Service) WithdrawUnstakedWithSigner(
	ctx context.Context, from string, signer chain.Signer,
) (string, error) {
	return s.stakeOpSigner(ctx, from, signer, func() (*api.TransactionExtention, error) {
		return s.client.WithdrawUnstaked(ctx, from)
	})
}

func (s *Service) CancelUnstakesWithSigner(
	ctx context.Context, from string, signer chain.Signer,
) (string, error) {
	return s.stakeOpSigner(ctx, from, signer, func() (*api.TransactionExtention, error) {
		return s.client.CancelAllUnstakes(ctx, from)
	})
}

// stakeOp is the shape every staking operation shares: have a node build the
// transaction, sign it locally, broadcast it, and drop the now-stale balance.
//
// Building is a read-only call, so it is retried across nodes; the broadcast
// below it never is.
func (s *Service) stakeOp(ctx context.Context, from string, key *ecdsa.PrivateKey, build func() (*api.TransactionExtention, error)) (string, error) {
	if err := address.Validate(from); err != nil {
		return "", fmt.Errorf("%w: invalid sender address: %s", ErrInvalidRequest, err)
	}

	tx, err := retry(ctx, s.nodes, build)
	if err != nil {
		return "", s.chainError("build transaction", err)
	}

	txid, err := s.submit(ctx, tx, key)
	if err != nil {
		return "", err
	}

	// Staking moves TRX between the spendable balance and the frozen one, so
	// the cached balance is wrong either way.
	s.invalidate(from)

	return txid, nil
}

func (s *Service) stakeOpSigner(
	ctx context.Context,
	from string,
	signer chain.Signer,
	build func() (*api.TransactionExtention, error),
) (string, error) {
	if signer == nil || signer.Family() != chain.FamilyTron {
		return "", errors.New("Tron signer is required")
	}
	if err := address.Validate(from); err != nil {
		return "", fmt.Errorf("%w: invalid sender address: %s", ErrInvalidRequest, err)
	}
	tx, err := retry(ctx, s.nodes, build)
	if err != nil {
		return "", s.chainError("build transaction", err)
	}
	txid, err := s.submitWithSigner(ctx, tx, signer)
	if err != nil {
		return "", err
	}
	s.invalidate(from)
	return txid, nil
}
