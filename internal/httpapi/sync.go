package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/space"
)

// ApplySync points the space manager at the sync folder the settings name, or
// turns sync off. It is what the wallet runs at start and after the setting
// changes.
func ApplySync(spaces *space.Manager, home string, settings config.SyncSettings, log *slog.Logger) error {
	if !settings.Enabled {
		spaces.SetSync(nil, "", log)
		return nil
	}
	if err := checkFolderInTime(settings.Folder); err != nil {
		return err
	}
	store, err := devicesync.NewFolderStore(settings.Folder)
	if err != nil {
		return err
	}
	deviceID, err := devicesync.DeviceID(home)
	if err != nil {
		return err
	}
	spaces.SetSync(store, deviceID, log)
	return nil
}

// folderCheckTimeout bounds the look at the sync folder before sync starts.
// A mount that hangs — a network volume that went away, a provider that is
// stuck — must not keep the wallet from starting, or a settings save from
// answering: it is reported as sync not running, like a folder that is not
// there.
var folderCheckTimeout = 5 * time.Second

// checkFolder is prepareFolder; a test puts one that hangs in its place.
var checkFolder = prepareFolder

// folderChecks holds the check in flight for each folder, process-wide, by
// cleaned path. A check given up on is left to finish, or not, on its own — a
// call into a hung mount cannot be interrupted — and it holds a thread while
// it is stuck. Starting another one beside it on every retry would pile up a
// stuck thread per attempt until the process hits its thread limit and dies.
var folderChecks struct {
	sync.Mutex
	byFolder map[string]*folderCheck
}

type folderCheck struct {
	done chan struct{}
	err  error // set before done is closed
}

// checkFolderInTime runs checkFolder for at most folderCheckTimeout. While an
// earlier check of the same folder has not returned, none is started: this
// one waits on that one, with the same bound, so that a folder that stays
// hung costs one stuck thread however often it is asked about.
func checkFolderInTime(folder string) error {
	return checkFolderWith(folder, checkFolder, folderCheckTimeout)
}

// checkFolderWith is checkFolderInTime with the check and its bound given, so
// that a retry that outlives its caller reads nothing that may change.
func checkFolderWith(folder string, check func(string) error, timeout time.Duration) error {
	key := filepath.Clean(folder)
	folderChecks.Lock()
	running, earlier := folderChecks.byFolder[key]
	if !earlier {
		if folderChecks.byFolder == nil {
			folderChecks.byFolder = make(map[string]*folderCheck)
		}
		running = &folderCheck{done: make(chan struct{})}
		folderChecks.byFolder[key] = running
		go func() {
			err := check(folder)
			folderChecks.Lock()
			running.err = err
			delete(folderChecks.byFolder, key)
			folderChecks.Unlock()
			close(running.done)
		}()
	}
	folderChecks.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-running.done:
		return running.err
	case <-timer.C:
		if earlier {
			return fmt.Errorf("the sync folder %s is still not answering: is the drive or cloud service available?", folder)
		}
		return fmt.Errorf("the sync folder %s did not answer within %s: is the drive or cloud service available?",
			folder, timeout)
	}
}

// prepareFolder makes the sync folder if it is missing, its parent never. A
// parent that is gone is a provider that went away — iCloud Drive turned off,
// a volume not mounted — and recreating the path would leave every later
// write in a plain local folder that nothing copies, while sync reports
// success.
func prepareFolder(folder string) error {
	parent := filepath.Dir(folder)
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return fmt.Errorf("the folder %s that holds the sync folder is not there: is the drive or cloud service available?", parent)
	}
	if err := os.Mkdir(folder, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create the sync folder: %w", err)
	}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		return fmt.Errorf("the sync folder %s is not a directory", folder)
	}
	return nil
}

// StartSync is ApplySync at start. A folder that has gone away is reported,
// not fatal: the wallet works without sync, the setting can be fixed in the
// UI, and the sync status says why sync is not running instead of showing it
// as on over nothing.
//
// Sync stays on over the folder while it is away: changes are sealed into
// their spaces' outboxes as they are made, the folder is looked at again
// every folderRetryInterval, and once it answers sync runs with it — what
// was prepared meanwhile is sent then, with no restart and no unlock.
func StartSync(spaces *space.Manager, home string, settings config.SyncSettings, log *slog.Logger) {
	err := ApplySync(spaces, home, settings, log)
	if err == nil {
		return
	}
	log.Warn("sync is not running", "error", err)
	reason := "Sync is turned on but not running: " + err.Error()
	folder := filepath.Clean(settings.Folder)
	deviceID, idErr := devicesync.DeviceID(home)
	if idErr != nil || !filepath.IsAbs(folder) {
		// Nothing to seal outboxes for, or with.
		spaces.SetSyncUnavailable(reason)
		return
	}
	check, timeout := checkFolder, folderCheckTimeout
	reopen := func() (devicesync.Store, error) {
		if err := checkFolderWith(folder, check, timeout); err != nil {
			return nil, err
		}
		return devicesync.NewFolderStore(folder)
	}
	spaces.SetSyncPending(folder, deviceID, reason, reopen, folderRetryInterval, log)
}

// folderRetryInterval is how often a sync folder that could not be opened at
// start is looked at again: the sync tick.
var folderRetryInterval = 30 * time.Second

type syncDTO struct {
	space.SyncOverview
	Folder string `json:"folder"`
	// SuggestedFolder is where iCloud Drive would keep it on this Mac, or ""
	// when iCloud Drive is not there.
	SuggestedFolder string `json:"suggested_folder"`
}

func (p *Platform) syncOverview(w http.ResponseWriter, r *http.Request) {
	settings := p.settings.Snapshot().Config.Sync
	writeJSON(w, http.StatusOK, syncDTO{
		SyncOverview: p.spaces.SyncOverview(r.Context()),
		Folder:       settings.Folder, SuggestedFolder: devicesync.SuggestedFolder(),
	})
}

// patchSync turns sync on or off and moves its folder.
//
// It takes no password. What it changes is where this machine writes files
// that open only with each space's phrase; pointing that somewhere a caller
// with the capability token can read gives the caller ciphertext.
func (p *Platform) patchSync(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled bool   `json:"enabled"`
		Folder  string `json:"folder"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	folder := strings.TrimSpace(request.Folder)
	if folder != "" {
		folder = filepath.Clean(folder)
	}
	next := p.settings.Snapshot().Config
	next.Sync = config.SyncSettings{Enabled: request.Enabled, Folder: folder}
	if err := p.settings.ValidateSync(next.Sync); err != nil {
		p.writePlatformError(w, err)
		return
	}
	// A stale revision is refused before anything is applied: applying first
	// would create the folder and restart sync for a request then refused.
	// SaveConfig checks again under its lock.
	if expected := revisionHeader(r); expected != "" && expected != p.settings.Snapshot().Revision {
		p.writePlatformError(w, config.ErrRevisionConflict)
		return
	}
	// Applied before it is saved, so a folder that cannot be created is
	// refused rather than stored.
	if err := ApplySync(p.spaces, p.settings.Home(), next.Sync, p.log); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := p.settings.SaveConfig(next, revisionHeader(r))
	if err != nil {
		// Back to what is stored.
		if err := ApplySync(p.spaces, p.settings.Home(), p.settings.Snapshot().Config.Sync, p.log); err != nil {
			p.log.Warn("sync is not running", "error", err)
		}
		p.writePlatformError(w, err)
		return
	}
	w.Header().Set("ETag", `"`+saved.Revision+`"`)
	writeJSON(w, http.StatusOK, settingsResponse(saved))
}

// syncSpace runs a pass over one space now; a locked one only sends.
func (p *Platform) syncSpace(w http.ResponseWriter, r *http.Request) {
	status, err := p.spaces.SyncNow(r.Context(), r.PathValue("space_id"))
	// A pass or a send that failed is reported in the status, like the
	// loop's — a locked space's failed send too; only a request that could
	// not run at all is an error.
	if err != nil && status.State != space.SyncError && status.SendError == "" {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
