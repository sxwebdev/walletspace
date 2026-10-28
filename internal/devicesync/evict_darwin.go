package devicesync

import (
	"io/fs"
	"syscall"
)

// brctl is iCloud Drive's command-line tool. It is named by its full path so
// that nothing earlier in PATH is run in its place.
const brctl = "/usr/bin/brctl"

func downloadCommand(path string) (string, []string, bool) {
	return brctl, []string{"download", path}, true
}

// sfDataless is SF_DATALESS from <sys/stat.h>: the file's name and metadata
// are here, its data is not, and reading it makes the provider fetch it.
const sfDataless = 0x40000000

// isDataless reads the flag from what lstat returned. Lstat itself does not
// fetch anything.
func isDataless(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Flags&sfDataless != 0
}
