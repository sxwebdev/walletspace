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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
)

const (
	SchemaVersion     = 1
	minPasswordLength = 8
)

var (
	ErrNotFound         = errors.New("space not found")
	ErrLocked           = errors.New("space is locked")
	ErrDuplicateKey     = errors.New("private key is already imported")
	ErrNoSeed           = errors.New("space does not have a mnemonic")
	ErrAccountNotFound  = errors.New("account not found")
	ErrFirstSpaceExists = errors.New("the first space was already created")
	ErrWeakPassword     = errors.New("space password is too short")
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
}

type Manager struct {
	home     string
	params   vault.Params
	autoLock time.Duration
	now      func() time.Time

	mu       sync.RWMutex
	files    map[string]File
	sessions map[string]*session
	stop     chan struct{}
	done     chan struct{}
	reset    chan struct{}
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
	if duration < 0 {
		return errors.New("auto-lock must not be negative")
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
	file := File{SchemaVersion: SchemaVersion, ID: id, Name: name, CreatedAt: now, UpdatedAt: now}
	if !req.ImportedOnly {
		addresses, err := account.DerivedAddresses(mnemonic, req.BIP39Passphrase, 0)
		if err != nil {
			clearPayload(&payload)
			return CreateResult{}, err
		}
		accountID, err := newID("acc_")
		if err != nil {
			clearPayload(&payload)
			return CreateResult{}, err
		}
		index := uint32(0)
		file.Accounts = []account.Account{{
			ID: accountID, Kind: account.KindDerived, Addresses: addresses, Index: &index,
			DerivationProfile: "bip44-v1", CreatedAt: now, UpdatedAt: now,
		}}
	}
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

func (m *Manager) Unlock(id, password string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	var payload Payload
	sessionKey, err := vault.UnlockJSON(password, file.Vault, aad(id), &payload)
	if err != nil {
		return err
	}
	if err := verifyPayload(file, payload); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return err
	}
	normalizePayload(&payload)
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

func (m *Manager) ChangePassword(id, currentPassword, newPassword string) error {
	if err := validateNewPassword(newPassword); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	var payload Payload
	if err := vault.OpenJSON(currentPassword, file.Vault, aad(id), &payload); err != nil {
		return err
	}
	defer clearPayload(&payload)
	if err := verifyPayload(file, payload); err != nil {
		return err
	}
	normalizePayload(&payload)
	container, err := vault.SealJSON(newPassword, payload, aad(id), m.params)
	if err != nil {
		return err
	}
	file.Vault = container
	file.UpdatedAt = m.now()
	if err := m.saveLocked(file); err != nil {
		return err
	}
	m.files[id] = file
	if active := m.sessions[id]; active != nil {
		active.key.Clear()
		var refreshed Payload
		sessionKey, err := vault.UnlockJSON(newPassword, container, aad(id), &refreshed)
		if err != nil {
			m.clearSessionLocked(id)
			return err
		}
		clearPayload(&active.payload)
		active.payload = refreshed
		active.key = sessionKey
		active.lastUsed = m.now()
	}
	return nil
}

func validateNewPassword(password string) error {
	if password == "" {
		return errors.New("space password is required")
	}
	if len(password) < minPasswordLength {
		return fmt.Errorf("%w: must be at least %d characters", ErrWeakPassword, minPasswordLength)
	}
	return nil
}

func (m *Manager) Derive(id, label string) (account.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return account.Account{}, err
	}
	if len(payload.Mnemonic) == 0 {
		return account.Account{}, ErrNoSeed
	}
	var next uint32
	for _, item := range file.Accounts {
		if item.Kind == account.KindDerived && item.Index != nil && *item.Index >= next {
			if *item.Index == ^uint32(0) {
				return account.Account{}, errors.New("derivation index exhausted")
			}
			next = *item.Index + 1
		}
	}
	addresses, err := account.DerivedAddresses(string(payload.Mnemonic), string(payload.BIP39Passphrase), next)
	if err != nil {
		return account.Account{}, err
	}
	accountID, err := newID("acc_")
	if err != nil {
		return account.Account{}, err
	}
	now := m.now()
	created := account.Account{
		ID: accountID, Label: strings.TrimSpace(label), Kind: account.KindDerived,
		Addresses: addresses, Index: &next, DerivationProfile: "bip44-v1",
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

func (m *Manager) Import(id, label, privateKey string) (ImportResult, error) {
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
	for _, item := range file.Accounts {
		if item.Kind == account.KindImported && item.Fingerprint == fingerprint {
			return ImportResult{}, ErrDuplicateKey
		}
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
		Addresses: addresses, KeyRef: keyRef, Fingerprint: fingerprint, ImportedAt: &importedAt,
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

func (m *Manager) ExportPrivateKey(id, accountID string, family account.Family) (string, error) {
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
	id, accountID string,
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
	key, err := privateKeyFor(item, payload, family)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	signer := &localSigner{key: key, family: family}
	defer signer.clear()
	return fn(signer)
}

func (m *Manager) Mnemonic(id string) (string, error) {
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

func (m *Manager) Backup(id string) ([]byte, error) {
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

func (m *Manager) RenameAccount(id, accountID, label string) (account.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return account.Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
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
			addresses, err := account.DerivedAddresses(string(payload.Mnemonic), string(payload.BIP39Passphrase), *item.Index)
			if err != nil {
				return err
			}
			if addresses[account.FamilyTron] != item.Addresses[account.FamilyTron] ||
				addresses[account.FamilyEVM] != item.Addresses[account.FamilyEVM] {
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
	addresses := item.Addresses
	item.Addresses = make(map[account.Family]string, len(addresses))
	for family, address := range addresses {
		item.Addresses[family] = address
	}
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

func aad(id string) []byte {
	return []byte(fmt.Sprintf("%s:%d", id, SchemaVersion))
}

func newID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)), nil
}
