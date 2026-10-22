package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/operation"
)

// not_broadcast tells the UI it may sign again under a new key, so it may only
// be sent when the record says exactly that. A failure the wallet could not
// record, and a lost answer, must reach the browser without it.
func TestOnlyARecordedFailureIsLabelledNotBroadcast(t *testing.T) {
	t.Parallel()
	const spaceID = "spc_test"
	finish := func(t *testing.T, ops *operation.Store, key string, err error) map[string]string {
		t.Helper()
		p := &Platform{operations: ops, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
		request.SetPathValue("space_id", spaceID)
		response := httptest.NewRecorder()
		p.finishTronOperation(response, request, "tron-nile", key, "", err)
		var body map[string]string
		if decodeErr := json.Unmarshal(response.Body.Bytes(), &body); decodeErr != nil {
			t.Fatalf("decode %q: %v", response.Body.String(), decodeErr)
		}
		return body
	}
	begin := func(t *testing.T, home, key string) *operation.Store {
		t.Helper()
		ops := operation.New(home)
		if _, _, err := ops.Begin(spaceID, key, "hash", "tron-nile"); err != nil {
			t.Fatal(err)
		}
		return ops
	}

	t.Run("refused before broadcast", func(t *testing.T) {
		t.Parallel()
		ops := begin(t, t.TempDir(), "k1")
		body := finish(t, ops, "k1", fmt.Errorf("%w: price moved", chain.ErrStateChanged))
		if body["code"] != codeNotBroadcast {
			t.Errorf("code = %q, want %q", body["code"], codeNotBroadcast)
		}
		if got, _, _ := ops.Get(spaceID, "k1"); got.Status != operation.StatusFailed {
			t.Errorf("status = %q, want failed", got.Status)
		}
	})

	t.Run("answer lost", func(t *testing.T) {
		t.Parallel()
		ops := begin(t, t.TempDir(), "k2")
		body := finish(t, ops, "k2", fmt.Errorf("%w: timeout", chain.ErrBroadcastUnknown))
		if body["code"] != "" || body["status"] != operation.StatusBroadcastUnknown {
			t.Errorf("body = %v, want broadcast_unknown without a code", body)
		}
	})

	t.Run("record could not be written", func(t *testing.T) {
		t.Parallel()
		home := t.TempDir()
		ops := begin(t, home, "k3")
		// The space's directory turns read-only: the outcome cannot be stored.
		dir := filepath.Join(home, "spaces", spaceID)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Skip("cannot make the operation store read-only here")
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if _, err := ops.Update(spaceID, "k3", "", operation.StatusFailed); err == nil {
			t.Skip("the store still writes; running as a user that ignores permissions")
		}
		body := finish(t, ops, "k3", errors.New("invalid request: refused"))
		if body["code"] != "" {
			t.Errorf("code = %q for a failure that was never recorded", body["code"])
		}
	})
}
