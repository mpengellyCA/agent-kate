// Package webui embeds either an explicitly built mobile bundle or the
// committed development stub. Node is never needed for an ordinary Go or CMake
// build: dist contains only .gitkeep in a fresh checkout.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

//go:embed placeholder.html
var placeholder []byte

// Handler serves a history-mode single-page app. With no explicit web build it
// serves the committed placeholder, rather than returning a blank page or
// making a developer install Node just to compile the core.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "remote web UI is unavailable", http.StatusServiceUnavailable)
		})
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
		if clean != "/" {
			if f, err := sub.Open(strings.TrimPrefix(clean, "/")); err == nil {
				_ = f.Close()
				if strings.HasPrefix(clean, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(w, r, sub)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		b = placeholder
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

// Built reports whether an explicit bundle was present at Go build time.
func Built() bool {
	f, err := distFS.Open("dist/index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
