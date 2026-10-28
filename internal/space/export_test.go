package space

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
)

// WriteSpaceWithoutPhrase puts on disk a space the way versions before every
// space had a phrase created an imported-only one: a vault with no mnemonic.
// The manager that should see it is started afterwards.
func WriteSpaceWithoutPhrase(home, id, password string, params vault.Params) error {
	now := time.Now().UTC()
	container, err := vault.SealJSON(password, Payload{
		Version: 1, ImportedKeys: map[string]KeyVariants{}, CreatedAt: now,
	}, aad(id), params)
	if err != nil {
		return err
	}
	data, err := json.Marshal(File{
		SchemaVersion: SchemaVersion, ID: id, Name: "imported only",
		CreatedAt: now, UpdatedAt: now, Vault: container,
	})
	if err != nil {
		return err
	}
	return storage.AtomicWrite(filepath.Join(home, "spaces", id, "space.json"), data)
}

// VaultContents opens a space's vault as it is on disk and reports what it
// holds besides the phrase: how many key variants each reference has, and the
// key tombstones. The keys themselves are wiped.
func VaultContents(home, id, password string) (map[string]int, []devicesync.Tombstone, error) {
	data, err := os.ReadFile(filepath.Join(home, "spaces", id, "space.json"))
	if err != nil {
		return nil, nil, err
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, err
	}
	var payload Payload
	defer clearPayload(&payload)
	if err := vault.OpenJSON(password, file.Vault, aad(id), &payload); err != nil {
		return nil, nil, err
	}
	counts := make(map[string]int, len(payload.ImportedKeys))
	for ref, variants := range payload.ImportedKeys {
		counts[ref] = len(variants)
	}
	return counts, payload.DeletedKeys, nil
}

// quietSync makes store the manager's sync folder without starting the sync
// loop: nothing is sent unless the test asks, so a test can line sends up
// as it needs.
func quietSync(m *Manager, store devicesync.Store, deviceID string) *syncer {
	s := newSyncer(store, deviceID, slog.New(slog.DiscardHandler))
	close(s.done) // no loop runs for it
	m.syncMu.Lock()
	previous := m.sync
	m.sync = s
	clear(m.syncStatus)
	m.syncMu.Unlock()
	if previous != nil {
		// As SetSync does: nothing of the previous syncer runs after this.
		previous.cancel()
		<-previous.done
		previous.requests.Wait()
		m.syncRun.Lock()
		m.syncRun.Unlock() //nolint:staticcheck // a barrier
	}
	return s
}

// SetSyncQuiet is quietSync for the package's black-box tests.
func SetSyncQuiet(m *Manager, store devicesync.Store, deviceID string) { quietSync(m, store, deviceID) }
