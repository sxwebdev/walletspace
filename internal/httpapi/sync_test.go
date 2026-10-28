package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/space"
)

type syncOverviewBody struct {
	Enabled bool                        `json:"enabled"`
	Error   string                      `json:"error"`
	Folder  string                      `json:"folder"`
	Spaces  map[string]space.SyncStatus `json:"spaces"`
}

func TestPlatformTurnsSyncOnAndOff(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	id := createSpace(t, fixture, "Main", "a long enough password")

	overview := decodeBody[syncOverviewBody](t, platformRequest(t, fixture.handler, http.MethodGet, "/api/sync", nil))
	if overview.Enabled || overview.Spaces[id].State != space.SyncOff {
		t.Fatalf("overview before = %+v, want off", overview)
	}

	// The wallet's own data folder is never a sync folder, and nothing is
	// created there for asking.
	inside := filepath.Join(fixture.settings.Home(), "cloud")
	refused := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/sync",
		map[string]any{"enabled": true, "folder": inside})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("PATCH sync into the data folder = %d %s", refused.Code, refused.Body.String())
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("a refused folder was created: %v", err)
	}

	folder := filepath.Join(t.TempDir(), "Walletspace")
	saved := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/sync",
		map[string]any{"enabled": true, "folder": folder})
	if saved.Code != http.StatusOK {
		t.Fatalf("PATCH sync = %d %s", saved.Code, saved.Body.String())
	}
	if got := decodeBody[struct {
		Sync struct {
			Enabled bool   `json:"enabled"`
			Folder  string `json:"folder"`
		} `json:"sync"`
	}](t, saved).Sync; !got.Enabled || got.Folder != folder {
		t.Fatalf("saved sync = %+v", got)
	}

	ran := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/sync", map[string]any{})
	if ran.Code != http.StatusOK {
		t.Fatalf("POST sync = %d %s", ran.Code, ran.Body.String())
	}
	if status := decodeBody[space.SyncStatus](t, ran); status.State != space.SyncOK || status.LastWrite.IsZero() {
		t.Fatalf("sync status = %+v", status)
	}
	if entries, err := os.ReadDir(filepath.Join(folder, "v1")); err != nil || len(entries) != 1 {
		t.Fatalf("folder after a pass = %v, %v; want one space", entries, err)
	}
	overview = decodeBody[syncOverviewBody](t, platformRequest(t, fixture.handler, http.MethodGet, "/api/sync", nil))
	if !overview.Enabled || overview.Folder != folder || overview.Spaces[id].State != space.SyncOK {
		t.Fatalf("overview while on = %+v", overview)
	}
	// How far this machine's change got: written, upload not knowable.
	if got := overview.Spaces[id]; got.Delivery != space.DeliveryUploadUnknown || got.LocalRevision == 0 ||
		got.WrittenRevision != got.LocalRevision {
		t.Fatalf("delivery while on = %+v", got)
	}

	off := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/sync",
		map[string]any{"enabled": false, "folder": folder})
	if off.Code != http.StatusOK {
		t.Fatalf("PATCH sync off = %d %s", off.Code, off.Body.String())
	}
	overview = decodeBody[syncOverviewBody](t, platformRequest(t, fixture.handler, http.MethodGet, "/api/sync", nil))
	if overview.Enabled || overview.Folder != folder {
		t.Fatalf("overview after off = %+v, want off with the folder remembered", overview)
	}
}

func TestPlatformRefusesASyncFolderThatIsAFile(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	file := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/sync",
		map[string]any{"enabled": true, "folder": file})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("PATCH sync onto a file = %d %s", response.Code, response.Body.String())
	}
	if fixture.settings.Snapshot().Config.Sync.Enabled {
		t.Fatal("a refused folder was saved")
	}
}

// A sync folder whose parent is gone belongs to a provider that went away:
// iCloud Drive turned off, a volume not mounted. Recreating the whole path
// would make a plain local folder that nothing copies, and every pass would
// report it as synced. Only the last folder is ever made.
func TestPlatformDoesNotRecreateAVanishedProvider(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	gone := filepath.Join(t.TempDir(), "com~apple~CloudDocs")
	folder := filepath.Join(gone, "Walletspace")
	response := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/sync",
		map[string]any{"enabled": true, "folder": folder})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("PATCH sync under a missing folder = %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Fatalf("the missing provider folder was created: %v", err)
	}

	// With the provider there, the sync folder itself is made.
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	response = platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/sync",
		map[string]any{"enabled": true, "folder": folder})
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH sync = %d %s", response.Code, response.Body.String())
	}
	if info, err := os.Stat(folder); err != nil || !info.IsDir() {
		t.Fatalf("sync folder = %v, %v", info, err)
	}
}

// A save refused for a stale revision changes nothing: no folder is made and
// sync is not restarted for it.
func TestPlatformRefusesAStaleSyncSaveBeforeApplyingIt(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	folder := filepath.Join(t.TempDir(), "Walletspace")
	data, err := json.Marshal(map[string]any{"enabled": true, "folder": folder})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/api/settings/sync", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", `"stale"`)
	request.Host = testHost
	request.Header.Set(httpapi.TokenHeader, testToken)
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusPreconditionFailed {
		t.Fatalf("PATCH sync with a stale revision = %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Fatalf("a refused save made the folder: %v", err)
	}
	overview := decodeBody[syncOverviewBody](t, platformRequest(t, fixture.handler, http.MethodGet, "/api/sync", nil))
	if overview.Enabled {
		t.Fatal("a refused save turned sync on")
	}
}

// Sync turned on in config.yaml whose folder cannot be opened at start does
// not stop the wallet, and does not hide either: the overview says sync is on
// and why it is not running, instead of reading "off" under a switch that
// reads "on".
func TestPlatformReportsSyncThatCouldNotStart(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	file := filepath.Join(t.TempDir(), "not-a-folder")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	httpapi.StartSync(fixture.spaces, fixture.settings.Home(), config.SyncSettings{Enabled: true, Folder: file},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	overview := decodeBody[syncOverviewBody](t, platformRequest(t, fixture.handler, http.MethodGet, "/api/sync", nil))
	if !overview.Enabled || overview.Error == "" {
		t.Fatalf("overview = %+v, want sync reported on and not running", overview)
	}
}

// A restore and an unlock say what their first sync found, so that the UI
// does not show a space as complete while it may still be missing what other
// computers wrote. A locked space whose send fails is answered with its
// status, like any pass that failed — not with an error.
func TestPlatformReportsTheFirstSyncAndALockedSpacesFailedSend(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	folder := filepath.Join(t.TempDir(), "Walletspace")
	if saved := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/sync",
		map[string]any{"enabled": true, "folder": folder}); saved.Code != http.StatusOK {
		t.Fatalf("PATCH sync = %d %s", saved.Code, saved.Body.String())
	}
	type firstSync struct {
		Sync  *space.FirstSync `json:"sync"`
		Space space.Summary    `json:"space"`
	}
	const password = "a long enough password"
	created := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces",
		map[string]any{"name": "Main", "password": password})
	body := decodeBody[firstSync](t, created)
	if created.Code != http.StatusCreated || body.Sync == nil || !body.Sync.Finished || body.Sync.Pending != 0 {
		t.Fatalf("POST /api/spaces = %d %s, want a finished first sync", created.Code, created.Body.String())
	}
	id := body.Space.ID
	lock := func() {
		t.Helper()
		if locked := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/lock", nil); locked.Code != http.StatusOK {
			t.Fatalf("lock = %d %s", locked.Code, locked.Body.String())
		}
	}
	lock()
	unlocked := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/unlock", map[string]any{"password": password})
	if got := decodeBody[firstSync](t, unlocked); unlocked.Code != http.StatusOK || got.Sync == nil || !got.Sync.Finished {
		t.Fatalf("unlock = %d %s, want a finished first sync", unlocked.Code, unlocked.Body.String())
	}

	// The folder goes away: nothing can be written, and the first sync of the
	// next unlock does not finish.
	if renamed := platformRequest(t, fixture.handler, http.MethodPatch, "/api/spaces/"+id, map[string]any{"name": "Renamed"}); renamed.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", renamed.Code, renamed.Body.String())
	}
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	lock()
	ran := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/sync", map[string]any{})
	if status := decodeBody[space.SyncStatus](t, ran); ran.Code != http.StatusOK || status.SendError == "" ||
		status.Delivery != space.DeliveryUnsent {
		t.Fatalf("POST sync of a locked space whose send fails = %d %s, want its status", ran.Code, ran.Body.String())
	}
	unlocked = platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+id+"/unlock", map[string]any{"password": password})
	if got := decodeBody[firstSync](t, unlocked); unlocked.Code != http.StatusOK || got.Sync == nil || got.Sync.Finished {
		t.Fatalf("unlock = %d %s, want an unfinished first sync", unlocked.Code, unlocked.Body.String())
	}
}
