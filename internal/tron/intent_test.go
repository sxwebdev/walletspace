package tron

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/sxwebdev/gotron/pkg/tronutils"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/gotron/schema/pb/core"
	"github.com/sxwebdev/walletspace/internal/chain"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	ownerAddr     = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	recipientAddr = "TJRabPrwbZy45sbavfcjinPJC18kjpRTv8"
	attackerAddr  = "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"
	tokenAddr     = "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf"
)

func mustAddress(t *testing.T, base58 string) []byte {
	t.Helper()
	raw, err := tronutils.DecodeCheck(base58)
	if err != nil {
		t.Fatalf("DecodeCheck(%s) error = %v", base58, err)
	}
	return raw
}

// wrap builds the transaction a well-behaved node would return for a contract,
// with the header it would have written just now.
func wrap(t *testing.T, kind core.Transaction_Contract_ContractType, message proto.Message, feeLimit int64) *core.Transaction {
	t.Helper()
	return wrapAt(t, time.Now(), kind, message, feeLimit)
}

// wrapAt is wrap with the node's clock supplied, for the cases that are about
// the header rather than the contract.
func wrapAt(
	t *testing.T,
	built time.Time,
	kind core.Transaction_Contract_ContractType,
	message proto.Message,
	feeLimit int64,
) *core.Transaction {
	t.Helper()
	parameter, err := anypb.New(message)
	if err != nil {
		t.Fatalf("anypb.New() error = %v", err)
	}
	return &core.Transaction{
		RawData: &core.TransactionRaw{
			RefBlockBytes: []byte{0x01, 0x02},
			RefBlockHash:  []byte{1, 2, 3, 4, 5, 6, 7, 8},
			// What a node actually writes: now, and a minute to get the
			// transaction into a block.
			Timestamp:  built.UnixMilli(),
			Expiration: built.Add(time.Minute).UnixMilli(),
			FeeLimit:   feeLimit,
			Contract: []*core.Transaction_Contract{
				{Type: kind, Parameter: parameter},
			},
		},
	}
}

// countingSigner records whether the private key was ever asked to sign. The
// audit's acceptance criterion is not merely that the request fails — it is
// that SignDigest is never reached.
type countingSigner struct{ calls int }

func (*countingSigner) Family() chain.Family { return chain.FamilyTron }
func (*countingSigner) PublicKey() []byte    { return make([]byte, 65) }
func (s *countingSigner) SignDigest(context.Context, []byte) ([]byte, error) {
	s.calls++
	return make([]byte, 65), nil
}

// A node is handed "transfer 1 TRX to the recipient" and is free to answer with
// something else entirely. Each case below changes exactly one thing in the
// node's reply; every one of them has to be refused before the key is touched.
func TestVerifyRejectsASubstitutedTRXTransfer(t *testing.T) {
	t.Parallel()

	intent := Intent{
		Kind: IntentTransferTRX, Owner: ownerAddr, To: recipientAddr, Amount: 1_000_000,
	}
	honest := func() *core.TransferContract {
		return &core.TransferContract{
			OwnerAddress: mustAddress(t, ownerAddr),
			ToAddress:    mustAddress(t, recipientAddr),
			Amount:       1_000_000,
		}
	}

	if err := intent.Verify(wrap(t, core.Transaction_Contract_TransferContract, honest(), 0)); err != nil {
		t.Fatalf("Verify() rejected the transaction that was asked for: %v", err)
	}

	tests := []struct {
		name     string
		tampered func() *core.Transaction
	}{
		{
			name: "recipient swapped for the attacker",
			tampered: func() *core.Transaction {
				value := honest()
				value.ToAddress = mustAddress(t, attackerAddr)
				return wrap(t, core.Transaction_Contract_TransferContract, value, 0)
			},
		},
		{
			name: "amount raised",
			tampered: func() *core.Transaction {
				value := honest()
				value.Amount = 999_000_000
				return wrap(t, core.Transaction_Contract_TransferContract, value, 0)
			},
		},
		{
			name: "owner swapped",
			tampered: func() *core.Transaction {
				value := honest()
				value.OwnerAddress = mustAddress(t, attackerAddr)
				return wrap(t, core.Transaction_Contract_TransferContract, value, 0)
			},
		},
		{
			name: "fee limit added",
			tampered: func() *core.Transaction {
				return wrap(t, core.Transaction_Contract_TransferContract, honest(), 1_000_000_000)
			},
		},
		{
			name: "contract type changed",
			tampered: func() *core.Transaction {
				return wrap(t, core.Transaction_Contract_TransferAssetContract, honest(), 0)
			},
		},
		{
			name: "signed under another permission",
			tampered: func() *core.Transaction {
				tx := wrap(t, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Contract[0].PermissionId = 2
				return tx
			},
		},
		{
			name: "a second contract appended to a correct first one",
			tampered: func() *core.Transaction {
				tx := wrap(t, core.Transaction_Contract_TransferContract, honest(), 0)
				drain := &core.TransferContract{
					OwnerAddress: mustAddress(t, ownerAddr),
					ToAddress:    mustAddress(t, attackerAddr),
					Amount:       999_000_000,
				}
				parameter, err := anypb.New(drain)
				if err != nil {
					t.Fatalf("anypb.New() error = %v", err)
				}
				tx.RawData.Contract = append(tx.RawData.Contract, &core.Transaction_Contract{
					Type: core.Transaction_Contract_TransferContract, Parameter: parameter,
				})
				return tx
			},
		},
		{
			name: "no contract at all",
			tampered: func() *core.Transaction {
				tx := wrap(t, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Contract = nil
				return tx
			},
		},
		{
			name: "scripts smuggled in",
			tampered: func() *core.Transaction {
				tx := wrap(t, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Scripts = []byte{0xde, 0xad}
				return tx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tx := tt.tampered()
			if err := intent.Verify(tx); !errors.Is(err, ErrIntentMismatch) {
				t.Fatalf("Verify() error = %v, want ErrIntentMismatch", err)
			}

			// And the same substitution must not reach the key.
			signer := &countingSigner{}
			_, err := (&Service{}).submitWithSigner(
				t.Context(), intent, &api.TransactionExtention{Transaction: tx}, signer,
			)
			if !errors.Is(err, ErrIntentMismatch) {
				t.Fatalf("submitWithSigner() error = %v, want ErrIntentMismatch", err)
			}
			if signer.calls != 0 {
				t.Errorf("SignDigest was called %d times for a substituted transaction", signer.calls)
			}
		})
	}
}

// A TRC20 transfer keeps who and how much inside opaque calldata, so a node can
// leave the contract address alone — the one field a shallow check would look
// at — and rewrite the payment.
func TestVerifyRejectsASubstitutedTRC20Transfer(t *testing.T) {
	t.Parallel()

	data, err := trc20TransferData(recipientAddr, big.NewInt(1_000_000))
	if err != nil {
		t.Fatalf("trc20TransferData() error = %v", err)
	}
	intent := Intent{
		Kind: IntentTriggerContract, Owner: ownerAddr, To: recipientAddr,
		Contract: tokenAddr, Data: data, FeeLimit: 50_000_000,
	}
	honest := func() *core.TriggerSmartContract {
		return &core.TriggerSmartContract{
			OwnerAddress:    mustAddress(t, ownerAddr),
			ContractAddress: mustAddress(t, tokenAddr),
			Data:            data,
		}
	}

	if err := intent.Verify(
		wrap(t, core.Transaction_Contract_TriggerSmartContract, honest(), 50_000_000),
	); err != nil {
		t.Fatalf("Verify() rejected the transfer that was asked for: %v", err)
	}

	attackerData, err := trc20TransferData(attackerAddr, big.NewInt(999_000_000))
	if err != nil {
		t.Fatalf("trc20TransferData() error = %v", err)
	}

	tests := []struct {
		name     string
		tampered func() *core.Transaction
	}{
		{
			name: "calldata rewritten to pay the attacker",
			tampered: func() *core.Transaction {
				value := honest()
				value.Data = attackerData
				return wrap(t, core.Transaction_Contract_TriggerSmartContract, value, 50_000_000)
			},
		},
		{
			name: "token contract swapped",
			tampered: func() *core.Transaction {
				value := honest()
				value.ContractAddress = mustAddress(t, attackerAddr)
				return wrap(t, core.Transaction_Contract_TriggerSmartContract, value, 50_000_000)
			},
		},
		{
			name: "TRX attached to the call",
			tampered: func() *core.Transaction {
				value := honest()
				value.CallValue = 500_000_000
				return wrap(t, core.Transaction_Contract_TriggerSmartContract, value, 50_000_000)
			},
		},
		{
			name: "fee limit raised",
			tampered: func() *core.Transaction {
				return wrap(t, core.Transaction_Contract_TriggerSmartContract, honest(), 5_000_000_000)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			signer := &countingSigner{}
			_, err := (&Service{}).submitWithSigner(
				t.Context(), intent, &api.TransactionExtention{Transaction: tt.tampered()}, signer,
			)
			if !errors.Is(err, ErrIntentMismatch) {
				t.Fatalf("submitWithSigner() error = %v, want ErrIntentMismatch", err)
			}
			if signer.calls != 0 {
				t.Errorf("SignDigest was called %d times for a substituted transfer", signer.calls)
			}
		})
	}
}

// Delegation hands resource to another account. Redirecting the receiver, or
// quietly locking it, costs the owner the stake for as long as the lock lasts.
func TestVerifyRejectsASubstitutedDelegation(t *testing.T) {
	t.Parallel()

	intent := Intent{
		Kind: IntentDelegate, Owner: ownerAddr, To: recipientAddr,
		Amount: 1_000_000, Resource: core.ResourceCode_ENERGY,
	}
	honest := func() *core.DelegateResourceContract {
		return &core.DelegateResourceContract{
			OwnerAddress:    mustAddress(t, ownerAddr),
			ReceiverAddress: mustAddress(t, recipientAddr),
			Balance:         1_000_000,
			Resource:        core.ResourceCode_ENERGY,
		}
	}

	if err := intent.Verify(
		wrap(t, core.Transaction_Contract_DelegateResourceContract, honest(), 0),
	); err != nil {
		t.Fatalf("Verify() rejected the delegation that was asked for: %v", err)
	}

	tests := []struct {
		name     string
		tampered func() *core.DelegateResourceContract
	}{
		{
			name: "receiver redirected",
			tampered: func() *core.DelegateResourceContract {
				value := honest()
				value.ReceiverAddress = mustAddress(t, attackerAddr)
				return value
			},
		},
		{
			name: "resource switched",
			tampered: func() *core.DelegateResourceContract {
				value := honest()
				value.Resource = core.ResourceCode_BANDWIDTH
				return value
			},
		},
		{
			name: "amount raised",
			tampered: func() *core.DelegateResourceContract {
				value := honest()
				value.Balance = 900_000_000
				return value
			},
		},
		{
			name: "locked so it cannot be reclaimed",
			tampered: func() *core.DelegateResourceContract {
				value := honest()
				value.Lock = true
				value.LockPeriod = 30
				return value
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tx := wrap(t, core.Transaction_Contract_DelegateResourceContract, tt.tampered(), 0)
			if err := intent.Verify(tx); !errors.Is(err, ErrIntentMismatch) {
				t.Fatalf("Verify() error = %v, want ErrIntentMismatch", err)
			}
		})
	}
}

// The parameter is an Any, so a node can label it as one contract while the
// bytes decode as another. UnmarshalTo checks the type URL; without that the
// wrong message would decode to a zero value and sail past the field checks.
func TestVerifyRejectsAMislabelledParameter(t *testing.T) {
	t.Parallel()

	intent := Intent{
		Kind: IntentTransferTRX, Owner: ownerAddr, To: recipientAddr, Amount: 1_000_000,
	}
	parameter, err := anypb.New(&core.FreezeBalanceV2Contract{
		OwnerAddress: mustAddress(t, ownerAddr), FrozenBalance: 1_000_000,
	})
	if err != nil {
		t.Fatalf("anypb.New() error = %v", err)
	}
	// Built through wrap so the header is beyond reproach: the refusal has to
	// come from the parameter's type URL and nothing else.
	tx := wrap(t, core.Transaction_Contract_TransferContract, &core.TransferContract{}, 0)
	tx.RawData.Contract[0].Parameter = parameter

	if err := intent.Verify(tx); !errors.Is(err, ErrIntentMismatch) {
		t.Fatalf("Verify() error = %v, want ErrIntentMismatch", err)
	}
}

// The four raw_data fields outside the contract are the node's to choose and
// are covered by the signature just as the contract is. The expiration is the
// one worth taking: a node that sets it a day out, collects a signature over a
// correct transfer and then reports a lost broadcast keeps a spendable transfer
// it can send whenever it likes.
func TestVerifyRejectsAHostileTransactionHeader(t *testing.T) {
	t.Parallel()

	intent := Intent{
		Kind: IntentTransferTRX, Owner: ownerAddr, To: recipientAddr, Amount: 1_000_000,
	}
	honest := func() *core.TransferContract {
		return &core.TransferContract{
			OwnerAddress: mustAddress(t, ownerAddr),
			ToAddress:    mustAddress(t, recipientAddr),
			Amount:       1_000_000,
		}
	}
	// Fixed so the bounds are exercised rather than raced against the clock.
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		tampered func() *core.Transaction
	}{
		{
			name: "valid for a day",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Expiration = now.Add(24 * time.Hour).UnixMilli()
				return tx
			},
		},
		{
			name: "valid for an hour",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Expiration = now.Add(time.Hour).UnixMilli()
				return tx
			},
		},
		{
			// Built recently enough to pass the timestamp bound, so the
			// expiration is the only thing that can refuse it.
			name: "already expired",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now.Add(-9*time.Minute), core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Expiration = now.Add(-8 * time.Minute).UnixMilli()
				return tx
			},
		},
		{
			name: "no expiration at all",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Expiration = 0
				return tx
			},
		},
		{
			name: "built last week",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now.Add(-7*24*time.Hour), core.Transaction_Contract_TransferContract, honest(), 0)
				// Expiration alone would pass; the stale timestamp is the tell.
				tx.RawData.Expiration = now.Add(time.Minute).UnixMilli()
				return tx
			},
		},
		{
			name: "built in the future",
			tampered: func() *core.Transaction {
				return wrapAt(t, now.Add(time.Hour), core.Transaction_Contract_TransferContract, honest(), 0)
			},
		},
		{
			name: "no timestamp at all",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Timestamp = 0
				return tx
			},
		},
		{
			name: "expires before it was built",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Timestamp = now.Add(30 * time.Second).UnixMilli()
				tx.RawData.Expiration = now.Add(10 * time.Second).UnixMilli()
				return tx
			},
		},
		{
			// The header is int64 milliseconds and the values are the node's,
			// so the extremes have to be refused by the bounds rather than by
			// what time.UnixMilli happens to do when it overflows.
			name: "expiration at the end of time",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Expiration = math.MaxInt64
				return tx
			},
		},
		{
			name: "timestamp at the start of time",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.Timestamp = math.MinInt64
				return tx
			},
		},
		{
			name: "reference block height of the wrong size",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.RefBlockBytes = []byte{0x01}
				return tx
			},
		},
		{
			name: "no reference block hash",
			tampered: func() *core.Transaction {
				tx := wrapAt(t, now, core.Transaction_Contract_TransferContract, honest(), 0)
				tx.RawData.RefBlockHash = nil
				return tx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := intent.verify(tt.tampered(), now); !errors.Is(err, ErrIntentMismatch) {
				t.Fatalf("verify() error = %v, want ErrIntentMismatch", err)
			}
		})
	}
}

// raw_data has fields no Intent has an opinion about, and a node picks every
// one of them. They are covered by the signature and billed by the byte, so a
// correct contract is not on its own enough to sign: a one-TRX transfer with a
// hundred kilobytes bolted to its side is still a hundred kilobytes of the
// node's bandwidth bill, paid out of the user's TRX and published on chain
// under the user's address for good.
func TestVerifyRejectsBytesNoIntentAsksFor(t *testing.T) {
	t.Parallel()

	intent := Intent{
		Kind: IntentTransferTRX, Owner: ownerAddr, To: recipientAddr, Amount: 1_000_000,
	}
	// Correct in every way the contract can be checked, so that each case below
	// is refused for the field it adds and nothing else.
	honest := func() *core.Transaction {
		return wrap(t, core.Transaction_Contract_TransferContract, &core.TransferContract{
			OwnerAddress: mustAddress(t, ownerAddr),
			ToAddress:    mustAddress(t, recipientAddr),
			Amount:       1_000_000,
		}, 0)
	}
	// A field number this build has no name for, carrying bytes of the node's
	// choosing: what a newer protocol version, or a node speaking its own
	// dialect, looks like once it has been decoded and kept.
	unknownField := protowire.AppendTag(nil, 99, protowire.BytesType)
	unknownField = protowire.AppendBytes(unknownField, []byte("a meaning this build cannot read"))

	tests := []struct {
		name     string
		tampered func() *core.Transaction
	}{
		{
			name: "a memo the user never wrote",
			tampered: func() *core.Transaction {
				tx := honest()
				tx.RawData.Data = []byte("attributed to the owner, forever")
				return tx
			},
		},
		{
			// The size is the cost. Bandwidth is charged per serialised byte and
			// paid in burnt TRX once the free daily allowance is gone, and
			// fee_limit — zero on a plain transfer, and a cap on energy rather
			// than bandwidth — does nothing about it.
			name: "a hundred kilobytes of memo billed to the user as bandwidth",
			tampered: func() *core.Transaction {
				tx := honest()
				tx.RawData.Data = bytes.Repeat([]byte{0xff}, 100*1024)
				return tx
			},
		},
		{
			name: "a reference block height naming a block other than ref_block_bytes",
			tampered: func() *core.Transaction {
				tx := honest()
				// ref_block_bytes says 0x0102; the low two bytes here say 0x0103.
				tx.RawData.RefBlockNum = 65_536_259
				return tx
			},
		},
		{
			// Chosen so that its low two bytes are exactly the 0x0102 in
			// ref_block_bytes: the agreement check sees nothing wrong with it,
			// and the only thing left to refuse it with is that no block has a
			// negative height. A node that wanted the field to carry something
			// of its own would pick a value like this one.
			name: "a negative reference block height that agrees with ref_block_bytes",
			tampered: func() *core.Transaction {
				tx := honest()
				tx.RawData.RefBlockNum = -65_278
				return tx
			},
		},
		{
			name: "a field on the transaction this build cannot name",
			tampered: func() *core.Transaction {
				tx := honest()
				tx.RawData.ProtoReflect().SetUnknown(unknownField)
				return tx
			},
		},
		{
			name: "a field on the contract this build cannot name",
			tampered: func() *core.Transaction {
				tx := honest()
				tx.RawData.Contract[0].ProtoReflect().SetUnknown(unknownField)
				return tx
			},
		},
		{
			// The parameter is wrapped in an Any, and the Any has room for
			// fields of its own beside the type URL and the payload. Rejecting
			// unknown fields on what the payload decodes to says nothing about
			// the envelope around it.
			name: "a field on the parameter envelope this build cannot name",
			tampered: func() *core.Transaction {
				tx := honest()
				tx.RawData.Contract[0].Parameter.ProtoReflect().SetUnknown(unknownField)
				return tx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tx := tt.tampered()
			if err := intent.Verify(tx); !errors.Is(err, ErrIntentMismatch) {
				t.Fatalf("Verify() error = %v, want ErrIntentMismatch", err)
			}

			// And the payload must not reach the key: a signature over it is
			// what makes the user pay for it.
			signer := &countingSigner{}
			_, err := deadNodeService(t).submitWithSigner(
				t.Context(), intent, &api.TransactionExtention{Transaction: tx}, signer,
			)
			if !errors.Is(err, ErrIntentMismatch) {
				t.Fatalf("submitWithSigner() error = %v, want ErrIntentMismatch", err)
			}
			if signer.calls != 0 {
				t.Errorf("SignDigest was called %d times for bytes nobody asked for", signer.calls)
			}
		})
	}
}

// The bounds have to leave a real node room to work: it builds against a block
// that is already a few seconds old, on a clock that is not ours.
func TestVerifyAcceptsTheHeaderARealNodeWrites(t *testing.T) {
	t.Parallel()

	intent := Intent{
		Kind: IntentTransferTRX, Owner: ownerAddr, To: recipientAddr, Amount: 1_000_000,
	}
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	contract := &core.TransferContract{
		OwnerAddress: mustAddress(t, ownerAddr),
		ToAddress:    mustAddress(t, recipientAddr),
		Amount:       1_000_000,
	}

	for _, skew := range []time.Duration{-90 * time.Second, 0, 90 * time.Second} {
		tx := wrapAt(t, now.Add(skew), core.Transaction_Contract_TransferContract, contract, 0)
		if err := intent.verify(tx, now); err != nil {
			t.Errorf("verify() with a node clock %s off = %v, want nil", skew, err)
		}
	}

	// java-tron writes the two bytes of the reference block height and leaves
	// the height itself at zero — the fixtures above, and every real answer. A
	// node that does fill the height in has to keep passing too, or the check on
	// it would be a rule about this build's habits rather than about the block:
	// the height and the bytes come from one number, so they agree.
	filled := wrapAt(t, now, core.Transaction_Contract_TransferContract, contract, 0)
	filled.RawData.RefBlockNum = 65_536_258 // low two bytes are the 0x0102 above
	if err := intent.verify(filled, now); err != nil {
		t.Errorf("verify() with the reference block height written out = %v, want nil", err)
	}
}

// The header check belongs on the signing path, not only in a unit test: it is
// the last thing between a node's answer and the private key.
func TestALongLivedTransactionNeverReachesTheKey(t *testing.T) {
	t.Parallel()

	intent := Intent{
		Kind: IntentTransferTRX, Owner: ownerAddr, To: recipientAddr, Amount: 1_000_000,
	}
	tx := wrap(t, core.Transaction_Contract_TransferContract, &core.TransferContract{
		OwnerAddress: mustAddress(t, ownerAddr),
		ToAddress:    mustAddress(t, recipientAddr),
		Amount:       1_000_000,
	}, 0)
	tx.RawData.Expiration = time.Now().Add(24 * time.Hour).UnixMilli()

	// A service with a reachable client rather than a zero value, so that a
	// regression here fails on the assertion below instead of panicking its way
	// through the rest of the package.
	signer := &countingSigner{}
	_, err := deadNodeService(t).submitWithSigner(
		t.Context(), intent, &api.TransactionExtention{Transaction: tx}, signer,
	)
	if !errors.Is(err, ErrIntentMismatch) {
		t.Fatalf("submitWithSigner() error = %v, want ErrIntentMismatch", err)
	}
	if signer.calls != 0 {
		t.Errorf("SignDigest was called %d times for a transaction valid for a day", signer.calls)
	}
}

// The txid must be derived from the bytes that get signed, not read out of the
// node's reply. A node reporting an unrelated one would have that value
// persisted and shown, leaving the real transaction untracked and unfindable.
func TestTransactionDigestIsSHA256OverCanonicalRawData(t *testing.T) {
	t.Parallel()

	tx := wrap(t, core.Transaction_Contract_TransferContract, &core.TransferContract{
		OwnerAddress: mustAddress(t, ownerAddr),
		ToAddress:    mustAddress(t, recipientAddr),
		Amount:       1_000_000,
	}, 0)

	digest, txid, err := transactionDigest(tx)
	if err != nil {
		t.Fatalf("transactionDigest() error = %v", err)
	}

	rawData, err := proto.Marshal(tx.GetRawData())
	if err != nil {
		t.Fatalf("proto.Marshal() error = %v", err)
	}
	want := sha256.Sum256(rawData)
	if digest != want {
		t.Errorf("digest = %x, want %x", digest, want)
	}
	if txid != hex.EncodeToString(want[:]) {
		t.Errorf("txid = %s, want %s", txid, hex.EncodeToString(want[:]))
	}

	// Changing any part of the transaction has to change the identifier, or the
	// two could not be tied to each other at all.
	tampered := wrap(t, core.Transaction_Contract_TransferContract, &core.TransferContract{
		OwnerAddress: mustAddress(t, ownerAddr),
		ToAddress:    mustAddress(t, attackerAddr),
		Amount:       1_000_000,
	}, 0)
	if _, other, digestErr := transactionDigest(tampered); digestErr != nil {
		t.Fatalf("transactionDigest() error = %v", digestErr)
	} else if other == txid {
		t.Error("a different recipient produced the same txid")
	}
}

// The whole point of computing it locally: whatever the node claims the id is,
// it is not what gets recorded.
func TestSubmitIgnoresTheTxidTheNodeReports(t *testing.T) {
	t.Parallel()

	tx := wrap(t, core.Transaction_Contract_TransferContract, &core.TransferContract{
		OwnerAddress: mustAddress(t, ownerAddr),
		ToAddress:    mustAddress(t, recipientAddr),
		Amount:       1_000_000,
	}, 0)
	extention := &api.TransactionExtention{
		Transaction: tx,
		Txid:        []byte{0xde, 0xad, 0xbe, 0xef}, // what a hostile node would prefer
	}

	_, local, err := transactionDigest(extention.GetTransaction())
	if err != nil {
		t.Fatalf("transactionDigest() error = %v", err)
	}
	if local == hex.EncodeToString(extention.GetTxid()) {
		t.Fatal("the fixture cannot distinguish the two values")
	}
	if len(local) != 64 {
		t.Errorf("local txid = %q, want 32 hex-encoded bytes", local)
	}
}
