package config

import "time"

// SetSyncFolderCheck replaces how the sync folder check resolves a path and
// how long it waits for it, until restore is called.
func SetSyncFolderCheck(eval func(string) (string, error), timeout time.Duration) (restore func()) {
	previousEval, previousTimeout := evalSymlinks, syncFolderCheckTimeout
	evalSymlinks, syncFolderCheckTimeout = eval, timeout
	return func() { evalSymlinks, syncFolderCheckTimeout = previousEval, previousTimeout }
}

// SyncFolderChecksInFlight is how many resolutions are still running,
// including those whose caller gave up on them.
func SyncFolderChecksInFlight() int { return len(syncFolderChecks) }
