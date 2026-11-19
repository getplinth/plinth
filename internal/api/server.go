// Package api exposes the REST API.
//
// Authentication: every /api/v1 route except /version needs an OIDC bearer
// token (see internal/auth). Authorization: admins manage integrations,
// blueprints and read the audit log; users submit and follow their own
// requests.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/store"
)

// Version is set at build time.
var Version = "dev"

const maxBody = 1 << 20 // 1 MiB

// Deps are the server's collaborators.
type Deps struct {
	Store *store.Store
	// Verifier checks tokens from identity providers: a single *auth.Verifier
	// or a *auth.Registry. Password sessions are checked against the store.
	Verifier auth.TokenVerifier
	Log      *slog.Logger
	// Providers is the set of identity providers people may sign in with.
	// Nil means none (password sign-in only).
	Providers *auth.Registry
	// EnvProvider is the provider configured through PLINTH_OIDC_*, shown
	// read-only in the control panel.
	EnvProvider *auth.Provider
	// AllowLoopbackIssuers permits http:// issuers and issuers on this machine.
	// For local testing only.
	AllowLoopbackIssuers bool
	// LoginBurst overrides how many password attempts one username may make
	// before being slowed down (default 10). For tests.
	LoginBurst int
	// ForceLocalLogin keeps password sign-in on whatever the setting says.
	ForceLocalLogin bool
	// Per-user limit for submitting and retrying requests. Zero values mean
	// the defaults (burst 5, 10 per minute).
	RateBurst     int
	RatePerMinute int

	// UI serves the web app for every non-API path; nil serves no UI.
	UI http.Handler
}

// Server serves the API.
type Server struct {
	d        Deps
	limiters limiters
	logins   *loginLimiter
}

// New builds the HTTP handler.
func New(d Deps) http.Handler {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	s := &Server{d: d, limiters: newLimiters(d.RateBurst, d.RatePerMinute), logins: newLoginLimiter(d.LoginBurst)}
	mux := http.NewServeMux()

	// public
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": Version})
	})

	mux.HandleFunc("GET /api/v1/config", s.publicConfig)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)

	// any authenticated user
	mux.Handle("GET /api/v1/me", s.user(s.me))
	mux.Handle("POST /api/v1/auth/logout", s.user(s.logout))
	mux.Handle("POST /api/v1/auth/password", s.user(s.changePassword))
	mux.Handle("GET /api/v1/blueprints", s.user(s.listBlueprints))
	mux.Handle("GET /api/v1/blueprints/{name}", s.user(s.getBlueprint))
	mux.Handle("GET /api/v1/quota-tiers", s.user(s.listTiers))
	mux.Handle("GET /api/v1/tenants", s.user(s.listTenants))
	mux.Handle("GET /api/v1/tenants/{name}", s.user(s.getTenant))
	mux.Handle("POST /api/v1/tenants/{name}/members", s.user(s.addMember))
	mux.Handle("PUT /api/v1/tenants/{name}/members/{user}", s.user(s.setMemberRole))
	mux.Handle("DELETE /api/v1/tenants/{name}/invitations/{id}", s.user(s.revokeInvitation))
	mux.Handle("DELETE /api/v1/tenants/{name}/members/{user}", s.user(s.removeMember))
	mux.Handle("POST /api/v1/requests", s.user(s.submitRequest))
	mux.Handle("GET /api/v1/requests", s.user(s.listRequests))
	mux.Handle("GET /api/v1/requests/{id}", s.user(s.getRequest))
	mux.Handle("POST /api/v1/requests/{id}/retry", s.user(s.retryRequest))
	mux.Handle("POST /api/v1/requests/{id}/cancel", s.user(s.decide(store.DecisionCancel)))

	// admins
	mux.Handle("GET /api/v1/integrations", s.admin(s.listIntegrations))
	mux.Handle("POST /api/v1/integrations", s.admin(s.createIntegration))
	mux.Handle("GET /api/v1/integrations/{name}", s.admin(s.getIntegration))
	mux.Handle("PUT /api/v1/integrations/{name}", s.admin(s.replaceIntegration))
	mux.Handle("DELETE /api/v1/integrations/{name}", s.admin(s.deleteIntegration))
	mux.Handle("POST /api/v1/integrations/{name}/validate", s.admin(s.validateIntegration))
	mux.Handle("POST /api/v1/blueprints", s.admin(s.createBlueprint))
	mux.Handle("PUT /api/v1/blueprints/{name}", s.admin(s.replaceBlueprint))
	mux.Handle("DELETE /api/v1/blueprints/{name}", s.admin(s.deleteBlueprint))
	mux.Handle("PUT /api/v1/quota-tiers/{name}", s.admin(s.putTier))
	mux.Handle("DELETE /api/v1/quota-tiers/{name}", s.admin(s.deleteTier))
	mux.Handle("POST /api/v1/requests/{id}/approve", s.admin(s.decide(store.DecisionApprove)))
	mux.Handle("POST /api/v1/requests/{id}/reject", s.admin(s.decide(store.DecisionReject)))
	mux.Handle("POST /api/v1/requests/{id}/release", s.admin(s.decide(store.DecisionRelease)))
	mux.Handle("GET /api/v1/audit", s.admin(s.listAudit))
	mux.Handle("GET /api/v1/local-users", s.admin(s.listLocalUsers))
	mux.Handle("POST /api/v1/local-users", s.admin(s.createLocalUser))
	mux.Handle("PUT /api/v1/local-users/{id}", s.admin(s.updateLocalUser))
	mux.Handle("POST /api/v1/local-users/{id}/reset-password", s.admin(s.resetLocalPassword))
	mux.Handle("DELETE /api/v1/local-users/{id}", s.admin(s.deleteLocalUser))
	mux.Handle("GET /api/v1/oidc-providers", s.admin(s.listProviders))
	mux.Handle("POST /api/v1/oidc-providers", s.admin(s.createProvider))
	mux.Handle("PUT /api/v1/oidc-providers/{id}", s.admin(s.updateProvider))
	mux.Handle("DELETE /api/v1/oidc-providers/{id}", s.admin(s.deleteProvider))
	mux.Handle("GET /api/v1/auth-settings", s.admin(s.getAuthSettings))
	mux.Handle("PUT /api/v1/auth-settings", s.admin(s.putAuthSettings))
	mux.Handle("GET /api/v1/notifications", s.admin(s.listNotifications))
	mux.Handle("POST /api/v1/notifications/test", s.admin(s.testNotification))
	mux.Handle("GET /api/v1/drivers", s.admin(s.listDrivers))

	if d.UI != nil {
		mux.Handle("/", d.UI) // everything that is not an API route
	}

	return s.common(mux)
}

// ---- middleware ---------------------------------------------------------------------

type ctxKey struct{}

// caller is the authenticated user attached to a request.
type caller struct {
	store.User
}

func (c caller) isAdmin() bool { return c.Role == auth.RoleAdmin }

func callerFrom(ctx context.Context) caller { c, _ := ctx.Value(ctxKey{}).(caller); return c }

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

// common adds security headers, a body size limit, and request logging. The
// log never contains the Authorization header, query strings or bodies.
func (s *Server) common(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		s.d.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"user", callerFrom(r.Context()).Email, "ms", time.Since(start).Milliseconds())
	})
}

// authenticate verifies the bearer token and loads the user.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (caller, bool) {
	if s.d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "authentication is not configured")
		return caller{}, false
	}
	h := r.Header.Get("Authorization")
	raw, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || strings.TrimSpace(raw) == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="plinth"`)
		writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return caller{}, false
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, auth.SessionPrefix) { // a password session
		u, err := s.d.Store.SessionUser(r.Context(), store.HashToken(raw), s.d.ForceLocalLogin)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="plinth", error="invalid_token"`)
			writeErr(w, http.StatusUnauthorized, "invalid token")
			return caller{}, false
		}
		return caller{u}, true
	}
	if s.d.Verifier == nil {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return caller{}, false
	}
	id, err := s.d.Verifier.Verify(r.Context(), raw)
	if err != nil {
		s.d.Log.Info("token rejected", "reason", err.Error()) // reason in the log, not in the response
		w.Header().Set("WWW-Authenticate", `Bearer realm="plinth", error="invalid_token"`)
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return caller{}, false
	}
	u, err := s.d.Store.UpsertUser(r.Context(), id.Sub, id.Email, id.EmailVerified, id.Role)
	if errors.Is(err, store.ErrForbidden) { // a provider sent a subject that is reserved for password accounts
		s.d.Log.Warn("token rejected", "reason", "reserved subject")
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return caller{}, false
	}
	if err != nil {
		s.d.Log.Error("upsert user", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return caller{}, false
	}
	return caller{u}, true
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, c caller)

func (s *Server) user(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if c.MustChange && !allowedWhilePasswordMustChange(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "choose a new password before doing anything else", "code": "password_change_required"})
			return
		}
		// Make the caller visible to the logging middleware.
		*r = *r.WithContext(context.WithValue(r.Context(), ctxKey{}, c))
		h(w, r, c)
	})
}

func (s *Server) admin(h handlerFunc) http.Handler {
	return s.user(func(w http.ResponseWriter, r *http.Request, c caller) {
		if !c.isAdmin() {
			writeErr(w, http.StatusForbidden, "administrator role required")
			return
		}
		h(w, r, c)
	})
}

// ---- helpers --------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// readJSON decodes exactly one JSON value, rejecting unknown fields and
// oversized bodies.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return false
	}
	if dec.More() {
		writeErr(w, http.StatusBadRequest, "invalid JSON: unexpected trailing data")
		return false
	}
	return true
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "no database configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.d.Store.Ping(ctx); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	_, _ = w.Write([]byte("ready"))
}

// me describes the caller, and whether Plinth can e-mail people (so the
// interface can say whether an invitation was mailed or has to be passed on).
func (s *Server) me(w http.ResponseWriter, r *http.Request, c caller) {
	email, _ := s.d.Store.NotificationChannels()
	writeJSON(w, http.StatusOK, struct {
		store.User
		NotifyEmail bool `json:"notify_email"`
	}{c.User, email})
}
