package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Provider is one OpenID Connect identity provider people may sign in with.
type Provider struct {
	// ID is empty for the provider configured through the environment, whose
	// users keep their original subject. For the others it namespaces the
	// subject, so two providers can never be mistaken for one another.
	ID          string
	Name        string
	DisplayName string
	Issuer      string
	ClientID    string // the audience tokens must carry
	Scopes      string
	GroupsClaim string
	AdminGroup  string
	// TrustUnverifiedEmail treats the address as verified without "email_verified".
	TrustUnverifiedEmail bool
	// ReadOnly marks the one from the environment: it cannot be changed in the control panel.
	ReadOnly bool
}

// Registry verifies tokens from any of its providers. The set can change while
// running: it is reloaded from Source, on demand and at most every few seconds.
// Discovery of a provider's keys happens on first use, so a provider that is
// down does not stop Plinth from starting or the others from working.
type Registry struct {
	// Source returns the providers configured by administrators.
	Source func(ctx context.Context) ([]Provider, error)
	// Static is the provider from the environment, if any.
	Static *Provider
	// AllowLoopback permits http:// issuers and issuers on this machine. For local testing only.
	AllowLoopback bool
	Log           interface{ Warn(string, ...any) }

	mu      sync.RWMutex
	byIss   map[string][]*entry
	enabled []Provider
	loaded  time.Time
	loadMu  sync.Mutex
}

type entry struct {
	p      Provider
	mu     sync.Mutex
	v      *oidc.IDTokenVerifier
	failed time.Time
}

const refreshEvery = 5 * time.Second

// Refresh reloads the providers from Source.
func (r *Registry) Refresh(ctx context.Context) error {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	var all []Provider
	if r.Static != nil {
		s := *r.Static
		s.ReadOnly = true
		all = append(all, s)
	}
	if r.Source != nil {
		list, err := r.Source(ctx)
		if err != nil {
			return err
		}
		all = append(all, list...)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old := map[string]*entry{}
	for _, es := range r.byIss {
		for _, e := range es {
			old[e.p.ID+"|"+e.p.Issuer+"|"+e.p.ClientID] = e // keep discovered keys when nothing changed
		}
	}
	r.byIss = map[string][]*entry{}
	r.enabled = all
	for _, p := range all {
		e := old[p.ID+"|"+p.Issuer+"|"+p.ClientID]
		if e == nil {
			e = &entry{}
		}
		e.p = p
		r.byIss[p.Issuer] = append(r.byIss[p.Issuer], e)
	}
	r.loaded = time.Now()
	return nil
}

// Providers lists the enabled providers, reloading them if the list is stale.
func (r *Registry) Providers(ctx context.Context) []Provider {
	r.mu.RLock()
	stale := time.Since(r.loaded) > refreshEvery
	r.mu.RUnlock()
	if stale {
		if err := r.Refresh(ctx); err != nil && r.Log != nil {
			r.Log.Warn("could not reload identity providers", "err", err.Error())
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Provider(nil), r.enabled...)
}

func (r *Registry) lookup(iss string) []*entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byIss[iss]
}

// Verify checks a token against the provider named in its "iss" claim.
func (r *Registry) Verify(ctx context.Context, raw string) (Identity, error) {
	iss, err := unverifiedIssuer(raw)
	if err != nil {
		return Identity{}, err
	}
	es := r.lookup(iss)
	if len(es) == 0 {
		// A provider may have just been added, perhaps through another replica.
		r.mu.RLock()
		recent := time.Since(r.loaded) < time.Second
		r.mu.RUnlock()
		if !recent {
			_ = r.Refresh(ctx)
			es = r.lookup(iss)
		}
	}
	if len(es) == 0 {
		return Identity{}, errors.New("auth: the token comes from an identity provider that is not configured")
	}
	var last error
	for _, e := range es {
		id, err := r.verifyWith(ctx, e, raw)
		if err == nil {
			return id, nil
		}
		last = err
	}
	return Identity{}, last
}

func (r *Registry) verifyWith(ctx context.Context, e *entry, raw string) (Identity, error) {
	v, err := r.verifier(ctx, e)
	if err != nil {
		return Identity{}, err
	}
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: %w", err)
	}
	groups := e.p.GroupsClaim
	if groups == "" {
		groups = "groups"
	}
	id, err := identityFrom(tok, e.p.AdminGroup, groups)
	if err != nil {
		return Identity{}, err
	}
	if e.p.ID != "" {
		id.Sub = "oidc:" + e.p.ID + ":" + id.Sub
	}
	if e.p.TrustUnverifiedEmail && strings.Contains(id.Email, "@") {
		id.EmailVerified = true
	}
	return id, nil
}

func (r *Registry) verifier(ctx context.Context, e *entry) (*oidc.IDTokenVerifier, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.v != nil {
		return e.v, nil
	}
	if !e.failed.IsZero() && time.Since(e.failed) < refreshEvery {
		return nil, errors.New("auth: the identity provider could not be reached a moment ago")
	}
	p, err := r.discover(ctx, e.p.Issuer)
	if err != nil {
		e.failed = time.Now()
		return nil, err
	}
	e.v = p.Verifier(&oidc.Config{ClientID: e.p.ClientID})
	return e.v, nil
}

// Check contacts an issuer the way sign-in will, so a wrong address or a
// provider that does not speak OpenID Connect is found when it is configured.
func (r *Registry) Check(ctx context.Context, issuer string) error {
	_, err := r.discover(ctx, issuer)
	return err
}

func (r *Registry) discover(ctx context.Context, issuer string) (*oidc.Provider, error) {
	if err := ValidateIssuer(issuer, r.AllowLoopback); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ctx = oidc.ClientContext(ctx, safeClient(r.AllowLoopback))
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("cannot discover %s: %w", issuer, err)
	}
	return p, nil
}

// unverifiedIssuer reads "iss" from a JWT without trusting it: only to choose which provider to verify with.
func unverifiedIssuer(raw string) (string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return "", errors.New("auth: not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", errors.New("auth: malformed token")
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Iss == "" {
		return "", errors.New("auth: token has no issuer")
	}
	return c.Iss, nil
}

// ValidateIssuer checks an issuer address before Plinth contacts it: https, a
// host, no credentials in the URL, and not a link-local address (where cloud
// metadata services live). Loopback and http are only for local testing.
func ValidateIssuer(issuer string, allowLoopback bool) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the issuer must be a web address such as https://login.example.com/realms/plinth, without credentials, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowLoopback) {
		return errors.New("the issuer must use https")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil {
		if blocked(ip, allowLoopback) {
			return errors.New("the issuer address is not allowed")
		}
	}
	return nil
}

func blocked(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	return ip.IsLoopback() && !allowLoopback
}

// safeClient connects to identity providers but refuses link-local addresses
// (checked after name resolution) and, unless allowed, this machine. It does
// not follow redirects to other hosts.
func safeClient(allowLoopback bool) *http.Client {
	d := &net.Dialer{
		Timeout: 8 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || blocked(ip, allowLoopback) {
				return errors.New("connection to that address is not allowed")
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 8 * time.Second, MaxIdleConns: 4},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Host != via[0].URL.Host {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}
