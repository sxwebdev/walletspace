package httpapi

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestSPAHandler(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"index.html":           {Data: []byte(`<div id="root"></div>`)},
		"favicon.svg":          {Data: []byte(`<svg/>`)},
		"assets/index-abc.js":  {Data: []byte(`console.log(1)`)},
		"assets/index-abc.css": {Data: []byte(`body{}`)},
	}
	handler := spaHandler(files)

	tests := []struct {
		name      string
		path      string
		wantCode  int
		wantBody  string
		wantCache string
	}{
		{name: "root is the shell", path: "/", wantCode: http.StatusOK, wantBody: `id="root"`, wantCache: "no-cache"},
		{name: "a client route is the shell", path: "/wallet/acc_1/tron-mainnet/send", wantCode: http.StatusOK, wantBody: `id="root"`, wantCache: "no-cache"},
		{name: "a fingerprinted asset is immutable", path: "/assets/index-abc.js", wantCode: http.StatusOK, wantBody: "console.log", wantCache: "immutable"},
		{name: "a plain file is served as is", path: "/favicon.svg", wantCode: http.StatusOK, wantBody: "<svg/>"},
		// A broken asset reference must not be papered over with HTML.
		{name: "a missing asset is not found", path: "/assets/gone.js", wantCode: http.StatusNotFound},
		// Cleaned to a path inside the embedded tree, so it is only ever a
		// route or a missing file there, never a file outside it.
		{name: "a dot-dot route stays inside", path: "/../../etc/passwd", wantCode: http.StatusOK, wantBody: `id="root"`},
		{name: "a dot-dot file stays inside", path: "/../../etc/passwd.txt", wantCode: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != tt.wantCode {
				t.Fatalf("GET %s = %d, want %d", tt.path, response.Code, tt.wantCode)
			}
			if !strings.Contains(response.Body.String(), tt.wantBody) {
				t.Errorf("GET %s body = %q, want it to contain %q", tt.path, response.Body, tt.wantBody)
			}
			if got := response.Header().Get("Cache-Control"); !strings.Contains(got, tt.wantCache) {
				t.Errorf("GET %s Cache-Control = %q, want %q", tt.path, got, tt.wantCache)
			}
		})
	}
}

// Without a frontend build the binary still serves a page, and it says how to
// get the real one instead of failing with a blank screen.
func TestUIFilesFallBackToThePlaceholder(t *testing.T) {
	t.Parallel()

	files, err := uiFiles()
	if err != nil {
		t.Fatalf("uiFiles() error = %v", err)
	}
	data, err := fs.ReadFile(files, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(string(data), `id="root"`) {
		t.Errorf("index.html = %q, want a page with the root element", data)
	}
}
