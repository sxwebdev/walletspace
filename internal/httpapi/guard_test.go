package httpapi_test

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/httpapi"
)

// guardRequest builds a request that satisfies every part of the boundary, so
// each test can break exactly one thing and see it rejected.
func guardRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var request *http.Request
	if reader == nil {
		request = httptest.NewRequestWithContext(t.Context(), method, path, nil)
	} else {
		request = httptest.NewRequestWithContext(t.Context(), method, path, reader)
	}
	request.Host = testHost
	request.Header.Set(httpapi.TokenHeader, testToken)
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func serve(t *testing.T, handler http.Handler, request *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// A page on a domain the attacker controls, whose DNS answer they flip to
// 127.0.0.1, reaches this socket with every browser-supplied signal agreeing
// that the request is same-origin: Sec-Fetch-Site says same-origin and Origin
// matches Host, because both are the attacker's own name. Only comparing Host
// against the address actually opened tells the two apart.
func TestGuardRejectsDNSRebinding(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			request := guardRequest(t, method, "/api/spaces", `{"name":"x"}`)
			request.Host = "attacker.example:8080"
			request.Header.Set("Origin", "http://attacker.example:8080")
			request.Header.Set("Sec-Fetch-Site", "same-origin")

			response := serve(t, fixture.handler, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body: %s)", response.Code, response.Body)
			}
			if len(fixture.spaces.List()) != 0 {
				t.Error("a rebound request created a space")
			}
		})
	}
}

// The port is the other half of the same defence: a rebinding page has to know
// where to aim before it can read anything back.
func TestGuardRejectsTheRightHostOnTheWrongPort(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	request := guardRequest(t, http.MethodGet, "/api/spaces", "")
	request.Host = "127.0.0.1:9999"

	if response := serve(t, fixture.handler, request); response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", response.Code, response.Body)
	}
}

// Loopback is a network route, not an authority boundary: any local process
// reaches the same socket, and it sets whichever headers it likes. The token is
// what actually separates the UI this process launched from everything else.
func TestGuardRequiresTheCapabilityToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
		set   bool
	}{
		{name: "absent"},
		{name: "empty", token: "", set: true},
		{name: "wrong", token: "not-the-token", set: true},
		{name: "prefix of the real token", token: testToken[:len(testToken)-1], set: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newPlatformFixture(t)
			// Reads are guarded too: a balance list names every address in a
			// space, and the streaming endpoint is a GET.
			for _, path := range []string{"/api/spaces", "/api/networks", "/api/settings", "/api/client.js"} {
				request := guardRequest(t, http.MethodGet, path, "")
				request.Header.Del(httpapi.TokenHeader)
				if tt.set {
					request.Header.Set(httpapi.TokenHeader, tt.token)
				}

				if response := serve(t, fixture.handler, request); response.Code != http.StatusUnauthorized {
					t.Errorf("GET %s status = %d, want 401 (body: %s)", path, response.Code, response.Body)
				}
			}

			request := guardRequest(t, http.MethodPost, "/api/spaces", `{"name":"x","password":"correct horse battery"}`)
			request.Header.Del(httpapi.TokenHeader)
			if tt.set {
				request.Header.Set(httpapi.TokenHeader, tt.token)
			}
			if response := serve(t, fixture.handler, request); response.Code != http.StatusUnauthorized {
				t.Errorf("POST status = %d, want 401 (body: %s)", response.Code, response.Body)
			}
			if len(fixture.spaces.List()) != 0 {
				t.Error("an unauthorized request created a space")
			}
		})
	}
}

// The UI itself is not secret, and a browser cannot put a header on a
// navigation — so the static files stay reachable without a token. They are
// useless without one: every call the page makes is guarded.
func TestGuardServesTheUIAndAssetsWithoutAToken(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)

	for _, path := range []string{"/", "/settings", "/app.js", "/services/client.js"} {
		request := guardRequest(t, http.MethodGet, path, "")
		request.Header.Del(httpapi.TokenHeader)

		if response := serve(t, fixture.handler, request); response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, response.Code)
		}
	}
}

// The launcher opens a 0600 file:// page which redirects to the loopback UI so
// the capability token never appears in another process's argv. Browsers mark
// that top-level navigation as cross-site; the static UI is public and must be
// allowed to load before its same-origin API requests can present the token.
func TestGuardAllowsCrossSiteNavigationToTheUI(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	request := guardRequest(t, http.MethodGet, "/", "")
	request.Header.Del(httpapi.TokenHeader)
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	request.Header.Set("Sec-Fetch-Mode", "navigate")

	response := serve(t, fixture.handler, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", response.Code, response.Body)
	}
}

func TestGuardRejectsCrossSiteAPIRead(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	request := guardRequest(t, http.MethodGet, "/api/spaces", "")
	request.Header.Set("Sec-Fetch-Site", "cross-site")

	response := serve(t, fixture.handler, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", response.Code, response.Body)
	}
}

func TestGuardRejectsCrossOriginAndCrossSite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{
			name:    "cross-site fetch",
			headers: map[string]string{"Sec-Fetch-Site": "cross-site"},
			want:    http.StatusForbidden,
		},
		{
			name:    "same-site but a different origin",
			headers: map[string]string{"Sec-Fetch-Site": "same-site"},
			want:    http.StatusForbidden,
		},
		{
			name:    "foreign Origin header",
			headers: map[string]string{"Origin": "https://evil.example"},
			want:    http.StatusForbidden,
		},
		{
			// The old guard compared host:port alone, so an https page on a
			// name that resolves to loopback matched. An origin is the scheme
			// as well as the authority.
			name:    "our authority on the wrong scheme",
			headers: map[string]string{"Origin": "https://" + testHost},
			want:    http.StatusForbidden,
		},
		{
			name:    "same-origin json is allowed through",
			headers: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + testHost},
			want:    http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := newPlatformFixture(t)
			request := guardRequest(t, http.MethodPost, "/api/spaces",
				`{"name":"x","password":"correct horse battery"}`)
			for name, value := range tt.headers {
				request.Header.Set(name, value)
			}

			if response := serve(t, fixture.handler, request); response.Code != tt.want {
				t.Fatalf("status = %d, want %d (body: %s)", response.Code, tt.want, response.Body)
			}
		})
	}
}

// The two staking endpoints that act on the whole pending queue send no body,
// so nothing sets a Content-Type for them. A POST without one is a CORS simple
// request, which a browser issues cross-site with no preflight to fail — the
// header has to be demanded even when there is nothing to describe.
func TestGuardRejectsWritesWithoutJSONContentType(t *testing.T) {
	t.Parallel()

	for _, contentType := range []string{"", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded"} {
		t.Run("content-type "+contentType, func(t *testing.T) {
			t.Parallel()

			fixture := newPlatformFixture(t)
			request := guardRequest(t, http.MethodPost, "/api/spaces", `{"name":"x"}`)
			request.Header.Del("Content-Type")
			if contentType != "" {
				request.Header.Set("Content-Type", contentType)
			}

			response := serve(t, fixture.handler, request)
			if response.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415 (body: %s)", response.Code, response.Body)
			}
		})
	}
}

func TestGuardRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	// An unbounded body would be accumulated whole before the request is even
	// looked at, which is enough to OOM a laptop.
	huge := `{"name":"` + strings.Repeat("A", 1<<20) + `"}`

	response := serve(t, fixture.handler, guardRequest(t, http.MethodPost, "/api/spaces", huge))
	if response.Code == http.StatusCreated {
		t.Fatal("an oversized body was accepted")
	}
	if len(fixture.spaces.List()) != 0 {
		t.Error("an oversized body still created a space")
	}
}

// Escaping is the first barrier against on-chain metadata reaching the DOM as
// markup; the policy is the second, and it has to be on every response because
// the document itself is what the browser applies it to.
func TestGuardSetsSecurityHeaders(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)

	for _, path := range []string{"/", "/settings", "/api/networks"} {
		response := serve(t, fixture.handler, guardRequest(t, http.MethodGet, path, ""))

		policy := response.Header().Get("Content-Security-Policy")
		for _, directive := range []string{
			"default-src 'none'", "script-src 'self'", "style-src 'self'",
			"frame-ancestors 'none'", "base-uri 'none'",
		} {
			if !strings.Contains(policy, directive) {
				t.Errorf("%s: CSP %q is missing %q", path, policy, directive)
			}
		}
		if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", path, got)
		}
		if got := response.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s: Referrer-Policy = %q, want no-referrer", path, got)
		}
	}
}

// A handler built without a boundary would serve spendable keys to anyone, so
// the mistake has to be fatal at construction rather than at the first request.
func TestNewPlatformRefusesAnEmptyBoundary(t *testing.T) {
	t.Parallel()

	if _, err := httpapi.LoopbackAccess("", testAddr{}); err == nil {
		t.Error("LoopbackAccess() accepted an empty token")
	}
}

type testAddr struct{}

func (testAddr) Network() string { return "tcp" }
func (testAddr) String() string  { return testHost }

// A path the mux would tidy into a real API route must not dodge the token
// check on the way there.
func TestGuardTokenCheckSurvivesPathTricks(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)

	for _, path := range []string{"//api/spaces", "/api/../api/spaces", "/api//spaces"} {
		request := guardRequest(t, http.MethodGet, path, "")
		request.Header.Del(httpapi.TokenHeader)

		response := serve(t, fixture.handler, request)
		if response.Code == http.StatusOK {
			t.Errorf("GET %s status = 200 without a token", path)
		}
	}
}

// A browser leaves the default port out of the Host header. A UI pinned to
// 127.0.0.1:80 therefore sends `Host: 127.0.0.1`, which matched none of the
// allowed spellings — so the application answered 403 to the very URL it had
// just printed, on every route.
func TestLoopbackAccessAcceptsTheDefaultPortSpelling(t *testing.T) {
	t.Parallel()

	access, err := httpapi.LoopbackAccess("token", &net.TCPAddr{
		IP: net.IPv4(127, 0, 0, 1), Port: 80,
	})
	if err != nil {
		t.Fatalf("LoopbackAccess() error = %v", err)
	}
	// An IPv6 literal keeps its brackets in a Host header even without a port.
	for _, host := range []string{
		"127.0.0.1", "127.0.0.1:80", "localhost", "localhost:80", "[::1]", "[::1]:80",
	} {
		if !slices.Contains(access.Hosts, host) {
			t.Errorf("Hosts = %q, missing %q", access.Hosts, host)
		}
	}
	// The port is still what pins the boundary; another one is not ours.
	if slices.Contains(access.Hosts, "127.0.0.1:8080") {
		t.Errorf("Hosts = %q, accepts a port we did not open", access.Hosts)
	}
}

// On any other port the bare host must stay out, or the port would stop being
// one of the two things a rebinding page has to know in advance.
func TestLoopbackAccessKeepsThePortOnNonDefaultPorts(t *testing.T) {
	t.Parallel()

	access, err := httpapi.LoopbackAccess("token", &net.TCPAddr{
		IP: net.IPv4(127, 0, 0, 1), Port: 8080,
	})
	if err != nil {
		t.Fatalf("LoopbackAccess() error = %v", err)
	}
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]", "::1"} {
		if slices.Contains(access.Hosts, host) {
			t.Errorf("Hosts = %q, accepts %q without a port", access.Hosts, host)
		}
	}
	if !slices.Contains(access.Hosts, "127.0.0.1:8080") {
		t.Errorf("Hosts = %q, missing the address actually opened", access.Hosts)
	}
}
