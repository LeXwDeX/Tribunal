package server

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	webui "github.com/LeXwDeX/tribunal/web"
)

const (
	webIndexFile    = "index.html"
	webAssetPrefix  = "assets/"
	webAssetMaxAge  = "public, max-age=31536000, immutable"
	webIndexControl = "no-cache"
)

func newWebFS() (fs.FS, error) {
	return fs.Sub(webui.Dist, "dist")
}

func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, r, http.StatusMethodNotAllowed, "bad_request", "method not allowed", nil)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api" {
		writeError(w, r, http.StatusNotFound, "not_found", "not found", nil)
		return
	}
	if isReservedWebPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." {
		s.serveWebFile(w, r, webIndexFile, webIndexControl)
		return
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	if info, err := fs.Stat(s.web, name); err == nil && !info.IsDir() {
		cache := webIndexControl
		if strings.HasPrefix(name, webAssetPrefix) {
			cache = webAssetMaxAge
		}
		s.serveWebFile(w, r, name, cache)
		return
	}
	assetLike := path.Ext(name) != "" || name == "assets" || strings.HasPrefix(name, webAssetPrefix)
	if assetLike {
		http.NotFound(w, r)
		return
	}
	s.serveWebFile(w, r, webIndexFile, webIndexControl)
}

func isReservedWebPath(value string) bool {
	switch value {
	case "/healthz", "/readyz", "/webhooks", "/auth":
		return true
	}
	return strings.HasPrefix(value, "/webhooks/") || strings.HasPrefix(value, "/auth/")
}

func (s *Server) serveWebFile(w http.ResponseWriter, r *http.Request, name, cacheControl string) {
	data, err := fs.ReadFile(s.web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	header.Set("Cache-Control", cacheControl)
	header.Set("X-Content-Type-Options", "nosniff")
	if contentType := mime.TypeByExtension(path.Ext(name)); contentType != "" {
		header.Set("Content-Type", contentType)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
