package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/config"
)

func TestSyncSettingsAreSavedAndChecked(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.Snapshot().Config.Sync; got.Enabled || got.Folder != "" {
		t.Fatalf("sync defaults = %+v, want off", got)
	}
	folder := t.TempDir()
	next := manager.Snapshot().Config
	next.Sync = config.SyncSettings{Enabled: true, Folder: folder}
	if _, err := manager.SaveConfig(next, ""); err != nil {
		t.Fatalf("SaveConfig(sync) error = %v", err)
	}
	reloaded, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Snapshot().Config.Sync; !got.Enabled || got.Folder != folder {
		t.Fatalf("reloaded sync = %+v", got)
	}

	tests := []struct {
		name     string
		settings config.SyncSettings
	}{
		{name: "on without a folder", settings: config.SyncSettings{Enabled: true}},
		{name: "a relative folder", settings: config.SyncSettings{Enabled: true, Folder: "Walletspace"}},
		{name: "an unclean folder", settings: config.SyncSettings{Enabled: true, Folder: folder + "/../x"}},
		// The wallet's own data must never be the thing that is synced.
		{name: "the data folder itself", settings: config.SyncSettings{Enabled: true, Folder: home}},
		{name: "inside the data folder", settings: config.SyncSettings{Enabled: true, Folder: filepath.Join(home, "spaces")}},
		{name: "around the data folder", settings: config.SyncSettings{Enabled: true, Folder: filepath.Dir(home)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := manager.Snapshot().Config
			next.Sync = tt.settings
			if _, err := manager.SaveConfig(next, ""); !errors.Is(err, config.ErrInvalidSettings) {
				t.Fatalf("SaveConfig(%+v) error = %v, want ErrInvalidSettings", tt.settings, err)
			}
		})
	}
	// Off with a folder remembered is fine: the form keeps the path.
	next = manager.Snapshot().Config
	next.Sync = config.SyncSettings{Folder: folder}
	if _, err := manager.SaveConfig(next, ""); err != nil {
		t.Fatalf("SaveConfig(off, folder kept) error = %v", err)
	}
}

// A folder on disk that no longer passes the rules turns sync off rather
// than keeping the wallet from starting.
func TestAStoredBadSyncFolderTurnsSyncOff(t *testing.T) {
	t.Parallel()

	for name, folder := range map[string]string{
		"relative":        "relative/folder",
		"the data folder": "",
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			if folder == "" {
				folder = home
			}
			data := "schema_version: 1\nserver:\n  addr: 127.0.0.1:0\n  open_browser: true\n" +
				"security:\n  auto_lock: 15m\nnode_discovery:\n  refresh_interval: 30m\n  request_timeout: 5s\n" +
				"sync:\n  enabled: true\n  folder: " + folder + "\n"
			if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			manager, err := config.NewHomeManager(home)
			if err != nil {
				t.Fatalf("NewHomeManager() error = %v", err)
			}
			if got := manager.Snapshot().Config.Sync; got.Enabled {
				t.Fatalf("sync = %+v, want off", got)
			}
		})
	}
}

// hang is how long a test waits before it calls a check hung. It only
// catches what never returns.
const hang = 30 * time.Second

func newSyncHome(t *testing.T, home string) *config.HomeManager {
	t.Helper()
	manager, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	return manager
}

func symlink(t *testing.T, target, link string) string {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return link
}

func saveSync(manager *config.HomeManager, folder string) error {
	next := manager.Snapshot().Config
	next.Sync = config.SyncSettings{Enabled: true, Folder: folder}
	_, err := manager.SaveConfig(next, "")
	return err
}

// checkRefused says a folder is refused both when sync is set up and when the
// setting is saved.
func checkRefused(t *testing.T, manager *config.HomeManager, folder string) {
	t.Helper()
	if err := manager.ValidateSync(config.SyncSettings{Enabled: true, Folder: folder}); !errors.Is(err, config.ErrInvalidSettings) {
		t.Errorf("ValidateSync(%s) error = %v, want ErrInvalidSettings", folder, err)
	}
	if err := saveSync(manager, folder); !errors.Is(err, config.ErrInvalidSettings) {
		t.Errorf("SaveConfig(%s) error = %v, want ErrInvalidSettings", folder, err)
	}
}

func checkAccepted(t *testing.T, manager *config.HomeManager, folder string) {
	t.Helper()
	if err := manager.ValidateSync(config.SyncSettings{Enabled: true, Folder: folder}); err != nil {
		t.Errorf("ValidateSync(%s) error = %v", folder, err)
	}
	if err := saveSync(manager, folder); err != nil {
		t.Errorf("SaveConfig(%s) error = %v", folder, err)
	}
}

func writeSyncConfig(t *testing.T, home, folder string) {
	t.Helper()
	data := "schema_version: 1\nserver:\n  addr: 127.0.0.1:0\n  open_browser: true\n" +
		"security:\n  auto_lock: 15m\nnode_discovery:\n  refresh_interval: 30m\n  request_timeout: 5s\n" +
		"sync:\n  enabled: true\n  folder: " + folder + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestASyncFolderReachedThroughALinkIsRefused(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	home := filepath.Join(base, "home")
	manager := newSyncHome(t, home)
	links := t.TempDir()
	intoHome := symlink(t, home, filepath.Join(links, "into-home"))
	aboveHome := symlink(t, base, filepath.Join(links, "above-home"))

	for _, folder := range []string{
		intoHome,
		filepath.Join(intoHome, "spaces"),
		// Not there yet: the part that exists is what is resolved.
		filepath.Join(intoHome, "not", "yet"),
		aboveHome,
	} {
		checkRefused(t, manager, folder)
	}

	// A link to somewhere else is just a folder.
	elsewhere := t.TempDir()
	checkAccepted(t, manager, filepath.Join(elsewhere, "Walletspace"))
	checkAccepted(t, manager, filepath.Join(symlink(t, elsewhere, filepath.Join(links, "elsewhere")), "Walletspace"))

	// A stored folder that leads into the data folder turns sync off at start.
	writeSyncConfig(t, home, filepath.Join(intoHome, "spaces"))
	if got := newSyncHome(t, home).Snapshot().Config.Sync; got.Enabled {
		t.Errorf("sync through a link into the data folder = %+v, want off", got)
	}
}

func TestASyncFolderInsideADataFolderReachedThroughALinkIsRefused(t *testing.T) {
	t.Parallel()

	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	home := symlink(t, real, filepath.Join(t.TempDir(), "walletspace"))
	manager := newSyncHome(t, home)
	checkRefused(t, manager, filepath.Join(real, "spaces"))
	checkRefused(t, manager, real)
}

func TestASyncFolderDifferingOnlyInCaseIsRefusedOnMacOS(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "darwin" {
		t.Skip("only macOS folds case by default")
	}
	base := t.TempDir()
	home := filepath.Join(base, "Home")
	manager := newSyncHome(t, home)
	if _, err := os.Stat(filepath.Join(base, "HOME")); err != nil {
		t.Skipf("the temporary folder is on a case-sensitive volume: %v", err)
	}
	for _, folder := range []string{
		filepath.Join(base, "HOME"),
		filepath.Join(base, "home", "Spaces"),
		filepath.Join(base, "home", "not", "yet"),
		strings.ToUpper(base),
	} {
		checkRefused(t, manager, folder)
	}
	checkAccepted(t, manager, filepath.Join(base, "Home2"))
}

// Not parallel: it replaces how paths are resolved.
func TestASyncFolderThatDoesNotAnswerIsBounded(t *testing.T) {
	home := t.TempDir()
	manager := newSyncHome(t, home)
	stored := filepath.Join(t.TempDir(), "Walletspace")
	if err := saveSync(manager, stored); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	started := make(chan struct{}, 16)
	restore := config.SetSyncFolderCheck(func(path string) (string, error) {
		started <- struct{}{}
		<-release
		return path, nil
	}, 20*time.Millisecond)
	t.Cleanup(func() {
		close(release)
		restore()
		for deadline := time.Now().Add(hang); config.SyncFolderChecksInFlight() > 0; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Error("checks still in flight after they were released")
				return
			}
		}
	})
	within := func(name string, call func()) {
		t.Helper()
		done := make(chan struct{})
		go func() {
			defer close(done)
			call()
		}()
		select {
		case <-done:
		case <-time.After(hang):
			t.Fatalf("%s waited on a folder that does not answer", name)
		}
	}

	var err error
	within("ValidateSync", func() {
		err = manager.ValidateSync(config.SyncSettings{Enabled: true, Folder: filepath.Join(t.TempDir(), "other")})
	})
	if !errors.Is(err, config.ErrInvalidSettings) {
		t.Fatalf("ValidateSync(no answer) error = %v, want ErrInvalidSettings", err)
	}

	// Another setting is still saved: the folder it keeps is not looked at.
	within("SaveConfig", func() {
		next := manager.Snapshot().Config
		next.Security.AutoLock = 30 * time.Minute
		_, err = manager.SaveConfig(next, "")
	})
	if err != nil {
		t.Fatalf("SaveConfig(auto-lock, same folder) error = %v", err)
	}

	// At start, a folder that does not answer keeps its setting.
	var restarted *config.HomeManager
	within("NewHomeManager", func() { restarted, err = config.NewHomeManager(home) })
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.Snapshot().Config.Sync; !got.Enabled || got.Folder != stored {
		t.Fatalf("sync after a start without an answer = %+v, want %s kept", got, stored)
	}

	// Checks that never return pile up only to a bound.
	for range 6 {
		within("ValidateSync", func() {
			err = manager.ValidateSync(config.SyncSettings{Enabled: true, Folder: filepath.Join(home, "..", "x")})
		})
	}
	const bound = 4
	for range bound {
		select {
		case <-started:
		case <-time.After(hang):
			t.Fatal("a check did not start")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if extra := len(started); extra > 0 {
		t.Fatalf("%d checks beyond the bound of %d were started", extra, bound)
	}

	// A folder inside the data folder as written is refused without asking
	// the disk, so a start that gets no answer still turns it off.
	writeSyncConfig(t, home, filepath.Join(home, "spaces"))
	within("NewHomeManager", func() { restarted, err = config.NewHomeManager(home) })
	if err != nil {
		t.Fatal(err)
	}
	if got := restarted.Snapshot().Config.Sync; got.Enabled {
		t.Fatalf("sync inside the data folder, disk not answering = %+v, want off", got)
	}
}
