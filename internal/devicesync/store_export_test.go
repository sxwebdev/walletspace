package devicesync

import (
	"context"
	"io/fs"
	"time"
)

// SetWriteFile replaces how one store writes a file, so a test can make a
// write that does not return.
func SetWriteFile(f *FolderStore, write func(path string, blob []byte) error) { f.writeFile = write }

// SetReadFile replaces how one store reads a file, so a test can make a read
// that is slow.
func SetReadFile(f *FolderStore, read func(path string) ([]byte, error)) { f.readFile = read }

// SetDataless replaces how one store tells a file kept without its data.
func SetDataless(f *FolderStore, check func(fs.FileInfo) bool) { f.dataless = check }

// SetRunner replaces the command one store runs to request a download, and
// how long it waits for it.
func SetRunner(f *FolderStore, run func(ctx context.Context, name string, args ...string) ([]byte, error), timeout time.Duration) {
	f.run = run
	f.downloadTimeout = timeout
}

// SetSlotWait replaces how long one store waits for a busy slot.
func SetSlotWait(f *FolderStore, wait time.Duration) { f.slotWait = wait }

var (
	IsDataless = isDataless
	RunCommand = runCommand
)

const DownloadTimeout = downloadTimeout
