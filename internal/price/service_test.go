package price_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/price"
)

func TestServiceCachesCurrentAndHistoricalQuotes(t *testing.T) {
	var requests atomic.Int32
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.HasPrefix(r.URL.Path, "/prices/current/") {
			fmt.Fprint(w, `{"coins":{"coingecko:ethereum":{"price":2500,"timestamp":1785499200}}}`)
			return
		}
		wantPath := fmt.Sprintf("/prices/historical/%d/", now.Add(-24*time.Hour).Unix())
		if !strings.HasPrefix(r.URL.Path, wantPath) {
			t.Fatalf("historical path = %q, want prefix %q", r.URL.Path, wantPath)
		}
		fmt.Fprint(w, `{"coins":{"coingecko:ethereum":{"price":2000,"timestamp":1785412800}}}`)
	}))
	t.Cleanup(server.Close)

	service := price.New(price.Options{
		BaseURL: server.URL,
		Client:  server.Client(),
		TTL:     5 * time.Minute,
		Now:     func() time.Time { return now },
	})

	for range 2 {
		snapshot, err := service.Quotes(t.Context(), []string{"coingecko:ethereum"})
		if err != nil {
			t.Fatalf("Quotes() error = %v", err)
		}
		quote, ok := snapshot.Quotes["coingecko:ethereum"]
		if !ok || !quote.Current.Equal(decimal.RequireFromString("2500")) ||
			!quote.Previous.Equal(decimal.RequireFromString("2000")) || snapshot.Stale {
			t.Fatalf("Quotes() = %+v", snapshot)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want one current and one historical request", got)
	}
}

func TestServiceReturnsStaleCacheWhenRefreshFails(t *testing.T) {
	var fail atomic.Bool
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		value := "10"
		if strings.Contains(r.URL.Path, "/historical/") {
			value = "8"
		}
		fmt.Fprintf(w, `{"coins":{"coingecko:tron":{"price":%s,"timestamp":1785499200}}}`, value)
	}))
	t.Cleanup(server.Close)

	service := price.New(price.Options{
		BaseURL: server.URL,
		Client:  server.Client(),
		TTL:     5 * time.Minute,
		Now:     func() time.Time { return now },
	})
	if _, err := service.Quotes(context.Background(), []string{"coingecko:tron"}); err != nil {
		t.Fatalf("initial Quotes() error = %v", err)
	}
	now = now.Add(6 * time.Minute)
	fail.Store(true)

	snapshot, err := service.Quotes(context.Background(), []string{"coingecko:tron"})
	if err != nil {
		t.Fatalf("stale Quotes() error = %v", err)
	}
	if !snapshot.Stale || !snapshot.Quotes["coingecko:tron"].Current.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("stale Quotes() = %+v", snapshot)
	}
}

func TestServiceReturnsPartialStaleCacheWhenRefreshFails(t *testing.T) {
	var fail atomic.Bool
	now := time.Date(2026, time.July, 31, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"coins":{"coingecko:ethereum":{"price":2500,"timestamp":1785499200}}}`)
	}))
	t.Cleanup(server.Close)

	service := price.New(price.Options{
		BaseURL: server.URL,
		Client:  server.Client(),
		TTL:     5 * time.Minute,
		Now:     func() time.Time { return now },
	})
	if _, err := service.Quotes(t.Context(), []string{"coingecko:ethereum"}); err != nil {
		t.Fatalf("initial Quotes() error = %v", err)
	}
	now = now.Add(6 * time.Minute)
	fail.Store(true)

	snapshot, err := service.Quotes(t.Context(), []string{
		"coingecko:ethereum",
		"coingecko:tron",
	})
	if err != nil {
		t.Fatalf("partial stale Quotes() error = %v", err)
	}
	if !snapshot.Stale || len(snapshot.Quotes) != 1 ||
		!snapshot.Quotes["coingecko:ethereum"].Current.Equal(decimal.NewFromInt(2500)) {
		t.Fatalf("partial stale Quotes() = %+v", snapshot)
	}
}

func TestServiceRejectsUnexpectedUpstreamContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "not json")
	}))
	t.Cleanup(server.Close)
	service := price.New(price.Options{BaseURL: server.URL, Client: server.Client()})

	if _, err := service.Quotes(t.Context(), []string{"coingecko:ethereum"}); err == nil {
		t.Fatal("Quotes() error = nil")
	}
}

func TestServiceKeepsCurrentPricesWhenHistoryIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/historical/") {
			http.Error(w, "history unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"coins":{"coingecko:ethereum":{"price":2500,"timestamp":1785499200}}}`)
	}))
	t.Cleanup(server.Close)
	service := price.New(price.Options{BaseURL: server.URL, Client: server.Client()})

	snapshot, err := service.Quotes(t.Context(), []string{"coingecko:ethereum"})
	if err != nil {
		t.Fatalf("Quotes() error = %v", err)
	}
	quote, ok := snapshot.Quotes["coingecko:ethereum"]
	if !ok || !quote.Current.Equal(decimal.NewFromInt(2500)) || quote.HasPrevious {
		t.Fatalf("Quotes() = %+v", snapshot)
	}
}

func TestServiceBatchesLargeIdentifierLists(t *testing.T) {
	const batchLimit = 50
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		segment := r.URL.EscapedPath()[strings.LastIndex(r.URL.EscapedPath(), "/")+1:]
		segment, err := url.PathUnescape(segment)
		if err != nil {
			t.Errorf("unescape identifiers: %v", err)
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		identifiers := strings.Split(segment, ",")
		if len(identifiers) > batchLimit {
			http.Error(w, "URI too long", http.StatusRequestURITooLong)
			return
		}
		coins := make(map[string]map[string]any, len(identifiers))
		for _, identifier := range identifiers {
			coins[identifier] = map[string]any{"price": 1, "timestamp": 1785499200}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"coins": coins}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	identifiers := make([]string, 0, 51)
	for index := range 51 {
		identifiers = append(identifiers, fmt.Sprintf("coingecko:test-%02d", index))
	}
	service := price.New(price.Options{BaseURL: server.URL, Client: server.Client()})
	snapshot, err := service.Quotes(t.Context(), identifiers)
	if err != nil {
		t.Fatalf("Quotes() error = %v", err)
	}
	if len(snapshot.Quotes) != len(identifiers) {
		t.Fatalf("quote count = %d, want %d", len(snapshot.Quotes), len(identifiers))
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("requests = %d, want two current and two historical batches", got)
	}
}
