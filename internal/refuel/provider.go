// Package refuel quotes small native-coin top-ups for a specific token transfer.
package refuel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/hostpolicy"
)

const DepositAddress = "0x391E7C679d29bD940d63be94AD22A25d25b5A604"

var ErrUnavailable = errors.New("gas top-up route unavailable")

// Provider implementations return only native coin deposits; no token approvals.
type Provider interface {
	Quote(context.Context, Request) (Quote, error)
	Delivery(context.Context, string, int64, string) (Delivery, error)
}

type Request struct {
	SourceChain      int64
	DestinationChain int64
	From             string
	Recipient        string
	Needed           *big.Int
}

type Quote struct {
	Provider       string          `json:"provider"`
	To             string          `json:"to"`
	Data           string          `json:"data"`
	Deposit        string          `json:"deposit"`  // source base units
	Expected       string          `json:"expected"` // destination base units
	DepositUSD     decimal.Decimal `json:"deposit_usd"`
	SourcePriceUSD decimal.Decimal `json:"source_price_usd"`
	MinimumLimited bool            `json:"minimum_limited"`
	ExpiresAt      time.Time       `json:"expires_at"`
}

type Delivery struct {
	Hash   string
	Status string // pending, delivered, cancelled
}

type Options struct {
	BaseURL string
	Client  *http.Client
	Now     func() time.Time
}
type GasZip struct {
	base        string
	client      *http.Client
	now         func() time.Time
	chainsMu    sync.Mutex
	chains      []gasChain
	chainsUntil time.Time
}

func New(options Options) *GasZip {
	if options.BaseURL == "" {
		options.BaseURL = "https://backend.gas.zip/v2"
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Client == nil {
		// Pin DNS answers at dial time and never send through a proxy or redirect.
		transport := &http.Transport{TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 8 * time.Second,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if hostpolicy.PublicIP(ip) {
						return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
					}
				}
				return nil, errors.New("gas provider resolved to a private address")
			},
		}
		options.Client = &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("gas provider redirects disabled") }}
	}
	return &GasZip{base: strings.TrimRight(options.BaseURL, "/"), client: options.Client, now: options.Now}
}

// The source deposit address is pinned to the provider's published deployments.
// A new network cannot become spendable merely because an API response names it.
func SupportedSource(chainID int64) bool {
	switch chainID {
	case 1, 10, 56, 137, 8217, 8453, 42161, 43114:
		return true
	default:
		return false
	}
}

type gasChain struct {
	Chain       int64           `json:"chain"`
	Short       uint16          `json:"short"`
	Inbound     bool            `json:"inbound"`
	Mainnet     bool            `json:"mainnet"`
	Decimals    uint8           `json:"decimals"`
	MinInbound  decimal.Decimal `json:"minInboundNative"`
	MinOutbound decimal.Decimal `json:"minOutboundNative"`
	MaxInbound  decimal.Decimal `json:"maxInboundNative"`
	Price       decimal.Decimal `json:"price"`
}

func integer(v decimal.Decimal) (*big.Int, error) {
	if v.IsNegative() || !v.Equal(v.Truncate(0)) || v.BigInt().BitLen() > 256 {
		return nil, fmt.Errorf("%w: invalid base-unit amount", ErrUnavailable)
	}
	return v.BigInt(), nil
}

func ceilRatio(value *big.Int, numerator, denominator int64) *big.Int {
	result := new(big.Int).Mul(value, big.NewInt(numerator))
	result.Add(result, big.NewInt(denominator-1))
	return result.Quo(result, big.NewInt(denominator))
}

func (g *GasZip) Quote(ctx context.Context, req Request) (Quote, error) {
	if !SupportedSource(req.SourceChain) || req.SourceChain == req.DestinationChain || req.Needed == nil || req.Needed.Sign() <= 0 || req.Needed.BitLen() > 256 ||
		!common.IsHexAddress(req.From) || !common.IsHexAddress(req.Recipient) || common.HexToAddress(req.Recipient) == (common.Address{}) {
		return Quote{}, fmt.Errorf("%w: invalid native top-up route", ErrUnavailable)
	}
	list, err := g.chainList(ctx)
	if err != nil {
		return Quote{}, err
	}
	var source, destination *gasChain
	for i := range list {
		c := &list[i]
		if c.Chain == req.SourceChain {
			source = c
		}
		if c.Chain == req.DestinationChain {
			destination = c
		}
	}
	if source == nil || destination == nil || !source.Inbound || !source.Mainnet || !destination.Mainnet || source.Decimals != 18 || destination.Decimals != 18 || destination.Short == 0 || !source.Price.IsPositive() {
		return Quote{}, fmt.Errorf("%w: network not supported", ErrUnavailable)
	}
	minimum, err := integer(destination.MinOutbound)
	if err != nil {
		return Quote{}, err
	}
	wanted := new(big.Int).Set(req.Needed)
	minimumLimited := minimum.Cmp(wanted) > 0
	if minimumLimited {
		wanted = ceilRatio(minimum, 10001, 10000)
	}
	var reverse struct {
		Chain    int64           `json:"chain"`
		Required decimal.Decimal `json:"required"`
	}
	path := fmt.Sprintf("/quoteReverse/%d/%s/%d", req.SourceChain, wanted, req.DestinationChain)
	if err := g.get(ctx, path, &reverse); err != nil {
		return Quote{}, err
	}
	deposit, err := integer(reverse.Required)
	if err != nil {
		return Quote{}, err
	}
	if reverse.Chain != req.DestinationChain || deposit.Sign() == 0 {
		return Quote{}, fmt.Errorf("%w: invalid reverse quote", ErrUnavailable)
	}
	sourceMin, err := integer(source.MinInbound)
	if err != nil {
		return Quote{}, err
	}
	if sourceMin.Cmp(deposit) > 0 {
		deposit.Set(sourceMin)
		minimumLimited = true
	}
	// API chain limits are floating-point numbers on the provider side. A 0.01%
	// rounding margin keeps their rounded minimum from being rejected on re-quote.
	deposit = ceilRatio(deposit, 10001, 10000)
	sourceMax, err := integer(source.MaxInbound)
	if err != nil {
		return Quote{}, err
	}
	if sourceMax.Sign() <= 0 || deposit.Cmp(sourceMax) > 0 {
		return Quote{}, fmt.Errorf("%w: provider deposit limit exceeded", ErrUnavailable)
	}
	query := url.Values{"from": {common.HexToAddress(req.From).Hex()}, "to": {common.HexToAddress(req.Recipient).Hex()}}
	var forward struct {
		Calldata string `json:"calldata"`
		Expires  int64  `json:"expires"`
		Quotes   []struct {
			Chain    int64           `json:"chain"`
			Expected decimal.Decimal `json:"expected"`
			USD      decimal.Decimal `json:"usd"`
			Error    string          `json:"error"`
		} `json:"quotes"`
	}
	if err := g.get(ctx, fmt.Sprintf("/quotes/%d/%s/%d?%s", req.SourceChain, deposit, req.DestinationChain, query.Encode()), &forward); err != nil {
		return Quote{}, err
	}
	if len(forward.Quotes) != 1 || forward.Quotes[0].Chain != req.DestinationChain || forward.Quotes[0].Error != "" {
		return Quote{}, fmt.Errorf("%w: incomplete outbound quote", ErrUnavailable)
	}
	expected, err := integer(forward.Quotes[0].Expected)
	if err != nil {
		return Quote{}, err
	}
	if expected.Cmp(req.Needed) < 0 {
		return Quote{}, fmt.Errorf("%w: output does not cover the transfer fee; refresh quote", ErrUnavailable)
	}
	calldata, err := hex.DecodeString(strings.TrimPrefix(forward.Calldata, "0x"))
	if err != nil {
		return Quote{}, fmt.Errorf("%w: invalid deposit data", ErrUnavailable)
	}
	// Validate the entire payload locally: exactly one chain and exactly this
	// recipient. Provider data can never add a second recipient or arbitrary call.
	want := []byte{2}
	want = append(want, common.HexToAddress(req.Recipient).Bytes()...)
	if strings.EqualFold(req.From, req.Recipient) {
		want = []byte{1}
	}
	want = append(want, byte(destination.Short>>8), byte(destination.Short))
	if !bytes.Equal(calldata, want) {
		return Quote{}, fmt.Errorf("%w: deposit recipient or destination does not match", ErrUnavailable)
	}
	expires := time.Unix(forward.Expires, 0).UTC()
	if expires.After(g.now().Add(45 * time.Second)) {
		expires = g.now().Add(45 * time.Second)
	}
	if !expires.After(g.now().Add(5 * time.Second)) {
		return Quote{}, fmt.Errorf("%w: quote expired", ErrUnavailable)
	}
	return Quote{Provider: "Gas.zip", To: DepositAddress, Data: "0x" + hex.EncodeToString(want), Deposit: deposit.String(), Expected: expected.String(),
		SourcePriceUSD: source.Price, DepositUSD: decimal.NewFromBigInt(deposit, -18).Mul(source.Price), MinimumLimited: minimumLimited, ExpiresAt: expires}, nil
}

func (g *GasZip) Delivery(ctx context.Context, hash string, destination int64, recipient string) (Delivery, error) {
	if len(hash) != 66 || !strings.HasPrefix(hash, "0x") {
		return Delivery{}, fmt.Errorf("%w: invalid deposit hash", ErrUnavailable)
	}
	if _, err := hex.DecodeString(hash[2:]); err != nil {
		return Delivery{}, fmt.Errorf("%w: invalid deposit hash", ErrUnavailable)
	}
	var response struct {
		Deposit struct {
			Status string `json:"status"`
		} `json:"deposit"`
		Txs []struct {
			Chain     int64  `json:"chain"`
			To        string `json:"to"`
			Hash      string `json:"hash"`
			Status    string `json:"status"`
			Cancelled bool   `json:"cancelled"`
			Refund    bool   `json:"refund"`
		} `json:"txs"`
	}
	if err := g.get(ctx, "/deposit/"+hash, &response); err != nil {
		return Delivery{}, err
	}
	if response.Deposit.Status == "CANCELLED" {
		return Delivery{Status: "cancelled"}, nil
	}
	for _, tx := range response.Txs {
		if tx.Chain == destination && strings.EqualFold(tx.To, recipient) && !tx.Refund && !tx.Cancelled && tx.Status == "CONFIRMED" {
			return Delivery{Status: "delivered", Hash: tx.Hash}, nil
		}
	}
	return Delivery{Status: "pending"}, nil
}

func (g *GasZip) get(ctx context.Context, path string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.base+path, nil)
	if err != nil {
		return err
	}
	response, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: request failed: %w", ErrUnavailable, err)
	}
	defer response.Body.Close()
	const limit = 2 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || len(body) > limit {
		return fmt.Errorf("%w: HTTP %s", ErrUnavailable, strconv.Itoa(response.StatusCode))
	}
	var apiError struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &apiError); err != nil {
		return fmt.Errorf("%w: invalid provider response", ErrUnavailable)
	}
	if apiError.Error != "" {
		return fmt.Errorf("%w: %s", ErrUnavailable, apiError.Error)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("%w: invalid provider response: %w", ErrUnavailable, err)
	}
	return nil
}

func (g *GasZip) chainList(ctx context.Context) ([]gasChain, error) {
	g.chainsMu.Lock()
	defer g.chainsMu.Unlock()
	if g.now().Before(g.chainsUntil) {
		return g.chains, nil
	}
	var response struct {
		Chains []gasChain `json:"chains"`
	}
	if err := g.get(ctx, "/chains", &response); err != nil {
		return nil, err
	}
	g.chains = response.Chains
	g.chainsUntil = g.now().Add(time.Minute)
	return g.chains, nil
}
