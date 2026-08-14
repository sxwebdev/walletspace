// Package space implements encrypted wallet collections and their lock sessions.
package space

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
)

const (
	SchemaVersion = 1
	// A vault password faces an offline attack on a stolen backup, where the
	// only real defence is length.
	minPasswordLength = 12
)

var (
	ErrNotFound         = errors.New("space not found")
	ErrLocked           = errors.New("space is locked")
	ErrDuplicateKey     = errors.New("private key is already imported")
	ErrNoSeed           = errors.New("space does not have a mnemonic")
	ErrAccountNotFound  = errors.New("account not found")
	ErrNetworkBinding   = errors.New("account is not available for this network")
	ErrFirstSpaceExists = errors.New("the first space was already created")
	ErrWeakPassword     = errors.New("space password is too weak")
	// ErrPasswordRequired marks a step-up that was not attempted at all, as
	// opposed to one that was attempted with the wrong password.
	ErrPasswordRequired = errors.New("space password is required")
)

type KeyEntry struct {
	Curve      string    `json:"curve"`
	PrivateKey []byte    `json:"private_key"`
	ImportedAt time.Time `json:"imported_at"`
}

type Payload struct {
	Version         int                 `json:"version"`
	Mnemonic        []byte              `json:"mnemonic,omitempty"`
	BIP39Passphrase []byte              `json:"bip39_passphrase,omitempty"`
	ImportedKeys    map[string]KeyEntry `json:"imported_keys"`
	CreatedAt       time.Time           `json:"created_at"`
}

type File struct {
	SchemaVersion int               `json:"schema_version"`
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Accounts      []account.Account `json:"accounts"`
	Vault         vault.Container   `json:"vault"`
}

type Summary struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Locked       bool      `json:"locked"`
	Seeded       bool      `json:"seeded"`
	AccountCount int       `json:"account_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type CreateRequest struct {
	Name            string
	Mnemonic        string
	BIP39Passphrase string
	Password        string
	ImportedOnly    bool
	ExpectEmpty     bool
}

type CreateResult struct {
	Space             Summary
	Accounts          []account.Account
	Mnemonic          string
	MnemonicGenerated bool
}

type ImportResult struct {
	Account account.Account
}

type LegacyAccount struct {
	Index       uint32
	Label       string
	TronAddress string
}

type session struct {
	payload  Payload
	key      *vault.SessionKey
	lastUsed time.Time
	// sendGrant is when the password last entered to authorise spending stops
	// counting. It lives on the session rather than beside it so that anything
	// that ends the session takes the grant with it: locking the space by hand,
	// the idle timer, and a password change, which replaces the session rather
	// than refreshing it for exactly this reason.
	sendGrant time.Time
}

type Manager struct {
	home     string
	params   vault.Params
	autoLock time.Duration
	now      func() time.Time

	// confirmSends and sendGrantTTL are the spending step-up: whether moving
	// funds asks for the password, and how long one answer lasts. Guarded by mu
	// like the maps, because settings change while the wallet runs.
	confirmSends bool
	sendGrantTTL time.Duration

	mu       sync.RWMutex
	files    map[string]File
	sessions map[string]*session
	stop     chan struct{}
	done     chan struct{}
	reset    chan struct{}

	// spaceLocks serialises the password-checking operations per space, and
	// kdfSlots bounds how many derivations run at once across all of them.
	// Neither can be mu: mu is released across a derivation precisely so that
	// one unlock does not freeze every other space for the length of an Argon2
	// run. See throttle.go.
	spaceLocks sync.Map
	// unknownSpaceLock stands in for ids that are not space ids, so a request
	// naming one cannot add a permanent entry to spaceLocks.
	unknownSpaceLock sync.Mutex
	kdfSlots         chan struct{}
}

func NewManager(home string, autoLock time.Duration, params vault.Params) (*Manager, error) {
	if err := storage.EnsureHome(home); err != nil {
		return nil, err
	}
	if params == (vault.Params{}) {
		params = vault.DefaultParams
	}
	m := &Manager{
		home: home, params: params, autoLock: autoLock, now: func() time.Time { return time.Now().UTC() },
		files: make(map[string]File), sessions: make(map[string]*session),
		stop: make(chan struct{}), done: make(chan struct{}), reset: make(chan struct{}, 1),
		kdfSlots: newKDFSlots(),
		// On until told otherwise. A manager built without settings is one in a
		// test or an early start-up, and the safe reading of "not configured
		// yet" is the one that asks.
		confirmSends: true, sendGrantTTL: defaultSendGrantTTL,
	}
	if err := m.scan(); err != nil {
		return nil, err
	}
	go m.lockLoop()
	return m, nil
}

func (m *Manager) Close() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.sessions {
		m.clearSessionLocked(id)
	}
}

func (m *Manager) SetAutoLock(duration time.Duration) error {
	// Zero used to mean "never expire", which left a decrypted seed in memory
	// for the lifetime of the process.
	if duration <= 0 {
		return errors.New("auto-lock cannot be disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.autoLock = duration
	m.expireLocked()
	select {
	case m.reset <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) scan() error {
	entries, err := os.ReadDir(filepath.Join(m.home, "spaces"))
	if err != nil {
		return fmt.Errorf("scan spaces: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !storage.ValidID(entry.Name(), "spc_") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.home, "spaces", entry.Name(), "space.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read space %s: %w", entry.Name(), err)
		}
		var file File
		if err := json.Unmarshal(data, &file); err != nil {
			return fmt.Errorf("decode space %s: %w", entry.Name(), err)
		}
		if err := validateFile(file, entry.Name()); err != nil {
			return err
		}
		m.files[file.ID] = file
	}
	return nil
}

func validateFile(file File, directoryID string) error {
	if file.SchemaVersion != SchemaVersion {
		return fmt.Errorf("space %s uses unsupported schema version %d", directoryID, file.SchemaVersion)
	}
	if file.ID != directoryID || !storage.ValidID(file.ID, "spc_") {
		return fmt.Errorf("space directory and file id do not match")
	}
	if strings.TrimSpace(file.Name) == "" {
		return fmt.Errorf("space %s has an empty name", file.ID)
	}
	seen := make(map[string]struct{}, len(file.Accounts))
	for _, item := range file.Accounts {
		if !storage.ValidID(item.ID, "acc_") {
			return fmt.Errorf("space %s has invalid account id %q", file.ID, item.ID)
		}
		if _, exists := seen[item.ID]; exists {
			return fmt.Errorf("space %s has duplicate account id %q", file.ID, item.ID)
		}
		seen[item.ID] = struct{}{}
	}
	return nil
}

func (m *Manager) List() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	out := make([]Summary, 0, len(m.files))
	for _, file := range m.files {
		out = append(out, m.summaryLocked(file))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (m *Manager) Get(id string) (Summary, []account.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	file, ok := m.files[id]
	if !ok {
		return Summary{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return m.summaryLocked(file), cloneAccounts(file.Accounts), nil
}

func (m *Manager) Create(req CreateRequest) (CreateResult, error) {
	if err := validateNewPassword(req.Password); err != nil {
		return CreateResult{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "default"
	}
	mnemonic := account.NormalizeMnemonic(req.Mnemonic)
	generated := false
	if req.ImportedOnly {
		if mnemonic != "" {
			return CreateResult{}, errors.New("imported-only space cannot have a mnemonic")
		}
	} else if mnemonic == "" {
		var err error
		mnemonic, err = account.GenerateMnemonic()
		if err != nil {
			return CreateResult{}, err
		}
		generated = true
	} else if err := account.ValidateMnemonic(mnemonic); err != nil {
		return CreateResult{}, err
	}

	now := m.now()
	id, err := newID("spc_")
	if err != nil {
		return CreateResult{}, err
	}
	payload := Payload{
		Version: 1, Mnemonic: []byte(mnemonic), BIP39Passphrase: []byte(req.BIP39Passphrase),
		ImportedKeys: make(map[string]KeyEntry), CreatedAt: now,
	}
	// Checked before the two derivations rather than after, so a caller that has
	// already hit the ceiling cannot spend 128 MiB of Argon2 per request finding
	// that out.
	if err := m.checkSpaceQuota(); err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}
	file := File{SchemaVersion: SchemaVersion, ID: id, Name: name, CreatedAt: now, UpdatedAt: now}
	releaseKDF := m.acquireKDF()
	container, err := vault.SealJSON(req.Password, payload, aad(id), m.params)
	releaseKDF()
	if err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}
	file.Vault = container
	releaseKDF = m.acquireKDF()
	_, sessionKey, err := vault.Unlock(req.Password, container, aad(id))
	releaseKDF()
	if err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.spaceQuotaLocked(); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return CreateResult{}, err
	}
	if req.ExpectEmpty && len(m.files) != 0 {
		sessionKey.Clear()
		clearPayload(&payload)
		return CreateResult{}, ErrFirstSpaceExists
	}
	if err := m.saveLocked(file); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return CreateResult{}, err
	}
	m.files[id] = file
	m.sessions[id] = &session{payload: payload, key: sessionKey, lastUsed: now}
	return CreateResult{
		Space: m.summaryLocked(file), Accounts: cloneAccounts(file.Accounts),
		Mnemonic: mnemonic, MnemonicGenerated: generated,
	}, nil
}

// ImportLegacy verifies every legacy Tron address and publishes one complete
// encrypted space. The legacy directory is never changed.
func (m *Manager) ImportLegacy(req CreateRequest, legacy []LegacyAccount) (CreateResult, error) {
	if err := validateNewPassword(req.Password); err != nil {
		return CreateResult{}, err
	}
	mnemonic := account.NormalizeMnemonic(req.Mnemonic)
	verified, err := validateLegacy(mnemonic, req.BIP39Passphrase, legacy)
	if err != nil {
		return CreateResult{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "default"
	}
	now := m.now()
	id, err := newID("spc_")
	if err != nil {
		return CreateResult{}, err
	}
	payload := Payload{
		Version: 1, Mnemonic: []byte(mnemonic), BIP39Passphrase: []byte(req.BIP39Passphrase),
		ImportedKeys: make(map[string]KeyEntry), CreatedAt: now,
	}
	file := File{
		SchemaVersion: SchemaVersion, ID: id, Name: name, CreatedAt: now, UpdatedAt: now,
		Accounts: make([]account.Account, 0, len(legacy)),
	}
	for _, old := range legacy {
		addresses := verified[old.Index]
		accountID, err := newID("acc_")
		if err != nil {
			clearPayload(&payload)
			return CreateResult{}, err
		}
		index := old.Index
		file.Accounts = append(file.Accounts, account.Account{
			ID: accountID, Label: old.Label, Kind: account.KindDerived, Addresses: addresses,
			Index: &index, DerivationProfile: "bip44-v1", CreatedAt: now, UpdatedAt: now,
		})
	}
	sort.Slice(file.Accounts, func(i, j int) bool { return *file.Accounts[i].Index < *file.Accounts[j].Index })
	container, err := vault.SealJSON(req.Password, payload, aad(id), m.params)
	if err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}
	file.Vault = container
	_, sessionKey, err := vault.Unlock(req.Password, container, aad(id))
	if err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.saveLocked(file); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return CreateResult{}, err
	}
	m.files[id] = file
	m.sessions[id] = &session{payload: payload, key: sessionKey, lastUsed: now}
	return CreateResult{
		Space: m.summaryLocked(file), Accounts: cloneAccounts(file.Accounts),
	}, nil
}

// ValidateLegacy performs the complete, read-only part of legacy migration.
// It is used by the CLI dry-run before a target home or vault is created.
func ValidateLegacy(mnemonic, passphrase string, legacy []LegacyAccount) error {
	_, err := validateLegacy(account.NormalizeMnemonic(mnemonic), passphrase, legacy)
	return err
}

func validateLegacy(
	mnemonic,
	passphrase string,
	legacy []LegacyAccount,
) (map[uint32]map[account.Family]string, error) {
	if err := account.ValidateMnemonic(mnemonic); err != nil {
		return nil, err
	}
	if len(legacy) == 0 {
		return nil, errors.New("legacy wallet list is empty")
	}
	verified := make(map[uint32]map[account.Family]string, len(legacy))
	for _, old := range legacy {
		if _, exists := verified[old.Index]; exists {
			return nil, fmt.Errorf("duplicate legacy derivation index %d", old.Index)
		}
		addresses, err := account.DerivedAddresses(mnemonic, passphrase, old.Index)
		if err != nil {
			return nil, err
		}
		if addresses[account.FamilyTron] != old.TronAddress {
			return nil, fmt.Errorf(
				"legacy address mismatch at index %d: derived %s, file contains %s",
				old.Index, addresses[account.FamilyTron], old.TronAddress,
			)
		}
		verified[old.Index] = addresses
	}
	return verified, nil
}

// Unlock derives the vault key and opens a session.
//
// The derivation is the expensive part and runs outside the manager's mutex:
// holding it across a 64 MiB Argon2 pass made one unlock block every other
// space, the space list and the auto-lock sweep. The per-space lock keeps two
// attempts on the same space in order, and it is also what stops a password
// change from replacing the container midway through this derivation.
func (m *Manager) Unlock(id, password string) error {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()

	m.mu.RLock()
	file, ok := m.files[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := m.checkUnlockCooldown(id); err != nil {
		return err
	}

	releaseKDF := m.acquireKDF()
	var payload Payload
	sessionKey, err := vault.UnlockJSON(password, file.Vault, aad(id), &payload)
	releaseKDF()
	if err != nil {
		if errors.Is(err, vault.ErrInvalidPassword) {
			m.recordUnlockFailure(id)
		}
		return err
	}
	if err := verifyPayload(file, payload); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return err
	}
	normalizePayload(&payload)
	m.clearUnlockFailures(id)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.clearSessionLocked(id)
	m.sessions[id] = &session{payload: payload, key: sessionKey, lastUsed: m.now()}
	return nil
}

func (m *Manager) Lock(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	m.clearSessionLocked(id)
	return nil
}

func (m *Manager) Rename(id, name string) (Summary, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Summary{}, errors.New("space name is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return Summary{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	file.Name = name
	file.UpdatedAt = m.now()
	if err := m.saveLocked(file); err != nil {
		return Summary{}, err
	}
	m.files[id] = file
	return m.summaryLocked(file), nil
}

// ChangePassword re-encrypts the vault under a new password.
//
// Three derivations happen here — opening the old container, sealing the new
// one and re-deriving the live session key — and all three run outside the
// manager's mutex. Under the old arrangement this was the single longest lock
// hold in the process. The per-space lock is what keeps the container this
// reads from being the one it writes back over.
func (m *Manager) ChangePassword(id, currentPassword, newPassword string) error {
	if err := validateNewPassword(newPassword); err != nil {
		return err
	}
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()

	m.mu.RLock()
	file, ok := m.files[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := m.checkUnlockCooldown(id); err != nil {
		return err
	}

	releaseKDF := m.acquireKDF()
	var payload Payload
	err := vault.OpenJSON(currentPassword, file.Vault, aad(id), &payload)
	releaseKDF()
	if err != nil {
		if errors.Is(err, vault.ErrInvalidPassword) {
			m.recordUnlockFailure(id)
		}
		return err
	}
	defer clearPayload(&payload)
	if err := verifyPayload(file, payload); err != nil {
		return err
	}
	normalizePayload(&payload)
	m.clearUnlockFailures(id)

	releaseKDF = m.acquireKDF()
	container, err := vault.SealJSON(newPassword, payload, aad(id), m.params)
	releaseKDF()
	if err != nil {
		return err
	}

	// An open session has to keep working under the new password, which means a
	// third derivation. It happens here rather than under the mutex, and only
	// when there is a session to refresh. The per-space lock is what makes the
	// check outside the mutex sound: nothing else can open or close a session
	// for this space while this call is in progress.
	var refreshed Payload
	var sessionKey *vault.SessionKey
	m.mu.RLock()
	hadSession := m.sessions[id] != nil
	m.mu.RUnlock()
	if hadSession {
		releaseKDF = m.acquireKDF()
		sessionKey, err = vault.UnlockJSON(newPassword, container, aad(id), &refreshed)
		releaseKDF()
		if err != nil {
			clearPayload(&refreshed)
			return err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok = m.files[id]
	if !ok {
		sessionKey.Clear()
		clearPayload(&refreshed)
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	file.Vault = container
	file.UpdatedAt = m.now()
	if err := m.saveLocked(file); err != nil {
		sessionKey.Clear()
		clearPayload(&refreshed)
		return err
	}
	m.files[id] = file
	// The session is replaced rather than refreshed in place. Swapping the key
	// and the payload left everything else on it standing, including a spending
	// grant opened under the password that was just replaced — so changing the
	// password did not end the window it had bought, and a transfer went through
	// on the strength of a secret that no longer opens the vault. Unlock is safe
	// from this only because it allocates a fresh session; this now does the
	// same on purpose.
	if active := m.sessions[id]; active != nil && sessionKey != nil {
		m.clearSessionLocked(id)
		m.sessions[id] = &session{payload: refreshed, key: sessionKey, lastUsed: m.now()}
	} else {
		sessionKey.Clear()
		clearPayload(&refreshed)
	}
	return nil
}

// commonPasswords are the ones that turn up first in every cracking wordlist.
//
// This is not a breach corpus and cannot be: shipping one would mean shipping
// megabytes, and checking against a hosted one would send a hash of the user's
// password off the machine, which this wallet will not do. It catches the
// handful that an offline attack on a stolen backup tries in its first second.
var commonPasswords = map[string]struct{}{
	"password": {}, "password1": {}, "password123": {}, "passw0rd": {},
	"12345678": {}, "123456789": {}, "1234567890": {}, "qwertyuiop": {},
	"letmein": {}, "welcome1": {}, "iloveyou": {}, "admin123": {},
	"walletspace": {}, "changeme": {}, "secret123": {}, "trustno1": {},
	"correcthorsebatterystaple": {},
}

// validateNewPassword bounds how weak a vault password may be.
//
// The password is the only thing between a copy of the encrypted backup and the
// seed inside it, and an offline attacker gets unlimited attempts at it. Length
// is what buys time against that, so it carries most of the weight here — a
// passphrase of several words beats a short string with punctuation in it.
func validateNewPassword(password string) error {
	if password == "" {
		return errors.New("space password is required")
	}
	if utf8.RuneCountInString(password) < minPasswordLength {
		return fmt.Errorf(
			"%w: use at least %d characters — a few unrelated words are easier to remember "+
				"and much harder to guess than a short password, and a password manager is better still",
			ErrWeakPassword, minPasswordLength,
		)
	}
	folded := strings.ToLower(strings.TrimSpace(password))
	if _, common := commonPasswords[folded]; common {
		return fmt.Errorf("%w: this is one of the first passwords an attacker tries", ErrWeakPassword)
	}
	if isSingleRepeatedRune(password) {
		return fmt.Errorf("%w: it is the same character repeated", ErrWeakPassword)
	}

	return nil
}

func isSingleRepeatedRune(value string) bool {
	var first rune
	for i, r := range value {
		if i == 0 {
			first = r
			continue
		}
		if r != first {
			return false
		}
	}

	return true
}

func (m *Manager) Derive(
	id, networkID string,
	family account.Family,
	label string,
) (account.Account, error) {
	if networkID == "" {
		return account.Account{}, errors.New("network is required")
	}
	if family != account.FamilyTron && family != account.FamilyEVM {
		return account.Account{}, fmt.Errorf("%w: %s", account.ErrUnsupportedFamily, family)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return account.Account{}, err
	}
	if len(payload.Mnemonic) == 0 {
		return account.Account{}, ErrNoSeed
	}
	used := make(map[uint32]struct{})
	for _, item := range file.Accounts {
		if item.Kind == account.KindDerived && item.BoundTo(networkID) && item.Index != nil {
			used[*item.Index] = struct{}{}
		}
	}
	var next uint32
	for {
		if _, exists := used[next]; !exists {
			break
		}
		if next == ^uint32(0) {
			return account.Account{}, errors.New("derivation index exhausted")
		}
		next++
	}
	// BIP44 has a family coin type but no standard per-network component.
	// Thus index 0 on two EVM networks is the same key. Reuse the wallet and
	// add a binding instead of persisting duplicate key identities.
	for i := range file.Accounts {
		item := &file.Accounts[i]
		if item.Kind != account.KindDerived || item.Family != family ||
			item.Index == nil || *item.Index != next {
			continue
		}
		item.NetworkIDs = appendNetwork(item.NetworkIDs, networkID)
		if item.Label == "" {
			item.Label = strings.TrimSpace(label)
		}
		item.UpdatedAt = m.now()
		file.UpdatedAt = item.UpdatedAt
		if err := m.saveLocked(file); err != nil {
			return account.Account{}, err
		}
		m.files[id] = file
		return cloneAccount(*item), nil
	}
	address, err := account.DerivedAddress(
		string(payload.Mnemonic), string(payload.BIP39Passphrase), family, next,
	)
	if err != nil {
		return account.Account{}, err
	}
	if err := accountQuotaLocked(file); err != nil {
		return account.Account{}, err
	}
	accountID, err := newID("acc_")
	if err != nil {
		return account.Account{}, err
	}
	now := m.now()
	created := account.Account{
		ID: accountID, Label: strings.TrimSpace(label), Kind: account.KindDerived,
		Family: family, NetworkIDs: []string{networkID},
		Addresses: map[account.Family]string{family: address},
		Index:     &next, DerivationProfile: "bip44-v1",
		CreatedAt: now, UpdatedAt: now,
	}
	file.Accounts = append(file.Accounts, created)
	file.UpdatedAt = now
	if err := m.saveLocked(file); err != nil {
		return account.Account{}, err
	}
	m.files[id] = file
	return cloneAccount(created), nil
}

func (m *Manager) Import(
	id, networkID string,
	family account.Family,
	label, privateKey string,
) (ImportResult, error) {
	if networkID == "" {
		return ImportResult{}, errors.New("network is required")
	}
	if family != account.FamilyTron && family != account.FamilyEVM {
		return ImportResult{}, fmt.Errorf("%w: %s", account.ErrUnsupportedFamily, family)
	}
	key, raw, err := account.ParsePrivateKey(privateKey)
	if err != nil {
		return ImportResult{}, err
	}
	defer clear(raw)
	fingerprint, err := account.Fingerprint(key)
	if err != nil {
		return ImportResult{}, err
	}
	addresses, err := account.ImportedAddresses(key)
	if err != nil {
		return ImportResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return ImportResult{}, err
	}
	for i := range file.Accounts {
		item := &file.Accounts[i]
		if item.Kind == account.KindImported && item.Fingerprint == fingerprint {
			if item.BoundTo(networkID) {
				return ImportResult{}, ErrDuplicateKey
			}
			item.NetworkIDs = appendNetwork(item.NetworkIDs, networkID)
			if item.Label == "" {
				item.Label = strings.TrimSpace(label)
			}
			item.UpdatedAt = m.now()
			file.UpdatedAt = item.UpdatedAt
			if err := m.saveLocked(file); err != nil {
				return ImportResult{}, err
			}
			m.files[id] = file
			return ImportResult{Account: cloneAccount(*item)}, nil
		}
	}
	if err := accountQuotaLocked(file); err != nil {
		return ImportResult{}, err
	}
	keyRef, err := newID("key_")
	if err != nil {
		return ImportResult{}, err
	}
	accountID, err := newID("acc_")
	if err != nil {
		return ImportResult{}, err
	}
	now := m.now()
	importedAt := now
	created := account.Account{
		ID: accountID, Label: strings.TrimSpace(label), Kind: account.KindImported,
		NetworkIDs: []string{networkID}, Addresses: addresses,
		KeyRef: keyRef, Fingerprint: fingerprint, ImportedAt: &importedAt,
		CreatedAt: now, UpdatedAt: now,
	}
	payload.ImportedKeys[keyRef] = KeyEntry{
		Curve: "secp256k1", PrivateKey: append([]byte(nil), raw...), ImportedAt: now,
	}
	file.Accounts = append(file.Accounts, created)
	file.UpdatedAt = now
	if err := m.resealAndSaveLocked(&file, payload, id); err != nil {
		delete(payload.ImportedKeys, keyRef)
		return ImportResult{}, err
	}
	m.files[id] = file
	return ImportResult{Account: cloneAccount(created)}, nil
}

// ExportPrivateKey hands over permanent control of one account, so it asks for
// the space password again rather than trusting the open session.
func (m *Manager) ExportPrivateKey(
	id, accountID string, family account.Family, password string,
) (string, error) {
	if family != account.FamilyTron && family != account.FamilyEVM {
		return "", fmt.Errorf("%w: %s", account.ErrUnsupportedFamily, family)
	}
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return "", err
	}
	item, err := findAccount(file.Accounts, accountID)
	if err != nil {
		return "", err
	}
	var keyBytes []byte
	switch item.Kind {
	case account.KindDerived:
		if item.Index == nil {
			return "", errors.New("derived account has no index")
		}
		if item.Family != "" && item.Family != family {
			return "", ErrNetworkBinding
		}
		key, err := account.DerivePrivateKey(string(payload.Mnemonic), string(payload.BIP39Passphrase), family, *item.Index)
		if err != nil {
			return "", err
		}
		keyBytes = crypto.FromECDSA(key)
	case account.KindImported:
		entry, ok := payload.ImportedKeys[item.KeyRef]
		if !ok {
			return "", errors.New("imported key is missing from vault")
		}
		keyBytes = append([]byte(nil), entry.PrivateKey...)
	default:
		return "", errors.New("unknown account kind")
	}
	defer clear(keyBytes)
	return hex.EncodeToString(keyBytes), nil
}

func (m *Manager) WithSigner(
	ctx context.Context,
	id, accountID, networkID string,
	family account.Family,
	fn func(chain.Signer) error,
) error {
	if fn == nil {
		return errors.New("signer callback is required")
	}
	m.mu.Lock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	item, err := findAccount(file.Accounts, accountID)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !item.BoundTo(networkID) {
		m.mu.Unlock()
		return ErrNetworkBinding
	}
	key, err := privateKeyFor(item, payload, family)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	signer := &localSigner{key: key, family: family}
	defer signer.clear()
	return fn(signer)
}

// Mnemonic reveals the seed every account in the space derives from, so it
// asks for the space password again rather than trusting the open session.
func (m *Manager) Mnemonic(id, password string) (string, error) {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, payload, err := m.unlockedLocked(id)
	if err != nil {
		return "", err
	}
	if len(payload.Mnemonic) == 0 {
		return "", ErrNoSeed
	}
	return string(payload.Mnemonic), nil
}

// defaultSendGrantTTL is what a manager uses until it is told otherwise. The
// bounds on a configured value are policy and live with the settings, exactly
// as the auto-lock range does.
const defaultSendGrantTTL = 5 * time.Minute

// ErrSendConfirmationRequired reports that spending needs the password again.
//
// It is deliberately distinct from ErrPasswordRequired: the caller is not being
// told that it sent a bad request, it is being told to ask the person at the
// keyboard and come back. The UI turns exactly this into a prompt.
var ErrSendConfirmationRequired = errors.New("confirm this transfer with the space password")

// SetSendConfirmation applies the spending step-up settings.
//
// It governs the windows opened after it, not the ones already outstanding: a
// grant is an absolute deadline fixed when the password was entered, so
// shortening the window leaves a live grant where it is. Turning the step-up
// off, meanwhile, is not reachable from the API at all — the setting is read
// from config.yaml at start, because a caller able to switch it off would have
// no need of the password it asks for.
func (m *Manager) SetSendConfirmation(enabled bool, ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultSendGrantTTL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.confirmSends = enabled
	m.sendGrantTTL = ttl
}

// ConfirmSend checks the password and opens the spending window.
//
// It goes through the same step-up as the exports, which means the same
// throttle, the same KDF semaphore and the same per-space lock — otherwise this
// would be a new unthrottled place to guess a password, which is precisely the
// defect the export step-up shipped with.
//
// The context is the caller's, and it is consulted once: after the password has
// been proven and before the window is written. That side of the derivation is
// deliberate. The dialog can be dismissed while this call is inside a
// derivation that takes the better part of a second, and the browser then tells
// the person nothing was confirmed — so the one thing a cancellation has to
// change is whether the window exists, and the only place that can be decided
// is here. Consulting it earlier would make something else depend on when the
// caller hung up: the verdict on the password, and with it the cooldown. A
// cancelled attempt therefore counts exactly as it would have counted had
// nobody hung up — a wrong password still costs a failure, a right one still
// clears the record — because a guess that can be taken back by closing the
// connection is a guess that is free.
func (m *Manager) ConfirmSend(ctx context.Context, id, password string) (time.Time, error) {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return time.Time{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Under the mutex, so nothing can slip between the decision and the write.
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	// Only for a space that is open. A grant on a locked space would sit there
	// waiting to be used by whoever unlocks it next — and "open" has to mean
	// what it means to the signer, so the idle sweep runs first: the derivation
	// above happens outside the mutex and takes long enough for a session to
	// reach its deadline while it runs.
	m.expireLocked()
	active := m.sessions[id]
	if active == nil {
		return time.Time{}, fmt.Errorf("%w: %s", ErrLocked, id)
	}
	now := m.now()
	// Entering the space password is the strongest evidence of presence this
	// wallet has, so it counts as use of the session. Without it, someone who
	// unlocked fourteen minutes ago and typed the password to authorise a
	// transfer could have the session swept out from under that transfer
	// seconds later. It makes six actions refresh the idle timer rather than
	// the five that read the decrypted payload — and the sixth is a password,
	// which is a stronger claim to be present than any of the other five.
	active.lastUsed = now
	active.sendGrant = now.Add(m.sendGrantTTL)
	return active.sendGrant, nil
}

// RequireSendConfirmation is the check every signing path makes before it
// spends anything.
//
// Without it the wallet's own rules do not line up: revealing a key asks for
// the password, and sending the funds that key controls does not — so anything
// that reaches the API of an unlocked space cannot steal the seed but can move
// everything it protects, one transfer at a time.
func (m *Manager) RequireSendConfirmation(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The idle sweep runs first, as it does in every other method that reads
	// m.sessions. The background sweep runs at most once a minute, so reading
	// the map as it stands let the gate answer "authorised" for up to that long
	// from a session whose auto-lock deadline had already passed: the caller was
	// waved through and the signing path then refused it with a locked-space
	// error the UI has no prompt for. Sweeping here turns that into the 423 the
	// UI does understand, and costs the write lock the sweep needs.
	//
	// It deliberately does not touch lastUsed. This is a check, reachable by
	// anything holding the capability token, and letting it refresh the idle
	// timer would hand that caller a way to keep a space unlocked indefinitely
	// by polling the gate it cannot pass.
	m.expireLocked()
	if !m.confirmSends {
		return nil
	}
	active := m.sessions[id]
	if active == nil {
		return fmt.Errorf("%w: %s", ErrLocked, id)
	}
	if active.sendGrant.IsZero() || !m.now().Before(active.sendGrant) {
		return ErrSendConfirmationRequired
	}
	return nil
}

// Backup hands over the space file, vault container and all, so it asks for the
// space password again — the same step-up the seed and private-key exports use.
//
// What leaves here is every secret in the space in a form that can be worked on
// at leisure and without a rate limit, which makes it worth more to an attacker
// than a single exported key. It is also the one export that does not need the
// space to be unlocked first, so before this it was the cheapest thing for a
// caller holding the capability token to take.
func (m *Manager) Backup(id, password string) ([]byte, error) {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	file, ok := m.files[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode backup: %w", err)
	}
	return append(data, '\n'), nil
}

func (m *Manager) Accounts(id string) ([]account.Account, error) {
	_, accounts, err := m.Get(id)
	return accounts, err
}

// BindNetwork explicitly makes an existing wallet available on a network.
// Derived wallets can only be bound to networks from their derivation family;
// imported secp256k1 keys can be bound to either supported family.
func (m *Manager) BindNetwork(
	id, accountID, networkID string,
	family account.Family,
) (account.Account, error) {
	if networkID == "" {
		return account.Account{}, errors.New("network is required")
	}
	if family != account.FamilyTron && family != account.FamilyEVM {
		return account.Account{}, fmt.Errorf("%w: %s", account.ErrUnsupportedFamily, family)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return account.Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	file.Accounts = cloneAccounts(file.Accounts)
	for i := range file.Accounts {
		item := &file.Accounts[i]
		if item.ID != accountID {
			continue
		}
		if item.Kind == account.KindDerived {
			if item.Family == "" {
				// Safe migration for pre-binding records: the user chooses the
				// original network, then the obsolete other-family projection
				// is removed from this wallet.
				if item.Addresses[family] == "" {
					return account.Account{}, ErrNetworkBinding
				}
				item.Family = family
				item.Addresses = map[account.Family]string{family: item.Addresses[family]}
			} else if item.Family != family {
				return account.Account{}, ErrNetworkBinding
			}
		}
		if item.Addresses[family] == "" {
			return account.Account{}, ErrNetworkBinding
		}
		item.NetworkIDs = appendNetwork(item.NetworkIDs, networkID)
		item.UpdatedAt = m.now()
		file.UpdatedAt = item.UpdatedAt
		if err := m.saveLocked(file); err != nil {
			return account.Account{}, err
		}
		m.files[id] = file
		return cloneAccount(*item), nil
	}
	return account.Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
}

func (m *Manager) RenameAccount(id, accountID, label string) (account.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return account.Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	file.Accounts = cloneAccounts(file.Accounts)
	for i := range file.Accounts {
		if file.Accounts[i].ID != accountID {
			continue
		}
		file.Accounts[i].Label = strings.TrimSpace(label)
		file.Accounts[i].UpdatedAt = m.now()
		file.UpdatedAt = m.now()
		if err := m.saveLocked(file); err != nil {
			return account.Account{}, err
		}
		m.files[id] = file
		return cloneAccount(file.Accounts[i]), nil
	}
	return account.Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
}

// confirmPassword re-derives the vault key from a password the caller just
// supplied, without disturbing the open session.
//
// Exporting a seed or a private key hands over permanent control of the funds,
// and an unlocked space is not evidence that the person asking is the owner: a
// tab left open, a same-origin script or any local client with the capability
// token all inherit that state. Re-entering the password is the one thing none
// of them can do, and it costs a full KDF — which is the point.
//
// That also makes this a password check reachable from the API, so it goes
// through the same throttle, semaphore and per-space lock as Unlock. Leaving it
// outside them would have left an unthrottled guessing oracle beside a
// throttled one, and callers would have paid a 64 MiB derivation while holding
// the manager's write mutex.
//
// The caller must NOT hold m.mu.
func (m *Manager) confirmPassword(id, password string) error {
	if password == "" {
		return fmt.Errorf("%w: the space password is required to reveal a secret", ErrPasswordRequired)
	}
	m.mu.RLock()
	file, ok := m.files[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := m.checkUnlockCooldown(id); err != nil {
		return err
	}

	releaseKDF := m.acquireKDF()
	var payload Payload
	key, err := vault.UnlockJSON(password, file.Vault, aad(id), &payload)
	releaseKDF()
	if err != nil {
		if errors.Is(err, vault.ErrInvalidPassword) {
			m.recordUnlockFailure(id)
		}
		return err
	}
	key.Clear()
	clearPayload(&payload)
	m.clearUnlockFailures(id)

	return nil
}

func (m *Manager) unlockedLocked(id string) (File, *Payload, error) {
	m.expireLocked()
	file, ok := m.files[id]
	if !ok {
		return File{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	active := m.sessions[id]
	if active == nil {
		return File{}, nil, ErrLocked
	}
	active.lastUsed = m.now()
	// File is a value, but its Accounts slice and the maps inside each account
	// otherwise still alias the authoritative entry in m.files. Mutating a
	// shallow copy before an AtomicWrite failure would publish an in-memory
	// state that was never persisted.
	file.Accounts = cloneAccounts(file.Accounts)
	return file, &active.payload, nil
}

func (m *Manager) resealAndSaveLocked(file *File, payload *Payload, id string) error {
	active := m.sessions[id]
	if active == nil || active.key == nil {
		return ErrLocked
	}
	container, err := active.key.SealJSON(payload, aad(id))
	if err != nil {
		return err
	}
	file.Vault = container
	return m.saveLocked(*file)
}

func (m *Manager) saveLocked(file File) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode space: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(m.home, "spaces", file.ID, "space.json")
	return storage.AtomicWrite(path, data)
}

func (m *Manager) summaryLocked(file File) Summary {
	_, unlocked := m.sessions[file.ID]
	seeded := false
	for _, item := range file.Accounts {
		if item.Kind == account.KindDerived {
			seeded = true
			break
		}
	}
	return Summary{
		ID: file.ID, Name: file.Name, Locked: !unlocked, Seeded: seeded,
		AccountCount: len(file.Accounts), CreatedAt: file.CreatedAt, UpdatedAt: file.UpdatedAt,
	}
}

func (m *Manager) expireLocked() {
	if m.autoLock <= 0 {
		return
	}
	now := m.now()
	for id, active := range m.sessions {
		if now.Sub(active.lastUsed) >= m.autoLock {
			m.clearSessionLocked(id)
		}
	}
}

func (m *Manager) lockLoop() {
	defer close(m.done)
	timer := time.NewTimer(m.lockInterval())
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			m.mu.Lock()
			m.expireLocked()
			interval := m.lockIntervalLocked()
			m.mu.Unlock()
			timer.Reset(interval)
		case <-m.reset:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(m.lockInterval())
		case <-m.stop:
			return
		}
	}
}

func (m *Manager) lockInterval() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lockIntervalLocked()
}

func (m *Manager) lockIntervalLocked() time.Duration {
	if m.autoLock > 0 && m.autoLock < time.Minute {
		return m.autoLock
	}
	return time.Minute
}

func (m *Manager) clearSessionLocked(id string) {
	active := m.sessions[id]
	if active == nil {
		return
	}
	clearPayload(&active.payload)
	active.key.Clear()
	delete(m.sessions, id)
}

func clearPayload(payload *Payload) {
	clear(payload.Mnemonic)
	clear(payload.BIP39Passphrase)
	for key, entry := range payload.ImportedKeys {
		clear(entry.PrivateKey)
		delete(payload.ImportedKeys, key)
	}
}

func normalizePayload(payload *Payload) {
	if payload.ImportedKeys == nil {
		payload.ImportedKeys = make(map[string]KeyEntry)
	}
}

func verifyPayload(file File, payload Payload) error {
	if payload.Version != 1 {
		return errors.New("unsupported vault payload")
	}
	for _, item := range file.Accounts {
		switch item.Kind {
		case account.KindDerived:
			if item.Index == nil || len(payload.Mnemonic) == 0 {
				return errors.New("derived account cannot be verified")
			}
			if item.Family == "" {
				// Pre-binding records projected one index into both families.
				// They remain readable but unavailable to networks until the
				// user explicitly assigns their original network.
				addresses, err := account.DerivedAddresses(
					string(payload.Mnemonic), string(payload.BIP39Passphrase), *item.Index,
				)
				if err != nil {
					return err
				}
				if addresses[account.FamilyTron] != item.Addresses[account.FamilyTron] ||
					addresses[account.FamilyEVM] != item.Addresses[account.FamilyEVM] {
					return errors.New("derived account address does not match vault")
				}
				continue
			}
			address, err := account.DerivedAddress(
				string(payload.Mnemonic), string(payload.BIP39Passphrase), item.Family, *item.Index,
			)
			if err != nil {
				return err
			}
			if address != item.Addresses[item.Family] {
				return errors.New("derived account address does not match vault")
			}
		case account.KindImported:
			entry, ok := payload.ImportedKeys[item.KeyRef]
			if !ok {
				return errors.New("imported account key is missing")
			}
			key, err := crypto.ToECDSA(entry.PrivateKey)
			if err != nil {
				return errors.New("imported account key is invalid")
			}
			fingerprint, err := account.Fingerprint(key)
			if err != nil || fingerprint != item.Fingerprint {
				return errors.New("imported account fingerprint does not match vault")
			}
		default:
			return errors.New("unknown account kind")
		}
	}
	return nil
}

func findAccount(items []account.Account, id string) (account.Account, error) {
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return account.Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
}

func privateKeyFor(item account.Account, payload *Payload, family account.Family) (*ecdsa.PrivateKey, error) {
	switch item.Kind {
	case account.KindDerived:
		if item.Index == nil {
			return nil, errors.New("derived account has no index")
		}
		if item.Family != "" && item.Family != family {
			return nil, ErrNetworkBinding
		}
		return account.DerivePrivateKey(
			string(payload.Mnemonic), string(payload.BIP39Passphrase), family, *item.Index,
		)
	case account.KindImported:
		entry, ok := payload.ImportedKeys[item.KeyRef]
		if !ok {
			return nil, errors.New("imported key is missing from vault")
		}
		keyBytes := append([]byte(nil), entry.PrivateKey...)
		defer clear(keyBytes)
		key, err := crypto.ToECDSA(keyBytes)
		if err != nil {
			return nil, errors.New("imported key is invalid")
		}
		return key, nil
	default:
		return nil, errors.New("unknown account kind")
	}
}

type localSigner struct {
	key    *ecdsa.PrivateKey
	family account.Family
}

func (s *localSigner) Family() chain.Family {
	return chain.Family(s.family)
}

func (s *localSigner) PublicKey() []byte {
	if s == nil || s.key == nil {
		return nil
	}
	return crypto.FromECDSAPub(&s.key.PublicKey)
}

func (s *localSigner) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.key == nil {
		return nil, errors.New("signer is cleared")
	}
	if len(digest) != 32 {
		return nil, errors.New("digest must be 32 bytes")
	}
	signature, err := crypto.Sign(digest, s.key)
	if err != nil {
		return nil, fmt.Errorf("sign digest: %w", err)
	}
	return signature, nil
}

func (s *localSigner) clear() {
	if s == nil || s.key == nil {
		return
	}
	s.key.D.SetInt64(0)
	s.key = nil
}

func cloneAccounts(items []account.Account) []account.Account {
	out := make([]account.Account, len(items))
	for i := range items {
		out[i] = cloneAccount(items[i])
	}
	return out
}

func cloneAccount(item account.Account) account.Account {
	item.NetworkIDs = append([]string(nil), item.NetworkIDs...)
	addresses := item.Addresses
	item.Addresses = make(map[account.Family]string, len(addresses))
	maps.Copy(item.Addresses, addresses)
	if item.Index != nil {
		index := *item.Index
		item.Index = &index
	}
	if item.ImportedAt != nil {
		importedAt := *item.ImportedAt
		item.ImportedAt = &importedAt
	}
	return item
}

func appendNetwork(ids []string, networkID string) []string {
	if slices.Contains(ids, networkID) {
		return ids
	}
	ids = append(ids, networkID)
	sort.Strings(ids)
	return ids
}

func aad(id string) []byte {
	return fmt.Appendf(nil, "%s:%d", id, SchemaVersion)
}

func newID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)), nil
}
