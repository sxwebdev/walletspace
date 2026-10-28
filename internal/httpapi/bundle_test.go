package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/space"
)

// rawRequest sends body as it is, which is how the browser uploads a file.
func rawRequest(t *testing.T, handler http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Host = testHost
	request.Header.Set(httpapi.TokenHeader, testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func createSpace(t *testing.T, fixture platformFixture, name, password string) string {
	t.Helper()
	created := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces", map[string]any{
		"name": name, "password": password,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /api/spaces = %d %s", created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Space space.Summary `json:"space"`
	}](t, created).Space.ID
}

func TestPlatformMovesSpacesBetweenMachines(t *testing.T) {
	t.Parallel()

	source := newPlatformFixture(t)
	first := createSpace(t, source, "first", "first space password")
	second := createSpace(t, source, "second", "second space password")
	both := func(secondPassword string) map[string]any {
		return map[string]any{"spaces": []map[string]string{
			{"id": first, "password": "first space password"},
			{"id": second, "password": secondPassword},
		}}
	}

	refused := platformRequest(t, source.handler, http.MethodPost, "/api/spaces/export", both("first space password"))
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("export with a wrong password = %d %s", refused.Code, refused.Body.String())
	}
	if strings.Contains(refused.Body.String(), "ciphertext") {
		t.Fatal("a refused export returned vault data")
	}

	exported := platformRequest(t, source.handler, http.MethodPost, "/api/spaces/export", both("second space password"))
	if exported.Code != http.StatusOK {
		t.Fatalf("export = %d %s", exported.Code, exported.Body.String())
	}
	if got := exported.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("export Cache-Control = %q, want no-store", got)
	}
	if got := exported.Header().Get("Content-Disposition"); !strings.HasPrefix(got, `attachment; filename="walletspace-`) ||
		!strings.HasSuffix(got, `.wsbackup"`) {
		t.Errorf("export Content-Disposition = %q", got)
	}

	target := newPlatformFixture(t)
	imported := rawRequest(t, target.handler, "/api/spaces/import", exported.Body.Bytes())
	if imported.Code != http.StatusOK {
		t.Fatalf("import = %d %s", imported.Code, imported.Body.String())
	}
	result := decodeBody[space.BundleImport](t, imported)
	if len(result.Imported) != 2 || len(result.Skipped) != 0 {
		t.Fatalf("import = %+v, want both spaces", result)
	}
	for _, summary := range result.Imported {
		if !summary.Locked {
			t.Errorf("imported space %s is unlocked", summary.ID)
		}
	}
	unlocked := platformRequest(t, target.handler, http.MethodPost, "/api/spaces/"+second+"/unlock",
		map[string]string{"password": "second space password"})
	if unlocked.Code != http.StatusOK {
		t.Fatalf("unlock imported space = %d %s", unlocked.Code, unlocked.Body.String())
	}

	again := rawRequest(t, target.handler, "/api/spaces/import", exported.Body.Bytes())
	if again.Code != http.StatusOK {
		t.Fatalf("second import = %d %s", again.Code, again.Body.String())
	}
	if result := decodeBody[space.BundleImport](t, again); len(result.Imported) != 0 || len(result.Skipped) != 2 {
		t.Fatalf("second import = %+v, want both skipped", result)
	}
}

func TestPlatformRefusesABadBackupFile(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	for _, tt := range []struct {
		name string
		body []byte
		want int
	}{
		{name: "not a backup", body: []byte(`{"format":"something-else","version":1,"spaces":[]}`), want: http.StatusBadRequest},
		{name: "not JSON", body: []byte(`nope`), want: http.StatusBadRequest},
		// The import route takes more than the default body allowance, and
		// still has a ceiling.
		{name: "too large", body: bytes.Repeat([]byte(" "), space.MaxBundleBytes+1), want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := rawRequest(t, fixture.handler, "/api/spaces/import", tt.body)
			if response.Code != tt.want {
				t.Fatalf("import = %d %s, want %d", response.Code, response.Body.String(), tt.want)
			}
		})
	}

	// A file larger than the default allowance but under the import ceiling
	// gets as far as being parsed.
	padded := append(bytes.Repeat([]byte(" "), 128<<10), []byte(`{"format":"walletspace-bundle","version":1,"spaces":[]}`)...)
	response := rawRequest(t, fixture.handler, "/api/spaces/import", padded)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "no spaces") {
		t.Fatalf("padded import = %d %s, want the parser's refusal", response.Code, response.Body.String())
	}
	// No other route got the raised allowance.
	large := map[string]any{"name": strings.Repeat("x", 128<<10), "password": "a long enough password"}
	data, err := json.Marshal(large)
	if err != nil {
		t.Fatal(err)
	}
	if response := rawRequest(t, fixture.handler, "/api/spaces", data); response.Code == http.StatusCreated {
		t.Fatal("POST /api/spaces accepted a body over the default allowance")
	}
}
