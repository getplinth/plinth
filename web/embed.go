// Package web serves the single-page application that is compiled into the
// binary. Build it with `make web` (Vite writes web/dist); without a build the
// handler serves a short notice instead, so the server still starts.
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var dist embed.FS

// Handler serves the embedded app. connectOrigins are the extra origins the
// browser may call, which must include the identity provider (login and token
// requests are made by the page itself).
func Handler(connectOrigins ...string) http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return newHandler(sub, func() []string { return connectOrigins })
}

// HandlerFunc is Handler for origins that change while running (identity
// providers an administrator adds in the control panel): origins is asked on
// every request, so a new provider works without a restart.
func HandlerFunc(origins func() []string) http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return newHandler(sub, origins)
}

// contentSecurityPolicy is deliberately strict: scripts, styles and fonts only
// from this origin, no inline script or style, no framing.
func contentSecurityPolicy(connectOrigins []string) string {
	connect := "'self'"
	for _, o := range connectOrigins {
		if o != "" {
			connect += " " + o
		}
	}
	return strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src " + connect,
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

const notBuilt = `<!doctype html><meta charset="utf-8"><title>Plinth</title>
<body style="font-family:system-ui;max-width:40rem;margin:4rem auto;padding:0 1rem">
<h1>Plinth</h1><p>The web interface has not been built into this binary.
Run <code>make web</code> and rebuild, or use the API at <code>/api/v1</code>.</p></body>`

func newHandler(files fs.FS, origins func() []string) http.Handler {
	index, indexErr := fs.ReadFile(files, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy(origins()))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Anything under /api that no API route matched is a missing API
		// endpoint, not an app page.
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}

		clean := path.Clean("/" + r.URL.Path)
		name := strings.TrimPrefix(clean, "/")

		// A real file (hashed build assets): cache for a year.
		if name != "" && name != "index.html" && path.Ext(name) != "" {
			f, err := files.Open(name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			st, err := f.Stat()
			if err != nil || st.IsDir() {
				http.NotFound(w, r)
				return
			}
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
			if rs, ok := f.(readSeeker); ok {
				http.ServeContent(w, r, name, st.ModTime(), rs)
				return
			}
			data, _ := fs.ReadFile(files, name)
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
			return
		}

		// Every other path is a page of the single-page app.
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "text/html; charset=utf-8")
		if indexErr != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(notBuilt))
			return
		}
		_, _ = w.Write(index)
	})
}

type readSeeker interface {
	Read([]byte) (int, error)
	Seek(int64, int) (int64, error)
}
