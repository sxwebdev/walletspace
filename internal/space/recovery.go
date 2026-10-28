package space

import (
	"errors"
	"fmt"

	"github.com/sxwebdev/walletspace/internal/account"
)

// ErrPhraseOfAnotherSpace refuses to give a space the phrase of another space
// on this machine. Both would sync through one folder and merge into one space
// for good.
var ErrPhraseOfAnotherSpace = errors.New("this recovery phrase already belongs to another space on this computer; adding it here would merge the two spaces for good")

// AddRecoveryPhrase gives a space made without one a recovery phrase, and
// returns it once for the person to write down.
//
// Spaces could once be created for imported keys only, with no phrase at all.
// Such a space had nothing to be restored from: its keys lived in one vault on
// one machine. The phrase added here is an ordinary BIP39 phrase. Nothing
// already in the space derives from it, but it is what sync keys are derived
// from, so from now on the phrase restores the space — imported keys included
// — wherever the space has synced.
//
// A space moved to several machines before it had a phrase must get the same
// phrase on all of them, or each copy syncs through a folder of its own and
// they never meet. So the phrase is generated on one machine, and on the others
// the person gives that phrase here instead of letting a new one be made.
//
// It writes a secret into the vault and hands it out, so it asks for the
// password again, like revealing a phrase does.
func (m *Manager) AddRecoveryPhrase(id, password, existing string) (string, error) {
	mnemonic := account.NormalizeMnemonic(existing)
	if mnemonic != "" {
		if err := account.ValidateMnemonic(mnemonic); err != nil {
			return "", err
		}
	}
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return "", err
	}
	if mnemonic == "" {
		generated, err := account.GenerateMnemonic()
		if err != nil {
			return "", err
		}
		mnemonic = generated
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return "", err
	}
	if len(payload.Mnemonic) > 0 {
		return "", ErrHasSeed
	}
	// Derived before the write, which asks sync for a pass: the pass has to
	// find the keys there.
	candidate := Payload{Mnemonic: []byte(mnemonic), BIP39Passphrase: payload.BIP39Passphrase}
	keys := deriveSyncKeys(&candidate)
	clear(candidate.Mnemonic)
	if keys != nil && m.spaceTagTakenLocked(id, keys.SpaceTag()) {
		keys.Clear()
		return "", ErrPhraseOfAnotherSpace
	}
	payload.Mnemonic = []byte(mnemonic)
	file.UpdatedAt = m.now()
	active := m.sessions[id]
	active.syncKeys = keys
	if err := m.resealAndSaveLocked(&file, payload, id); err != nil {
		// The session must keep matching what is on disk.
		clear(payload.Mnemonic)
		payload.Mnemonic = nil
		active.syncKeys.Clear()
		active.syncKeys = nil
		return "", fmt.Errorf("save recovery phrase: %w", err)
	}
	m.files[id] = file
	return mnemonic, nil
}

// spaceTagTakenLocked reports whether another space here syncs under tag: an
// unlocked one by its keys, a locked one by the tag its last pass left in
// sync.json. A locked space that has never written to a folder — sync off, or
// not yet passed — has left no tag, and cannot be told apart without its
// password.
func (m *Manager) spaceTagTakenLocked(id, tag string) bool {
	for other := range m.files {
		if other == id {
			continue
		}
		if active := m.sessions[other]; active != nil {
			if active.syncKeys != nil && active.syncKeys.SpaceTag() == tag {
				return true
			}
			continue
		}
		if cache, err := m.readSyncCache(other); err == nil && cache.SpaceTag == tag {
			return true
		}
	}
	return false
}
