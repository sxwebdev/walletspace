package devicesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"time"
)

// Downloader is a Store that can ask its provider to bring down a file it
// has evicted, or keeps dataless. It is optional: a store that is not one has
// nothing to ask, and its evicted files stay missing until the person brings
// them down.
type Downloader interface {
	// Download asks for one device's file. A nil error means the request was
	// taken, not that the file is here: a later List says when it is.
	Download(ctx context.Context, spaceTag, deviceTag string) error
}

// ErrDownloadUnsupported reports a folder whose provider cannot be asked to
// download a file from here: anywhere but macOS, or a Mac without brctl.
var ErrDownloadUnsupported = errors.New("the sync folder cannot be asked to download files")

// downloadTimeout bounds one download request. brctl may wait for the
// download to finish, and a provider that is offline never finishes it.
const downloadTimeout = 10 * time.Second

var _ Downloader = (*FolderStore)(nil)

// Download asks the provider for one device's file. On macOS that is iCloud
// Drive's own tool, brctl, given the file's real path — not its placeholder's.
// Asking about a file that is already here is harmless. A folder that is not
// in iCloud Drive refuses, and the error says so; the caller cannot tell the
// two apart from here, so a request that keeps failing means "keep the folder
// downloaded".
func (f *FolderStore) Download(ctx context.Context, spaceTag, deviceTag string) error {
	path, err := f.path(spaceTag, deviceTag)
	if err != nil {
		return err
	}
	name, args, ok := downloadCommand(path)
	if !ok {
		return ErrDownloadUnsupported
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, f.downloadTimeout)
	defer cancel()
	out, err := f.run(runCtx, name, args...)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		// Whoever asked went away; that is theirs to report, not the folder's.
		return ctx.Err()
	case runCtx.Err() != nil:
		return fmt.Errorf("the request to download a sync file did not finish in %s", f.downloadTimeout)
	case errors.Is(err, fs.ErrNotExist):
		// brctl itself is missing.
		return fmt.Errorf("%w: %w", ErrDownloadUnsupported, err)
	}
	return fmt.Errorf("request a download of a sync file: %w%s", err, outputDetail(out))
}

// outputDetail is what a failed command printed, short enough for a status.
func outputDetail(out []byte) string {
	const most = 200
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return ""
	}
	if len(out) > most {
		out = append(out[:most:most], "..."...)
	}
	return ": " + string(out)
}

// runCommand runs a program and returns what it printed. Ending ctx kills
// it; WaitDelay keeps a child that inherited its output from holding the
// call open after that.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	return cmd.CombinedOutput()
}
