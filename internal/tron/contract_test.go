package tron

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/schema/pb/core"
)

// A deployment that is valid apart from whatever the test changes.
func validDeployment() Deployment {
	return Deployment{
		Name: "Token",
		// 22 bytes: an init that copies ten bytes of runtime out of itself and
		// returns them. Short, but a real contract as far as the encoder cares.
		Bytecode:                   "600a600c600039600a6000f3602a60805260206080f3",
		FeeLimit:                   decimal.NewFromInt(1000),
		ConsumeUserResourcePercent: 100,
		OriginEnergyLimit:          10_000_000,
	}
}

// Deploying is the one operation here that carries an arbitrary blob, so most
// of what can be wrong with it is wrong before a node is involved. Every case
// below has to be refused with ErrInvalidRequest: the HTTP layer keys on it to
// answer 400 rather than 502, and a 502 would first be retried against every
// node — with a body that may be half a megabyte.
//
// The Service holds no client at all, so reaching the chain would panic, which
// is the second half of the guarantee under test.
func TestDeployRejectsBadInputBeforeCallingTheChain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		from  string
		spoil func(*Deployment)
	}{
		{
			name: "an invalid sender",
			from: "nonsense",
		},
		{
			name:  "no bytecode",
			spoil: func(d *Deployment) { d.Bytecode = "" },
		},
		{
			// "0x" is hex for nothing at all, and deploying it would put an
			// empty contract on chain rather than fail.
			name:  "bytecode that is only a prefix",
			spoil: func(d *Deployment) { d.Bytecode = "0x" },
		},
		{
			// Left-padding an odd-length string shifts every byte after it by
			// half a byte, so truncated input would deploy something else
			// entirely instead of being refused.
			name:  "truncated bytecode",
			spoil: func(d *Deployment) { d.Bytecode = "600a600c60003" },
		},
		{
			name:  "bytecode that is not hex",
			spoil: func(d *Deployment) { d.Bytecode = "not hex at all" },
		},
		{
			name:  "an ABI that is not JSON",
			spoil: func(d *Deployment) { d.ABI = "{" },
		},
		{
			// A bare object parses, but it is not Tron's envelope, and letting
			// it through would deploy a contract with an empty ABI.
			name:  "an ABI that is neither shape",
			spoil: func(d *Deployment) { d.ABI = `{"name":"transfer"}` },
		},
		{
			name:  "an unknown ABI entry type",
			spoil: func(d *Deployment) { d.ABI = `[{"type":"modifier","name":"onlyOwner"}]` },
		},
		{
			name:  "constructor arguments that do not encode",
			spoil: func(d *Deployment) { d.ConstructorParams = `[{"uint256":"not a number"}]` },
		},
		{
			// Zero leaves the field unset and the node applies its own default,
			// which is well below what a deployment costs — so the transaction
			// would be accepted, charged for, and die half-built.
			name:  "no fee limit",
			spoil: func(d *Deployment) { d.FeeLimit = decimal.Zero },
		},
		{
			name:  "a negative fee limit",
			spoil: func(d *Deployment) { d.FeeLimit = decimal.NewFromInt(-1) },
		},
		{
			name:  "a fee limit finer than a SUN",
			spoil: func(d *Deployment) { d.FeeLimit = decimal.New(1, -9) },
		},
		{
			name:  "a resource share above 100",
			spoil: func(d *Deployment) { d.ConsumeUserResourcePercent = 101 },
		},
		{
			name:  "a negative resource share",
			spoil: func(d *Deployment) { d.ConsumeUserResourcePercent = -1 },
		},
		{
			name:  "no origin energy limit",
			spoil: func(d *Deployment) { d.OriginEnergyLimit = 0 },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := validDeployment()
			if tt.spoil != nil {
				tt.spoil(&d)
			}

			from := someAddress
			if tt.from != "" {
				from = tt.from
			}

			// Pricing runs the same gauntlet: it also builds the deployment,
			// so every one of these has to be caught before it goes anywhere.
			for name, call := range map[string]func() error{
				"Deploy": func() error {
					_, err := newTestService(nil).Deploy(t.Context(), from, d, nil)
					return err
				},
				"EstimateDeploy": func() error {
					_, err := newTestService(nil).EstimateDeploy(t.Context(), from, d)
					return err
				},
			} {
				err := call()
				if err == nil {
					t.Fatalf("%s: got nil, want an error", name)
				}

				if !errors.Is(err, ErrInvalidRequest) {
					t.Errorf("%s: error %v does not wrap ErrInvalidRequest, so the API would answer 502", name, err)
				}
			}
		})
	}
}

// The valid case has to reach the chain, or the table above would pass just as
// well against a function that refuses everything.
func TestDeployAcceptsAValidDeployment(t *testing.T) {
	t.Parallel()

	d := validDeployment()
	d.ABI = `[{"inputs":[],"stateMutability":"nonpayable","type":"constructor"}]`
	d.ConstructorParams = ""

	if _, err := deployRequest(someAddress, d); err != nil {
		t.Fatalf("deployRequest() error = %v, want a request the chain would take", err)
	}
}

func TestDeployFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  core.Transaction_ResultContractResult
		message string
		want    string
	}{
		{
			name:   "a deployment that ran",
			result: core.Transaction_Result_SUCCESS,
		},
		{
			// The zero value, which a receipt carrying no contract result at
			// all also has. Calling that a failed deployment would be worse
			// than saying nothing.
			name:   "no contract result",
			result: core.Transaction_Result_DEFAULT,
		},
		{
			// What a fee limit set too low produces, and the reason the receipt
			// is waited for at all.
			name:   "a fee limit that did not cover the constructor",
			result: core.Transaction_Result_OUT_OF_ENERGY,
			want:   "OUT_OF_ENERGY",
		},
		{
			name:    "a constructor that gave up",
			result:  core.Transaction_Result_REVERT,
			message: "not the owner",
			want:    "REVERT: not the owner",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := deployFailure(&core.TransactionInfo{
				Receipt:    &core.ResourceReceipt{Result: tt.result},
				ResMessage: []byte(tt.message),
			})

			if got != tt.want {
				t.Errorf("deployFailure() = %q, want %q", got, tt.want)
			}
		})
	}
}
