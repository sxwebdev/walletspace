//go:build !darwin

package devicesync

import "io/fs"

// downloadCommand has no command to offer: only iCloud Drive on macOS is
// asked for files, and only there are files kept without their data.
func downloadCommand(string) (string, []string, bool) { return "", nil, false }

func isDataless(fs.FileInfo) bool { return false }
