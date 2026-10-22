package refuel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/storage"
)

var ErrPlanNotFound = errors.New("gas top-up quote not found; calculate again")

type Target struct {
	AccountID string `json:"account_id"`
	AssetID   string `json:"asset_id"`
	To        string `json:"to"`
	Amount    string `json:"amount"`
}

type Plan struct {
	ID              string                 `json:"id"`
	NetworkID       string                 `json:"network_id"` // token's network
	Target          Target                 `json:"target"`
	Recipient       string                 `json:"recipient"` // owner of the token, not token recipient
	SourceNetworkID string                 `json:"source_network_id"`
	SourceAccountID string                 `json:"source_account_id"`
	SourceAddress   string                 `json:"source_address"`
	SourceLabel     string                 `json:"source_label"`
	Quote           Quote                  `json:"quote"`
	SourceEstimate  chain.TransferEstimate `json:"source_estimate"`
	TargetEstimate  chain.TransferEstimate `json:"target_estimate"`
	Required        string                 `json:"required"`
	Expected        string                 `json:"expected"`
	Remainder       string                 `json:"remainder"`
	TotalSource     string                 `json:"total_source"`
	TotalUSD        string                 `json:"total_usd,omitempty"`
	Started         bool                   `json:"started"`
	Finished        bool                   `json:"finished"`
	CreatedAt       time.Time              `json:"created_at"`
}

type Store struct {
	home string
	mu   sync.Mutex
}

func NewStore(home string) *Store { return &Store{home: home} }

func (s *Store) path(spaceID string) (string, error) {
	if !storage.ValidID(spaceID, "spc_") {
		return "", errors.New("invalid space id")
	}
	return filepath.Join(s.home, "spaces", spaceID, "refuel.json"), nil
}
func (s *Store) load(spaceID string) (map[string]Plan, error) {
	path, err := s.path(spaceID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]Plan), nil
	}
	if err != nil {
		return nil, err
	}
	var plans map[string]Plan
	if err := json.Unmarshal(data, &plans); err != nil {
		return nil, fmt.Errorf("read gas top-up plans: %w", err)
	}
	if plans == nil {
		plans = make(map[string]Plan)
	}
	return plans, nil
}
func (s *Store) save(spaceID string, plans map[string]Plan) error {
	path, err := s.path(spaceID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(plans)
	if err != nil {
		return err
	}
	return storage.AtomicWrite(path, data)
}
func (s *Store) Put(spaceID string, plan Plan) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plans, err := s.load(spaceID)
	if err != nil {
		return Plan{}, err
	}
	now := time.Now().UTC()
	for id, p := range plans {
		if (!p.Started && now.After(p.Quote.ExpiresAt)) || (p.Finished && now.Sub(p.CreatedAt) > 7*24*time.Hour) {
			delete(plans, id)
		}
	}
	if len(plans) >= 512 {
		return Plan{}, errors.New("too many gas top-up plans; finish pending transfers first")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Plan{}, err
	}
	plan.ID = hex.EncodeToString(id[:])
	plan.CreatedAt = now
	plans[plan.ID] = plan
	if err := s.save(spaceID, plans); err != nil {
		return Plan{}, err
	}
	return plan, nil
}
func (s *Store) Get(spaceID, id string) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plans, err := s.load(spaceID)
	if err != nil {
		return Plan{}, err
	}
	plan, ok := plans[id]
	if !ok {
		return Plan{}, ErrPlanNotFound
	}
	return plan, nil
}
func (s *Store) Mark(spaceID, id string, finished bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plans, err := s.load(spaceID)
	if err != nil {
		return err
	}
	plan, ok := plans[id]
	if !ok {
		return ErrPlanNotFound
	}
	plan.Started = true
	plan.Finished = finished
	plans[id] = plan
	return s.save(spaceID, plans)
}

// Active prevents a second top-up for a wallet while its previous deposit is
// unresolved, including quotes opened in another tab or before a restart.
func (s *Store) Active(spaceID, networkID, accountID string) (Plan, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plans, err := s.load(spaceID)
	if err != nil {
		return Plan{}, false, err
	}
	for _, plan := range plans {
		if plan.Started && !plan.Finished && plan.NetworkID == networkID && plan.Target.AccountID == accountID {
			return plan, true, nil
		}
	}
	return Plan{}, false, nil
}

func (s *Store) Claim(spaceID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	plans, err := s.load(spaceID)
	if err != nil {
		return err
	}
	plan, ok := plans[id]
	if !ok {
		return ErrPlanNotFound
	}
	for otherID, other := range plans {
		if otherID != id && other.Started && !other.Finished && other.NetworkID == plan.NetworkID && other.Target.AccountID == plan.Target.AccountID {
			return fmt.Errorf("a gas top-up is already pending for this wallet; resume %s", otherID)
		}
	}
	plan.Started = true
	plans[id] = plan
	return s.save(spaceID, plans)
}
