package raydium

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/bits"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
)

// DefaultBaseURL is the Raydium Trade API. It needs no key; its documentation
// allows anonymous use and limits quotes to 120 requests a minute per IP.
const DefaultBaseURL = "https://transaction-v1.raydium.io"

const (
	// QuoteInterval is the least time between two quote requests: 2 a
	// second, half of what the API allows a minute.
	QuoteInterval = 500 * time.Millisecond
	// MaxQuoteResponse bounds the body read from the API; a quote is about
	// a kilobyte.
	MaxQuoteResponse = 64 << 10
	// MaxHops is the longest route accepted.
	MaxHops = 2

	defaultRetryAfter = time.Minute
	maxRetryAfter     = 10 * time.Minute
	maxErrorBody      = 256
	bpsDenominator    = 10_000
)

var (
	// ErrNoRoute means the API found no route between the two mints.
	ErrNoRoute = errors.New("raydium: no route")
	// ErrAPI means the API answered with an error.
	ErrAPI = errors.New("raydium: trade API error")
	// ErrRateLimited means the API asked to slow down, or the client is
	// still waiting out such a request. See RateLimitedError.
	ErrRateLimited = errors.New("raydium: trade API rate limit")
	// ErrMalformedRoute means the API's answer is not a route this package
	// can trust: a field is missing or malformed, or the route does not
	// connect the requested mints with the requested amount and slippage.
	ErrMalformedRoute = errors.New("raydium: malformed route")
	// ErrUnsupportedRoute means the route is well formed but goes through
	// something this wallet does not swap on: more than MaxHops hops, or a
	// program other than AMM v4, CPMM and CLMM.
	ErrUnsupportedRoute = errors.New("raydium: unsupported route")
)

// RateLimitedError is a 429 from the API, or a quote refused locally while the
// last 429's Retry-After has not passed.
type RateLimitedError struct {
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("%v: retry after %v", ErrRateLimited, e.RetryAfter)
}

func (e *RateLimitedError) Unwrap() error { return ErrRateLimited }

// Route is a quote from the Trade API, checked against the request.
type Route struct {
	// ID is the API's handle of the quote; informational.
	ID         string
	InputMint  solana.PublicKey
	OutputMint solana.PublicKey
	AmountIn   uint64
	// AmountOut is the output the API expects.
	AmountOut uint64
	// MinAmountOut is the API's otherAmountThreshold: AmountOut less the
	// requested slippage.
	MinAmountOut   uint64
	SlippageBps    uint16
	PriceImpactPct float64
	Hops           []Hop
}

// Hop is one pool of a route.
type Hop struct {
	Pool solana.PublicKey
	// Program is the pool's program when the API names it; otherwise the
	// zero key, and the caller learns it from the pool account's owner.
	Program    solana.PublicKey
	InputMint  solana.PublicKey
	OutputMint solana.PublicKey
	FeeMint    solana.PublicKey
	FeeAmount  uint64
	// FeeRateBps is the pool's fee as the API states it; informational.
	FeeRateBps float64
	// RemainingAccounts are the CLMM tick arrays the API chose; check them
	// with CLMMPool.CheckTickArrays before use.
	RemainingAccounts []solana.PublicKey
}

// Quoter asks the Trade API for exact-input routes, at most one request per
// QuoteInterval, and stops asking while a 429's Retry-After lasts.
type Quoter struct {
	client      *http.Client
	baseURL     *url.URL
	poolKeysURL *url.URL

	mu           sync.Mutex
	next         time.Time
	blockedUntil time.Time
}

// QuoterOption configures a Quoter.
type QuoterOption func(*quoterOptions)

type quoterOptions struct{ poolKeysURL string }

// WithPoolKeysURL points LookupTables at another pool-keys API than
// DefaultPoolKeysURL.
func WithPoolKeysURL(url string) QuoterOption {
	return func(o *quoterOptions) { o.poolKeysURL = url }
}

// NewQuoter returns a quoter that sends its requests through client — the
// caller's guarded client; nil is refused rather than replaced by
// http.DefaultClient — to baseURL, or to DefaultBaseURL when it is empty.
func NewQuoter(client *http.Client, baseURL string, options ...QuoterOption) (*Quoter, error) {
	if client == nil {
		return nil, errors.New("raydium: NewQuoter needs an HTTP client")
	}
	var o quoterOptions
	for _, option := range options {
		option(&o)
	}
	base, err := parseBase(cmp.Or(baseURL, DefaultBaseURL))
	if err != nil {
		return nil, err
	}
	keys, err := parseBase(cmp.Or(o.poolKeysURL, DefaultPoolKeysURL))
	if err != nil {
		return nil, err
	}
	return &Quoter{client: client, baseURL: base, poolKeysURL: keys}, nil
}

func parseBase(raw string) (*url.URL, error) {
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("raydium: base URL: %w", err)
	}
	if (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return nil, fmt.Errorf("raydium: base URL %q is not an http(s) URL", raw)
	}
	return base, nil
}

// Quote asks for a route selling amountIn of inputMint for outputMint with at
// most slippageBps of slippage, and checks that the answer is that route.
func (q *Quoter) Quote(ctx context.Context, inputMint, outputMint solana.PublicKey, amountIn uint64, slippageBps uint16) (Route, error) {
	if inputMint == outputMint {
		return Route{}, fmt.Errorf("raydium: cannot swap %s for itself", inputMint)
	}
	if amountIn == 0 {
		return Route{}, errors.New("raydium: amount is zero")
	}
	if slippageBps >= bpsDenominator {
		return Route{}, fmt.Errorf("raydium: slippage %d bps", slippageBps)
	}
	endpoint := q.baseURL.JoinPath("compute", "swap-base-in")
	endpoint.RawQuery = url.Values{
		"inputMint":   {inputMint.String()},
		"outputMint":  {outputMint.String()},
		"amount":      {strconv.FormatUint(amountIn, 10)},
		"slippageBps": {strconv.FormatUint(uint64(slippageBps), 10)},
		"txVersion":   {"LEGACY"},
	}.Encode()
	body, err := q.get(ctx, endpoint, "quote")
	if err != nil {
		return Route{}, err
	}
	return parseRoute(body, inputMint, outputMint, amountIn, slippageBps)
}

// get sends one paced GET and returns the body of a 200 answer of at most
// MaxQuoteResponse bytes; a 429 starts the Retry-After ban.
func (q *Quoter) get(ctx context.Context, endpoint *url.URL, what string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("raydium: %s request: %w", what, err)
	}
	req.Header.Set("Accept", "application/json")
	if err := q.wait(ctx); err != nil {
		return nil, err
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("raydium: %s request: %w", what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxQuoteResponse+1))
	if err != nil {
		return nil, fmt.Errorf("raydium: reading %s: %w", what, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, q.rateLimited(resp.Header.Get("Retry-After"))
	}
	if len(body) > MaxQuoteResponse {
		return nil, fmt.Errorf("%w: response larger than %d bytes", ErrAPI, MaxQuoteResponse)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrAPI, resp.StatusCode, snippet(body))
	}
	return body, nil
}

// wait takes the next request slot, sleeping until it comes, or fails at once
// while a Retry-After lasts. A caller that took its slot before a 429 arrived
// checks again when the slot comes, so no request goes out during the ban.
func (q *Quoter) wait(ctx context.Context) error {
	q.mu.Lock()
	now := time.Now()
	if err := q.blocked(now); err != nil {
		q.mu.Unlock()
		return err
	}
	slot := q.next
	if slot.Before(now) {
		slot = now
	}
	q.next = slot.Add(QuoteInterval)
	q.mu.Unlock()
	delay := slot.Sub(now)
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.blocked(time.Now())
	}
}

// blocked returns a RateLimitedError while the last Retry-After lasts. The
// caller holds q.mu.
func (q *Quoter) blocked(now time.Time) error {
	if now.Before(q.blockedUntil) {
		return &RateLimitedError{RetryAfter: q.blockedUntil.Sub(now)}
	}
	return nil
}

// rateLimited records a 429: no request is sent until Retry-After (seconds or
// an HTTP date; a minute when missing or unreadable, at most ten) has passed.
func (q *Quoter) rateLimited(header string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	wait := defaultRetryAfter
	if seconds, err := strconv.ParseUint(strings.TrimSpace(header), 10, 32); err == nil {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(header); err == nil {
		wait = max(at.Sub(now), 0)
	}
	wait = min(wait, maxRetryAfter)
	// Slots already handed out stay booked: those inside the ban are
	// forfeited by their holders, who find the ban when they wake, and are in
	// the past once it ends, so the first caller after the ban goes at once;
	// those beyond the ban keep the pacing.
	q.blockedUntil = now.Add(wait)
	return &RateLimitedError{RetryAfter: wait}
}

func snippet(body []byte) string {
	s := strings.ToValidUTF8(string(body), "?")
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "…"
	}
	return s
}

type apiResponse struct {
	ID      string    `json:"id"`
	Success bool      `json:"success"`
	Msg     string    `json:"msg"`
	Data    *apiQuote `json:"data"`
}

type apiQuote struct {
	SwapType             string   `json:"swapType"`
	InputMint            string   `json:"inputMint"`
	InputAmount          string   `json:"inputAmount"`
	OutputMint           string   `json:"outputMint"`
	OutputAmount         string   `json:"outputAmount"`
	OtherAmountThreshold string   `json:"otherAmountThreshold"`
	SlippageBps          *int64   `json:"slippageBps"`
	PriceImpactPct       float64  `json:"priceImpactPct"`
	ReferrerAmount       string   `json:"referrerAmount"`
	ActualInputAmount    *string  `json:"actualInputAmount"`
	RoutePlan            []apiHop `json:"routePlan"`
}

type apiHop struct {
	PoolID            string   `json:"poolId"`
	ProgramID         string   `json:"programId"`
	InputMint         string   `json:"inputMint"`
	OutputMint        string   `json:"outputMint"`
	FeeMint           string   `json:"feeMint"`
	FeeRate           float64  `json:"feeRate"`
	FeeAmount         string   `json:"feeAmount"`
	RemainingAccounts []string `json:"remainingAccounts"`
}

// parseRoute decodes an API answer and checks it is the route asked for.
// Unknown fields are ignored; duplicate fields and invalid UTF-8 are refused
// (encoding/json/v2 defaults).
func parseRoute(body []byte, inputMint, outputMint solana.PublicKey, amountIn uint64, slippageBps uint16) (Route, error) {
	var resp apiResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Route{}, fmt.Errorf("%w: %w", ErrMalformedRoute, err)
	}
	if !resp.Success {
		if resp.Msg == "ROUTE_NOT_FOUND" {
			return Route{}, ErrNoRoute
		}
		return Route{}, fmt.Errorf("%w: %s", ErrAPI, snippet([]byte(resp.Msg)))
	}
	d := resp.Data
	if d == nil {
		return Route{}, fmt.Errorf("%w: no data", ErrMalformedRoute)
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrMalformedRoute}, args...)...)
	}
	if d.SwapType != "BaseIn" {
		return Route{}, bad("swap type %q", d.SwapType)
	}
	route := Route{ID: resp.ID, SlippageBps: slippageBps, PriceImpactPct: d.PriceImpactPct}
	var err error
	if route.InputMint, err = parseKey(d.InputMint); err != nil || route.InputMint != inputMint {
		return Route{}, bad("input mint %q", d.InputMint)
	}
	if route.OutputMint, err = parseKey(d.OutputMint); err != nil || route.OutputMint != outputMint {
		return Route{}, bad("output mint %q", d.OutputMint)
	}
	if route.AmountIn, err = parseAmount(d.InputAmount); err != nil || route.AmountIn != amountIn {
		return Route{}, bad("input amount %q", d.InputAmount)
	}
	if d.ActualInputAmount != nil && *d.ActualInputAmount != d.InputAmount {
		return Route{}, bad("actual input amount %q", *d.ActualInputAmount)
	}
	if d.ReferrerAmount != "" && d.ReferrerAmount != "0" {
		return Route{}, bad("referrer amount %q", d.ReferrerAmount)
	}
	if d.SlippageBps == nil || *d.SlippageBps != int64(slippageBps) {
		return Route{}, bad("slippage is not %d bps", slippageBps)
	}
	if route.AmountOut, err = parseAmount(d.OutputAmount); err != nil || route.AmountOut == 0 {
		return Route{}, bad("output amount %q", d.OutputAmount)
	}
	if route.MinAmountOut, err = parseAmount(d.OtherAmountThreshold); err != nil || route.MinAmountOut == 0 {
		return Route{}, bad("minimum output %q", d.OtherAmountThreshold)
	}
	if floor := withSlippage(route.AmountOut, slippageBps); route.MinAmountOut < floor || route.MinAmountOut > route.AmountOut {
		return Route{}, bad("minimum output %d outside [%d, %d]", route.MinAmountOut, floor, route.AmountOut)
	}
	if len(d.RoutePlan) == 0 {
		return Route{}, bad("empty route plan")
	}
	if len(d.RoutePlan) > MaxHops {
		return Route{}, fmt.Errorf("%w: %d hops", ErrUnsupportedRoute, len(d.RoutePlan))
	}
	// With at most two hops, a hop never selling its own output and the
	// route ending at outputMint leave no room for a cycle.
	pools := map[solana.PublicKey]bool{}
	from := inputMint
	for i, raw := range d.RoutePlan {
		hop, err := parseHop(raw)
		if err != nil {
			return Route{}, fmt.Errorf("hop %d: %w", i, err)
		}
		if hop.InputMint != from {
			return Route{}, bad("hop %d sells %s, want %s", i, hop.InputMint, from)
		}
		if pools[hop.Pool] {
			return Route{}, bad("pool %s used twice", hop.Pool)
		}
		pools[hop.Pool] = true
		from = hop.OutputMint
		route.Hops = append(route.Hops, hop)
	}
	if from != outputMint {
		return Route{}, bad("route ends at %s, want %s", from, outputMint)
	}
	return route, nil
}

func parseHop(raw apiHop) (Hop, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{ErrMalformedRoute}, args...)...)
	}
	var hop Hop
	var err error
	if hop.Pool, err = parseKey(raw.PoolID); err != nil {
		return Hop{}, bad("pool %q", raw.PoolID)
	}
	if raw.ProgramID != "" {
		if hop.Program, err = parseKey(raw.ProgramID); err != nil {
			return Hop{}, bad("program %q", raw.ProgramID)
		}
		if KindOf(hop.Program) == KindUnknown {
			return Hop{}, fmt.Errorf("%w: program %s", ErrUnsupportedRoute, hop.Program)
		}
	}
	if hop.InputMint, err = parseKey(raw.InputMint); err != nil {
		return Hop{}, bad("input mint %q", raw.InputMint)
	}
	if hop.OutputMint, err = parseKey(raw.OutputMint); err != nil || hop.OutputMint == hop.InputMint {
		return Hop{}, bad("output mint %q", raw.OutputMint)
	}
	if raw.FeeMint != "" {
		if hop.FeeMint, err = parseKey(raw.FeeMint); err != nil || (hop.FeeMint != hop.InputMint && hop.FeeMint != hop.OutputMint) {
			return Hop{}, bad("fee mint %q", raw.FeeMint)
		}
	}
	if raw.FeeAmount != "" {
		if hop.FeeAmount, err = parseAmount(raw.FeeAmount); err != nil {
			return Hop{}, bad("fee amount %q", raw.FeeAmount)
		}
	}
	if raw.FeeRate < 0 || raw.FeeRate >= bpsDenominator {
		return Hop{}, bad("fee rate %v", raw.FeeRate)
	}
	hop.FeeRateBps = raw.FeeRate
	if len(raw.RemainingAccounts) > MaxTickArrays {
		return Hop{}, bad("%d remaining accounts", len(raw.RemainingAccounts))
	}
	if hop.Program != (solana.PublicKey{}) && hop.Program != CLMMProgram && len(raw.RemainingAccounts) != 0 {
		return Hop{}, bad("remaining accounts on a %s pool", KindOf(hop.Program))
	}
	seen := map[solana.PublicKey]bool{hop.Pool: true}
	for _, s := range raw.RemainingAccounts {
		key, err := parseKey(s)
		if err != nil || seen[key] {
			return Hop{}, bad("remaining account %q", s)
		}
		seen[key] = true
		hop.RemainingAccounts = append(hop.RemainingAccounts, key)
	}
	return hop, nil
}

// parseKey accepts only the canonical base58 form of a 32-byte key.
func parseKey(s string) (solana.PublicKey, error) {
	key, err := solana.PublicKeyFromBase58(s)
	if err != nil {
		return solana.PublicKey{}, err
	}
	// solana-go v1.25.0 already refuses every non-canonical spelling tried
	// in the tests; this keeps the rule if the decoder ever gets laxer.
	if key.String() != s {
		return solana.PublicKey{}, fmt.Errorf("non-canonical key %q", s)
	}
	return key, nil
}

// parseAmount accepts a decimal u64 without sign, spaces or leading zeros.
func parseAmount(s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if strconv.FormatUint(v, 10) != s {
		return 0, fmt.Errorf("non-canonical amount %q", s)
	}
	return v, nil
}

// withSlippage is floor(amount × (10000 − bps) / 10000).
func withSlippage(amount uint64, bps uint16) uint64 {
	v := new(big.Int).SetUint64(amount)
	v.Mul(v, big.NewInt(int64(bpsDenominator-int(bps))))
	return v.Quo(v, big.NewInt(bpsDenominator)).Uint64()
}

// CheckQuotedOut compares the output a quote promises with the output the
// pool's own curve gives, and refuses the quote when they differ by more
// than maxDeviationBps of the local figure. Use it for AMM v4 and CPMM hops
// (AMMV4Pool.AmountOut, CPMMPool.AmountOut); the exact output comes from
// simulation.
func CheckQuotedOut(local, quoted, maxDeviationBps uint64) error {
	diff := max(local, quoted) - min(local, quoted)
	// diff × 10000 > maxDeviationBps × local, in 128 bits.
	dHi, dLo := bits.Mul64(diff, bpsDenominator)
	lHi, lLo := bits.Mul64(local, maxDeviationBps)
	if dHi > lHi || (dHi == lHi && dLo > lLo) {
		return fmt.Errorf("%w: the quote pays %d, the pool's curve %d", ErrMalformedRoute, quoted, local)
	}
	return nil
}
