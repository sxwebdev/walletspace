package space

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/vault"
)

var throttleKDF = vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1}

// An unlock costs one Argon2 pass by design; without a cooldown that is the
// only thing standing between a local caller with the capability token and an
// unlimited online guessing rate.
func TestFailedUnlocksEarnAGrowingCooldown(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager, err := NewManager(home, time.Hour, throttleKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)
	created, err := manager.Create(CreateRequest{Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID

	// The free attempts answer the same way a single mistype does — nothing
	// about the throttle is visible until it engages.
	for attempt := range freeUnlockAttempts {
		err := manager.Unlock(id, "wrong-password-here")
		if !errors.Is(err, vault.ErrInvalidPassword) {
			t.Fatalf("attempt %d error = %v, want ErrInvalidPassword", attempt, err)
		}
	}
	if err := manager.Unlock(id, "wrong-password-here"); !errors.Is(err, vault.ErrInvalidPassword) {
		t.Fatalf("first throttled attempt error = %v, want ErrInvalidPassword", err)
	}
	if err := manager.Unlock(id, "wrong-password-here"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("error = %v, want ErrTooManyAttempts", err)
	}
	// The correct password is refused too. Telling the two apart would hand an
	// attacker a way to confirm a guess without waiting.
	if err := manager.Unlock(id, "correct-horse-battery"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("error = %v, want the cooldown to apply to any password", err)
	}

	// Restarting the process is not a reset: an attacker who can reach the API
	// can usually also start the wallet again.
	if _, err := os.Stat(filepath.Join(home, "spaces", id, "unlock.json")); err != nil {
		t.Fatalf("cooldown was not persisted: %v", err)
	}
	manager.Close()
	restarted, err := NewManager(home, time.Hour, throttleKDF)
	if err != nil {
		t.Fatalf("NewManager(restart) error = %v", err)
	}
	t.Cleanup(restarted.Close)
	if err := restarted.Unlock(id, "wrong-password-here"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("after restart error = %v, want the cooldown to survive", err)
	}

	// Once the wait is over, the right password clears the record entirely.
	restarted.now = func() time.Time { return time.Now().UTC().Add(2 * maxUnlockDelay) }
	if err := restarted.Unlock(id, "correct-horse-battery"); err != nil {
		t.Fatalf("Unlock() after the cooldown error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "spaces", id, "unlock.json")); !os.IsNotExist(err) {
		t.Errorf("a correct password left the cooldown record in place: %v", err)
	}
}

// The delay has to grow and has to be unpredictable: a fixed schedule lets an
// attacker sleep exactly long enough and keep a steady rate.
func TestUnlockDelayGrowsWithJitterAndStopsAtTheCeiling(t *testing.T) {
	t.Parallel()

	// Monotonicity is a property of the schedule, not of a single draw: past
	// the ceiling the jitter makes consecutive draws go up and down freely.
	previous := time.Duration(0)
	for failures := range freeUnlockAttempts + 12 {
		base := backoffFor(failures)
		if base < previous {
			t.Errorf("backoffFor(%d) = %s, shorter than the previous %s", failures, base, previous)
		}
		if base > maxUnlockDelay {
			t.Errorf("backoffFor(%d) = %s, past the ceiling", failures, base)
		}
		if delay := unlockDelay(failures); delay < base || delay > base+base/4 {
			t.Errorf("unlockDelay(%d) = %s, want within a quarter above %s", failures, delay, base)
		}
		previous = base
	}
	// A huge counter must not shift the delay into an overflow.
	if got := backoffFor(1 << 30); got != maxUnlockDelay {
		t.Errorf("backoffFor(huge) = %s, want the ceiling", got)
	}

	seen := make(map[time.Duration]struct{})
	for range 40 {
		seen[unlockDelay(freeUnlockAttempts+6)] = struct{}{}
	}
	if len(seen) < 2 {
		t.Error("unlockDelay() is not jittered; the next window is predictable")
	}
}

// The manager's mutex used to be held across the whole derivation, so one
// unlock froze every other space — the space list, the auto-lock sweep and any
// other unlock — for the length of an Argon2 pass.
func TestASlowUnlockDoesNotBlockOtherSpaces(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(t.TempDir(), time.Hour, throttleKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)
	first, err := manager.Create(CreateRequest{Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	second, err := manager.Create(CreateRequest{Password: "another-good-secret"})
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}

	var wait sync.WaitGroup
	// Enough concurrent derivations to saturate the semaphore several times
	// over. None of them may stop the reads below from completing.
	for range 8 {
		wait.Go(func() {
			_ = manager.Unlock(first.Space.ID, "correct-horse-battery")
		})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			manager.List()
			if _, _, err := manager.Get(second.Space.ID); err != nil {
				t.Errorf("Get() during a concurrent unlock error = %v", err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("reads were blocked behind a derivation")
	}
	wait.Wait()
}

// Every space costs a decrypted payload once unlocked, and every account is
// derived, stored and polled on each network it is bound to. Neither had a
// ceiling, so a caller that reached the API could grow both until the machine
// gave out — and what it created was written to disk and reloaded on restart.
func TestSpaceAndAccountQuotas(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(t.TempDir(), time.Hour, throttleKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)

	// Filled directly rather than through Create: the point is the ceiling, and
	// paying for maxSpaces real derivations to reach it proves nothing extra.
	manager.mu.Lock()
	for range maxSpaces {
		id, err := newID("spc_")
		if err != nil {
			manager.mu.Unlock()
			t.Fatalf("newID() error = %v", err)
		}
		manager.files[id] = File{SchemaVersion: SchemaVersion, ID: id, Name: "filler"}
	}
	manager.mu.Unlock()

	_, err = manager.Create(CreateRequest{Password: "correct-horse-battery"})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Create() past the ceiling error = %v, want ErrQuotaExceeded", err)
	}

	full := File{Accounts: make([]account.Account, maxAccountsPerSpace)}
	if err := accountQuotaLocked(full); !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("accountQuotaLocked(full) error = %v, want ErrQuotaExceeded", err)
	}
	room := File{Accounts: make([]account.Account, maxAccountsPerSpace-1)}
	if err := accountQuotaLocked(room); err != nil {
		t.Errorf("accountQuotaLocked(room) error = %v", err)
	}
}

// Revealing a seed or a private key re-derives the vault key, so it is a
// password check reachable from the API — and it used to run outside the
// throttle entirely, leaving an unlimited guessing oracle next to a throttled
// one. It also held the manager's write mutex across the derivation.
func TestSecretExportGoesThroughTheSameThrottleAsUnlock(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager, err := NewManager(home, time.Hour, throttleKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)
	created, err := manager.Create(CreateRequest{Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	id := created.Space.ID

	for attempt := range freeUnlockAttempts + 1 {
		if _, err := manager.Mnemonic(id, "wrong-password-here"); !errors.Is(err, vault.ErrInvalidPassword) {
			t.Fatalf("attempt %d error = %v, want ErrInvalidPassword", attempt, err)
		}
	}
	if _, err := manager.Mnemonic(id, "wrong-password-here"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("Mnemonic() error = %v, want ErrTooManyAttempts", err)
	}
	// The two paths share one counter, so guessing through the export cannot be
	// used to sidestep the cooldown on unlocking, or the other way round.
	if err := manager.Unlock(id, "correct-horse-battery"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("Unlock() error = %v, want the export failures to count", err)
	}
	if _, err := os.Stat(filepath.Join(home, "spaces", id, "unlock.json")); err != nil {
		t.Fatalf("export failures were not persisted: %v", err)
	}

	manager.now = func() time.Time { return time.Now().UTC().Add(2 * maxUnlockDelay) }
	if _, err := manager.Mnemonic(id, "correct-horse-battery"); err != nil {
		t.Fatalf("Mnemonic() after the cooldown error = %v", err)
	}
}

// The step-up derivation must not be held under the manager's write mutex; that
// is the lock hold Unlock and ChangePassword were restructured to remove.
func TestSecretExportDoesNotBlockTheWholeManager(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(t.TempDir(), time.Hour, throttleKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)
	created, err := manager.Create(CreateRequest{Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			_, _ = manager.Mnemonic(created.Space.ID, "correct-horse-battery")
		})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			manager.List()
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("reads were blocked behind a step-up derivation")
	}
	wait.Wait()
}

// The per-space mutex map is keyed by a path segment the caller chooses, so
// only a space that exists may create an entry — otherwise a loop of requests
// naming invented ids would add a mutex per distinct string for ever.
func TestUnknownSpaceIdsDoNotAccumulateLocks(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(t.TempDir(), time.Hour, throttleKDF)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(manager.Close)

	for i := range 500 {
		// Well-formed but non-existent: checking the format alone would not have
		// bounded anything, since an id of the right shape is free to invent.
		id := fmt.Sprintf("spc_%026d", i)
		if err := manager.Unlock(id, "whatever-password"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Unlock(%s) error = %v, want ErrNotFound", id, err)
		}
	}
	stored := 0
	manager.spaceLocks.Range(func(_, _ any) bool { stored++; return true })
	if stored != 0 {
		t.Errorf("spaceLocks holds %d entries for spaces that do not exist", stored)
	}
}
