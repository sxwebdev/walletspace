package space

// The grant tests live inside the package because all three turn on m.now.
//
// Every alternative was tried and fails: shrinking the window to nothing races
// a wall clock whose monotonic reading .UTC() has stripped, and a tiny
// auto-lock makes the background sweep fire at the deadline, so the gap between
// sweeps — where two of these bugs lived — never opens. autolock_internal_test
// and throttle_test are here for the same reason. Everything that can be asked
// of this package from outside stays in manager_test, which is where the rest
// of the public surface is exercised as a caller sees it.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/vault"
)

// The cheapest parameters the vault accepts, as throttle_test uses: these tests
// are about deadlines, not about how long a derivation takes.
var grantKDF = vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}

func newGrantManager(t *testing.T, home string) *Manager {
	t.Helper()
	manager, err := NewManager(home, 15*time.Minute, grantKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)
	return manager
}

// advanceClock puts the manager on a clock the test can move, and hands back
// the handle that moves it.
//
// Every deadline the manager keeps — the idle timer, the spending window, the
// unlock cooldown — is measured against m.now, so moving that is how a test
// reaches a deadline instead of waiting for one or shrinking it to nothing.
// Shrinking it does not work: m.now is time.Now().UTC(), and .UTC() strips the
// monotonic reading, so two consecutive reads of a wall clock are frequently
// the same instant. The offset is guarded because the background sweep reads
// the clock from its own goroutine; the assignment itself happens immediately
// after construction, a minute before that goroutine's first tick.
func advanceClock(manager *Manager) func(time.Duration) {
	var mu sync.Mutex
	var offset time.Duration
	manager.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return time.Now().UTC().Add(offset)
	}
	return func(step time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		offset += step
	}
}

// Unlocking a space says who was there when it was opened. Spending from it is
// a separate question, asked of whoever is there now.
func TestSpendingNeedsItsOwnConfirmation(t *testing.T) {
	t.Parallel()

	manager := newGrantManager(t, t.TempDir())
	advance := advanceClock(manager)
	created, err := manager.Create(CreateRequest{Name: "spend", Password: "correct-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID

	if err := manager.RequireSendConfirmation(id); !errors.Is(err, ErrSendConfirmationRequired) {
		t.Fatalf("RequireSendConfirmation(unlocked space) = %v, want ErrSendConfirmationRequired", err)
	}
	if _, err := manager.ConfirmSend(t.Context(), id, "wrong-password"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("ConfirmSend(wrong) error = %v, want ErrInvalidPassword", err)
	}
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, ErrSendConfirmationRequired) {
		t.Fatal("a failed confirmation opened the window anyway")
	}

	expires, err := manager.ConfirmSend(t.Context(), id, "correct-password")
	if err != nil {
		t.Fatalf("ConfirmSend() error = %v", err)
	}
	if !expires.After(time.Now()) {
		t.Errorf("the window expires at %s, which is not in the future", expires)
	}
	if err := manager.RequireSendConfirmation(id); err != nil {
		t.Fatalf("RequireSendConfirmation() after confirming = %v, want nil", err)
	}

	// Locking takes the window with it, or a grant would sit there waiting for
	// whoever unlocks the space next.
	if err := manager.Lock(id); err != nil {
		t.Fatalf("Lock() error = %v", err)
	}
	if err := manager.Unlock(id, "correct-password"); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, ErrSendConfirmationRequired) {
		t.Errorf("RequireSendConfirmation() after re-unlocking = %v, want the window to be gone", err)
	}

	// And the window is a window: it closes on its own. The clock moves rather
	// than the window shrinking to a nanosecond, which is what this used to do:
	// m.now() reads a wall clock with the monotonic part stripped, so the two
	// reads either side of a nanosecond window landed on the same instant often
	// enough that the grant was still open on the next line about half the time.
	// Under -race everything was slow enough to hide it, so CI stayed green
	// while `go test ./...` went red.
	if _, err := manager.ConfirmSend(t.Context(), id, "correct-password"); err != nil {
		t.Fatalf("ConfirmSend() error = %v", err)
	}
	advance(defaultSendGrantTTL - time.Second)
	if err := manager.RequireSendConfirmation(id); err != nil {
		t.Fatalf("RequireSendConfirmation() a second inside the window = %v, want nil", err)
	}
	advance(2 * time.Second)
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, ErrSendConfirmationRequired) {
		t.Errorf("RequireSendConfirmation() after the window = %v, want it expired", err)
	}

	// A window of zero length is not a window without an end: it falls back to
	// the default, because "not configured" has to read as the careful answer.
	manager.SetSendConfirmation(true, 0)
	expires, err = manager.ConfirmSend(t.Context(), id, "correct-password")
	if err != nil {
		t.Fatalf("ConfirmSend() with an unset window error = %v", err)
	}
	if left := expires.Sub(manager.now()); left <= defaultSendGrantTTL-time.Minute || left > defaultSendGrantTTL {
		t.Errorf("an unset window lasts %s, want the %s default", left, defaultSendGrantTTL)
	}

	// Turned off, nothing is asked. That is a setting someone has to choose.
	manager.SetSendConfirmation(false, 5*time.Minute)
	if err := manager.RequireSendConfirmation(id); err != nil {
		t.Errorf("RequireSendConfirmation() with the step-up off = %v, want nil", err)
	}
}

// The gate has to see the session the signer will see. Reading the map as it
// stood, it did not: the background sweep runs once a minute, so for up to that
// long a session whose idle deadline had passed was still there to be waved
// through — and the signing path then refused the transfer with a locked-space
// error the UI has no prompt for.
func TestTheSpendingGateSeesTheIdleDeadline(t *testing.T) {
	t.Parallel()

	// newManager locks a space after fifteen idle minutes.
	manager := newGrantManager(t, t.TempDir())
	advance := advanceClock(manager)
	created, err := manager.Create(CreateRequest{Name: "idle", Password: "correct-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID
	// A window that outlives the session, so the only thing under test is
	// whether the deadline is noticed.
	manager.SetSendConfirmation(true, time.Hour)
	if _, err := manager.ConfirmSend(t.Context(), id, "correct-password"); err != nil {
		t.Fatalf("ConfirmSend() error = %v", err)
	}

	advance(30 * time.Minute)
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, ErrLocked) {
		t.Fatalf("RequireSendConfirmation() past the deadline = %v, want ErrLocked", err)
	}
	// Not merely reported as locked: the session is gone, so nothing is left
	// holding a decrypted payload for the next caller to spend from.
	manager.mu.RLock()
	_, alive := manager.sessions[id]
	manager.mu.RUnlock()
	if alive {
		t.Error("an expired session survived the gate that refused it")
	}

	// Opening a window has to see the deadline too, or a password entered after
	// it would furnish a grant on a session the next sweep is about to discard.
	if err := manager.Unlock(id, "correct-password"); err != nil {
		t.Fatalf("Unlock() error = %v", err)
	}
	advance(30 * time.Minute)
	if _, err := manager.ConfirmSend(t.Context(), id, "correct-password"); !errors.Is(err, ErrLocked) {
		t.Errorf("ConfirmSend() past the deadline = %v, want ErrLocked", err)
	}
}

// Typing the space password is presence, and the idle timer measures presence.
// Without counting it, someone who unlocked fourteen minutes into a fifteen
// minute window and then confirmed a transfer could have the session swept out
// from under that very transfer seconds later.
func TestConfirmingASpendCountsAsUsingTheSpace(t *testing.T) {
	t.Parallel()

	// newManager locks a space after fifteen idle minutes.
	manager := newGrantManager(t, t.TempDir())
	advance := advanceClock(manager)
	created, err := manager.Create(CreateRequest{Name: "idle", Password: "correct-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID

	advance(14 * time.Minute)
	if _, err := manager.ConfirmSend(t.Context(), id, "correct-password"); err != nil {
		t.Fatalf("ConfirmSend() error = %v", err)
	}
	// Eighteen minutes after it was unlocked, but four after the password: the
	// session has eleven minutes left and the window one.
	advance(4 * time.Minute)
	if err := manager.RequireSendConfirmation(id); err != nil {
		t.Fatalf("RequireSendConfirmation() four minutes after the password = %v, want nil", err)
	}

	// A refresh, not an exemption — and the check above must not have granted
	// one of its own. It needs nothing but the capability token, so a caller
	// polling it could otherwise hold a space open indefinitely: had it counted
	// as use, the session would still be alive here.
	advance(12 * time.Minute)
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, ErrLocked) {
		t.Errorf("RequireSendConfirmation() sixteen minutes after the password = %v, want ErrLocked", err)
	}
}

// A dialog can be dismissed while this call is still inside a derivation that
// takes the better part of a second. The browser aborts the request and tells
// the person nothing was confirmed, so nothing may be confirmed: the window is
// what the call produces, and refusing to produce it is the last thing that
// happens before it would be written.
func TestAnAbandonedConfirmationOpensNoWindow(t *testing.T) {
	t.Parallel()

	manager := newGrantManager(t, t.TempDir())
	created, err := manager.Create(CreateRequest{Name: "spend", Password: "correct-password"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID

	// A guess that was made, and left its mark on the cooldown.
	if _, err := manager.ConfirmSend(t.Context(), id, "wrong-password"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("ConfirmSend(wrong) error = %v, want ErrInvalidPassword", err)
	}
	if got := manager.readAttempts(id).Failures; got != 1 {
		t.Fatalf("failures after one wrong password = %d, want 1", got)
	}

	// Abandoned with the right password: no window — and the mark is gone,
	// which is what shows the context is consulted after the derivation rather
	// than instead of it. A caller that hung up before the call and one that
	// hung up halfway through the derivation take this same path.
	abandoned, dismiss := context.WithCancel(t.Context())
	dismiss()
	if _, err := manager.ConfirmSend(abandoned, id, "correct-password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ConfirmSend(abandoned) error = %v, want context.Canceled", err)
	}
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, ErrSendConfirmationRequired) {
		t.Errorf("RequireSendConfirmation() after an abandoned confirmation = %v, want no window", err)
	}
	if got := manager.readAttempts(id).Failures; got != 0 {
		t.Errorf("failures after an abandoned but correct password = %d, want the record cleared", got)
	}

	// Hanging up does not take a guess back either. A wrong password the caller
	// abandons still costs a failure, or cancelling every request would be a way
	// to guess for ever without meeting the cooldown.
	second, dismissSecond := context.WithCancel(t.Context())
	dismissSecond()
	if _, err := manager.ConfirmSend(second, id, "wrong-password"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("ConfirmSend(abandoned, wrong) error = %v, want the verdict on the password", err)
	}
	if got := manager.readAttempts(id).Failures; got != 1 {
		t.Errorf("failures after an abandoned wrong password = %d, want the guess to count", got)
	}
	if err := manager.RequireSendConfirmation(id); !errors.Is(err, ErrSendConfirmationRequired) {
		t.Errorf("RequireSendConfirmation() after an abandoned guess = %v, want no window", err)
	}

	// And the same question of a real interleaving, where the dismissal lands
	// wherever it lands. Either answer is allowed; the two have to agree. An
	// error with a live window behind it is the outcome the browser cannot
	// survive, because it has already said nothing was confirmed.
	for round := range 4 {
		inFlight, dismissInFlight := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			_, confirmErr := manager.ConfirmSend(inFlight, id, "correct-password")
			result <- confirmErr
		}()
		dismissInFlight()
		confirmErr := <-result
		gate := manager.RequireSendConfirmation(id)
		switch {
		case confirmErr != nil && gate == nil:
			t.Fatalf("round %d: a confirmation that reported %v left a window open", round, confirmErr)
		case confirmErr == nil && gate != nil:
			t.Fatalf("round %d: a confirmation that reported success left no window: %v", round, gate)
		}
		if err := manager.Lock(id); err != nil {
			t.Fatalf("round %d: Lock() error = %v", round, err)
		}
		if err := manager.Unlock(id, "correct-password"); err != nil {
			t.Fatalf("round %d: Unlock() error = %v", round, err)
		}
	}
}
