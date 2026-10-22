package refuel_test

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/refuel"
)

const from = "0x1111111111111111111111111111111111111111"
const recipient = "0x2222222222222222222222222222222222222222"

func TestGasZipReverseQuote(t *testing.T) {
	t.Parallel()
	for _, sameAddress := range []bool{false, true} {
		t.Run(fmt.Sprint(sameAddress), func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC()
			to := recipient
			if sameAddress {
				to = from
			}
			var received string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/chains":
					fmt.Fprint(w, `{"chains":[{"chain":1,"inbound":true,"mainnet":true,"decimals":18,"minInboundNative":"10000000000000","maxInboundNative":"1000000000000000000","price":"2000"},{"chain":8217,"short":33,"mainnet":true,"decimals":18,"minOutboundNative":"1000000000000000"}]}`)
				case strings.HasPrefix(r.URL.Path, "/quoteReverse/1/"):
					fmt.Fprint(w, `{"chain":8217,"required":"5000000000000"}`)
				case strings.HasPrefix(r.URL.Path, "/quotes/1/"):
					received = r.URL.Path
					if !strings.EqualFold(r.URL.Query().Get("from"), from) || !strings.EqualFold(r.URL.Query().Get("to"), to) {
						t.Error("recipient addresses lost")
					}
					data := "0x02" + to[2:] + "0021"
					if sameAddress {
						data = "0x010021"
					}
					result := map[string]any{"expires": now.Add(time.Minute).Unix(), "calldata": data, "quotes": []any{map[string]any{"chain": 8217, "expected": "900719925474099312345", "usd": 0.015}}}
					body, err := json.Marshal(result)
					if err != nil {
						t.Error(err)
					}
					w.Write(body)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			}))
			t.Cleanup(server.Close)
			provider := refuel.New(refuel.Options{BaseURL: server.URL, Client: server.Client(), Now: func() time.Time { return now }})
			quote, err := provider.Quote(t.Context(), refuel.Request{SourceChain: 1, DestinationChain: 8217, From: from, Recipient: to, Needed: big.NewInt(1000000000000000)})
			if err != nil {
				t.Fatal(err)
			}
			if quote.Deposit != "10001000000000" || quote.Expected != "900719925474099312345" || quote.To != refuel.DepositAddress || !quote.MinimumLimited || quote.DepositUSD.String() != "0.020002" {
				t.Fatalf("quote = %+v", quote)
			}
			if !strings.Contains(received, "/10001000000000/8217") || quote.ExpiresAt.After(now.Add(45*time.Second)) {
				t.Fatalf("unbounded quote: %+v", quote)
			}
		})
	}
}

func TestGasZipRefusesUnsafeOrUnusableQuotes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, forward string }{
		{"wrong recipient", `{"calldata":"0x0211111111111111111111111111111111111111110021","quotes":[{"chain":8217,"expected":"1000000000000000000"}]}`},
		{"extra destination", `{"calldata":"0x02222222222222222222222222222222222222222200210039","quotes":[{"chain":8217,"expected":"1000000000000000000"}]}`},
		{"insufficient output", `{"calldata":"0x0222222222222222222222222222222222222222220021","quotes":[{"chain":8217,"expected":"1"}]}`},
		{"wrong chain", `{"calldata":"0x0222222222222222222222222222222222222222220021","quotes":[{"chain":56,"expected":"1000000000000000000"}]}`},
		{"expired", `{"expires":1,"calldata":"0x0222222222222222222222222222222222222222220021","quotes":[{"chain":8217,"expected":"1000000000000000000"}]}`},
		{"provider error", `{"error":"Below Minimum Inbound Amount"}`},
		{"fractional base units", `{"quotes":[{"chain":8217,"expected":"1.5"}]}`},
		{"invalid hex", `{"calldata":"0xzz","quotes":[{"chain":8217,"expected":"1000000000000000000"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/chains":
					fmt.Fprint(w, `{"chains":[{"chain":1,"inbound":true,"mainnet":true,"decimals":18,"minInboundNative":"1","maxInboundNative":"1000000000000000000","price":"2000"},{"chain":8217,"short":33,"mainnet":true,"decimals":18}]}`)
				case strings.HasPrefix(r.URL.Path, "/quoteReverse/"):
					fmt.Fprint(w, `{"chain":8217,"required":"1000000000000"}`)
				default:
					fmt.Fprint(w, tt.forward)
				}
			}))
			t.Cleanup(server.Close)
			provider := refuel.New(refuel.Options{BaseURL: server.URL, Client: server.Client()})
			_, err := provider.Quote(t.Context(), refuel.Request{SourceChain: 1, DestinationChain: 8217, From: from, Recipient: recipient, Needed: big.NewInt(1000000000000000)})
			if !errors.Is(err, refuel.ErrUnavailable) {
				t.Fatalf("error = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestGasZipRejectsInvalidRoutesWithoutNetwork(t *testing.T) {
	t.Parallel()
	provider := refuel.New(refuel.Options{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("invalid route reached network")
		return nil, errors.New("unexpected network")
	})}})
	for _, req := range []refuel.Request{
		{SourceChain: 1234, DestinationChain: 8217, From: from, Recipient: recipient, Needed: big.NewInt(1)},
		{SourceChain: 1, DestinationChain: 1, From: from, Recipient: recipient, Needed: big.NewInt(1)},
		{SourceChain: 1, DestinationChain: 8217, From: from, Recipient: recipient},
		{SourceChain: 1, DestinationChain: 8217, From: from, Recipient: recipient, Needed: big.NewInt(-1)},
		{SourceChain: 1, DestinationChain: 8217, From: from, Recipient: "0x0000000000000000000000000000000000000000", Needed: big.NewInt(1)},
	} {
		if _, err := provider.Quote(t.Context(), req); !errors.Is(err, refuel.ErrUnavailable) {
			t.Fatalf("invalid route error = %v", err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGasZipDeliveryMatchesRecipientAndChain(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, status, to string
		chain            int64
		refund           bool
		want             string
	}{
		{name: "delivered", status: "CONFIRMED", to: recipient, chain: 8217, want: "delivered"},
		{name: "wrong recipient", status: "CONFIRMED", to: from, chain: 8217, want: "pending"},
		{name: "wrong chain", status: "CONFIRMED", to: recipient, chain: 1, want: "pending"},
		{name: "unconfirmed", status: "PENDING", to: recipient, chain: 8217, want: "pending"},
		{name: "refund", status: "CONFIRMED", to: recipient, chain: 8217, refund: true, want: "pending"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"deposit":{"status":"CONFIRMED"},"txs":[{"chain":%d,"to":%q,"status":%q,"refund":%t,"hash":"0xdelivery"}]}`, tt.chain, tt.to, tt.status, tt.refund)
			}))
			t.Cleanup(server.Close)
			provider := refuel.New(refuel.Options{BaseURL: server.URL, Client: server.Client()})
			result, err := provider.Delivery(t.Context(), "0x"+strings.Repeat("1", 64), 8217, recipient)
			if err != nil || result.Status != tt.want {
				t.Fatalf("delivery = %+v, %v", result, err)
			}
		})
	}
}
