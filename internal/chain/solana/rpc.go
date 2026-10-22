package solana

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
)

const (
	// maxResponseBytes bounds what one RPC answer may make this process read.
	// The SDK decodes whatever arrives; a node that streams without end would
	// otherwise be bounded only by the client timeout. The largest legitimate
	// answer here is a token-account listing, sliced to 72 bytes per account,
	// which stays far below this for any wallet with fewer than ~40,000 token
	// accounts.
	maxResponseBytes = 8 << 20

	// Public Solana RPC allows about 100 requests per 10 s per IP and 40 per
	// 10 s per method. An endpoint without a credential is treated as public:
	// one request per 100 ms overall and one per 250 ms per method stays
	// inside both, with balance reads spread over two workers.
	publicInterval           = 100 * time.Millisecond
	publicMethodInterval     = 250 * time.Millisecond
	balanceConcurrencyPublic = 2
	balanceConcurrencyKeyed  = 8
)

// errResponseTooLarge reports an answer larger than maxResponseBytes.
var errResponseTooLarge = errors.New("Solana RPC answer is too large")

// limitedBody fails the read that crosses the limit instead of silently
// truncating, so a cut answer is never decoded as a complete one.
type limitedBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		// One byte past the limit tells "exactly the limit" from "more".
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.body.Close() }

// limitedTransport caps every response body read through it.
type limitedTransport struct{ base http.RoundTripper }

func (t limitedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &limitedBody{body: response.Body, remaining: maxResponseBytes}
	return response, nil
}

// boundedClient wraps the guarded client the resolver handed out. It keeps the
// guarded transport, timeout and redirect policy and only adds the size cap;
// the provider's client itself is not modified.
func boundedClient(client *http.Client) (*http.Client, error) {
	if client == nil || client.Transport == nil {
		// Without the provider's transport the request would leave through
		// http.DefaultTransport, past the address policy every RPC connection
		// is held to.
		return nil, errors.New("a guarded HTTP client is required for Solana RPC")
	}
	return &http.Client{
		Transport:     limitedTransport{base: client.Transport},
		Timeout:       client.Timeout,
		CheckRedirect: client.CheckRedirect,
		Jar:           client.Jar,
	}, nil
}

// requestHeaders is the endpoint's credential set without the two headers the
// JSON-RPC exchange depends on. The SDK applies custom headers last, so a stored
// Content-Type would otherwise replace the one the node needs.
func requestHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	out := headers.Clone()
	out.Del("Content-Type")
	out.Del("Accept")
	if len(out) == 0 {
		return nil
	}
	return out
}

// newRPC builds an SDK client for one endpoint over the bounded guarded client.
func newRPC(endpoint string, headers http.Header, httpClient *http.Client) (*rpc.Client, error) {
	bounded, err := boundedClient(httpClient)
	if err != nil {
		return nil, err
	}
	return rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(endpoint, &jsonrpc.RPCClientOpts{
		HTTPClient: bounded, CustomHeader: requestHeaders(headers),
	})), nil
}

// pacer spaces requests to one public endpoint. Each caller reserves the next
// free instant and waits for it, so concurrent callers queue instead of
// bursting.
type pacer struct {
	mu   sync.Mutex
	next map[string]time.Time
}

func (p *pacer) wait(ctx context.Context, method string) error {
	p.mu.Lock()
	if p.next == nil {
		p.next = make(map[string]time.Time)
	}
	now := time.Now()
	at := now
	if global := p.next[""]; global.After(at) {
		at = global
	}
	if perMethod := p.next[method]; perMethod.After(at) {
		at = perMethod
	}
	p.next[""] = at.Add(publicInterval)
	p.next[method] = at.Add(publicMethodInterval)
	p.mu.Unlock()
	delay := at.Sub(now)
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// node is one verified endpoint.
type node struct {
	endpoint string
	client   *rpc.Client
	// keyed nodes carry a provider credential and are not paced: the public
	// limits do not describe them.
	keyed bool
	pacer *pacer
}

// nodes is the verified endpoint set of one network, in the resolver's order.
type nodes struct {
	list      []*node
	preferred atomic.Int64
	slots     chan struct{}
}

func newNodes(list []*node) *nodes {
	workers := balanceConcurrencyPublic
	if list[0].keyed {
		workers = balanceConcurrencyKeyed
	}
	return &nodes{list: list, slots: make(chan struct{}, workers)}
}

// do runs call against the preferred node and fails over to the next one when
// the node did not give a usable answer. A request the node judged malformed is
// not retried elsewhere: every node would judge it the same way.
func (n *nodes) do(ctx context.Context, method string, call func(*rpc.Client) error) error {
	start := int(n.preferred.Load())
	var failures []error
	for i := range n.list {
		index := (start + i) % len(n.list)
		current := n.list[index]
		if !current.keyed {
			if err := current.pacer.wait(ctx, method); err != nil {
				return err
			}
		}
		err := readable(call(current.client))
		if err == nil {
			if index != start {
				n.preferred.Store(int64(index))
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%s: %w", method, ctxErr)
		}
		if !retryable(err) {
			return fmt.Errorf("%s: %w", method, err)
		}
		failures = append(failures, fmt.Errorf("%s: %w", method, err))
	}
	return errors.Join(failures...)
}

// rpcFailure is a JSON-RPC error object the node answered with. The SDK's own
// text for it is a debug dump of the struct, which is not something to show a
// person; the original stays reachable for errors.As.
type rpcFailure struct{ cause *jsonrpc.RPCError }

func (e rpcFailure) Error() string {
	return fmt.Sprintf("node answered %d: %s", e.cause.Code, e.cause.Message)
}

func (e rpcFailure) Unwrap() error { return e.cause }

func readable(err error) error {
	if rpcErr, ok := errors.AsType[*jsonrpc.RPCError](err); ok && rpcErr != nil {
		return rpcFailure{cause: rpcErr}
	}
	return err
}

// retryable reports whether another node might answer where this one did not.
// Only the JSON-RPC codes for a request that is itself wrong are final.
func retryable(err error) bool {
	if rpcErr, ok := errors.AsType[*jsonrpc.RPCError](err); ok {
		switch rpcErr.Code {
		case -32600, -32601, -32602:
			return false
		}
	}
	return true
}

// acquire takes a balance-read slot, shared by every stream of the network,
// unless ctx ends first.
func (n *nodes) acquire(ctx context.Context) (func(), error) {
	select {
	case n.slots <- struct{}{}:
		return func() { <-n.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// once sends a call to the preferred node only. A broadcast is not repeated
// on another node: whether the first one took it is exactly what is unknown.
func (n *nodes) once(ctx context.Context, method string, call func(*rpc.Client) error) error {
	current := n.list[int(n.preferred.Load())]
	if !current.keyed {
		if err := current.pacer.wait(ctx, method); err != nil {
			return err
		}
	}
	if err := readable(call(current.client)); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	return nil
}
