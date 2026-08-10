// Package httpapi serves the JSON API and the embedded single-page UI.
package httpapi

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/sxwebdev/xutils/randutil"
)

//go:embed ui
var uiFS embed.FS

const (
	// requestTimeout bounds every on-chain operation triggered from the UI.
	requestTimeout = 45 * time.Second
	// deployTimeout covers a deployment, which waits for its receipt on top of
	// building and broadcasting: a contract that ran out of energy is only
	// visible once the transaction is in a block.
	deployTimeout = 90 * time.Second
	// A large portfolio can legitimately take longer than the ordinary request
	// timeout on rate-limited public nodes. The stream stays bounded, while
	// still allowing the browser to cancel it immediately.
	balanceStreamTimeout = 5 * time.Minute
)

const (
	// maxBodyBytes caps request bodies. Every payload here is a handful of short
	// fields, so anything larger is a mistake or an attempt to exhaust memory.
	maxBodyBytes = 64 << 10
	// maxDeployBodyBytes caps the one payload that is not: a contract travels as
	// hex, so it is twice its own size, and the ABI rides along with it. The
	// chain's own limit on deployed code is a few tens of kilobytes.
	maxDeployBodyBytes = 512 << 10
)

// bodyLimit is how much of a request body will be read. Only a deployment needs
// more than the default, and giving every endpoint its allowance would mean a
// single POST could park half a megabyte of nonsense in memory before the
// address on it is even looked at.
func bodyLimit(path string) int64 {
	// Whole final segments, not a substring: matching "/deploy" anywhere would
	// hand the raised limit to "…/deploy-anything" and to any later route that
	// merely embeds the word.
	if strings.HasSuffix(path, "/deploy") || strings.HasSuffix(path, "/deploy-estimate") {
		return maxDeployBodyBytes
	}

	return maxBodyBytes
}

// TokenHeader carries the capability token on every API request.
const TokenHeader = "X-Walletspace-Token" //nolint:gosec // header name, not a credential

// contentSecurityPolicy is a second barrier behind contextual escaping. On-chain
// metadata (a token symbol, a contract name) is attacker-controlled text that
// reaches the DOM, and both fragment sinks in the UI go through
// createContextualFragment, which — unlike innerHTML — executes a <script> it
// parses. An escaping regression would therefore be code execution in the
// wallet's own origin, so the policy denies everything the UI does not need.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// NewToken mints a capability token for one run of the process.
//
// Loopback is a network route, not an authority boundary: every local process,
// and every web page that can trick a browser into resolving a name to
// 127.0.0.1, reaches the same socket. The token is what actually separates the
// UI this process launched from everything else on the machine.
func NewToken() (string, error) {
	token, err := randutil.GenerateKey(randutil.RecommendedKeySize)
	if err != nil {
		return "", fmt.Errorf("generate capability token: %w", err)
	}

	return token, nil
}

// Access is the authorization boundary in front of the API: the capability
// token callers must present, and the exact set of Host values that correspond
// to the socket actually opened.
type Access struct {
	Token string
	Hosts []string
}

// LoopbackAccess describes the boundary for a listener bound to loopback.
//
// The Host header is chosen by the caller, so it can only be trusted once it is
// checked against an address we know we own. Taking the port from the live
// net.Listener rather than from configuration also keeps the two in step when
// the configured port is 0 and the kernel picks one.
func LoopbackAccess(token string, addr net.Addr) (Access, error) {
	if token == "" {
		return Access{}, fmt.Errorf("capability token is required")
	}
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return Access{}, fmt.Errorf("split listen address %q: %w", addr, err)
	}

	// Every loopback spelling of the port we opened, and nothing else. A
	// rebinding attack arrives with the attacker's own hostname in Host, which
	// is not on this list however the DNS answer was rigged.
	hosts := make([]string, 0, 6)
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		hosts = append(hosts, net.JoinHostPort(host, port))
		// A browser leaves the default port out of the Host header, so a UI
		// pinned to :80 would otherwise send a Host none of the entries above
		// match — and the application would 403 its own printed URL. Added only
		// for port 80, so this does not widen anything for the usual case. An
		// IPv6 literal keeps its brackets even without a port.
		if port == "80" {
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			hosts = append(hosts, host)
		}
	}

	return Access{Token: token, Hosts: hosts}, nil
}

func (a Access) allowsHost(host string) bool {
	for _, allowed := range a.Hosts {
		if strings.EqualFold(host, allowed) {
			return true
		}
	}

	return false
}

// allowsOrigin compares the whole origin. Matching only host:port would accept
// https://<our host> from a page that is not ours, and the scheme is part of
// what makes an origin.
func (a Access) allowsOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" {
		return false
	}

	return a.allowsHost(parsed.Host)
}

func (a Access) authorized(r *http.Request) bool {
	presented := r.Header.Get(TokenHeader)
	if a.Token == "" || presented == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(presented), []byte(a.Token)) == 1
}

func securityHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Security-Policy", contentSecurityPolicy)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
}

// guard is the authorization boundary for the local API.
//
// The service holds spendable keys, so neither the loopback bind nor the
// browser's own headers can be the thing that decides who may drive it:
//
//   - Host is checked against the addresses this process actually listens on.
//     That is what stops DNS rebinding, where a page the user visits keeps its
//     own hostname while the name resolves to 127.0.0.1, making every
//     header-based check agree that the request is same-origin.
//   - Origin and Fetch Metadata on protected /api/ requests must describe this
//     same origin. The public UI itself remains navigable from the launcher's
//     file:// redirect before it has a chance to present the token.
//   - A capability token is required on every /api/ route, reads included. The
//     static UI is served without one: it is not secret, and a browser cannot
//     set a header on a navigation.
//
// Fetch metadata and the JSON content type stay on as CSRF hardening. They are
// defence in depth, not authentication — an ordinary local process sets
// whichever headers it likes.
func (a Access) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		cleanedPath := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
		apiRequest := strings.HasPrefix(cleanedPath, "/api/")

		if !a.allowsHost(r.Host) {
			writeError(w, http.StatusForbidden, "unexpected Host header")
			return
		}

		if apiRequest {
			if origin := r.Header.Get("Origin"); origin != "" && !a.allowsOrigin(origin) {
				writeError(w, http.StatusForbidden, "cross-origin requests are not allowed")
				return
			}

			switch r.Header.Get("Sec-Fetch-Site") {
			case "", "same-origin", "none":
			default:
				writeError(w, http.StatusForbidden, "cross-site requests are not allowed")
				return
			}
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Required even from a request with no body at all. A cross-site
			// POST that sets no Content-Type is a CORS simple request, which a
			// browser sends without asking permission first; demanding JSON is
			// what forces the preflight that then fails. Accepting a missing
			// header would leave the bodyless endpoints — withdraw and
			// cancel-unstakes, both of which move staked TRX — resting on the
			// Sec-Fetch-Site and Origin checks alone.
			media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || media != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "expected Content-Type: application/json")
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(r.URL.Path))
		}

		// Reads are guarded too. A balance list names every address in a space,
		// and the streaming endpoint is a GET, so leaving reads open would hand
		// the whole portfolio to any local process that can guess the port.
		//
		// The prefix is matched on the cleaned path so that "//api/spaces" or
		// "/api/../api/spaces" cannot slip past the check and then be tidied up
		// into a real route by the mux.
		if apiRequest && !a.authorized(r) {
			writeError(w, http.StatusUnauthorized, "missing or invalid capability token")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already out; nothing left but to note it.
		slog.Default().Debug("write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
