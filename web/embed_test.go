package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func app() http.Handler {
	return newHandler(fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>Plinth</title><div id=root></div>")},
		"assets/app-1a2b.js": {Data: []byte("console.log(1)")},
	}, []string{"https://idp.example.com"})
}

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestSinglePageRoutesServeIndex(t *testing.T) {
	for _, p := range []string{"/", "/requests", "/requests/123", "/admin/integrations", "/callback"} {
		rec := get(app(), "GET", p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "id=root") {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: the app shell must not be cached for long: %q", p, rec.Header().Get("Cache-Control"))
		}
	}
}

func TestAssetsAreCachedAndMissingOnesAre404(t *testing.T) {
	rec := get(app(), "GET", "/assets/app-1a2b.js")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if rec := get(app(), "GET", "/assets/missing.js"); rec.Code != 404 {
		t.Errorf("missing asset: %d", rec.Code)
	}
	if rec := get(app(), "GET", "/assets/"); rec.Code != 200 { // a directory-like path is an app route
		t.Errorf("assets dir: %d", rec.Code)
	}
}

func TestUnknownAPIPathsAreJSON404NotTheApp(t *testing.T) {
	for _, p := range []string{"/api", "/api/v1/nope", "/api/v1/requests/x/y"} {
		rec := get(app(), "GET", p)
		if rec.Code != 404 || !strings.Contains(rec.Header().Get("Content-Type"), "json") || strings.Contains(rec.Body.String(), "root") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestStrictSecurityHeaders(t *testing.T) {
	csp := get(app(), "GET", "/").Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'",
		"connect-src 'self' https://idp.example.com", "base-uri 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*"} {
		if strings.Contains(csp, bad) {
			t.Errorf("CSP must not contain %q: %s", bad, csp)
		}
	}
}

func TestOnlyReadMethodsAndPathTraversalIsContained(t *testing.T) {
	if rec := get(app(), "POST", "/"); rec.Code != 405 {
		t.Errorf("POST: %d", rec.Code)
	}
	if rec := get(app(), "GET", "/../../etc/passwd"); strings.Contains(rec.Body.String(), "root:") {
		t.Error("path traversal")
	}
}

func TestMissingBuildStillStarts(t *testing.T) {
	h := newHandler(fstest.MapFS{}, nil)
	rec := get(h, "GET", "/")
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "make web") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
