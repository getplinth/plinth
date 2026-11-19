package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/auth/authtest"
	"github.com/getplinth/plinth/web"
)

func do(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPublicEndpointsAndHeaders(t *testing.T) {
	h := New(Deps{})
	for _, p := range []string{"/healthz", "/api/v1/version"} {
		rec := do(h, "GET", p, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", p, rec.Code)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" ||
			!strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatalf("%s: missing security headers: %v", p, rec.Header())
		}
	}
	if rec := do(h, "GET", "/readyz", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz without a database: %d", rec.Code)
	}
}

// Without a configured verifier and store nothing protected is reachable.
func TestProtectedRoutesFailClosedWhenUnconfigured(t *testing.T) {
	h := New(Deps{})
	for _, p := range []string{"/api/v1/me", "/api/v1/integrations", "/api/v1/audit", "/api/v1/requests"} {
		if rec := do(h, "GET", p, "any-token", ""); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: got %d, want 503", p, rec.Code)
		}
	}
}

func TestBadTokensGet401WithoutDetails(t *testing.T) {
	p := authtest.New("plinth")
	defer p.Close()
	v, err := auth.New(context.Background(), auth.Config{Issuer: p.Issuer(), Audience: "plinth"})
	if err != nil {
		t.Fatal(err)
	}
	// A non-nil Store pointer is required for the route to be considered
	// configured; authentication fails before it is used.
	h := New(Deps{Verifier: v, Store: nonNilStore()})
	cases := map[string]string{
		"no header": "",
		"garbage":   "garbage",
		"foreign":   p.ForeignToken("u"),
		"wrong aud": p.Token(authtest.Claims{Sub: "u", Audience: "other"}),
	}
	for name, tok := range cases {
		rec := do(h, "GET", "/api/v1/me", tok, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "signature") || strings.Contains(rec.Body.String(), "audience") {
			t.Errorf("%s: response explains why the token failed: %s", name, rec.Body.String())
		}
	}
	// A header that is not a Bearer token is not accepted either.
	req := httptest.NewRequest("GET", "/api/v1/me", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("basic auth: got %d", rec.Code)
	}
}

// The API and the web app share one server: API routes keep their own strict
// policy and JSON errors, everything else is the app with its own policy.
func TestAPIAndUIShareAServer(t *testing.T) {
	h := New(Deps{UI: web.Handler("https://idp.example.com")})

	api := do(h, "GET", "/api/v1/version", "", "")
	if api.Code != 200 || !strings.Contains(api.Header().Get("Content-Security-Policy"), "default-src 'none'") ||
		strings.Contains(api.Header().Get("Content-Security-Policy"), "script-src") {
		t.Fatalf("API responses keep the locked-down policy: %d %v", api.Code, api.Header())
	}
	cfg := do(h, "GET", "/api/v1/config", "", "")
	if cfg.Code != 200 || !strings.Contains(cfg.Body.String(), `"providers":[]`) || !strings.Contains(cfg.Body.String(), `"local_login":false`) {
		t.Fatalf("public config: %d %s", cfg.Code, cfg.Body.String())
	}
	if cfg.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("config must not be cached: %v", cfg.Header())
	}

	missing := do(h, "GET", "/api/v1/definitely-not-a-route", "", "")
	if missing.Code != 404 || !strings.Contains(missing.Header().Get("Content-Type"), "json") {
		t.Fatalf("unknown API paths must be JSON 404s, not the app: %d %s", missing.Code, missing.Body.String())
	}

	page := do(h, "GET", "/requests/some-id", "", "")
	if !strings.Contains(page.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(page.Header().Get("Content-Security-Policy"), "script-src 'self'") ||
		!strings.Contains(page.Header().Get("Content-Security-Policy"), "connect-src 'self' https://idp.example.com") {
		t.Fatalf("app pages: %d %v", page.Code, page.Header())
	}
	// The protected API still rejects anonymous callers while the UI is mounted.
	if rec := do(h, "GET", "/api/v1/integrations", "", ""); rec.Code == 200 {
		t.Fatal("mounting the UI must not open the API")
	}
}
