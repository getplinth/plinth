// Package auth verifies OIDC bearer tokens and maps them to a role.
//
// Plinth has no user database of its own: it trusts an identity provider
// (Keycloak, Entra ID, Okta, Google, ...). A request carries
// "Authorization: Bearer <JWT>". The token must be signed by the provider
// (keys discovered from the issuer), unexpired, and issued for this
// application (audience = the configured client id). Members of the admin
// group get the "admin" role, everyone else is a "user".
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Identity is the verified caller.
type Identity struct {
	Sub   string
	Email string
	// EmailVerified is true only when the token has an e-mail address AND the
	// identity provider says it checked that the person owns it
	// ("email_verified"). Anything that grants access by e-mail address (such as
	// team invitations) must rely on this and not on Email alone.
	EmailVerified bool
	Role          string
}

// Verifier validates tokens.
type Verifier struct {
	v           *oidc.IDTokenVerifier
	adminGroup  string
	groupsClaim string
}

// Config configures a Verifier.
type Config struct {
	Issuer      string // OIDC issuer URL, discovery is fetched from it
	Audience    string // expected "aud" (the client id registered for Plinth)
	AdminGroup  string // members get the admin role; empty = nobody is admin
	GroupsClaim string // name of the claim listing groups, default "groups"
}

// New contacts the issuer once to discover its signing keys endpoint.
func New(ctx context.Context, c Config) (*Verifier, error) {
	if c.Issuer == "" || c.Audience == "" {
		return nil, errors.New("auth: issuer and audience are required")
	}
	p, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: cannot discover issuer %s: %w", c.Issuer, err)
	}
	claim := c.GroupsClaim
	if claim == "" {
		claim = "groups"
	}
	return &Verifier{
		v:           p.Verifier(&oidc.Config{ClientID: c.Audience}),
		adminGroup:  c.AdminGroup,
		groupsClaim: claim,
	}, nil
}

// Verify checks a raw token and returns the caller. Errors are deliberately
// generic to the caller; the reason is only in the wrapped error for logs.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	tok, err := v.v.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: %w", err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("auth: claims: %w", err)
	}
	id := Identity{Sub: tok.Subject, Role: RoleUser}
	if id.Sub == "" {
		return Identity{}, errors.New("auth: token has no subject")
	}
	if e, ok := claims["email"].(string); ok && e != "" {
		id.Email = e
		id.EmailVerified = claimTrue(claims["email_verified"])
	} else if u, ok := claims["preferred_username"].(string); ok && u != "" {
		id.Email = u
	} else {
		id.Email = id.Sub
	}
	if v.adminGroup != "" {
		if groups, ok := claims[v.groupsClaim].([]any); ok {
			for _, g := range groups {
				if s, _ := g.(string); s == v.adminGroup {
					id.Role = RoleAdmin
				}
			}
		}
	}
	return id, nil
}

// claimTrue reads a boolean claim. Most providers send a JSON boolean, a few
// send the string "true". Missing or anything else is false.
func claimTrue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(x, "true")
	}
	return false
}
