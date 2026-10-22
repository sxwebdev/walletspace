package httpapi

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// The React UI is built by Vite into ui/dist (see ui/vite.config.ts and
// `make ui`). The build output is not committed, so a checkout that has not
// run the frontend build still compiles and its tests still pass: it serves
// the placeholder page instead, which says how to build the real one.
const (
	uiBuildDir       = "ui/dist"
	uiPlaceholderDir = "ui/placeholder"
)

// uiFiles picks the built UI when there is one and the placeholder otherwise.
func uiFiles() (fs.FS, error) {
	built, err := fs.Sub(uiFS, uiBuildDir)
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(built, "index.html"); err == nil {
		return built, nil
	}

	return fs.Sub(uiFS, uiPlaceholderDir)
}

// spaHandler serves the single-page UI: a file that exists is served as it is,
// and any other extensionless path is a client-side route, which gets
// index.html so a reload or a pasted link lands on the right screen.
func spaHandler(files fs.FS) http.Handler {
	fileServer := http.FileServerFS(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleaned := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
		name := strings.TrimPrefix(cleaned, "/")

		if name != "" && name != "index.html" {
			info, err := fs.Stat(files, name)
			if err == nil && !info.IsDir() {
				// Vite fingerprints everything under assets/, so a name there
				// never changes content and can be cached for good.
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "UI unavailable", http.StatusInternalServerError)
				return
			}
			// A missing file with an extension is a broken asset reference,
			// not a route; answering it with HTML would only hide the bug.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}

		data, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.Error(w, "UI unavailable", http.StatusInternalServerError)
			return
		}
		// index.html names the current fingerprinted bundle, so it must be
		// revalidated or an upgrade keeps loading the previous release.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
}
