// Package price retrieves and caches public USD market quotes.
package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/sync/errgroup"
)

const (
	defaultBaseURL = "https://coins.llama.fi"
	defaultTTL     = 5 * time.Minute
	maxResponse    = 2 << 20
	maxBatchSize   = 50
)

type Quote struct {
	Current     decimal.Decimal `json:"current_usd"`
	Previous    decimal.Decimal `json:"previous_24h_usd"`
	HasPrevious bool            `json:"has_previous_24h"`
	Timestamp   time.Time       `json:"timestamp"`
}

type Snapshot struct {
	Quotes map[string]Quote `json:"quotes"`
	Stale  bool             `json:"stale"`
}

type Provider interface {
	Quotes(ctx context.Context, identifiers []string) (Snapshot, error)
}

type Options struct {
	BaseURL string
	Client  *http.Client
	TTL     time.Duration
	Now     func() time.Time
}

type cacheEntry struct {
	quote     Quote
	found     bool
	fetchedAt time.Time
}

type Service struct {
	baseURL string
	client  *http.Client
	ttl     time.Duration
	now     func() time.Time

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

func New(options Options) *Service {
	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	ttl := options.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		baseURL: baseURL, client: client, ttl: ttl, now: now,
		cache: make(map[string]cacheEntry),
	}
}

func (s *Service) Quotes(ctx context.Context, identifiers []string) (Snapshot, error) {
	identifiers = uniqueSorted(identifiers)
	if len(identifiers) == 0 {
		return Snapshot{Quotes: map[string]Quote{}}, nil
	}
	now := s.now()
	missing := s.expired(identifiers, now)
	if len(missing) == 0 {
		return s.snapshot(identifiers, false), nil
	}

	quotes, err := s.fetch(ctx, missing, now)
	if err != nil {
		if s.hasAny(identifiers) {
			return s.snapshot(identifiers, true), nil
		}
		return Snapshot{}, err
	}
	s.mu.Lock()
	for _, identifier := range missing {
		quote, found := quotes[identifier]
		s.cache[identifier] = cacheEntry{quote: quote, found: found, fetchedAt: now}
	}
	s.mu.Unlock()
	return s.snapshot(identifiers, false), nil
}

func (s *Service) expired(identifiers []string, now time.Time) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	missing := make([]string, 0, len(identifiers))
	for _, identifier := range identifiers {
		entry, ok := s.cache[identifier]
		if !ok || now.Sub(entry.fetchedAt) >= s.ttl {
			missing = append(missing, identifier)
		}
	}
	return missing
}

func (s *Service) hasAny(identifiers []string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, identifier := range identifiers {
		if _, ok := s.cache[identifier]; ok {
			return true
		}
	}
	return false
}

func (s *Service) snapshot(identifiers []string, stale bool) Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	quotes := make(map[string]Quote, len(identifiers))
	for _, identifier := range identifiers {
		entry, ok := s.cache[identifier]
		if ok && entry.found {
			quotes[identifier] = entry.quote
		}
	}
	return Snapshot{Quotes: quotes, Stale: stale}
}

func (s *Service) fetch(
	ctx context.Context,
	identifiers []string,
	now time.Time,
) (map[string]Quote, error) {
	var current, previous map[string]llamaQuote
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		var err error
		current, err = s.request(groupCtx, "/prices/current/", identifiers)
		return err
	})
	group.Go(func() error {
		path := fmt.Sprintf("/prices/historical/%d/", now.Add(-24*time.Hour).Unix())
		previous, _ = s.request(groupCtx, path, identifiers)
		return nil
	})
	if err := group.Wait(); err != nil {
		return nil, err
	}

	out := make(map[string]Quote, len(current))
	for identifier, item := range current {
		quote := Quote{Current: item.Price}
		if item.Timestamp > 0 {
			quote.Timestamp = time.Unix(item.Timestamp, 0).UTC()
		}
		if old, ok := previous[identifier]; ok {
			quote.Previous = old.Price
			quote.HasPrevious = true
		}
		out[identifier] = quote
	}
	return out, nil
}

type llamaQuote struct {
	Price     decimal.Decimal `json:"price"`
	Timestamp int64           `json:"timestamp"`
}

func (s *Service) request(
	ctx context.Context,
	prefix string,
	identifiers []string,
) (map[string]llamaQuote, error) {
	out := make(map[string]llamaQuote, len(identifiers))
	for start := 0; start < len(identifiers); start += maxBatchSize {
		end := min(start+maxBatchSize, len(identifiers))
		batch, err := s.requestBatch(ctx, prefix, identifiers[start:end])
		if err != nil {
			return nil, err
		}
		maps.Copy(out, batch)
	}
	return out, nil
}

func (s *Service) requestBatch(
	ctx context.Context,
	prefix string,
	identifiers []string,
) (map[string]llamaQuote, error) {
	path := url.PathEscape(strings.Join(identifiers, ","))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+prefix+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build price request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "walletspace/price-feed")
	response, err := s.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request prices: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("price provider returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read price response: %w", err)
	}
	if len(data) > maxResponse {
		return nil, errors.New("price response is too large")
	}
	var payload struct {
		Coins map[string]llamaQuote `json:"coins"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode price response: %w", err)
	}
	if payload.Coins == nil {
		return nil, errors.New("price response has no coins")
	}
	return payload.Coins, nil
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
