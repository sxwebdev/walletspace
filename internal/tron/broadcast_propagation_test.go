package tron

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jellydator/ttlcache/v3"
	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/gotron/schema/pb/core"
	"github.com/sxwebdev/walletspace/internal/chain"
	"google.golang.org/protobuf/types/known/anypb"
)

// deadNodeService talks to a port nothing is listening on, so every broadcast
// fails the way a lost answer does: no reply from the node at all.
func deadNodeService(t *testing.T) *Service {
	t.Helper()
	c, err := client.New(client.Config{
		Nodes: []client.NodeConfig{{
			Protocol: client.ProtocolHTTP, Address: "http://127.0.0.1:1",
		}},
		Network: "mainnet", Blockchain: "tron",
	})
	if err != nil {
		t.Fatalf("client.New() error = %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &Service{
		client: c, log: slog.New(slog.NewTextHandler(io.Discard, nil)), nodes: 1,
		cache:        newBalanceCache(balanceRetention),
		balanceSlots: make(chan struct{}, 1),
		estimates:    ttlcache.New(ttlcache.WithTTL[string, Estimate](estimateTTL)),
	}
}

func trxTransferTx(t *testing.T) (Intent, *api.TransactionExtention) {
	t.Helper()
	intent := Intent{
		Kind: IntentTransferTRX, Owner: ownerAddr, To: recipientAddr, Amount: 1_000_000,
	}
	parameter, err := anypb.New(&core.TransferContract{
		OwnerAddress: mustAddress(t, ownerAddr), ToAddress: mustAddress(t, recipientAddr),
		Amount: 1_000_000,
	})
	if err != nil {
		t.Fatalf("anypb.New() error = %v", err)
	}
	return intent, &api.TransactionExtention{Transaction: &core.Transaction{
		RawData: &core.TransactionRaw{Contract: []*core.Transaction_Contract{{
			Type: core.Transaction_Contract_TransferContract, Parameter: parameter,
		}}},
	}}
}

// The transaction id is computed from the bytes that were signed, so it is
// known whether or not the node ever answered. Returning it with the error is
// the whole of the broadcast_unknown design: without it the HTTP layer has
// nothing to record and reports a lost answer as a plain failure, which is what
// invites a second transfer.
func TestSubmitReturnsTheTxidWhenTheAnswerIsLost(t *testing.T) {
	t.Parallel()

	service := deadNodeService(t)
	intent, tx := trxTransferTx(t)
	signer := &countingSigner{}

	txid, err := service.submitWithSigner(t.Context(), intent, tx, signer)
	if !errors.Is(err, chain.ErrBroadcastUnknown) {
		t.Fatalf("submitWithSigner() error = %v, want ErrBroadcastUnknown", err)
	}
	if len(txid) != 64 {
		t.Fatalf("submitWithSigner() txid = %q, want the locally computed id", txid)
	}
	if signer.calls != 1 {
		t.Errorf("SignDigest called %d times, want 1", signer.calls)
	}
}

// Every operation wrapper used to end with `return "", err`, throwing away the
// id submitWithSigner had just computed — which made the branch above
// unreachable from the API. stakeOpSigner stands in for all of them: it takes
// its transaction from a callback, so it can be driven without a live node.
func TestOperationWrappersPropagateTheTxidWithTheError(t *testing.T) {
	t.Parallel()

	service := deadNodeService(t)
	intent, tx := trxTransferTx(t)
	signer := &countingSigner{}

	txid, err := service.stakeOpSigner(
		t.Context(), ownerAddr, intent, signer,
		func() (*api.TransactionExtention, error) { return tx, nil },
	)
	if !errors.Is(err, chain.ErrBroadcastUnknown) {
		t.Fatalf("stakeOpSigner() error = %v, want ErrBroadcastUnknown", err)
	}
	if len(txid) != 64 {
		t.Errorf("stakeOpSigner() txid = %q, want the id to survive the error", txid)
	}
}
