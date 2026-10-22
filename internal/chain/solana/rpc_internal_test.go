package solana

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/sxwebdev/walletspace/internal/network"
)

// The cap reads a body of exactly the limit to its end and fails the one that
// goes past it, rather than truncating it silently.
func TestLimitedBody(t *testing.T) {
	t.Parallel()

	for size, wantErr := range map[int]error{maxResponseBytes: nil, maxResponseBytes + 1: errResponseTooLarge} {
		body := &limitedBody{body: io.NopCloser(bytes.NewReader(make([]byte, size))), remaining: maxResponseBytes}
		data, err := io.ReadAll(body)
		if !errors.Is(err, wantErr) {
			t.Errorf("size %d: error = %v, want %v", size, err, wantErr)
		}
		if len(data) != maxResponseBytes {
			t.Errorf("size %d: read %d bytes, want %d", size, len(data), maxResponseBytes)
		}
		if err := body.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}
}

// A credential can sit in a header or anywhere a provider puts it in the URL;
// only an endpoint with neither is held to the public limits.
func TestKeyedEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		endpoint string
		headers  http.Header
		want     bool
	}{
		{endpoint: "https://api.mainnet.solana.com", want: false},
		{endpoint: "https://api.devnet.solana.com/", want: false},
		{endpoint: "https://api.mainnet.solana.com", headers: http.Header{"X-Api-Key": {"k"}}, want: true},
		{endpoint: "https://api.mainnet.solana.com", headers: http.Header{"Content-Type": {"x"}}, want: false},
		{endpoint: "https://user:secret@rpc.example", want: true},
		{endpoint: "https://mainnet.helius-rpc.com/?api-key=secret", want: true},
		{endpoint: "https://solana-mainnet.g.alchemy.com/v2/secret", want: true},
	}
	for _, tt := range tests {
		if got := keyed(tt.endpoint, tt.headers); got != tt.want {
			t.Errorf("keyed(%q, %v) = %v, want %v", tt.endpoint, tt.headers, got, tt.want)
		}
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range registry.List() {
		if item.Family != Family {
			continue
		}
		for _, endpoint := range item.RPCFallbacks {
			if keyed(endpoint, nil) {
				t.Errorf("built-in fallback %s is taken for a keyed endpoint", endpoint)
			}
		}
	}
}

// A broadcast waits for its paced turn on a public node, and a caller that
// leaves before it comes sends nothing.
func TestOnceWaitsForItsTurnOrLeaves(t *testing.T) {
	t.Parallel()

	paced := &pacer{next: map[string]time.Time{"": time.Now().Add(time.Hour)}}
	calls := 0
	list := &nodes{list: []*node{{endpoint: "https://rpc.example", pacer: paced}}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	err := list.once(ctx, "sendTransaction", func(*rpc.Client) error { calls++; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
		t.Errorf("once() = %v after %d calls, want the deadline before any call", err, calls)
	}
}
