// Package identity assembles the identity providers people may sign in with:
// the one configured through the environment (if any) and those an
// administrator configured in the control panel.
package identity

import (
	"context"
	"net/url"
	"time"

	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/store"
)

// Logger is the part of slog the registry needs.
type Logger interface{ Warn(string, ...any) }

// NewRegistry builds a registry whose providers follow the database. env, if
// not nil, is shown read-only and keeps its users' original identities.
func NewRegistry(st *store.Store, env *auth.Provider, allowLoopback bool, log Logger) *auth.Registry {
	if env != nil {
		env.ReadOnly = true
	}
	return &auth.Registry{
		Static:        env,
		AllowLoopback: allowLoopback,
		Log:           log,
		Source: func(ctx context.Context) ([]auth.Provider, error) {
			list, err := st.ListOIDCProviders(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]auth.Provider, 0, len(list))
			for _, p := range list {
				if !p.Enabled {
					continue
				}
				out = append(out, auth.Provider{ID: p.ID, Name: p.Name, DisplayName: p.DisplayName, Issuer: p.Issuer, ClientID: p.ClientID,
					Scopes: p.Scopes, GroupsClaim: p.GroupsClaim, AdminGroup: p.AdminGroup, TrustUnverifiedEmail: p.TrustUnverifiedEmail})
			}
			return out, nil
		},
	}
}

// ConnectOrigins returns the origins of the enabled providers, which the
// browser must be allowed to call (sign-in and token requests come from the page).
func ConnectOrigins(r *auth.Registry) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out []string
	for _, p := range r.Providers(ctx) {
		if u, err := url.Parse(p.Issuer); err == nil && u.Host != "" {
			out = append(out, u.Scheme+"://"+u.Host)
		}
	}
	return out
}
