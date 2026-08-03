package space

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/sxwebdev/walletspace/internal/storage"
)

// ErrTooManyAttempts reports that a space is in its unlock cooldown.
//
// It deliberately says nothing about the password. An attacker who could tell
// "wrong password" from "throttled" would know which guesses were worth
// repeating once the wait was over.
var ErrTooManyAttempts = errors.New("too many unlock attempts; wait before trying again")

const (
	// freeUnlockAttempts is what a person mistyping a password gets through
	// without noticing anything.
	freeUnlockAttempts = 3
	// The wait doubles per failure past the free ones: 2s, 4s, 8s … up to the
	// ceiling. Ten wrong guesses cost about half an hour, which is nothing to a
	// user who eventually remembers the password and everything to a script.
	baseUnlockDelay = 2 * time.Second
	maxUnlockDelay  = 15 * time.Minute
	// maxUnlockDoublings caps the shift so the delay cannot overflow.
	maxUnlockDoublings = 20
)

// attemptRecord is the on-disk cooldown state for one space.
//
// It is a file rather than a counter in memory because restarting the process
// is otherwise a free reset, and starting the wallet again is not something an
// attacker who can reach the API finds difficult.
type attemptRecord struct {
	Failures int       `json:"failures"`
	NextTry  time.Time `json:"next_try"`
}

// kdfSlots bounds how many Argon2 derivations run at once.
//
// Each one is 64 MiB and several passes by design. Without a bound, concurrent
// unlocks multiply that: ten in flight is well over half a gigabyte of live
// allocation and every core busy, which is enough to make the rest of the
// wallet — including the auto-lock sweep — stop responding.
func newKDFSlots() chan struct{} {
	slots := min(max(runtime.NumCPU()/2, 1), 4)
	return make(chan struct{}, slots)
}

// acquireKDF blocks until a derivation slot is free.
func (m *Manager) acquireKDF() func() {
	m.kdfSlots <- struct{}{}
	return func() { <-m.kdfSlots }
}

// lockSpace serialises the expensive, password-checking operations on one
// space against each other.
//
// The manager's own mutex cannot do this job: it guards the maps and has to be
// released across a derivation, or one unlock would freeze every other space,
// the space list and the auto-lock sweep for the length of an Argon2 run. This
// is what keeps a password change from swapping the container out from under an
// unlock that is midway through deriving a key for the old one.
// The map is keyed by a caller-supplied path segment, so only a space that
// actually exists gets an entry — which bounds the map by the space quota.
// Checking the format alone would not: an id of the right shape is trivial to
// invent, and a loop of requests naming fresh ones would add a mutex per
// distinct string for ever. Callers reject the unknown id a moment later on
// their own; a shared lock is enough to get them there.
func (m *Manager) lockSpace(id string) func() {
	m.mu.RLock()
	_, known := m.files[id]
	m.mu.RUnlock()
	if !known {
		m.unknownSpaceLock.Lock()
		return m.unknownSpaceLock.Unlock
	}
	value, _ := m.spaceLocks.LoadOrStore(id, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// checkUnlockCooldown reports whether this space is currently allowed another
// attempt. Callers must hold the space lock.
func (m *Manager) checkUnlockCooldown(id string) error {
	record := m.readAttempts(id)
	if record.Failures <= freeUnlockAttempts || !m.now().Before(record.NextTry) {
		return nil
	}
	return fmt.Errorf(
		"%w: %s", ErrTooManyAttempts,
		record.NextTry.Sub(m.now()).Round(time.Second),
	)
}

// recordUnlockFailure extends the cooldown. Callers must hold the space lock.
func (m *Manager) recordUnlockFailure(id string) {
	record := m.readAttempts(id)
	record.Failures++
	record.NextTry = m.now().Add(unlockDelay(record.Failures))
	m.writeAttempts(id, record)
}

// clearUnlockFailures forgets the cooldown after a correct password.
func (m *Manager) clearUnlockFailures(id string) {
	if !storage.ValidID(id, "spc_") {
		return
	}
	_ = os.Remove(m.attemptsPath(id))
}

// backoffFor is the schedule without the jitter: it doubles per failure past
// the free ones and stops at the ceiling.
func backoffFor(failures int) time.Duration {
	doublings := max(failures-freeUnlockAttempts-1, 0)
	if doublings > maxUnlockDoublings {
		doublings = maxUnlockDoublings
	}
	delay := baseUnlockDelay << doublings
	if delay > maxUnlockDelay || delay <= 0 {
		delay = maxUnlockDelay
	}
	return delay
}

// unlockDelay is the schedule with jitter added.
//
// The jitter is not decoration. A fixed schedule lets an attacker sleep exactly
// the right amount and keep a steady rate; spreading each wait over a quarter
// of its length means the next window cannot be predicted, and a client that
// guesses too early only pushes the wait further out.
func unlockDelay(failures int) time.Duration {
	delay := backoffFor(failures)
	spread, err := rand.Int(rand.Reader, big.NewInt(int64(delay/4)+1))
	if err != nil {
		return delay
	}
	return delay + time.Duration(spread.Int64())
}

func (m *Manager) attemptsPath(id string) string {
	return filepath.Join(m.home, "spaces", id, "unlock.json")
}

func (m *Manager) readAttempts(id string) attemptRecord {
	if !storage.ValidID(id, "spc_") {
		return attemptRecord{}
	}
	data, err := os.ReadFile(m.attemptsPath(id))
	if err != nil {
		return attemptRecord{}
	}
	var record attemptRecord
	if json.Unmarshal(data, &record) != nil {
		return attemptRecord{}
	}
	// A clock that moved backwards, or a hand-edited file, must not be able to
	// park a space behind an unreachable date.
	if record.NextTry.After(m.now().Add(maxUnlockDelay * 2)) {
		record.NextTry = m.now().Add(maxUnlockDelay)
	}
	return record
}

func (m *Manager) writeAttempts(id string, record attemptRecord) {
	if !storage.ValidID(id, "spc_") {
		return
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	// Best effort. Losing the counter costs throttling on the next restart,
	// which is strictly better than refusing to run.
	_ = storage.AtomicWrite(m.attemptsPath(id), append(data, '\n'))
}
