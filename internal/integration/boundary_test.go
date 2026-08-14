package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/httpapi"
)

// The audit's acceptance criterion for SEC-01, run against a listening socket
// rather than a handler: a page served from the attacker's domain, whose name
// they have pointed at 127.0.0.1, reaches this port with their hostname in the
// Host header and every browser-side check agreeing that it is same-origin.
func TestARebindingPageIsRefusedAtTheSocket(t *testing.T) {
	t.Parallel()

	wallet := start(t)
	spaceID := wallet.createSpace()

	// Everything the browser would send for a genuinely same-origin request to
	// attacker.example, and nothing more. The Origin header is deliberately
	// absent: the guard refuses a foreign Origin with the same 403, so a request
	// carrying both cannot say which check did the work — and this test passed
	// with the Host check deleted outright until the header was removed.
	//
	// The token is supplied, because the page does not have one and the point
	// here is what remains when it does.
	rebound := wallet.raw(http.MethodPost, "/api/spaces/"+spaceID+"/mnemonic",
		map[string]any{"password": spacePassword},
		header("Sec-Fetch-Site", "same-origin"),
		header(httpapi.TokenHeader, wallet.token),
		forgedHost("attacker.example:8080"),
	)
	if rebound.status != http.StatusForbidden {
		t.Fatalf("rebound mnemonic request = %d %s, want 403", rebound.status, rebound.text())
	}
	// On the message, not just the status, for the same reason.
	if !strings.Contains(rebound.text(), "unexpected Host header") {
		t.Fatalf("rebound request refused by %q, want the Host check", rebound.text())
	}
	if strings.Contains(rebound.text(), knownPhrase) {
		t.Fatal("the recovery phrase came back to a rebound request")
	}

	// The right hostname on the wrong port is a different site, and a browser
	// treats it as one, so the guard has to as well.
	wrongPort := wallet.raw(http.MethodPost, "/api/spaces/"+spaceID+"/mnemonic",
		map[string]any{"password": spacePassword},
		header(httpapi.TokenHeader, wallet.token),
		forgedHost("127.0.0.1:1"),
	)
	if wrongPort.status != http.StatusForbidden {
		t.Fatalf("request for 127.0.0.1:1 = %d %s, want 403", wrongPort.status, wrongPort.text())
	}

	// The same request from the wallet's own origin still works, or none of the
	// refusals above would be evidence of anything.
	ours := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/mnemonic",
		map[string]any{"password": spacePassword},
		header("Origin", "http://"+wallet.host),
		header("Sec-Fetch-Site", "same-origin"),
	)
	if ours.status != http.StatusOK {
		t.Fatalf("same-origin mnemonic request = %d %s, want 200", ours.status, ours.text())
	}
	if !strings.Contains(ours.text(), knownPhrase) {
		t.Fatalf("same-origin request returned no phrase: %s", ours.text())
	}
}

// Origin is checked too, and on its own. Splitting it from the Host case is
// what keeps either one from covering for the other.
func TestACrossOriginPageIsRefusedOnItsOrigin(t *testing.T) {
	t.Parallel()

	wallet := start(t)
	spaceID := wallet.createSpace()

	foreign := wallet.raw(http.MethodPost, "/api/spaces/"+spaceID+"/mnemonic",
		map[string]any{"password": spacePassword},
		header("Origin", "http://attacker.example:8080"),
		header(httpapi.TokenHeader, wallet.token),
	)
	if foreign.status != http.StatusForbidden {
		t.Fatalf("cross-origin request = %d %s, want 403", foreign.status, foreign.text())
	}
	if !strings.Contains(foreign.text(), "cross-origin") {
		t.Fatalf("refused by %q, want the Origin check", foreign.text())
	}
}

// A local process that guessed the port has no token, and reads leak as much as
// writes: the balance list names every address in the space.
func TestALocalProcessWithoutTheTokenGetsNothing(t *testing.T) {
	t.Parallel()

	wallet := start(t)
	spaceID := wallet.createSpace()

	for _, path := range []string{
		"/api/spaces",
		"/api/spaces/" + spaceID,
		"/api/settings",
		"/api/spaces/" + spaceID + "/accounts",
	} {
		// With the token first. Without this the loop proves only that the
		// guard 401s anything under /api/ — including a path that no longer
		// routes anywhere, which is exactly what happened when the UI's own
		// modules were moved out of this namespace.
		authorised := wallet.call(http.MethodGet, path, nil)
		if authorised.status != http.StatusOK {
			t.Fatalf("GET %s with the token = %d %s, want 200", path, authorised.status, authorised.text())
		}
		// No browser headers at all, which is what a curl or a sandboxed
		// process looks like, and is deliberately allowed past Fetch Metadata.
		anonymous := wallet.raw(http.MethodGet, path, nil)
		if anonymous.status != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d %s, want 401", path, anonymous.status, anonymous.text())
		}
	}

	// A near miss is still a miss: the comparison is constant-time, not a
	// prefix match.
	almost := wallet.raw(http.MethodGet, "/api/spaces", nil,
		header(httpapi.TokenHeader, wallet.token[:len(wallet.token)-1]))
	if almost.status != http.StatusUnauthorized {
		t.Errorf("truncated token = %d, want 401", almost.status)
	}
}

// The launcher opens a file:// page that redirects to the UI, so the first
// navigation is cross-site by construction. The page has to load — it is what
// presents the token afterwards — while the API stays closed to the same
// cross-site marking.
func TestTheLauncherBootstrapWorksAndOpensNothingElse(t *testing.T) {
	t.Parallel()

	wallet := start(t)

	page := wallet.raw(http.MethodGet, "/", nil,
		header("Sec-Fetch-Site", "cross-site"),
		header("Sec-Fetch-Mode", "navigate"),
	)
	if page.status != http.StatusOK {
		t.Fatalf("cross-site navigation to the UI = %d, want 200", page.status)
	}
	if got := page.header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q, want the strict policy", got)
	}

	// The module the page loads next is static and public too, or the browser
	// could not fetch it: a module import carries no headers.
	script := wallet.raw(http.MethodGet, "/services/client.js", nil)
	if script.status != http.StatusOK {
		t.Errorf("GET /services/client.js = %d, want 200", script.status)
	}

	// And that is the end of what being cross-site buys.
	api := wallet.raw(http.MethodGet, "/api/spaces", nil,
		header(httpapi.TokenHeader, wallet.token),
		header("Sec-Fetch-Site", "cross-site"),
	)
	if api.status != http.StatusForbidden {
		t.Errorf("cross-site API read = %d %s, want 403", api.status, api.text())
	}
}

// BE-4. The backup is the whole vault in a file: it survives the space being
// locked, it can be attacked offline, and before the step-up it was the
// cheapest thing on the API for anyone holding the token.
func TestTheBackupNeedsThePasswordEvenWhileUnlocked(t *testing.T) {
	t.Parallel()

	wallet := start(t)
	spaceID := wallet.createSpace()
	path := "/api/spaces/" + spaceID + "/backup"

	withoutPassword := wallet.call(http.MethodPost, path, map[string]any{})
	if withoutPassword.status == http.StatusOK {
		t.Fatalf("the backup downloaded without a password: %s", withoutPassword.text())
	}
	wrongPassword := wallet.call(http.MethodPost, path, map[string]any{"password": "not-the-password"})
	if wrongPassword.status == http.StatusOK {
		t.Fatalf("the backup downloaded on a wrong password: %s", wrongPassword.text())
	}

	correct := wallet.call(http.MethodPost, path, map[string]any{"password": spacePassword})
	if correct.status != http.StatusOK {
		t.Fatalf("backup with the password = %d %s, want 200", correct.status, correct.text())
	}
	if !strings.Contains(correct.text(), `"vault"`) {
		t.Errorf("backup does not look like a space file: %s", correct.text())
	}
	if correct.header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a file full of secrets",
			correct.header.Get("Cache-Control"))
	}
}

// The cooldown is one counter shared by every path that checks a password, and
// that is the part worth testing from outside: it was not, once. The step-up on
// the exports ran no cooldown and recorded no failure, so beside a throttled
// unlock sat an export endpoint that could be guessed at without limit.
func TestGuessingIsThrottledAcrossEveryPasswordCheck(t *testing.T) {
	t.Parallel()

	wallet := start(t)
	spaceID := wallet.createSpace()

	// The space stays unlocked on purpose. Locking it first would make every
	// assertion below satisfiable by "space is locked" instead of by the
	// cooldown — which is how this test came to pass with the throttle removed
	// from the step-up path entirely.
	//
	// Every guess goes through the export endpoint, so what is being measured
	// is whether that path feeds the shared counter at all. Three attempts are
	// free for someone who mistyped; the fourth starts the wait.
	for i := range 4 {
		guess := map[string]any{"password": "guess-number-" + string(rune('a'+i))}
		wrong := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/mnemonic", guess)
		if wrong.status == http.StatusOK {
			t.Fatalf("the export endpoint accepted a guess")
		}
	}

	// Unlock is now in a cooldown it never saw a failure on, which is the whole
	// point: one counter, not one per endpoint. And while the wait holds, the
	// correct password is refused too — answering it would tell an attacker
	// which guess to keep without their having to wait for it.
	unlock := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/unlock",
		map[string]any{"password": spacePassword})
	if unlock.status == http.StatusOK {
		t.Fatal("guessing at the export endpoint left unlock unthrottled")
	}
	if !strings.Contains(unlock.text(), "attempts") {
		t.Errorf("error = %s, want the throttle rather than a password verdict", unlock.text())
	}
	if strings.Contains(strings.ToLower(unlock.text()), "invalid password") {
		t.Errorf("error = %s, which distinguishes a wrong password from a wait", unlock.text())
	}

	// The step-up is refused for the same reason, with the same silence about
	// which of the two it is.
	phrase := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/mnemonic",
		map[string]any{"password": spacePassword})
	if phrase.status == http.StatusOK {
		t.Fatal("the export endpoint answered the correct password during its own cooldown")
	}
	if strings.Contains(phrase.text(), knownPhrase) {
		t.Fatal("the recovery phrase came back during a cooldown")
	}
}

// SEC-11's step-up, over the wire and on both exports. An unlocked space is not
// evidence that the person asking is the owner.
func TestExportsAskForThePasswordAgain(t *testing.T) {
	t.Parallel()

	wallet := start(t)
	spaceID := wallet.createSpace()
	accountID := wallet.deriveEVMAccount(spaceID)

	phrase := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/mnemonic", map[string]any{})
	if phrase.status == http.StatusOK {
		t.Errorf("the recovery phrase came back without a password: %s", phrase.text())
	}
	key := wallet.call(http.MethodPost,
		"/api/spaces/"+spaceID+"/accounts/"+accountID+"/private-key",
		map[string]any{"family": "evm"})
	if key.status == http.StatusOK {
		t.Errorf("a private key came back without a password: %s", key.text())
	}

	// Both endpoints answer when the password is supplied. Without this the
	// test is satisfied by a 404 — a renamed route, a wrong account id, a
	// handler that never reaches the step-up at all — and would report the
	// step-up as covered while never exercising it.
	revealed := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/mnemonic",
		map[string]any{"password": spacePassword})
	if revealed.status != http.StatusOK || !strings.Contains(revealed.text(), knownPhrase) {
		t.Fatalf("mnemonic with the password = %d %s, want the phrase", revealed.status, revealed.text())
	}
	exported := wallet.call(http.MethodPost,
		"/api/spaces/"+spaceID+"/accounts/"+accountID+"/private-key",
		map[string]any{"family": "evm", "password": spacePassword})
	if exported.status != http.StatusOK {
		t.Fatalf("private key with the password = %d %s, want 200", exported.status, exported.text())
	}
}
