// Package ui embeds and serves the optional operator WebUI bundle.
package ui

import (
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed assets
var assets embed.FS

// Handler serves the packaged WebUI, or an explicit unavailable response when
// a Go-only build contains only the checked-in fallback placeholder.
func Handler() http.Handler {
	bundle, _ := fs.Sub(assets, "assets")
	_, err := fs.Stat(bundle, "index.html")
	built := err == nil
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			http.Error(w, "WebUI bundle is not included in this build", http.StatusServiceUnavailable)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, "/admin/")), "/")
		if name == "." || name == "" {
			name = "index.html"
		}
		if strings.HasPrefix(name, "assets/") {
			if !serveFile(w, r, bundle, name) {
				http.NotFound(w, r)
			}
			return
		}
		if serveFile(w, r, bundle, name) {
			return
		}
		serveFile(w, r, bundle, "index.html")
	})
}

func serveFile(w http.ResponseWriter, r *http.Request, bundle fs.FS, name string) bool {
	file, err := bundle.Open(name)
	if err != nil {
		return false
	}
	defer file.Close()
	reader, ok := file.(io.ReadSeeker)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		return false
	}
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), reader)
	return true
}
