package tron

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/walletspace/internal/chain"
)

// The whole of SEC-06 turns on this classification. A rejection means no
// transaction was accepted and building another one is safe; anything else
// means the transaction may be on chain, and the caller must be stopped from
// signing a second one for the same intent.
func TestBroadcastErrorSeparatesRejectionFromSilence(t *testing.T) {
	t.Parallel()

	wrapped := &client.BroadcastError{Code: api.Return_BANDWITH_ERROR}
	tests := []struct {
		name         string
		err          error
		cause        error // what must still be reachable; defaults to err
		wantUnknown  bool
		wantAccepted bool
	}{
		{
			name: "node rejected the transaction",
			err:  &client.BroadcastError{Code: api.Return_CONTRACT_VALIDATE_ERROR},
		},
		{
			// The node answered, so nothing was accepted — even though the
			// reason is about the node rather than the transaction.
			name: "node was busy",
			err:  &client.BroadcastError{Code: api.Return_SERVER_BUSY},
		},
		{
			// chainError rebuilds the message around the rejection it found, so
			// the wrapper is not what has to survive — the typed rejection is.
			name:  "rejection reached us wrapped",
			err:   fmt.Errorf("broadcast: %w", wrapped),
			cause: wrapped,
		},
		{
			// The node already holds this exact transaction, which is what a
			// re-broadcast of the same signed bytes looks like from its side.
			name:         "duplicate",
			err:          &client.BroadcastError{Code: api.Return_DUP_TRANSACTION_ERROR},
			wantAccepted: true,
		},
		{
			name:        "answer never arrived",
			err:         context.DeadlineExceeded,
			wantUnknown: true,
		},
		{
			name:        "connection cut",
			err:         io.ErrUnexpectedEOF,
			wantUnknown: true,
		},
		{
			name:        "transport gave up",
			err:         fmt.Errorf("read tcp: %w", io.EOF),
			wantUnknown: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestService(nil)
			got := s.broadcastError(tt.err)
			if tt.wantAccepted {
				if got != nil {
					t.Fatalf("broadcastError() = %v, want nil for an already-known transaction", got)
				}
				return
			}
			if got == nil {
				t.Fatal("broadcastError() = nil, want an error")
			}
			if unknown := errors.Is(got, chain.ErrBroadcastUnknown); unknown != tt.wantUnknown {
				t.Errorf(
					"wraps ErrBroadcastUnknown = %v, want %v (error: %v)",
					unknown, tt.wantUnknown, got,
				)
			}
			// The original cause has to survive either way — it is what the log
			// and the error message are built from.
			cause := tt.cause
			if cause == nil {
				cause = tt.err
			}
			if !errors.Is(got, cause) {
				t.Errorf("broadcastError() = %v, lost the cause %v", got, cause)
			}
		})
	}
}
