package space

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
)

// Sync keeps each unlocked space current with the other machines that hold
// its phrase. The design is docs/device-sync.md; the format and the merge are
// in package devicesync. What is here is the part that touches secrets:
// deriving the keys from the seed, turning a space into a snapshot and back,
// and checking everything another machine sent against the vault before any
// of it is kept.
//
// A space syncs only while it is unlocked, because its keys exist only then.
// Nothing about sync is ever written to disk in a form that opens without the
// phrase. What a change makes is sealed into the space's outbox while the keys
// are there, and sent from it whether the space is open or not (outbox.go).

const (
	// syncInterval is how often unlocked spaces look at the folder. iCloud and
	// Dropbox report changes to watchers unreliably, and the folder is small.
	syncInterval = 30 * time.Second
	// syncDebounce gathers a burst of local edits into one write.
	syncDebounce = 500 * time.Millisecond
	// syncRunTimeout bounds one pass over one space.
	syncRunTimeout = 30 * time.Second
	// syncUnlockWait is how long an unlock waits for its first sync, so that
	// the space opens already current. A slow folder is finished in the
	// background.
	syncUnlockWait = 3 * time.Second
	// syncFlushWait bounds the sends made while the wallet shuts down: what
	// does not reach the folder by then stays in the outbox for the next
	// start.
	syncFlushWait = 2 * time.Second
	// downloadRequestTimeout bounds one download request: a provider that is
	// offline never answers it.
	downloadRequestTimeout = 10 * time.Second
	// downloadAttempts is how many download requests for one file may fail
	// in a row before the file is reported missing rather than downloading.
	downloadAttempts = 3
)

// Sync states of a space.
const (
	SyncOff      = "off"       // sync is not enabled on this machine
	SyncNoPhrase = "no_phrase" // the space has no recovery phrase to derive keys from
	SyncLocked   = "locked"    // the space has not been unlocked since sync started
	SyncOK       = "ok"
	SyncError    = "error"
)

// Why files are missing.
const (
	// MissingCannotAsk: the folder's provider cannot be asked to download
	// a file from here — not iCloud Drive, or not macOS.
	MissingCannotAsk = "cannot_ask"
	// MissingRequestsFailed: it was asked, and each request failed
	// downloadAttempts times in a row.
	MissingRequestsFailed = "requests_failed"
)

// Why this machine's file in the folder is merged before it is replaced.
const (
	// MergeFirstNotMerged: the space is locked; an unlock merges it.
	MergeFirstNotMerged = "not_merged"
	// MergeFirstNotDownloaded: it cannot be read yet.
	MergeFirstNotDownloaded = "not_downloaded"
	// MergeFirstNewerVersion: it was written by a newer Walletspace, in a
	// form this one does not read. Updating Walletspace here merges it.
	MergeFirstNewerVersion = "newer_version"
	// MergeFirstFailed: it was read, and the merge did not complete (the
	// status's error says why).
	MergeFirstFailed = "merge_failed"
)

// Delivery states: how far this machine's latest change to a space got.
const (
	// DeliveryUnsent: the folder does not hold the latest change yet.
	DeliveryUnsent = "unsent"
	// DeliveryUploadUnknown: the latest change is written to this machine's
	// file in the folder. Whether the provider has uploaded it cannot be
	// asked: no provider, iCloud Drive included, tells a program without cgo.
	DeliveryUploadUnknown = "upload_unknown"
)

// SyncStatus is what the last pass over a space found.
type SyncStatus struct {
	State     string    `json:"state"`
	LastSync  time.Time `json:"last_sync,omitzero"`
	LastWrite time.Time `json:"last_write,omitzero"`
	// Devices is how many machines' files were read, this one's included.
	Devices int `json:"devices"`
	// Pending counts files the provider has not downloaded yet, this
	// machine's own included: it is merged before it is replaced.
	Pending int `json:"pending"`
	// Missing counts the pending files that will not arrive by themselves:
	// the provider cannot be asked for them, or kept refusing. Keeping the
	// folder downloaded brings them.
	Missing int `json:"missing"`
	// MissingReason is why they will not: MissingCannotAsk or
	// MissingRequestsFailed.
	MissingReason string `json:"missing_reason,omitempty"`
	// Unreadable counts files that did not open: damaged, or not ours.
	Unreadable int    `json:"unreadable"`
	Error      string `json:"error,omitempty"`
	// Duplicates are wallets that share a key with an older one.
	Duplicates []string `json:"duplicates,omitempty"`
	// Dropped are wallets another machine sent that the vault does not vouch
	// for. They were not kept.
	Dropped []string `json:"dropped,omitempty"`
	// KeyConflicts are key references that hold more than one key: machines
	// disagreed about what the reference means. Every key is kept, and each
	// wallet uses the one that makes its address.
	KeyConflicts []string `json:"key_conflicts,omitempty"`
	// FutureRecords counts records stamped well ahead of this machine's
	// clock: one machine's clock is wrong, and timestamps decide merges.
	FutureRecords int `json:"future_records,omitempty"`

	// Delivery is how far this machine's latest change got (Delivery*
	// constants). Empty when sync is off or the space has no phrase.
	Delivery string `json:"delivery,omitempty"`
	// NeedsUnlock says that the space is locked with a change that is not
	// sealed for the folder yet — the wallet stopped between the change and
	// its outbox, or the space has not been open since sync was turned on
	// here — or with MergeFirst. Only an unlock can do either.
	NeedsUnlock bool `json:"needs_unlock,omitempty"`
	// MergeFirst says that this machine's file in the folder was not
	// replaced: it is not what this machine last wrote there, and may hold
	// what the space does not (the data folder was restored from a backup,
	// say), or it cannot be read yet. It is merged before it is replaced.
	MergeFirst bool `json:"merge_first,omitempty"`
	// MergeFirstReason says why: one of the MergeFirst* constants.
	MergeFirstReason string `json:"merge_first_reason,omitempty"`
	// SendError is why the last attempt to send the outbox failed. It stays
	// queued and is tried again.
	SendError string `json:"send_error,omitempty"`
	// The revisions behind Delivery: of the space, of what the outbox holds,
	// and of what was last found in or written to the folder (0 when
	// nothing was).
	LocalRevision   uint64 `json:"local_revision"`
	OutboxRevision  uint64 `json:"outbox_revision"`
	WrittenRevision uint64 `json:"written_revision"`
}

// SyncOverview is the state of sync on this machine.
type SyncOverview struct {
	Enabled bool                  `json:"enabled"`
	Spaces  map[string]SyncStatus `json:"spaces"`
	// Unknown counts spaces in the folder this machine does not hold.
	//
	// A space is recognised by its tag, and its tag is known here only once it
	// has synced here: the tag comes from the seed. So a space that is here but
	// has not been unlocked since sync was turned on looks, from the folder,
	// exactly like one that is not here. Those are Unidentified, and Unknown
	// leaves room for them — an undercount says "unlock your spaces first",
	// where an overcount would send the person to restore a space they have.
	Unknown      int    `json:"unknown"`
	Unidentified int    `json:"unidentified"`
	Error        string `json:"error,omitempty"`
}

type syncer struct {
	store    devicesync.Store
	deviceID string
	// folder names the store, so that what was written to one folder is not
	// taken for written to another (see storeFolder).
	folder string
	log    *slog.Logger
	dirty  chan string
	// ctx ends when the syncer is replaced or turned off. Every pass made with
	// this syncer runs under it, the ones started outside its loop included,
	// so that turning sync off stops a pass in flight instead of waiting out
	// its timeout — and nothing is written to a folder after it was let go.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// requests holds the download requests in flight (requestDownloads);
	// asking guards which spaces have one, and noDownload is set once the
	// provider said it cannot be asked.
	requests   sync.WaitGroup
	asking     map[string]bool
	noDownload atomic.Bool
}

// syncCache is the public part of a space's sync identity, kept beside its
// file so that a locked start can still recognise its folder, and the record
// of its delivery. Nothing in it opens anything.
type syncCache struct {
	SpaceTag string `json:"space_tag"`
	// LastWrite is when this machine last wrote its file to Folder.
	LastWrite time.Time `json:"last_write,omitzero"`
	// The outbox was sealed by installation DeviceID as device DeviceTag of
	// the space, at OutboxRevision. A record from another device id belongs
	// to an installation this one was copied from, and its outbox is
	// dropped.
	DeviceID       string `json:"device_id,omitempty"`
	DeviceTag      string `json:"device_tag,omitempty"`
	OutboxRevision uint64 `json:"outbox_revision,omitempty"`
	// WrittenRevision is the outbox revision last written to, or found in,
	// Folder; absent when nothing was.
	Folder          string  `json:"folder,omitempty"`
	WrittenRevision *uint64 `json:"written_revision,omitempty"`
	// WrittenSum is the SHA-256 of the file this machine last wrote to, or
	// found in, Folder. A file there with that sum is this machine's own
	// last write, and may be replaced without merging it first.
	WrittenSum string `json:"written_sha256,omitempty"`
	// NoPhrase: the space had no recovery phrase when it was last open. It
	// has no tag and nothing to deliver; locked, it is reported as such
	// rather than as waiting for an unlock that cannot help.
	NoPhrase bool `json:"no_phrase,omitempty"`
}

// SetSync turns sync on with store as the shared folder, or off with a nil
// store. It may be called again at any time; the previous store is let go
// once the pass in flight is done.
func (m *Manager) SetSync(store devicesync.Store, deviceID string, log *slog.Logger) {
	m.replaceSync(nil, store, deviceID, log, "")
}

// SetSyncPending turns sync on over a folder that could not be opened —
// folder names it, and reason says why. Spaces seal their changes into
// their outboxes as with any folder, so nothing made meanwhile waits for an
// unlock; sending fails with reason until the folder is back. Every retry,
// reopen is asked for the folder again, one attempt at a time, and the
// store it returns replaces this one — unless sync was set otherwise since.
func (m *Manager) SetSyncPending(
	folder, deviceID, reason string, reopen func() (devicesync.Store, error), retry time.Duration, log *slog.Logger,
) {
	pending := m.replaceSync(nil, unavailableStore{root: folder, err: errors.New(reason)}, deviceID, log, reason)
	go func() {
		ticker := time.NewTicker(retry)
		defer ticker.Stop()
		for {
			select {
			case <-pending.ctx.Done():
				return
			case <-ticker.C:
			}
			store, err := reopen()
			if err != nil {
				continue
			}
			m.replaceSync(pending, store, deviceID, log, "")
			return
		}
	}()
}

// unavailableStore stands for a folder that could not be opened. Its root is
// the folder's, so that what is recorded about it holds once it is opened.
type unavailableStore struct {
	root string
	err  error
}

func (u unavailableStore) Root() string                             { return u.root }
func (u unavailableStore) Spaces(context.Context) ([]string, error) { return nil, u.err }
func (u unavailableStore) List(context.Context, string) ([]devicesync.Entry, error) {
	return nil, u.err
}
func (u unavailableStore) Get(context.Context, string, string) ([]byte, error) { return nil, u.err }
func (u unavailableStore) Put(context.Context, string, string, []byte) error   { return u.err }

// replaceSync is SetSync. With onlyIf, it changes nothing unless sync still
// runs with onlyIf. unavailable is what the overview reports as the reason
// sync is not running. It returns the new syncer, or nil.
func (m *Manager) replaceSync(
	onlyIf *syncer, store devicesync.Store, deviceID string, log *slog.Logger, unavailable string,
) *syncer {
	if log == nil {
		log = slog.Default()
	}
	m.syncMu.Lock()
	if onlyIf != nil && m.sync != onlyIf {
		m.syncMu.Unlock()
		return nil
	}
	previous := m.sync
	m.sync = nil
	m.syncUnavailable = unavailable
	if store != nil {
		m.sync = newSyncer(store, deviceID, log)
		clear(m.syncStatus)
		clear(m.sendErrors)
		clear(m.downloadFailures)
		clear(m.mergeFirst)
	}
	current := m.sync
	m.syncMu.Unlock()
	if previous != nil {
		previous.cancel()
		<-previous.done
		previous.requests.Wait()
		// Every pass and every send holds syncRun. Once it is free, none of
		// the previous syncer's is left — they were cancelled above — so the
		// old folder is not written again after this returns.
		m.syncRun.Lock()
		m.syncRun.Unlock() //nolint:staticcheck // a barrier, not a critical section
	}
	if current != nil {
		// The loop starts with every space: open ones sync at once rather
		// than at the next tick, and every outbox not in the folder yet is
		// sent, locked spaces' included.
		go m.syncLoop(current)
	}
	return current
}

func newSyncer(store devicesync.Store, deviceID string, log *slog.Logger) *syncer {
	ctx, cancel := context.WithCancel(context.Background())
	return &syncer{
		store: store, deviceID: deviceID, folder: storeFolder(store), log: log,
		dirty: make(chan string, 64), ctx: ctx, cancel: cancel, done: make(chan struct{}),
		asking: make(map[string]bool),
	}
}

// storeFolder names a store for the delivery record: a folder by its path.
// A store with no path — a test's — is one folder.
func storeFolder(store devicesync.Store) string {
	if rooted, ok := store.(interface{ Root() string }); ok {
		return rooted.Root()
	}
	return "store"
}

func (s *syncer) mark(id string) {
	select {
	case s.dirty <- id:
	default:
		// Full: a pass over every space is coming at the next tick anyway.
	}
}

func (m *Manager) currentSyncer() *syncer {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	return m.sync
}

// SetSyncUnavailable records why sync is turned on but not running — its
// folder could not be opened at start. The overview reports it until sync is
// set again, so that Settings does not show sync as on over nothing.
func (m *Manager) SetSyncUnavailable(reason string) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.syncUnavailable = reason
}

// markDirty asks for a pass over a space after a local change.
func (m *Manager) markDirty(id string) {
	if s := m.currentSyncer(); s != nil {
		s.mark(id)
	}
}

func (m *Manager) spaceIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Sorted(maps.Keys(m.files))
}

func (m *Manager) syncLoop(s *syncer) {
	defer close(s.done)
	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	// Every space, at the start and on every tick: an open one syncs, and any
	// one sends what its outbox holds and the folder does not.
	everySpace := func() bool {
		for _, id := range m.spaceIDs() {
			if s.ctx.Err() != nil {
				return false
			}
			m.syncOnce(s, id, nil)
		}
		return true
	}
	if !everySpace() {
		return
	}
	pending := make(map[string]struct{})
	var debounce <-chan time.Time
	for {
		select {
		case <-s.ctx.Done():
			return
		case id := <-s.dirty:
			pending[id] = struct{}{}
			if debounce == nil {
				debounce = time.After(syncDebounce)
			}
		case <-debounce:
			debounce = nil
			for _, id := range slices.Sorted(maps.Keys(pending)) {
				if s.ctx.Err() != nil {
					return
				}
				m.syncOnce(s, id, nil)
			}
			clear(pending)
		case <-ticker.C:
			if !everySpace() {
				return
			}
		}
	}
}

// syncOnce runs a pass over a space and sends its outbox, then asks for the
// files the pass found missing. passed, if given, is called between the two:
// download requests take their time, and nothing waits for them.
func (m *Manager) syncOnce(s *syncer, id string, passed func()) {
	ctx, cancel := context.WithTimeout(s.ctx, m.syncTimeout)
	defer cancel()
	result, err := m.passSpace(ctx, s, id)
	if passed != nil {
		passed()
	}
	// A pass cut short because sync was turned off did not fail.
	if err != nil && s.ctx.Err() == nil {
		s.log.Warn("space sync failed", "space_id", id, "error", err)
	}
	m.requestDownloads(s, id, result)
}

// syncAfterUnlock runs the first pass for a space that just opened, waiting a
// little for it so the space opens current.
func (m *Manager) syncAfterUnlock(id string) *FirstSync {
	s := m.currentSyncer()
	if s == nil {
		return nil
	}
	done := make(chan struct{})
	go m.syncOnce(s, id, func() { close(done) })
	select {
	case <-done:
	case <-time.After(syncUnlockWait):
		// Still reading the folder: whatever it brings comes later.
		if m.syncStatusOf(id).State == SyncNoPhrase {
			return nil
		}
		return &FirstSync{}
	}
	status := m.syncStatusOf(id)
	switch status.State {
	case SyncNoPhrase, SyncOff:
		return nil
	}
	return &FirstSync{Finished: status.State == SyncOK, Pending: status.Pending, Missing: status.Missing}
}

// FirstSync is what the pass an unlock or a restore waits for had found
// when the wait ended. Anything short of finished with nothing pending is a
// space that may still be missing what other machines wrote, and must not
// be shown as complete.
type FirstSync struct {
	// Finished: the pass ended within the wait, without an error.
	Finished bool `json:"finished"`
	// Pending and Missing are the pass's counts of other machines' files
	// not downloaded yet (see SyncStatus).
	Pending int `json:"pending"`
	Missing int `json:"missing"`
}

// SyncNow runs a pass over a space and returns what it found. A locked space
// only sends its outbox.
func (m *Manager) SyncNow(ctx context.Context, id string) (SyncStatus, error) {
	s := m.currentSyncer()
	if s == nil {
		return SyncStatus{State: SyncOff}, nil
	}
	// The same bound the loop's passes have: a pass holds syncRun, and every
	// other pass, and turning sync off, waits for it.
	ctx, cancel := context.WithTimeout(ctx, m.syncTimeout)
	defer cancel()
	result, err := m.passSpace(ctx, s, id)
	m.requestDownloads(s, id, result)
	return m.syncStatusOf(id), err
}

// syncStatusOf is a space's last pass with its delivery as it is now.
func (m *Manager) syncStatusOf(id string) SyncStatus {
	m.syncMu.Lock()
	s := m.sync
	status, passed := m.syncStatus[id]
	sendError := m.sendErrors[id]
	m.syncMu.Unlock()
	if s == nil {
		return SyncStatus{State: SyncOff}
	}
	if !passed {
		status = SyncStatus{State: SyncLocked}
	}
	status.SendError = sendError
	m.addDelivery(s, id, &status)
	return status
}

func (m *Manager) setSyncStatus(s *syncer, id string, status SyncStatus) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	if m.sync == s {
		m.syncStatus[id] = status
	}
}

// SyncOverview reports sync on this machine: every space's last pass, and how
// many spaces in the folder are not here.
func (m *Manager) SyncOverview(ctx context.Context) SyncOverview {
	m.syncMu.Lock()
	s, unavailable := m.sync, m.syncUnavailable
	m.syncMu.Unlock()
	overview := SyncOverview{Spaces: map[string]SyncStatus{}}
	m.mu.RLock()
	ids := slices.Sorted(maps.Keys(m.files))
	m.mu.RUnlock()
	for _, id := range ids {
		overview.Spaces[id] = m.syncStatusOf(id)
	}
	if s == nil {
		if unavailable != "" {
			overview.Enabled, overview.Error = true, unavailable
		}
		return overview
	}
	overview.Enabled = true
	tags, err := s.store.Spaces(ctx)
	if err != nil {
		overview.Error = err.Error()
		return overview
	}
	known := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if cache, err := m.readSyncCache(id); err == nil && cache.SpaceTag != "" {
			known[cache.SpaceTag] = struct{}{}
		} else if overview.Spaces[id].State != SyncNoPhrase {
			// A space without a phrase has no folder to be confused with.
			overview.Unidentified++
		}
	}
	unmatched := 0
	for _, tag := range tags {
		if _, ok := known[tag]; !ok {
			unmatched++
		}
	}
	overview.Unknown = max(0, unmatched-overview.Unidentified)
	return overview
}

func (m *Manager) syncCachePath(id string) string {
	return filepath.Join(m.home, "spaces", id, "sync.json")
}

func (m *Manager) readSyncCache(id string) (syncCache, error) {
	data, err := os.ReadFile(m.syncCachePath(id))
	if err != nil {
		return syncCache{}, err
	}
	var cache syncCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return syncCache{}, err
	}
	return cache, nil
}

// rememberSpaceTagLocked records an open space's tag in its sync.json, sync
// on or off, so that the space can be recognised while locked: by the sync
// overview, and by AddRecoveryPhrase refusing a phrase another local space
// already has. A space without a phrase has no tag. A failed write only
// leaves the tag unknown until the next unlock.
func (m *Manager) rememberSpaceTagLocked(id string) {
	active := m.sessions[id]
	if active == nil {
		return
	}
	cache, _ := m.readSyncCache(id)
	if active.syncKeys == nil {
		// No phrase, so no tag: remembered too, so that the space locked is
		// not reported as waiting for an unlock to send its changes.
		if !cache.NoPhrase {
			cache.NoPhrase = true
			_ = m.writeSyncCache(id, cache)
		}
		return
	}
	tag := active.syncKeys.SpaceTag()
	if cache.SpaceTag == tag && !cache.NoPhrase {
		return
	}
	cache.SpaceTag, cache.NoPhrase = tag, false
	_ = m.writeSyncCache(id, cache)
}

// writeSyncCache replaces a space's sync.json. It is read, changed and
// written under the manager's lock, so that two writers do not lose each
// other's fields.
func (m *Manager) writeSyncCache(id string, cache syncCache) error {
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	return storage.AtomicWrite(m.syncCachePath(id), append(data, '\n'))
}

// deriveSyncKeys gives a payload with a phrase its sync keys, and one without
// none.
func deriveSyncKeys(payload *Payload) *devicesync.Keys {
	if len(payload.Mnemonic) == 0 {
		return nil
	}
	seed, err := account.Seed(string(payload.Mnemonic), string(payload.BIP39Passphrase))
	if err != nil {
		// A vault whose phrase does not validate was refused at unlock.
		return nil
	}
	defer clear(seed)
	keys, err := devicesync.DeriveKeys(seed)
	if err != nil {
		return nil
	}
	return keys
}

func newSession(payload Payload, key *vault.SessionKey, now time.Time) *session {
	return &session{payload: payload, key: key, lastUsed: now, syncKeys: deriveSyncKeys(&payload)}
}

// snapshotOf turns a space into the snapshot this machine writes. The keys in
// it are copies: the snapshot outlives the lock it was taken under.
func snapshotOf(file File, payload *Payload) devicesync.Snapshot {
	snap := devicesync.Snapshot{
		Version: devicesync.SnapshotVersion,
		Name:    file.Name, NameUpdatedAt: file.NameUpdatedAt,
		Accounts:        cloneAccounts(file.Accounts),
		DeletedAccounts: slices.Clone(file.DeletedAccounts),
		DeletedKeys:     slices.Clone(payload.DeletedKeys),
		ImportedKeys:    snapshotKeys(payload.ImportedKeys),
	}
	// A default name has no time, and loses to any name set on purpose
	// anywhere. A name set before names had times was set on purpose, at the
	// latest when the space was made: it counts from then, so that it still
	// wins over the default of a copy restored later — otherwise the two tie
	// at no time at all and the merge settles them by spelling.
	if snap.NameUpdatedAt.IsZero() && snap.Name != defaultName {
		snap.NameUpdatedAt = file.CreatedAt
	}
	// A name longer than a snapshot carries — set before names were bounded —
	// is cut to fit rather than leave the space unable to sync. Tombstones are
	// never cut: sending only some of them would quietly weaken the deletions
	// the rest record. A space past their bound fails its pass instead (see
	// devicesync.Sealable).
	if runes := []rune(snap.Name); len(runes) > devicesync.MaxNameRunes {
		snap.Name = string(runes[:devicesync.MaxNameRunes])
	}
	return snap
}

// snapshotKeys copies a vault's keys into the form a snapshot carries.
func snapshotKeys(keys map[string]KeyVariants) map[string][]devicesync.Key {
	out := make(map[string][]devicesync.Key, len(keys))
	for ref, variants := range keys {
		list := make([]devicesync.Key, 0, len(variants))
		for _, entry := range variants {
			curve := entry.Curve
			if curve == "" {
				curve = devicesync.CurveSecp256k1
			}
			list = append(list, devicesync.Key{Curve: curve, PrivateKey: slices.Clone(entry.PrivateKey), ImportedAt: entry.ImportedAt})
		}
		out[ref] = list
	}
	return out
}

// vaultKeys copies a snapshot's keys into the form a vault keeps.
func vaultKeys(keys map[string][]devicesync.Key) map[string]KeyVariants {
	out := make(map[string]KeyVariants, len(keys))
	for ref, list := range keys {
		variants := make(KeyVariants, 0, len(list))
		for _, key := range list {
			variants = append(variants, KeyEntry{Curve: key.Curve, PrivateKey: slices.Clone(key.PrivateKey), ImportedAt: key.ImportedAt})
		}
		out[ref] = variants
	}
	return out
}

// syncSpace is passSpace followed by its download requests.
func (m *Manager) syncSpace(ctx context.Context, s *syncer, id string) error {
	result, err := m.passSpace(ctx, s, id)
	m.requestDownloads(s, id, result)
	return err
}

// passResult is what a pass leaves for the download requests after it.
type passResult struct {
	spaceTag string
	// absent are other machines' files the pass found not downloaded.
	absent []string
	// at is the pass's LastSync, which names its status.
	at time.Time
}

// passSpace is one pass over one space: read what the other machines wrote,
// merge it with what is here, keep what the vault vouches for, and send this
// machine's outbox to the folder. A locked space, or one without a phrase,
// only sends.
//
// The folder is read and written outside the manager's lock — a provider can
// take its time over a file — and the merge is redone under the lock against
// the space as it is by then, so an edit made while the folder was being read
// is merged, not overwritten.
func (m *Manager) passSpace(ctx context.Context, s *syncer, id string) (passResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.ctx, cancel)()
	m.syncRun.Lock()
	defer m.syncRun.Unlock()
	// A pass that took its syncer before SetSync let it go, and syncRun after
	// SetSync's barrier, must not touch the old folder. AfterFunc cancels ctx
	// from another goroutine, which may not have run yet: check here.
	if err := s.ctx.Err(); err != nil {
		return passResult{}, err
	}
	result, own, err := m.mergeSpace(ctx, s, id)
	if ctx.Err() != nil {
		return result, err
	}
	// What was prepared goes out whether the merge worked or not: the outbox
	// holds the space as it was saved here, and a merge that failed changed
	// nothing.
	sendErr := m.deliver(ctx, s, id, own)
	if err != nil {
		return result, err
	}
	return result, sendErr
}

// mergeSpace is the reading half of a pass. It reports what it saw of this
// machine's own file, or nil when it did not get as far as the folder.
func (m *Manager) mergeSpace(ctx context.Context, s *syncer, id string) (passResult, *ownCopy, error) {
	m.mu.Lock()
	m.expireLocked()
	active := m.sessions[id]
	file, ok := m.files[id]
	if !ok {
		m.mu.Unlock()
		return passResult{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if active == nil {
		m.mu.Unlock()
		m.setSyncStatus(s, id, SyncStatus{State: SyncLocked})
		return passResult{}, nil, nil
	}
	if active.syncKeys == nil {
		m.mu.Unlock()
		m.setSyncStatus(s, id, SyncStatus{State: SyncNoPhrase})
		return passResult{}, nil, nil
	}
	keys := active.syncKeys.Clone()
	defer keys.Clear()
	spaceTag := keys.SpaceTag()
	deviceTag := keys.DeviceTag(s.deviceID, id)
	status := SyncStatus{State: SyncOK}
	fail := func(err error) error {
		// A pass cut short by whoever started it — a request that went away,
		// sync turned off — did not fail: what the last pass found stands.
		if errors.Is(err, context.Canceled) {
			return err
		}
		status.State = SyncError
		status.Error = err.Error()
		m.setSyncStatus(s, id, status)
		return err
	}
	// The space as saved goes into the outbox before the folder is read, if
	// it is not there yet: after a crash between a change and its outbox, or
	// a new device id. A slow folder must not keep it from being ready to
	// send when the space locks.
	if err := m.ensureOutboxLocked(s, file, &active.payload, keys); err != nil {
		m.mu.Unlock()
		return passResult{}, nil, fail(err)
	}
	mnemonic, passphrase := slices.Clone(active.payload.Mnemonic), slices.Clone(active.payload.BIP39Passphrase)
	local := snapshotOf(file, &active.payload)
	m.mu.Unlock()
	defer local.Clear()
	seed, err := seedOf(mnemonic, passphrase)
	clear(mnemonic)
	clear(passphrase)
	if err != nil {
		return passResult{}, nil, err
	}
	// Taken out of the session so that verification can run without the
	// manager's lock, and derived once per pass.
	defer clear(seed)

	entries, err := s.store.List(ctx, spaceTag)
	if err != nil {
		return passResult{}, nil, fail(fmt.Errorf("list sync folder: %w", err))
	}
	result := passResult{spaceTag: spaceTag}
	var remotes []devicesync.Snapshot
	defer func() {
		for i := range remotes {
			remotes[i].Clear()
		}
	}()
	own := &ownCopy{}
	for _, entry := range entries {
		// This machine's own file is read and merged like any other: it may
		// hold what the space does not (see deliver). Until it can be read
		// it is pending, and it is not replaced.
		mine := entry.DeviceTag == deviceTag
		if mine {
			own.listed = true
			own.opaque = entry.Evicted
		}
		if entry.Evicted {
			status.Pending++
			result.absent = append(result.absent, entry.DeviceTag)
			continue
		}
		blob, err := s.store.Get(ctx, spaceTag, entry.DeviceTag)
		if mine && err == nil {
			own.blob = blob
		}
		switch {
		case errors.Is(err, devicesync.ErrNotDownloaded), err != nil && (entry.Dataless || mine):
			// Gone between the listing and the read, or a file the provider
			// could not fetch: not here yet, not damaged.
			status.Pending++
			result.absent = append(result.absent, entry.DeviceTag)
			continue
		case err != nil:
			status.Unreadable++
			continue
		}
		snap, err := devicesync.Open(keys, spaceTag, entry.DeviceTag, blob)
		if err != nil {
			status.Unreadable++
			if mine {
				// Authenticated but not understood — a later release's
				// format — holds what this build cannot merge, and must be
				// kept for that release. One that does not authenticate holds
				// nothing any build can recover.
				own.newer = errors.Is(err, devicesync.ErrInvalidSnapshot)
				own.damaged = !own.newer
			}
			continue
		}
		own.opened = own.opened || mine
		remotes = append(remotes, snap)
	}
	if err := ctx.Err(); err != nil {
		// Cut short — by a timeout, or because sync was turned off. What was
		// read is not the folder; merging it would be harmless, but writing
		// back after sync was let go is not.
		return passResult{}, nil, fail(err)
	}
	status.Devices = len(remotes)
	status.Missing, status.MissingReason = m.missingOf(s, id, result.absent)

	// Verify what the merge would add, outside the lock: a derived wallet is
	// checked by deriving it again, which is not free.
	preview, _ := devicesync.Merge(append([]devicesync.Snapshot{local}, remotes...), m.now())
	verdicts := m.verifyIncoming(local, preview, seed)
	preview.Clear()

	m.mu.Lock()
	if m.sessions[id] != active {
		// Locked, or re-opened under a new password, while the folder was being
		// read. The next pass starts over.
		m.mu.Unlock()
		m.setSyncStatus(s, id, SyncStatus{State: SyncLocked})
		return passResult{}, own, nil
	}
	file = m.files[id]
	current := snapshotOf(file, &active.payload)
	merged, report := devicesync.Merge(append([]devicesync.Snapshot{current}, remotes...), m.now())
	current.Clear()
	merge, err := m.applyMergeLocked(file, &active.payload, merged, verdicts, seed)
	merged.Clear()
	if err != nil {
		m.mu.Unlock()
		return passResult{}, own, fail(err)
	}
	if merge.changed {
		// Saved with a new revision, and sealed into the outbox with it.
		if err := m.keepMergeLocked(id, active, merge); err != nil {
			m.mu.Unlock()
			clearKeys(merge.keys)
			return passResult{}, own, fail(err)
		}
		// As saved: with its new revision.
		file = m.files[id]
	} else {
		// The same keys the space holds, copied: nothing to adopt, so the
		// copies are wiped rather than left to the collector on every pass.
		clearKeys(merge.keys)
	}
	// Normally a no-op: keepMergeLocked sealed the outbox with the merge. A
	// pass made with a syncer that is not the manager's — only tests make
	// one — seals it here.
	err = m.ensureOutboxLocked(s, file, &active.payload, keys)
	m.mu.Unlock()
	if err != nil {
		return passResult{}, own, fail(err)
	}

	status.Dropped = merge.dropped
	status.KeyConflicts = report.KeyConflicts
	status.Duplicates = report.Duplicates
	status.FutureRecords = report.FutureRecords
	status.LastSync = m.now()
	result.at = status.LastSync
	m.setSyncStatus(s, id, status)
	// Opened and merged — the outbox now holds whatever it held — or damaged
	// past recovery.
	own.merged = own.opened || own.damaged
	return result, own, nil
}

// verifyIncoming checks every wallet the merge would bring in that is not
// already here as it is. The answer is keyed by the wallet's encoding, so a
// copy that changes between the preview and the merge under the lock is
// checked again rather than waved through.
func (m *Manager) verifyIncoming(local, preview devicesync.Snapshot, seed []byte) map[string]error {
	known := make(map[string]struct{}, len(local.Accounts))
	for _, item := range local.Accounts {
		known[string(encodeJSON(devicesync.NormalizeAccount(item)))] = struct{}{}
	}
	// Checked against the keys the merge keeps: every variant of every
	// reference without a tombstone, this machine's own among them.
	keys := vaultKeys(preview.ImportedKeys)
	defer clearKeys(keys)
	verdicts := make(map[string]error)
	for _, item := range preview.Accounts {
		encoded := string(encodeJSON(item))
		if _, ok := known[encoded]; ok {
			continue
		}
		verdicts[encoded] = verifyIncomingAccount(item, seed, keys)
	}
	return verdicts
}

// verifyIncomingAccount is verifyAccount for a wallet another machine sent.
// A derived wallet must also carry no address its derivation does not make:
// verifyAccount checks the one its family uses, and an extra entry for another
// chain would be shown as the wallet's receive address on that chain. Wallets
// already on disk keep verifyAccount's rule alone — refusing an old record
// there would lock the space.
func verifyIncomingAccount(item account.Account, seed []byte, keys map[string]KeyVariants) error {
	if item.Kind == account.KindDerived {
		made := []account.Family{item.Family}
		if item.Family == "" {
			// Records from before network bindings carry both of these.
			made = []account.Family{account.FamilyTron, account.FamilyEVM}
		}
		for family := range item.Addresses {
			if !slices.Contains(made, family) {
				return fmt.Errorf("derived wallet carries an address for %s it is not derived for", family)
			}
		}
	}
	return verifyAccount(item, seed, keys)
}

// mergeResult is what a merge would make of a space: its next file and the
// vault's next keys and key tombstones. Nothing of it is kept yet.
type mergeResult struct {
	next        File
	keys        map[string]KeyVariants
	deletedKeys []devicesync.Tombstone
	// dropped are wallets the merge brought that the vault does not vouch for.
	dropped []string
	changed bool
}

// applyMergeLocked turns a merged snapshot into the space's next file and
// vault contents. A wallet is kept if it is here already, unchanged, or if the
// vault vouches for it; anything else is dropped and reported. Nothing is
// written.
//
// The keys are the merge's, whole: every variant of every reference without a
// tombstone. The merge is a union with this machine's own snapshot, so no key
// this vault holds is missing from it unless its reference was deleted.
func (m *Manager) applyMergeLocked(
	file File, payload *Payload, merged devicesync.Snapshot, verdicts map[string]error, seed []byte,
) (mergeResult, error) {
	result := mergeResult{keys: vaultKeys(merged.ImportedKeys), deletedKeys: merged.DeletedKeys}
	known := make(map[string]struct{}, len(file.Accounts))
	mine := make(map[string]account.Account, len(file.Accounts))
	for _, item := range file.Accounts {
		known[string(encodeJSON(devicesync.NormalizeAccount(item)))] = struct{}{}
		mine[item.ID] = item
	}
	next := file
	next.Accounts = make([]account.Account, 0, len(merged.Accounts))
	for _, item := range merged.Accounts {
		encoded := string(encodeJSON(item))
		if _, ok := known[encoded]; !ok {
			verdict, checked := verdicts[encoded]
			if !checked {
				verdict = verifyIncomingAccount(item, seed, result.keys)
			}
			if local, here := mine[item.ID]; verdict == nil && here && !sameIdentity(local, item) {
				// Checked against a key it brought along, a copy could carry the
				// wallet off to that key: this one would sign with it, and its own
				// would be left with no wallet. A wallet stays what it is.
				verdict = errors.New("another machine's copy changes which key or address the wallet is")
			}
			if verdict != nil {
				result.dropped = append(result.dropped, item.ID)
				// A newer copy that does not check out does not take the
				// wallet away: the copy here was checked when the space was
				// unlocked, and nothing deleted it.
				local, here := mine[item.ID]
				if !here {
					continue
				}
				item = local
			}
		}
		if item.Kind == account.KindImported {
			// Its key may have been tombstoned elsewhere. Variants are removed
			// with their reference only, so a reference that is still here
			// still holds the key the wallet was checked against.
			if _, ok := result.keys[item.KeyRef]; !ok {
				if !slices.Contains(result.dropped, item.ID) {
					result.dropped = append(result.dropped, item.ID)
				}
				continue
			}
		}
		next.Accounts = append(next.Accounts, item)
	}
	if len(next.Accounts) > maxAccountsPerSpace {
		clearKeys(result.keys)
		return mergeResult{}, fmt.Errorf(
			"%w: the merged space would have more than %d wallets", ErrQuotaExceeded, maxAccountsPerSpace)
	}
	// A name is taken only if this machine would accept it from the person:
	// space.json with a blank name is refused at start, and that refusal
	// stops the whole wallet, every space with it.
	if strings.TrimSpace(merged.Name) != "" {
		next.Name = merged.Name
		next.NameUpdatedAt = merged.NameUpdatedAt
	}
	next.DeletedAccounts = merged.DeletedAccounts

	result.changed = next.Name != file.Name || !next.NameUpdatedAt.Equal(file.NameUpdatedAt) ||
		// No wallets is no wallets, whether the file says null or [].
		(len(next.Accounts) > 0 || len(file.Accounts) > 0) && !bytes.Equal(encodeJSON(next.Accounts), encodeJSON(file.Accounts)) ||
		!slices.EqualFunc(next.DeletedAccounts, file.DeletedAccounts, sameTombstone) ||
		!slices.EqualFunc(result.deletedKeys, payload.DeletedKeys, sameTombstone) ||
		!sameKeys(result.keys, payload.ImportedKeys)
	if result.changed {
		next.UpdatedAt = m.now()
	}
	result.next = next
	return result, nil
}

// keepMergeLocked saves what a merge made of a space: the copy aside first if
// it removes something, then the file and the vault together. It refuses a
// result this machine could not write to the folder — past a snapshot's
// bounds, tombstones included — before anything is saved, so a pass that
// cannot finish leaves the space as it was.
func (m *Manager) keepMergeLocked(id string, active *session, result mergeResult) error {
	proposed := active.payload
	proposed.ImportedKeys, proposed.DeletedKeys = result.keys, result.deletedKeys
	check := snapshotOf(result.next, &proposed)
	err := devicesync.Sealable(check)
	check.Clear()
	if err != nil {
		return fmt.Errorf("the space cannot be synced as it would be after this merge: %w", err)
	}
	if err := m.keepPreviousLocked(m.files[id], result.next, &active.payload, result.keys); err != nil {
		return err
	}
	previousKeys, previousDeleted := active.payload.ImportedKeys, active.payload.DeletedKeys
	active.payload.ImportedKeys, active.payload.DeletedKeys = result.keys, result.deletedKeys
	next := result.next
	if err := m.resealAndSaveQuietLocked(&next, &active.payload, id); err != nil {
		active.payload.ImportedKeys, active.payload.DeletedKeys = previousKeys, previousDeleted
		return err
	}
	clearKeys(previousKeys)
	m.files[id] = next
	return nil
}

// sameIdentity reports whether two copies of one wallet are the same wallet.
// An imported wallet is its key and the addresses it receives on: the
// reference, the fingerprint and the addresses may not change between copies,
// or a copy that brought its own key along could move the wallet to it. A
// derived wallet is its place in the seed — derivation profile and index — and
// the curve its family signs on. Within a curve its family and addresses may
// change: binding a record from before network bindings (Tron and EVM) does
// both, and two machines binding or renaming it at once must converge; any
// copy's addresses are derived again from the seed before it is kept. A move
// to another curve — a Tron wallet arriving as the Solana wallet at its index
// — is a key no machine here would ever make it, and its own address would
// vanish from the list.
func sameIdentity(here, there account.Account) bool {
	if here.Kind != there.Kind {
		return false
	}
	switch here.Kind {
	case account.KindImported:
		return here.KeyRef == there.KeyRef && here.Fingerprint == there.Fingerprint &&
			maps.Equal(here.Addresses, there.Addresses)
	case account.KindDerived:
		return here.DerivationProfile == there.DerivationProfile &&
			(here.Index == nil) == (there.Index == nil) && (here.Index == nil || *here.Index == *there.Index) &&
			derivedCurve(here.Family) == derivedCurve(there.Family)
	default:
		return false
	}
}

// derivedCurve is the curve a derived wallet of family signs on. A record
// from before network bindings is a Tron and EVM wallet; a family no curve is
// known for is a group of its own.
func derivedCurve(family account.Family) string {
	if family == "" {
		return string(account.CurveSecp256k1)
	}
	curve, err := account.CurveOf(family)
	if err != nil {
		return "family:" + string(family)
	}
	return string(curve)
}

func sameTombstone(a, b devicesync.Tombstone) bool {
	return a.ID == b.ID && a.DeletedAt.Equal(b.DeletedAt)
}

func encodeJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func sameKeys(a, b map[string]KeyVariants) bool {
	if len(a) != len(b) {
		return false
	}
	for ref, x := range a {
		y, ok := b[ref]
		if !ok || !slices.EqualFunc(x, y, func(p, q KeyEntry) bool {
			return p.Curve == q.Curve && bytes.Equal(p.PrivateKey, q.PrivateKey) && p.ImportedAt.Equal(q.ImportedAt)
		}) {
			return false
		}
	}
	return true
}

// keepPreviousLocked copies the space file aside as space.json.prev before a
// merge takes a wallet or a key away. A merge removes things only on a
// tombstone, and a tombstone is a deliberate deletion somewhere — but this is
// the one place where data leaves the space without the person acting on this
// machine, and a copy costs nothing.
func (m *Manager) keepPreviousLocked(file, next File, payload *Payload, nextKeys map[string]KeyVariants) error {
	kept := make(map[string]struct{}, len(next.Accounts))
	for _, item := range next.Accounts {
		kept[item.ID] = struct{}{}
	}
	removes := false
	for _, item := range file.Accounts {
		if _, ok := kept[item.ID]; !ok {
			removes = true
			break
		}
	}
	for ref := range payload.ImportedKeys {
		if _, ok := nextKeys[ref]; !ok {
			removes = true
			break
		}
	}
	if !removes {
		return nil
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode previous space: %w", err)
	}
	return storage.AtomicWrite(m.previousPath(file.ID), append(data, '\n'))
}

func (m *Manager) previousPath(id string) string {
	return filepath.Join(m.home, "spaces", id, "space.json.prev")
}

// resealPrevious re-encrypts the copy keepPreviousLocked wrote under a new
// password, and returns it to be written with the space. The copy holds the
// whole vault as it was — phrase and keys — so left under the old password, a
// password changed because it leaked would still open it.
//
// It returns nil when there is no copy, or one that does not open with the
// current password: nothing here can re-seal that, and a password change is
// not refused over a safety copy.
func (m *Manager) resealPrevious(id, currentPassword, newPassword string) []byte {
	data, err := os.ReadFile(m.previousPath(id))
	if err != nil {
		return nil
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil
	}
	var payload Payload
	defer clearPayload(&payload)
	release := m.acquireKDF()
	err = vault.OpenJSON(currentPassword, file.Vault, aad(id), &payload)
	release()
	if err != nil {
		return nil
	}
	release = m.acquireKDF()
	file.Vault, err = vault.SealJSON(newPassword, payload, aad(id), m.params)
	release()
	if err != nil {
		return nil
	}
	resealed, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil
	}
	return append(resealed, '\n')
}
