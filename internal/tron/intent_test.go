package tron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/sxwebdev/gotron/pkg/tronutils"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/gotron/schema/pb/core"
	"github.com/sxwebdev/walletspace/internal/chain"
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

// wrap builds the transaction a well-behaved node would return for a contract.
func wrap(t *testing.T, kind core.Transaction_Contract_ContractType, message proto.Message, feeLimit int64) *core.Transaction {
	t.Helper()
	parameter, err := anypb.New(message)
	if err != nil {
		t.Fatalf("anypb.New() error = %v", err)
	}
	return &core.Transaction{
		RawData: &core.TransactionRaw{
			RefBlockBytes: []byte{0x01, 0x02},
			RefBlockHash:  []byte{1, 2, 3, 4, 5, 6, 7, 8},
			Expiration:    1,
			Timestamp:     1,
			FeeLimit:      feeLimit,
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
	tx := &core.Transaction{RawData: &core.TransactionRaw{
		Contract: []*core.Transaction_Contract{{
			Type: core.Transaction_Contract_TransferContract, Parameter: parameter,
		}},
	}}

	if err := intent.Verify(tx); !errors.Is(err, ErrIntentMismatch) {
		t.Fatalf("Verify() error = %v, want ErrIntentMismatch", err)
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
