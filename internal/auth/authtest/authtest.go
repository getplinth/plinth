// Package authtest is a minimal OIDC identity provider for tests and local
// development: it serves discovery and signing keys, mints tokens, and can
// offer a browser sign-in (authorization code with PKCE). It has no passwords
// and must never run anywhere reachable by untrusted people.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Provider is a running fake identity provider.
type Provider struct {
	Server   *httptest.Server
	Audience string
	key      *rsa.PrivateKey
	mux      *http.ServeMux
	login    *loginState
	public   string // address clients use, when it differs from where we listen
}

// New starts a provider on a random port whose tokens are issued for audience.
func New(audience string) *Provider {
	p, err := newProvider(audience, nil)
	if err != nil {
		panic(err)
	}
	return p
}

// NewAt starts a provider on a fixed address (for the development stack).
func NewAt(audience, addr string) (*Provider, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return newProvider(audience, l)
}

func newProvider(audience string, l net.Listener) (*Provider, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	p := &Provider{Audience: audience, key: key}
	mux := http.NewServeMux()
	p.mux = mux
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                p.Issuer(),
			"jwks_uri":                              p.Issuer() + "/keys",
			"authorization_endpoint":                p.Issuer() + "/authorize",
			"token_endpoint":                        p.Issuer() + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"subject_types_supported":               []string{"public"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
		}})
	})
	p.Server = httptest.NewUnstartedServer(mux)
	if l != nil {
		p.Server.Listener.Close()
		p.Server.Listener = l
	}
	p.Server.Start()
	return p, nil
}

func (p *Provider) Close() { p.Server.Close() }

// Issuer is the address clients (and tokens) know the provider by.
func (p *Provider) Issuer() string {
	if p.public != "" {
		return p.public
	}
	return p.Server.URL
}

// SetPublicURL makes the provider advertise a different address than the one
// it listens on, for example a Kubernetes service name. Call it before use.
func (p *Provider) SetPublicURL(u string) { p.public = strings.TrimRight(u, "/") }

// Claims describes a token to mint. Zero values get sensible defaults.
type Claims struct {
	Sub      string
	Email    string
	Groups   []string
	Issuer   string        // default: this provider
	Audience string        // default: this provider's audience
	Expires  time.Duration // default 1h; negative = already expired
	Nonce    string        // echoed into the token when set (authorization code flow)
}

// Token returns a signed token for the claims.
func (p *Provider) Token(c Claims) string {
	if c.Issuer == "" {
		c.Issuer = p.Issuer()
	}
	if c.Audience == "" {
		c.Audience = p.Audience
	}
	if c.Expires == 0 {
		c.Expires = time.Hour
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		panic(err)
	}
	now := time.Now()
	std := jwt.Claims{
		Issuer: c.Issuer, Subject: c.Sub, Audience: jwt.Audience{c.Audience},
		IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(c.Expires)),
	}
	extra := map[string]any{"email": c.Email, "groups": c.Groups}
	if c.Nonce != "" {
		extra["nonce"] = c.Nonce
	}
	s, err := jwt.Signed(sig).Claims(std).Claims(extra).Serialize()
	if err != nil {
		panic(err)
	}
	return s
}

// ForeignToken is signed by a different key than the provider publishes.
func (p *Provider) ForeignToken(sub string) string {
	other := New(p.Audience)
	defer other.Close()
	other.Server.Config.Handler = nil
	return other.Token(Claims{Sub: sub, Issuer: p.Server.URL})
}
