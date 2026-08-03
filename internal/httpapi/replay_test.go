package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/operation"
)

// Advising a new idempotency key means "build and sign a second transaction".
// That is only ever safe when the first provably never reached a node, and the
// Tron paths used to reach it for every failure — including a broadcast whose
// answer was lost — which is how one transfer could become two.
func TestReplayOnlyInvitesANewTransactionWhenTheOldOneCannotExist(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		existing   operation.Operation
		wantReject bool
		wantBody   string
	}{
		{
			name:       "still building",
			existing:   operation.Operation{Status: operation.StatusBuilding},
			wantReject: true,
			wantBody:   "still in progress",
		},
		{
			name:       "failed before broadcast",
			existing:   operation.Operation{Status: operation.StatusFailed},
			wantReject: true,
			wantBody:   "new idempotency key",
		},
		{
			// A rejected transaction can still have an id: the recorder writes
			// it before the signature. Having an id is not what makes a retry
			// unsafe — having sent it is.
			name: "rejected after its id was recorded",
			existing: operation.Operation{
				Status: operation.StatusFailed, TxHash: "abc",
			},
			wantReject: true,
			wantBody:   "new idempotency key",
		},
		{
			name: "signed and on its way",
			existing: operation.Operation{
				Status: operation.StatusBroadcasting, TxHash: "abc",
			},
		},
		{
			name: "answer was lost",
			existing: operation.Operation{
				Status: operation.StatusBroadcastUnknown, TxHash: "abc",
			},
		},
		{
			name:     "accepted",
			existing: operation.Operation{Status: operation.StatusPending, TxHash: "abc"},
		},
		{
			name:     "confirmed",
			existing: operation.Operation{Status: operation.StatusConfirmed, TxHash: "abc"},
		},
		{
			// Nothing writes this, but being wrong towards "it may exist" costs
			// a confusing answer while being wrong the other way costs funds.
			name:     "unrecognised",
			existing: operation.Operation{Status: "something-new"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			got := rejectIncompleteReplay(recorder, tt.existing)
			if got != tt.wantReject {
				t.Fatalf("rejectIncompleteReplay() = %v, want %v", got, tt.wantReject)
			}
			if !tt.wantReject {
				return
			}
			if recorder.Code != http.StatusConflict {
				t.Errorf("status = %d, want %d", recorder.Code, http.StatusConflict)
			}
			if !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to mention %q", recorder.Body.String(), tt.wantBody)
			}
		})
	}
}
