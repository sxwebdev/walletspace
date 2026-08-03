package space

import (
	"errors"
	"fmt"
)

// ErrQuotaExceeded reports that a limit on stored objects has been reached.
var ErrQuotaExceeded = errors.New("limit reached")

// The ceilings are set well above what this wallet is for and well below what
// makes the process unhealthy.
//
// Every space costs a decrypted payload in memory once unlocked, and every
// account in one is derived, stored and polled for balances on every network it
// is bound to. Neither had any bound at all, so a caller that reached the API
// could grow both until the machine gave out — and unlike a request flood, what
// it created was written to disk and reloaded on the next start.
const (
	maxSpaces           = 64
	maxAccountsPerSpace = 512
)

func (m *Manager) checkSpaceQuota() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.spaceQuotaLocked()
}

// spaceQuotaLocked must be called with m.mu held, in either mode.
func (m *Manager) spaceQuotaLocked() error {
	if len(m.files) >= maxSpaces {
		return fmt.Errorf("%w: at most %d spaces", ErrQuotaExceeded, maxSpaces)
	}
	return nil
}

// accountQuotaLocked must be called with m.mu held.
func accountQuotaLocked(file File) error {
	if len(file.Accounts) >= maxAccountsPerSpace {
		return fmt.Errorf(
			"%w: at most %d accounts in one space", ErrQuotaExceeded, maxAccountsPerSpace,
		)
	}
	return nil
}
