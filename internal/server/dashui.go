package server

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:static
var dashDist embed.FS

// dashFS is the built Vite app, served under /dash/.
var dashFS = func() fs.FS {
	sub, err := fs.Sub(dashDist, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

// handleDashUI serves the SPA. /dash/ and /dash -> index.html; other paths
// fall back to index.html too (client-side routing), except real files
// under /dash/assets/.
func (s *Server) handleDashUI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/dash")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		p = "index.html"
	}
	clean := path.Clean(p)
	if clean == "index.html" || strings.HasPrefix(clean, "assets/") {
		serveDashFile(w, r, clean)
		return
	}
	// Serve real root assets (favicon.ico, logo.png, …) when they exist.
	if _, err := fs.Stat(dashFS, clean); err == nil {
		serveDashFile(w, r, clean)
		return
	}
	// SPA fallback: unknown client routes get the shell.
	serveDashFile(w, r, "index.html")
}

func serveDashFile(w http.ResponseWriter, r *http.Request, p string) {
	data, err := fs.ReadFile(dashFS, path.Clean(p))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "text/plain"
	switch {
	case strings.HasSuffix(p, ".html"):
		ct = "text/html; charset=utf-8"
	case strings.HasSuffix(p, ".js"):
		ct = "application/javascript"
	case strings.HasSuffix(p, ".css"):
		ct = "text/css"
	case strings.HasSuffix(p, ".svg"):
		ct = "image/svg+xml"
	case strings.HasSuffix(p, ".png"):
		ct = "image/png"
	case strings.HasSuffix(p, ".ico"):
		ct = "image/x-icon"
	case strings.HasSuffix(p, ".woff2"):
		ct = "font/woff2"
	case strings.HasSuffix(p, ".json"):
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
