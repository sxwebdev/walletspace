package raydium_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

type quoteRequest struct {
	in, out  solana.PublicKey
	amount   uint64
	slippage uint16
}

// quoteFixture reads testdata/quotes/<name> and the request it answered,
// as testdata/quotes/index.json records it.
func quoteFixture(t *testing.T, name string) ([]byte, quoteRequest) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "quotes", name))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		Files map[string]struct {
			InputMint   string `json:"inputMint"`
			OutputMint  string `json:"outputMint"`
			Amount      string `json:"amount"`
			SlippageBps uint16 `json:"slippageBps"`
		} `json:"files"`
	}
	readJSON(t, filepath.Join("quotes", "index.json"), &index)
	f, ok := index.Files[name]
	if !ok || f.InputMint == "" {
		t.Fatalf("index.json has no request for %s", name)
	}
	amount, err := strconv.ParseUint(f.Amount, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return body, quoteRequest{key(f.InputMint), key(f.OutputMint), amount, f.SlippageBps}
}

// serve answers every request with body and status, and counts requests.
func serve(t *testing.T, status int, header http.Header, body []byte) (*raydium.Quoter, *atomic.Int32, *atomic.Pointer[http.Request]) {
	t.Helper()
	var hits atomic.Int32
	var last atomic.Pointer[http.Request]
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store(r)
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	q, err := raydium.NewQuoter(srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return q, &hits, &last
}

func replayQuote(t *testing.T, name string) raydium.Route {
	t.Helper()
	body, req := quoteFixture(t, name)
	q, _, _ := serve(t, http.StatusOK, nil, body)
	route, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage)
	if err != nil {
		t.Fatal(err)
	}
	return route
}

func TestQuoteRequest(t *testing.T) {
	t.Parallel()
	body, req := quoteFixture(t, "sol_usdc_2hop.json")
	q, hits, last := serve(t, http.StatusOK, nil, body)
	if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); err != nil {
		t.Fatal(err)
	}
	r := last.Load()
	if hits.Load() != 1 || r.Method != http.MethodGet || r.URL.Path != "/compute/swap-base-in" || r.Header.Get("Accept") != "application/json" {
		t.Fatalf("request %s %s, accept %q, %d hits", r.Method, r.URL.Path, r.Header.Get("Accept"), hits.Load())
	}
	want := "amount=1000000000&inputMint=So11111111111111111111111111111111111111112&outputMint=EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v&slippageBps=50&txVersion=LEGACY"
	if r.URL.RawQuery != want {
		t.Fatalf("query %s\nwant  %s", r.URL.RawQuery, want)
	}
}

// Every captured quote parses into the route its JSON states, read here
// with a generic decoder.
func TestQuoteParsesCapturedRoutes(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(filepath.Join("testdata", "quotes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "index.json" || name == "noroute.json" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body, _ := quoteFixture(t, name)
			var raw struct {
				ID   string         `json:"id"`
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			route := replayQuote(t, name)
			d := raw.Data
			if route.ID != raw.ID || route.InputMint != key(d["inputMint"].(string)) || route.OutputMint != key(d["outputMint"].(string)) ||
				strconv.FormatUint(route.AmountIn, 10) != d["inputAmount"] || strconv.FormatUint(route.AmountOut, 10) != d["outputAmount"] ||
				strconv.FormatUint(route.MinAmountOut, 10) != d["otherAmountThreshold"] || route.PriceImpactPct != d["priceImpactPct"] ||
				float64(route.SlippageBps) != d["slippageBps"] {
				t.Errorf("route %+v\nJSON %v", route, d)
			}
			plan := d["routePlan"].([]any)
			if len(route.Hops) != len(plan) {
				t.Fatalf("%d hops, JSON has %d", len(route.Hops), len(plan))
			}
			for i, h := range plan {
				h := h.(map[string]any)
				hop := route.Hops[i]
				var remaining []solana.PublicKey
				for _, r := range h["remainingAccounts"].([]any) {
					remaining = append(remaining, key(r.(string)))
				}
				if hop.Pool != key(h["poolId"].(string)) || hop.InputMint != key(h["inputMint"].(string)) || hop.OutputMint != key(h["outputMint"].(string)) ||
					hop.FeeMint != key(h["feeMint"].(string)) || strconv.FormatUint(hop.FeeAmount, 10) != h["feeAmount"] || hop.FeeRateBps != h["feeRate"] ||
					!slices.Equal(hop.RemainingAccounts, remaining) || hop.Program != (solana.PublicKey{}) {
					t.Errorf("hop %d %+v\nJSON %v", i, hop, h)
				}
			}
		})
	}
}

func TestQuoteNoRoute(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("testdata", "quotes", "noroute.json"))
	if err != nil {
		t.Fatal(err)
	}
	q, _, _ := serve(t, http.StatusOK, nil, body)
	if _, err := q.Quote(t.Context(), usdc, key("11111111111111111111111111111111"), 1000, 50); !errors.Is(err, raydium.ErrNoRoute) {
		t.Fatalf("err = %v", err)
	}
}

// mutate decodes a captured quote, applies f to it and encodes it again.
func mutate(t *testing.T, name string, f func(resp map[string]any, data map[string]any, plan []any)) []byte {
	t.Helper()
	body, _ := quoteFixture(t, name)
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	data, _ := resp["data"].(map[string]any)
	var plan []any
	if data != nil {
		plan, _ = data["routePlan"].([]any)
	}
	f(resp, data, plan)
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQuoteRefusesMalformedRoutes(t *testing.T) {
	t.Parallel()
	hop := func(plan []any, i int) map[string]any { return plan[i].(map[string]any) }
	const twoHop = "sol_usdc_2hop.json" // SOL → USDT (3nMF) → USDC (BZtg)
	const clmm = "usdc_usdt.json"       // one CLMM hop with one tick array
	tests := []struct {
		name    string
		fixture string
		edit    func(resp, data map[string]any, plan []any)
		want    error
	}{
		{"API error", twoHop, func(r, _ map[string]any, _ []any) { r["success"] = false; r["msg"] = "INSUFFICIENT_LIQUIDITY" }, raydium.ErrAPI},
		{"no data", twoHop, func(r, _ map[string]any, _ []any) { delete(r, "data") }, raydium.ErrMalformedRoute},
		{"base out", twoHop, func(_, d map[string]any, _ []any) { d["swapType"] = "BaseOut" }, raydium.ErrMalformedRoute},
		{"other input mint", twoHop, func(_, d map[string]any, _ []any) { d["inputMint"] = usdt.String() }, raydium.ErrMalformedRoute},
		{"bad input mint", twoHop, func(_, d map[string]any, _ []any) { d["inputMint"] = "So1111111111111111111111111111111111111111" }, raydium.ErrMalformedRoute},
		{"other output mint", twoHop, func(_, d map[string]any, _ []any) { d["outputMint"] = usdt.String() }, raydium.ErrMalformedRoute},
		{"other amount", twoHop, func(_, d map[string]any, _ []any) { d["inputAmount"] = "999999999" }, raydium.ErrMalformedRoute},
		{"amount with leading zero", twoHop, func(_, d map[string]any, _ []any) { d["inputAmount"] = "01000000000" }, raydium.ErrMalformedRoute},
		{"amount as a number", twoHop, func(_, d map[string]any, _ []any) { d["inputAmount"] = 1e9 }, raydium.ErrMalformedRoute},
		{"other actual input", twoHop, func(_, d map[string]any, _ []any) { d["actualInputAmount"] = "999999999" }, raydium.ErrMalformedRoute},
		{"referrer cut", twoHop, func(_, d map[string]any, _ []any) { d["referrerAmount"] = "10000000" }, raydium.ErrMalformedRoute},
		{"no slippage", twoHop, func(_, d map[string]any, _ []any) { delete(d, "slippageBps") }, raydium.ErrMalformedRoute},
		{"other slippage", twoHop, func(_, d map[string]any, _ []any) { d["slippageBps"] = 100 }, raydium.ErrMalformedRoute},
		{"zero output", twoHop, func(_, d map[string]any, _ []any) { d["outputAmount"] = "0" }, raydium.ErrMalformedRoute},
		{"negative output", twoHop, func(_, d map[string]any, _ []any) { d["outputAmount"] = "-1" }, raydium.ErrMalformedRoute},
		{"zero minimum", twoHop, func(_, d map[string]any, _ []any) { d["otherAmountThreshold"] = "0" }, raydium.ErrMalformedRoute},
		// 104370770 × 9950 / 10000 = 103848916.15: one below is more
		// slippage than asked for.
		{"minimum below the slippage", twoHop, func(_, d map[string]any, _ []any) { d["otherAmountThreshold"] = "103848915" }, raydium.ErrMalformedRoute},
		{"minimum above the output", twoHop, func(_, d map[string]any, _ []any) { d["otherAmountThreshold"] = "104370771" }, raydium.ErrMalformedRoute},
		{"empty plan", twoHop, func(_, d map[string]any, _ []any) { d["routePlan"] = []any{} }, raydium.ErrMalformedRoute},
		{"three hops", twoHop, func(_, d map[string]any, p []any) { d["routePlan"] = append(p, p[1]) }, raydium.ErrUnsupportedRoute},
		{"hops not connected", twoHop, func(_, _ map[string]any, p []any) {
			hop(p, 1)["inputMint"] = wsol.String()
			hop(p, 1)["feeMint"] = wsol.String()
		}, raydium.ErrMalformedRoute},
		{"route ends elsewhere", twoHop, func(_, _ map[string]any, p []any) { hop(p, 1)["outputMint"] = useless.String() }, raydium.ErrMalformedRoute},
		{"route returns to its input", twoHop, func(_, d map[string]any, p []any) {
			hop(p, 0)["outputMint"] = usdc.String()
			hop(p, 1)["inputMint"] = usdc.String()
			hop(p, 1)["outputMint"] = wsol.String()
			hop(p, 1)["feeMint"] = usdc.String()
		}, raydium.ErrMalformedRoute},
		{"same pool twice", twoHop, func(_, _ map[string]any, p []any) { hop(p, 1)["poolId"] = hop(p, 0)["poolId"] }, raydium.ErrMalformedRoute},
		{"hop to itself", clmm, func(_, _ map[string]any, p []any) { hop(p, 0)["outputMint"] = usdc.String() }, raydium.ErrMalformedRoute},
		{"bad pool", twoHop, func(_, _ map[string]any, p []any) { hop(p, 0)["poolId"] = "x" }, raydium.ErrMalformedRoute},
		{"non-canonical pool", twoHop, func(_, _ map[string]any, p []any) { hop(p, 0)["poolId"] = " " + hop(p, 0)["poolId"].(string) }, raydium.ErrMalformedRoute},
		{"unknown program", twoHop, func(_, _ map[string]any, p []any) {
			hop(p, 0)["programId"] = "5quBtoiQqxF9Jv6KYKctB59NT3gtJD2Y65kdnB1Uev3h" // Raydium stable swap
		}, raydium.ErrUnsupportedRoute},
		{"bad program", twoHop, func(_, _ map[string]any, p []any) { hop(p, 0)["programId"] = "nope" }, raydium.ErrMalformedRoute},
		{"tick arrays on an AMM v4 pool", clmm, func(_, _ map[string]any, p []any) { hop(p, 0)["programId"] = raydium.AMMV4Program.String() }, raydium.ErrMalformedRoute},
		{"fee in a third mint", twoHop, func(_, _ map[string]any, p []any) { hop(p, 0)["feeMint"] = useless.String() }, raydium.ErrMalformedRoute},
		{"bad fee amount", twoHop, func(_, _ map[string]any, p []any) { hop(p, 0)["feeAmount"] = "1.5" }, raydium.ErrMalformedRoute},
		{"negative fee rate", twoHop, func(_, _ map[string]any, p []any) { hop(p, 0)["feeRate"] = -1 }, raydium.ErrMalformedRoute},
		{"fee rate of 100%", twoHop, func(_, _ map[string]any, p []any) { hop(p, 0)["feeRate"] = 10000 }, raydium.ErrMalformedRoute},
		{"bad input of a hop", twoHop, func(_, _ map[string]any, p []any) { hop(p, 0)["inputMint"] = "" }, raydium.ErrMalformedRoute},
		{"bad remaining account", clmm, func(_, _ map[string]any, p []any) { hop(p, 0)["remainingAccounts"] = []any{"zz"} }, raydium.ErrMalformedRoute},
		{"remaining account repeated", clmm, func(_, _ map[string]any, p []any) {
			a := hop(p, 0)["remainingAccounts"].([]any)[0]
			hop(p, 0)["remainingAccounts"] = []any{a, a}
		}, raydium.ErrMalformedRoute},
		{"pool as a remaining account", clmm, func(_, _ map[string]any, p []any) { hop(p, 0)["remainingAccounts"] = []any{hop(p, 0)["poolId"]} }, raydium.ErrMalformedRoute},
		{"too many remaining accounts", clmm, func(_, _ map[string]any, p []any) {
			var many []any
			for i := range raydium.MaxTickArrays + 1 {
				many = append(many, solana.PublicKeyFromBytes(slices.Repeat([]byte{byte(i + 1)}, 32)).String())
			}
			hop(p, 0)["remainingAccounts"] = many
		}, raydium.ErrMalformedRoute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, req := quoteFixture(t, tt.fixture)
			q, _, _ := serve(t, http.StatusOK, nil, mutate(t, tt.fixture, tt.edit))
			if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestQuoteAcceptsVariants(t *testing.T) {
	t.Parallel()
	const clmm = "usdc_usdt.json"
	tests := []struct {
		name  string
		edit  func(resp, data map[string]any, plan []any)
		check func(t *testing.T, r raydium.Route)
	}{
		{"named CLMM program", func(_, _ map[string]any, p []any) { p[0].(map[string]any)["programId"] = raydium.CLMMProgram.String() }, func(t *testing.T, r raydium.Route) {
			if r.Hops[0].Program != raydium.CLMMProgram {
				t.Errorf("program %s", r.Hops[0].Program)
			}
		}},
		{"no actual input, referrer or fee fields", func(_, d map[string]any, p []any) {
			delete(d, "actualInputAmount")
			delete(d, "referrerAmount")
			delete(p[0].(map[string]any), "feeMint")
			delete(p[0].(map[string]any), "feeAmount")
		}, func(t *testing.T, r raydium.Route) {
			if r.Hops[0].FeeMint != (solana.PublicKey{}) || r.Hops[0].FeeAmount != 0 {
				t.Errorf("hop %+v", r.Hops[0])
			}
		}},
		{"unknown fields", func(r, d map[string]any, p []any) {
			r["extra"] = 1
			d["routeVersion"] = "v9"
			p[0].(map[string]any)["poolType"] = "x"
		}, func(*testing.T, raydium.Route) {}},
		{"minimum equal to the output", func(_, d map[string]any, _ []any) { d["otherAmountThreshold"] = d["outputAmount"] }, func(*testing.T, raydium.Route) {}},
		{"fee on the output mint", func(_, _ map[string]any, p []any) {
			p[0].(map[string]any)["feeMint"] = usdt.String()
		}, func(t *testing.T, r raydium.Route) {
			if r.Hops[0].FeeMint != usdt {
				t.Errorf("fee mint %s", r.Hops[0].FeeMint)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, req := quoteFixture(t, clmm)
			q, _, _ := serve(t, http.StatusOK, nil, mutate(t, clmm, tt.edit))
			r, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage)
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, r)
		})
	}
}

func TestQuoteRefusesBadJSON(t *testing.T) {
	t.Parallel()
	body, req := quoteFixture(t, "usdc_usdt.json")
	dup := strings.Replace(string(body), `"swapType":"BaseIn"`, `"swapType":"BaseIn","swapType":"BaseIn"`, 1)
	if dup == string(body) {
		t.Fatal("fixture changed shape")
	}
	for name, b := range map[string][]byte{
		"duplicate key":  []byte(dup),
		"truncated":      body[:len(body)/2],
		"trailing data":  append(slices.Clone(body), []byte(" {}")...),
		"invalid UTF-8":  []byte(strings.Replace(string(body), "BaseIn", "Base\xffIn", 1)),
		"not an object":  []byte(`[]`),
		"empty response": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _, _ := serve(t, http.StatusOK, nil, b)
			if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); !errors.Is(err, raydium.ErrMalformedRoute) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestQuoteHTTPErrors(t *testing.T) {
	t.Parallel()
	body, req := quoteFixture(t, "usdc_usdt.json")
	t.Run("server error", func(t *testing.T) {
		t.Parallel()
		q, _, _ := serve(t, http.StatusBadGateway, nil, []byte(strings.Repeat("x", 1000)))
		_, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage)
		if !errors.Is(err, raydium.ErrAPI) || !strings.Contains(err.Error(), "502") || len(err.Error()) > 400 {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		t.Parallel()
		padded := append(slices.Clone(body[:len(body)-1]), []byte(`,"pad":"`+strings.Repeat("x", raydium.MaxQuoteResponse)+`"}`)...)
		q, _, _ := serve(t, http.StatusOK, nil, padded)
		if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); !errors.Is(err, raydium.ErrAPI) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("exactly the limit", func(t *testing.T) {
		t.Parallel()
		prefix := append(slices.Clone(body[:len(body)-1]), []byte(`,"pad":"`)...)
		pad := raydium.MaxQuoteResponse - len(prefix) - len(`"}`)
		exact := append(prefix, []byte(strings.Repeat("x", pad)+`"}`)...)
		if len(exact) != raydium.MaxQuoteResponse {
			t.Fatalf("built %d bytes", len(exact))
		}
		q, _, _ := serve(t, http.StatusOK, nil, exact)
		if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); err != nil {
			t.Fatalf("a response of exactly MaxQuoteResponse bytes: %v", err)
		}
	})
	t.Run("body cut off", func(t *testing.T) {
		t.Parallel()
		cut := errors.New("connection reset")
		client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"id":`), errReader{cut}))}, nil
		})}
		q, err := raydium.NewQuoter(client, "https://quote.test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); !errors.Is(err, cut) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("transport", func(t *testing.T) {
		t.Parallel()
		refused := errors.New("connection refused")
		client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { return nil, refused })}
		q, err := raydium.NewQuoter(client, "https://quote.test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); !errors.Is(err, refused) || errors.Is(err, raydium.ErrAPI) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestQuoteRateLimit(t *testing.T) {
	t.Parallel()
	body, req := quoteFixture(t, "usdc_usdt.json")
	for _, tt := range []struct {
		name  string
		after string
		want  time.Duration
	}{
		{"seconds", "7", 7 * time.Second},
		{"zero", "0", 0},
		{"missing", "", time.Minute},
		{"garbage", "soon", time.Minute},
		{"capped", "86400", 10 * time.Minute},
		{"HTTP date", "date", 90 * time.Second},
		{"HTTP date in the past", "past", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var limited atomic.Bool
				limited.Store(true)
				var hits atomic.Int32
				srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if limited.Load() {
						switch tt.after {
						case "date":
							w.Header().Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
						case "past":
							w.Header().Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
						case "":
						default:
							w.Header().Set("Retry-After", tt.after)
						}
						w.WriteHeader(http.StatusTooManyRequests)
						return
					}
					_, _ = w.Write(body)
				}))
				q, err := raydium.NewQuoter(srv.Client(), srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				quote := func() error {
					_, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage)
					return err
				}
				err = quote()
				rl, ok := errors.AsType[*raydium.RateLimitedError](err)
				if !ok || !errors.Is(err, raydium.ErrRateLimited) || rl.RetryAfter != tt.want || !strings.Contains(err.Error(), "retry after "+tt.want.String()) {
					t.Fatalf("err = %v, want a rate limit of %v", err, tt.want)
				}
				limited.Store(false)
				if tt.want > time.Second {
					// Asking again before Retry-After fails at once
					// without a request.
					synctest.Sleep(tt.want - time.Second)
					start := time.Now()
					err := quote()
					if rl, ok := errors.AsType[*raydium.RateLimitedError](err); !ok || rl.RetryAfter != time.Second || time.Since(start) != 0 || hits.Load() != 1 {
						t.Fatalf("during Retry-After: %v, %d requests", err, hits.Load())
					}
					synctest.Sleep(time.Second)
				}
				if err := quote(); err != nil || hits.Load() != 2 {
					t.Fatalf("after Retry-After: %v, %d requests", err, hits.Load())
				}
			})
		})
	}
}

func TestQuotePacing(t *testing.T) {
	t.Parallel()
	body, req := quoteFixture(t, "usdc_usdt.json")
	synctest.Test(t, func(t *testing.T) {
		var times []time.Time
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			times = append(times, time.Now())
			_, _ = w.Write(body)
		}))
		q, err := raydium.NewQuoter(srv.Client(), srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); err != nil {
				t.Fatal(err)
			}
		}
		// Back to back: exactly QuoteInterval apart, not less, not more.
		for i := 1; i < len(times); i++ {
			if d := times[i].Sub(times[i-1]); d != raydium.QuoteInterval {
				t.Errorf("request %d came %v after the previous one", i, d)
			}
		}
		// After a pause longer than the interval, no wait.
		synctest.Sleep(2 * time.Second)
		start := time.Now()
		if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited %v after a pause", waited)
		}
		// A caller whose context ends while waiting for its slot gets the
		// context's error and sends nothing.
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		sent := len(times)
		if _, err := q.Quote(ctx, req.in, req.out, req.amount, req.slippage); !errors.Is(err, context.DeadlineExceeded) || len(times) != sent {
			t.Errorf("cancelled wait: %v, %d requests sent", err, len(times)-sent)
		}
	})
}

func TestQuoteRefusesBadArguments(t *testing.T) {
	t.Parallel()
	body, _ := quoteFixture(t, "usdc_usdt.json")
	q, hits, _ := serve(t, http.StatusOK, nil, body)
	for name, call := range map[string]func() error{
		"same mints":    func() error { _, err := q.Quote(t.Context(), usdc, usdc, 1, 50); return err },
		"zero amount":   func() error { _, err := q.Quote(t.Context(), usdc, usdt, 0, 50); return err },
		"100% slippage": func() error { _, err := q.Quote(t.Context(), usdc, usdt, 1, 10000); return err },
		"nil context": func() error {
			var ctx context.Context
			_, err := q.Quote(ctx, usdc, usdt, 1, 50) //nolint:staticcheck // the nil context is the point
			return err
		},
		"done context": func() error {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := q.Quote(ctx, usdc, usdt, 1, 50)
			return err
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests sent for refused arguments", hits.Load())
	}
}

func TestNewQuoter(t *testing.T) {
	t.Parallel()
	if _, err := raydium.NewQuoter(nil, ""); err == nil {
		t.Error("nil client accepted")
	}
	for _, base := range []string{"ftp://example.com", "://", "https://", "example.com"} {
		if _, err := raydium.NewQuoter(http.DefaultClient, base); err == nil {
			t.Errorf("base URL %q accepted", base)
		}
	}
	// An empty base URL means the Trade API: the request goes there.
	var got string
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		got = r.URL.Scheme + "://" + r.URL.Host + r.URL.Path
		return nil, errors.New("stop")
	})}
	q, err := raydium.NewQuoter(client, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = q.Quote(t.Context(), usdc, usdt, 1, 50)
	if got != raydium.DefaultBaseURL+"/compute/swap-base-in" || raydium.DefaultBaseURL != "https://transaction-v1.raydium.io" {
		t.Errorf("request went to %s", got)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Callers already waiting for their slot when a 429 arrives do not send
// during the ban; slots that fall after it keep the pacing.
func TestQuoteRateLimitStopsWaitingCallers(t *testing.T) {
	t.Parallel()
	body, req := quoteFixture(t, "usdc_usdt.json")
	for _, tt := range []struct {
		name       string
		retryAfter string
		wantSent   []time.Duration // when each request reached the server
		wantErrs   []time.Duration // RetryAfter of every refused caller, sorted; the first is the 429 itself
	}{
		{"ban outlasts the queue", "60", []time.Duration{0}, []time.Duration{58500 * time.Millisecond, 59 * time.Second, 59500 * time.Millisecond, time.Minute}},
		{"ban of zero", "0", []time.Duration{0, 500 * time.Millisecond, time.Second, 1500 * time.Millisecond}, []time.Duration{0}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var mu sync.Mutex
				var sent []time.Duration
				// The 429 is held back until every caller has taken its
				// slot: otherwise a caller that starts after the 429 landed
				// (fake time does not move while the first request is
				// served) is refused on entry with the full ban instead of
				// queueing.
				release := make(chan struct{})
				srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					sent = append(sent, time.Since(start))
					first := len(sent) == 1
					mu.Unlock()
					if first {
						<-release
						w.Header().Set("Retry-After", tt.retryAfter)
						w.WriteHeader(http.StatusTooManyRequests)
						return
					}
					_, _ = w.Write(body)
				}))
				q, err := raydium.NewQuoter(srv.Client(), srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				var refused []time.Duration
				for range 4 {
					wg.Go(func() {
						_, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage)
						if rl, ok := errors.AsType[*raydium.RateLimitedError](err); ok {
							mu.Lock()
							refused = append(refused, rl.RetryAfter)
							mu.Unlock()
						} else if err != nil {
							t.Errorf("Quote: %v", err)
						}
					})
				}
				// All four are now blocked: one in the held request, three
				// on their slot timers.
				synctest.Wait()
				close(release)
				wg.Wait()
				slices.Sort(refused)
				if !slices.Equal(sent, tt.wantSent) || !slices.Equal(refused, tt.wantErrs) {
					t.Fatalf("sent at %v, refused with %v; want %v and %v", sent, refused, tt.wantSent, tt.wantErrs)
				}
				// The first caller once the ban is over goes at once.
				synctest.Sleep(start.Add(time.Minute).Sub(time.Now()))
				before := time.Now()
				if _, err := q.Quote(t.Context(), req.in, req.out, req.amount, req.slippage); err != nil || time.Since(before) != 0 {
					t.Fatalf("after the ban: %v, waited %v", err, time.Since(before))
				}
			})
		})
	}
}

func serveKeys(t *testing.T, status int, body []byte) (*raydium.Quoter, *atomic.Int32, *atomic.Pointer[http.Request]) {
	t.Helper()
	var hits atomic.Int32
	var last atomic.Pointer[http.Request]
	srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		last.Store(r)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	q, err := raydium.NewQuoter(srv.Client(), srv.URL, raydium.WithPoolKeysURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return q, &hits, &last
}
