package space

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/storage"
)

// Delivery (docs/device-sync.md, "Delivery"). A change to an open space is
// sealed, in the same call that saves it, into spaces/<id>/outbox.wsync —
// the very file this machine keeps in the folder — and sync.json records the
// revision it holds. Sending copies the outbox to the folder. It needs no
// key, so a change made a moment before the space locks still leaves, and a
// folder that is slow or gone never holds up a change or a lock.
//
// Every send runs under syncRun, like every pass, so sends of a space happen
// one after another, each with the outbox as it is when it starts: an older
// outbox is never written over a newer one. The outbox file and its record
// are written, and read for a send, under the manager's lock, so a send
// never acknowledges a revision it did not carry.

// ownCopy is what the folder holds under this machine's name in a space.
type ownCopy struct {
	listed bool
	// opaque: listed, but its bytes cannot be had — an evicted placeholder.
	opaque bool
	// blob is what it holds, when it was read; nil when the read failed.
	blob []byte
	// opened: a pass opened it and took it into its merge. newer: it is
	// sealed under the space's keys, and this build does not read what is
	// in it. damaged: it does not authenticate.
	opened, newer, damaged bool
	// merged: a pass merged it into the space, and the outbox was sealed
	// from the result — or it is damaged past recovery. Nothing in it is
	// lost by replacing it.
	merged bool
}

// fileSum names a sealed file in the delivery record.
func fileSum(blob []byte) string {
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func (m *Manager) outboxPath(id string) string {
	return filepath.Join(m.home, "spaces", id, "outbox.wsync")
}

// readOutboxTags reads which file an outbox was sealed for. ok is false when
// there is no outbox, or none that reads as one.
func (m *Manager) readOutboxTags(id string) (spaceTag, deviceTag string, ok bool) {
	file, err := os.Open(m.outboxPath(id))
	if err != nil {
		return "", "", false
	}
	defer file.Close()
	head := make([]byte, devicesync.EnvelopeHeaderLength)
	if _, err := io.ReadFull(file, head); err != nil {
		return "", "", false
	}
	return devicesync.EnvelopeTags(head)
}

// prepareOutboxLocked seals a change to an open space into its outbox, when
// sync is on. A failure is logged, not returned: the change itself is saved,
// the status shows it unsent, and the space's next pass seals it again.
func (m *Manager) prepareOutboxLocked(file File, payload *Payload) {
	s := m.currentSyncer()
	active := m.sessions[file.ID]
	if s == nil || active == nil || active.syncKeys == nil {
		return
	}
	if err := m.sealOutboxLocked(s, file, payload, active.syncKeys); err != nil {
		s.log.Warn("prepare sync outbox", "space_id", file.ID, "error", err)
	}
}

// ensureOutboxLocked seals the outbox unless it already holds this revision
// of the space, sealed for this machine's file.
func (m *Manager) ensureOutboxLocked(s *syncer, file File, payload *Payload, keys *devicesync.Keys) error {
	cache, _ := m.readSyncCache(file.ID)
	spaceTag, deviceTag, ok := m.readOutboxTags(file.ID)
	if ok && cache.DeviceID == s.deviceID && cache.OutboxRevision == file.LocalRevision &&
		spaceTag == keys.SpaceTag() && deviceTag == keys.DeviceTag(s.deviceID, file.ID) {
		return nil
	}
	return m.sealOutboxLocked(s, file, payload, keys)
}

// sealOutboxLocked writes the outbox, then its record. A crash between the
// two leaves a record older than the file, which only understates what is
// ready to send.
func (m *Manager) sealOutboxLocked(s *syncer, file File, payload *Payload, keys *devicesync.Keys) error {
	snap := snapshotOf(file, payload)
	defer snap.Clear()
	snap.WrittenAt = m.now()
	spaceTag, deviceTag := keys.SpaceTag(), keys.DeviceTag(s.deviceID, file.ID)
	blob, err := devicesync.Seal(keys, spaceTag, deviceTag, snap)
	if err != nil {
		return fmt.Errorf("seal snapshot: %w", err)
	}
	if err := storage.AtomicWrite(m.outboxPath(file.ID), blob); err != nil {
		return fmt.Errorf("write sync outbox: %w", err)
	}
	cache, _ := m.readSyncCache(file.ID)
	if cache.DeviceID != s.deviceID || cache.DeviceTag != deviceTag {
		// What was written before was another writer's file.
		cache = syncCache{}
	}
	cache.SpaceTag, cache.DeviceID, cache.DeviceTag = spaceTag, s.deviceID, deviceTag
	cache.OutboxRevision = file.LocalRevision
	if err := m.writeSyncCache(file.ID, cache); err != nil {
		return fmt.Errorf("record sync outbox: %w", err)
	}
	return nil
}

// deliver sends a space's outbox to this machine's file in the folder,
// unless the folder holds it already: compared byte for byte, so a file the
// folder lost is sent again whatever was recorded. seen is what a pass just
// found under this machine's name; with nil, deliver looks. The caller holds
// syncRun.
//
// This machine's file in the folder is an input like any other machine's,
// and is never replaced before it has been merged. It may hold what the
// outbox does not: the data folder was restored from a backup, and the file
// was written after the backup was taken — today's imported key in it, last
// night's outbox here. So it is replaced only when it is missing, when a
// pass has just merged it, or when it is the very file this machine last
// wrote there. Anything else waits for an unlock to merge it first.
func (m *Manager) deliver(ctx context.Context, s *syncer, id string, seen *ownCopy) error {
	m.mu.Lock()
	cache, _ := m.readSyncCache(id)
	blob, err := os.ReadFile(m.outboxPath(id))
	if errors.Is(err, os.ErrNotExist) {
		m.mu.Unlock()
		return nil
	}
	if err != nil {
		m.mu.Unlock()
		return m.sendFailed(s, id, fmt.Errorf("read sync outbox: %w", err))
	}
	spaceTag, deviceTag, ok := devicesync.EnvelopeTags(blob)
	if !ok || cache.DeviceID != s.deviceID || cache.SpaceTag != spaceTag || cache.DeviceTag != deviceTag {
		// Sealed by an installation this one was copied from — its device id
		// is not this one's — or not a sealed file at all. Sending it would
		// write another writer's file. The next unlock seals the space under
		// this machine's own tag.
		err := m.discardOutboxLocked(id, cache)
		m.mu.Unlock()
		return err
	}
	revision := cache.OutboxRevision
	m.mu.Unlock()

	own := seen
	if own == nil {
		if own, err = lookOwn(ctx, s.store, spaceTag, deviceTag); err != nil {
			return m.sendFailed(s, id, fmt.Errorf("list sync folder: %w", err))
		}
	}
	written := cache.Folder == s.folder && cache.WrittenRevision != nil
	switch {
	case own.listed && own.blob != nil && bytes.Equal(own.blob, blob):
		// Already there: written by an earlier run, or the record of the
		// write was lost.
		m.acknowledge(s, id, revision, fileSum(blob), false)
		return nil
	case own.opaque && written && *cache.WrittenRevision == revision:
		// Written, and listed: as far as can be told without fetching it.
		m.acknowledge(s, id, revision, "", false)
		return nil
	case !own.listed, own.merged,
		own.blob != nil && cache.Folder == s.folder && cache.WrittenSum != "" && cache.WrittenSum == fileSum(own.blob):
		// Missing, merged, or this machine's own last write: nothing in the
		// folder is lost by replacing it.
	default:
		// A version of this file the space may not hold: merge it first.
		if !own.opaque {
			m.forgetWritten(s, id)
		}
		reason := MergeFirstNotMerged
		switch {
		case own.newer:
			reason = MergeFirstNewerVersion
		case own.opaque || own.blob == nil:
			reason = MergeFirstNotDownloaded
		case own.opened:
			reason = MergeFirstFailed
		}
		m.setMergeFirst(s, id, reason)
		return nil
	}
	if err := s.store.Put(ctx, spaceTag, deviceTag, blob); err != nil {
		if !own.opaque {
			// The folder is known not to hold what was recorded as written.
			m.forgetWritten(s, id)
		}
		return m.sendFailed(s, id, fmt.Errorf("write sync file: %w", err))
	}
	m.acknowledge(s, id, revision, fileSum(blob), true)
	return nil
}

// setMergeFirst records that a space's file in the folder was left alone
// because it may hold what the space does not.
func (m *Manager) setMergeFirst(s *syncer, id, reason string) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	if m.sync != s {
		return
	}
	// Locked, the file cannot be opened to tell; what an open pass found
	// out about it still holds.
	if reason == MergeFirstNotMerged && m.mergeFirst[id] == MergeFirstNewerVersion {
		return
	}
	m.mergeFirst[id] = reason
}

// lookOwn finds this machine's file in a space's folder and reads it, unless
// it is an evicted placeholder.
func lookOwn(ctx context.Context, store devicesync.Store, spaceTag, deviceTag string) (*ownCopy, error) {
	entries, err := store.List(ctx, spaceTag)
	if err != nil {
		return nil, err
	}
	own := &ownCopy{}
	for _, entry := range entries {
		if entry.DeviceTag != deviceTag {
			continue
		}
		own.listed = true
		if entry.Evicted {
			own.opaque = true
			return own, nil
		}
		// A dataless file is read like any other: the read fetches it, and
		// what it holds decides whether it may be replaced.
		if blob, err := store.Get(ctx, spaceTag, deviceTag); err == nil {
			own.blob = blob
		} else if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return own, nil
	}
	return own, nil
}

// acknowledge records that the folder holds the outbox at revision — just
// written there, or found there. Only for the syncer sync runs with now: a
// folder that was let go is not the one the record is about.
//
// sum names the file the folder now holds ("" keeps the one recorded).
func (m *Manager) acknowledge(s *syncer, id string, revision uint64, sum string, wrote bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isCurrent(s) {
		return
	}
	m.syncMu.Lock()
	delete(m.sendErrors, id)
	delete(m.mergeFirst, id)
	m.syncMu.Unlock()
	cache, err := m.readSyncCache(id)
	if err != nil || cache.DeviceID != s.deviceID {
		// Discarded since it was read.
		return
	}
	if cache.Folder == s.folder && cache.WrittenRevision != nil && *cache.WrittenRevision == revision && !wrote &&
		(sum == "" || sum == cache.WrittenSum) {
		return
	}
	if cache.Folder != s.folder {
		cache.LastWrite, cache.WrittenSum = time.Time{}, ""
	}
	cache.Folder = s.folder
	cache.WrittenRevision = &revision
	if sum != "" {
		cache.WrittenSum = sum
	}
	if wrote {
		cache.LastWrite = m.now()
	}
	if err := m.writeSyncCache(id, cache); err != nil {
		s.log.Warn("record sync delivery", "space_id", id, "error", err)
	}
}

// forgetWritten drops the record of a write the folder no longer holds.
func (m *Manager) forgetWritten(s *syncer, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isCurrent(s) {
		return
	}
	cache, err := m.readSyncCache(id)
	if err != nil || cache.WrittenRevision == nil || cache.Folder != s.folder {
		return
	}
	cache.WrittenRevision = nil
	if err := m.writeSyncCache(id, cache); err != nil {
		s.log.Warn("record sync delivery", "space_id", id, "error", err)
	}
}

// discardOutboxLocked removes an outbox that is not this installation's, and
// every record of its delivery. The space's tag stays: it comes from the
// phrase, not from the installation.
func (m *Manager) discardOutboxLocked(id string, cache syncCache) error {
	if err := os.Remove(m.outboxPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("discard sync outbox: %w", err)
	}
	if err := m.writeSyncCache(id, syncCache{SpaceTag: cache.SpaceTag}); err != nil {
		return fmt.Errorf("discard sync outbox: %w", err)
	}
	return nil
}

// sendFailed keeps why a send failed for the status. The outbox stays: the
// next pass, tick or start sends it again.
func (m *Manager) sendFailed(s *syncer, id string, err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	m.syncMu.Lock()
	if m.sync == s {
		m.sendErrors[id] = err.Error()
	}
	m.syncMu.Unlock()
	return err
}

func (m *Manager) isCurrent(s *syncer) bool {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	return m.sync == s
}

// addDelivery fills in how far the space's latest change got.
func (m *Manager) addDelivery(s *syncer, id string, status *SyncStatus) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	file, ok := m.files[id]
	if !ok {
		return
	}
	active, open := m.sessions[id]
	cache, _ := m.readSyncCache(id)
	// An open space knows; a locked one is what it was when last open.
	noPhrase := cache.NoPhrase
	if open {
		noPhrase = active.syncKeys == nil
	}
	if noPhrase {
		// Nothing to deliver, and nothing an unlock would change.
		status.State = SyncNoPhrase
		return
	}
	if status.State == SyncNoPhrase && !open {
		// The last pass was before the space was given a phrase.
		status.State = SyncLocked
	}
	if status.State == SyncOK && status.SendError != "" {
		status.State, status.Error = SyncError, status.SendError
	}
	status.LocalRevision = file.LocalRevision
	status.LastWrite = time.Time{}
	prepared := false
	if cache.DeviceID == s.deviceID {
		status.OutboxRevision = cache.OutboxRevision
		spaceTag, deviceTag, ok := m.readOutboxTags(id)
		prepared = ok && spaceTag == cache.SpaceTag && deviceTag == cache.DeviceTag &&
			cache.OutboxRevision == file.LocalRevision
		if cache.Folder == s.folder && cache.WrittenRevision != nil {
			status.WrittenRevision = *cache.WrittenRevision
			status.LastWrite = cache.LastWrite
			if *cache.WrittenRevision == file.LocalRevision {
				status.Delivery = DeliveryUploadUnknown
				return
			}
		}
	}
	status.Delivery = DeliveryUnsent
	m.syncMu.Lock()
	if m.sync == s {
		status.MergeFirstReason = m.mergeFirst[id]
	}
	m.syncMu.Unlock()
	status.MergeFirst = status.MergeFirstReason != ""
	// An unlock does not help a file only a newer Walletspace reads.
	status.NeedsUnlock = !open &&
		(!prepared || status.MergeFirst && status.MergeFirstReason != MergeFirstNewerVersion)
}

// flushSync sends every outbox the folder does not hold yet, for at most
// wait: the wallet is shutting down, and must not wait on the folder. What
// does not leave now is sent at the next start.
func (m *Manager) flushSync(wait time.Duration) {
	s := m.currentSyncer()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, wait)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, id := range m.spaceIDs() {
			m.syncRun.Lock()
			if ctx.Err() == nil {
				_ = m.deliver(ctx, s, id, nil)
			}
			m.syncRun.Unlock()
			if ctx.Err() != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// missingOf counts the absent files that will not arrive by themselves: the
// folder cannot be asked for them, or a request for each failed
// downloadAttempts times in a row. It forgets the failures of files that are
// no longer absent.
func (m *Manager) missingOf(s *syncer, id string, absent []string) (int, string) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	if m.sync == s {
		for key := range m.downloadFailures {
			if space, tag, _ := strings.Cut(key, "/"); space == id && !slices.Contains(absent, tag) {
				delete(m.downloadFailures, key)
			}
		}
	}
	if _, ok := s.store.(devicesync.Downloader); !ok || s.noDownload.Load() {
		if len(absent) == 0 {
			return 0, ""
		}
		return len(absent), MissingCannotAsk
	}
	missing := 0
	for _, tag := range absent {
		if m.downloadFailures[id+"/"+tag] >= downloadAttempts {
			missing++
		}
	}
	if missing == 0 {
		return 0, ""
	}
	return missing, MissingRequestsFailed
}

// requestDownloads asks the provider for the files a pass found absent, in
// the background: a request may take seconds, and neither an unlock nor the
// next space waits for it. A space has one round of requests at a time, and
// a failed request fails nothing but itself.
func (m *Manager) requestDownloads(s *syncer, id string, pass passResult) {
	downloader, ok := s.store.(devicesync.Downloader)
	if !ok || len(pass.absent) == 0 || s.noDownload.Load() || s.ctx.Err() != nil {
		return
	}
	m.syncMu.Lock()
	if m.sync != s || s.asking[id] {
		m.syncMu.Unlock()
		return
	}
	s.asking[id] = true
	s.requests.Add(1)
	m.syncMu.Unlock()
	go func() {
		defer s.requests.Done()
		defer func() {
			m.syncMu.Lock()
			delete(s.asking, id)
			m.syncMu.Unlock()
		}()
		// Each request has its own bound, and the round only the syncer's:
		// a provider that answers every request slowly still gets each one
		// counted, so that its files do reach "missing".
		for _, tag := range pass.absent {
			ctx, cancel := context.WithTimeout(s.ctx, m.downloadTimeout)
			err := downloader.Download(ctx, pass.spaceTag, tag)
			cancel()
			if s.ctx.Err() != nil {
				return
			}
			if errors.Is(err, devicesync.ErrDownloadUnsupported) {
				s.noDownload.Store(true)
				break
			}
			m.syncMu.Lock()
			if m.sync == s {
				if err == nil {
					delete(m.downloadFailures, id+"/"+tag)
				} else {
					m.downloadFailures[id+"/"+tag]++
				}
			}
			m.syncMu.Unlock()
			if err != nil {
				s.log.Debug("request a sync file download", "space_id", id, "error", err)
			}
		}
		// The pass's status counted these before they were asked for.
		missing, reason := m.missingOf(s, id, pass.absent)
		m.syncMu.Lock()
		defer m.syncMu.Unlock()
		if status, ok := m.syncStatus[id]; ok && m.sync == s && status.LastSync.Equal(pass.at) {
			status.Missing, status.MissingReason = missing, reason
			m.syncStatus[id] = status
		}
	}()
}
