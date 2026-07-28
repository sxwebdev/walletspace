package tron

import (
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/client"
)

// A second valid address, so a delegation has two distinct sides.
const otherAddress = "TUwbUgKvC1RsT3qShxmZcfMpvMdbE6JPST"

// Every one of these is refused before a node is contacted, so the failure must
// carry ErrInvalidRequest: the HTTP layer keys on it to answer 400 rather than
// 502, and a 502 would first be retried against every node.
//
// The Service holds no client at all, so reaching the chain would panic — which
// is the second half of the guarantee under test.
func TestStakingRejectsBadInputBeforeCallingTheChain(t *testing.T) {
	t.Parallel()

	one := decimal.NewFromInt(1)

	tests := []struct {
		name string
		call func(s *Service) error
	}{
		{
			name: "stake of zero",
			call: func(s *Service) error {
				_, err := s.Stake(t.Context(), someAddress, ResourceBandwidth, decimal.Zero, nil)
				return err
			},
		},
		{
			name: "stake of a negative amount",
			call: func(s *Service) error {
				_, err := s.Stake(t.Context(), someAddress, ResourceEnergy, decimal.NewFromInt(-5), nil)
				return err
			},
		},
		{
			name: "stake of an unknown resource",
			call: func(s *Service) error {
				_, err := s.Stake(t.Context(), someAddress, Resource("cpu"), one, nil)
				return err
			},
		},
		{
			name: "stake from an invalid address",
			call: func(s *Service) error {
				_, err := s.Stake(t.Context(), "nonsense", ResourceBandwidth, one, nil)
				return err
			},
		},
		{
			name: "unstake of zero",
			call: func(s *Service) error {
				_, err := s.Unstake(t.Context(), someAddress, ResourceBandwidth, decimal.Zero, nil)
				return err
			},
		},
		{
			name: "delegate to an invalid address",
			call: func(s *Service) error {
				_, err := s.Delegate(t.Context(), someAddress, "nonsense", ResourceEnergy, one, nil)
				return err
			},
		},
		{
			// The chain refuses this outright, and the refusal reads as a node
			// failure; catching it here says what is actually wrong.
			name: "delegate to self",
			call: func(s *Service) error {
				_, err := s.Delegate(t.Context(), someAddress, someAddress, ResourceEnergy, one, nil)
				return err
			},
		},
		{
			// Converting a resource amount into TRX takes a node call, and it
			// runs before stakeOp ever looks at the sender. Without an up-front
			// check a malformed `from` costs a round-trip per node and comes
			// back as "read resource rates: invalid address length", which is
			// not an ErrInvalidRequest — so the API would answer 502.
			name: "delegate from an invalid address",
			call: func(s *Service) error {
				_, err := s.Delegate(t.Context(), "nonsense", otherAddress, ResourceEnergy, one, nil)
				return err
			},
		},
		{
			name: "reclaim from an invalid address",
			call: func(s *Service) error {
				_, err := s.Reclaim(t.Context(), "nonsense", otherAddress, ResourceEnergy, one, nil)
				return err
			},
		},
		{
			// Delegation counts in resource units, and the conversion needs a
			// node — so a zero has to be caught before that call, not after.
			name: "delegate of zero",
			call: func(s *Service) error {
				_, err := s.Delegate(t.Context(), someAddress, otherAddress, ResourceEnergy, decimal.Zero, nil)
				return err
			},
		},
		{
			name: "delegate of an unknown resource",
			call: func(s *Service) error {
				_, err := s.Delegate(t.Context(), someAddress, otherAddress, Resource("cpu"), one, nil)
				return err
			},
		},
		{
			name: "reclaim everything to an invalid address",
			call: func(s *Service) error {
				_, err := s.ReclaimAll(t.Context(), someAddress, "nonsense", ResourceEnergy, nil)
				return err
			},
		},
		{
			name: "reclaim everything from self",
			call: func(s *Service) error {
				_, err := s.ReclaimAll(t.Context(), someAddress, someAddress, ResourceEnergy, nil)
				return err
			},
		},
		{
			name: "reclaim to an invalid address",
			call: func(s *Service) error {
				_, err := s.Reclaim(t.Context(), someAddress, "nonsense", ResourceEnergy, one, nil)
				return err
			},
		},
		{
			name: "reclaim of zero",
			call: func(s *Service) error {
				_, err := s.Reclaim(t.Context(), someAddress, otherAddress, ResourceEnergy, decimal.Zero, nil)
				return err
			},
		},
		{
			name: "withdraw from an invalid address",
			call: func(s *Service) error {
				_, err := s.WithdrawUnstaked(t.Context(), "nonsense", nil)
				return err
			},
		},
		{
			name: "cancel from an invalid address",
			call: func(s *Service) error {
				_, err := s.CancelUnstakes(t.Context(), "nonsense", nil)
				return err
			},
		},
		{
			name: "resources of an invalid address",
			call: func(s *Service) error {
				_, err := s.Resources(t.Context(), "nonsense")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.call(newTestService(nil))
			if err == nil {
				t.Fatal("got nil, want an error")
			}

			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("error %v does not wrap ErrInvalidRequest, so the API would answer 502", err)
			}
		})
	}
}

func TestSplitDelegationsMergesRowsForTheSamePair(t *testing.T) {
	t.Parallel()

	locked := time.UnixMilli(1_800_000_000_000)

	lent := []client.Delegation{
		{
			To: otherAddress,
			// One record carries both resources and usually only one is set;
			// the empty half must not become a row of 0 TRX.
			Bandwidth:       0,
			Energy:          200_000_000,
			EnergyExpiresAt: locked,
		},
		{
			To:        otherAddress,
			Bandwidth: 1_500_000,
		},
		{
			// A second record for the same pair. The node can return these, and
			// the UI reads one row per pair — both to show what is lent out and
			// to size a reclaim.
			To:     otherAddress,
			Energy: 50_000_000,
		},
	}

	got := splitDelegations(lent)

	if len(got) != 2 {
		t.Fatalf("got %d rows, want one per resource: %+v", len(got), got)
	}

	if got[0].Resource != ResourceEnergy || !got[0].Amount.Equal(decimal.NewFromInt(250)) {
		t.Errorf("energy row = %+v, want the two records summed to 250 TRX", got[0])
	}

	// The address has to be base58: it goes straight into a reclaim request.
	if got[0].To != otherAddress {
		t.Errorf("receiver = %q, want %q", got[0].To, otherAddress)
	}

	// The merged row is reclaimable only once its latest lock has passed.
	if !got[0].LockedUntil.Equal(locked) {
		t.Errorf("lock = %s, want the latest of the merged records (%s)", got[0].LockedUntil, locked)
	}

	if got[1].Resource != ResourceBandwidth || !got[1].Amount.Equal(decimal.RequireFromString("1.5")) {
		t.Errorf("bandwidth row = %+v, want 1.5 TRX", got[1])
	}

	if !got[1].LockedUntil.IsZero() {
		t.Errorf("unlocked row reported a lock until %s", got[1].LockedUntil)
	}
}

func TestReclaimable(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name    string
		rows    []Delegation
		want    string
		wantErr bool
	}{
		{
			name: "sums every row for the pair",
			rows: []Delegation{
				{To: otherAddress, Resource: ResourceEnergy, Amount: decimal.NewFromInt(200)},
				{To: otherAddress, Resource: ResourceEnergy, Amount: decimal.NewFromInt(50)},
			},
			want: "250",
		},
		{
			name: "ignores the other resource and the other receiver",
			rows: []Delegation{
				{To: otherAddress, Resource: ResourceEnergy, Amount: decimal.NewFromInt(200)},
				{To: otherAddress, Resource: ResourceBandwidth, Amount: decimal.NewFromInt(7)},
				{To: someAddress, Resource: ResourceEnergy, Amount: decimal.NewFromInt(999)},
			},
			want: "200",
		},
		{
			// The chain refuses the whole reclaim, so refusing it here says
			// which delegation is in the way and until when.
			name: "refuses while any of it is locked",
			rows: []Delegation{
				{To: otherAddress, Resource: ResourceEnergy, Amount: decimal.NewFromInt(200), LockedUntil: future},
			},
			wantErr: true,
		},
		{
			name: "an expired lock is no lock",
			rows: []Delegation{
				{To: otherAddress, Resource: ResourceEnergy, Amount: decimal.NewFromInt(200), LockedUntil: past},
			},
			want: "200",
		},
		{
			name:    "nothing delegated",
			rows:    []Delegation{{To: someAddress, Resource: ResourceEnergy, Amount: decimal.NewFromInt(5)}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := reclaimable(tt.rows, otherAddress, ResourceEnergy, now)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("reclaimable() = %s, want an error", got)
				}

				// The HTTP layer keys on this to answer 400 rather than 502.
				if !errors.Is(err, ErrInvalidRequest) {
					t.Errorf("error %v does not wrap ErrInvalidRequest", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("reclaimable() error = %v", err)
			}

			if !got.Equal(decimal.RequireFromString(tt.want)) {
				t.Errorf("reclaimable() = %s, want %s", got, tt.want)
			}
		})
	}
}
