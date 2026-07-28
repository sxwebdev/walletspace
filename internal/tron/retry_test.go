package tron

import (
	"context"
	"errors"
	"testing"
)

func TestRetry(t *testing.T) {
	t.Parallel()

	boom := errors.New("node unreachable")

	tests := []struct {
		name       string
		attempts   int
		failsFirst int // how many leading calls fail
		wantCalls  int
		wantErr    bool
		wantResult string
	}{
		{name: "first call succeeds", attempts: 3, failsFirst: 0, wantCalls: 1, wantResult: "ok"},
		{name: "succeeds on the last attempt", attempts: 3, failsFirst: 2, wantCalls: 3, wantResult: "ok"},
		{name: "gives up after the budget", attempts: 3, failsFirst: 5, wantCalls: 3, wantErr: true},
		// A zero budget still means one try, never zero.
		{name: "zero budget still tries once", attempts: 0, failsFirst: 0, wantCalls: 1, wantResult: "ok"},
		{name: "zero budget failing", attempts: 0, failsFirst: 1, wantCalls: 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			got, err := retry(t.Context(), tt.attempts, func() (string, error) {
				calls++
				if calls <= tt.failsFirst {
					return "", boom
				}

				return "ok", nil
			})

			if calls != tt.wantCalls {
				t.Errorf("call count = %d, want %d", calls, tt.wantCalls)
			}

			if tt.wantErr {
				if !errors.Is(err, boom) {
					t.Fatalf("error = %v, want the underlying failure", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("retry() error = %v", err)
			}

			if got != tt.wantResult {
				t.Errorf("retry() = %q, want %q", got, tt.wantResult)
			}
		})
	}
}

func TestRetryStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	calls := 0
	_, err := retry(ctx, 5, func() (string, error) {
		calls++
		return "", errors.New("should not run")
	})

	if calls != 0 {
		t.Errorf("call count = %d, want 0 for an already canceled context", calls)
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
