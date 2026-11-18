package authtest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/getplinth/plinth/internal/auth"
)

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type flow struct {
	t *testing.T
	p *Provider
	c *http.Client
}

func newFlow(t *testing.T) *flow {
	p := New("plinth-ui")
	t.Cleanup(p.Close)
	p.EnableLogin([]User{
		{Sub: "alice", Email: "alice@example.test", Name: "Alice", Groups: []string{"devs"}},
		{Sub: "root", Email: "root@example.test", Name: "Root", Groups: []string{"plinth-admins"}},
	})
	return &flow{t: t, p: p, c: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// authorize walks the browser steps and returns the authorization code.
func (f *flow) authorize(user, verifier, redirect string) (code, state string, status int) {
	f.t.Helper()
	form := url.Values{
		"client_id": {"plinth-ui"}, "redirect_uri": {redirect}, "state": {"st-1"}, "nonce": {"nonce-1"},
		"code_challenge": {challenge(verifier)}, "scope": {"openid"}, "user": {user},
	}
	resp, err := f.c.PostForm(f.p.Issuer()+"/authorize", form)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return "", "", resp.StatusCode
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	return loc.Query().Get("code"), loc.Query().Get("state"), resp.StatusCode
}

func (f *flow) exchange(code, verifier, redirect string) (map[string]any, int) {
	f.t.Helper()
	resp, err := f.c.PostForm(f.p.Issuer()+"/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"redirect_uri": {redirect}, "client_id": {"plinth-ui"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode
}

const redirect = "http://127.0.0.1:8080/callback"

func TestFullBrowserFlowGivesATokenPlinthAccepts(t *testing.T) {
	f := newFlow(t)
	code, state, status := f.authorize("1", "verifier-123", redirect)
	if status != http.StatusFound || code == "" || state != "st-1" {
		t.Fatalf("authorize: %d code=%q state=%q", status, code, state)
	}
	tok, status := f.exchange(code, "verifier-123", redirect)
	if status != 200 {
		t.Fatalf("token: %d %v", status, tok)
	}
	v, err := auth.New(context.Background(), auth.Config{Issuer: f.p.Issuer(), Audience: "plinth-ui", AdminGroup: "plinth-admins"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Verify(context.Background(), tok["id_token"].(string))
	if err != nil || id.Role != auth.RoleAdmin || id.Email != "root@example.test" {
		t.Fatalf("verify: %+v %v", id, err)
	}
}

func TestLoginPageListsUsersAndEscapesInput(t *testing.T) {
	f := newFlow(t)
	q := url.Values{"client_id": {"plinth-ui"}, "response_type": {"code"}, "redirect_uri": {redirect},
		"code_challenge": {"c"}, "code_challenge_method": {"S256"}, "state": {`"><script>x</script>`}}
	resp, err := http.Get(f.p.Issuer() + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	body := sb.String()
	if resp.StatusCode != 200 || !strings.Contains(body, "Alice") || !strings.Contains(body, "Root") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "<script>x") {
		t.Fatal("parameters must be HTML-escaped")
	}
}

func TestRejectsBadAuthorizationRequests(t *testing.T) {
	f := newFlow(t)
	base := url.Values{"client_id": {"plinth-ui"}, "response_type": {"code"}, "redirect_uri": {redirect},
		"code_challenge": {"c"}, "code_challenge_method": {"S256"}}
	mutate := map[string]func(url.Values){
		"wrong client":    func(v url.Values) { v.Set("client_id", "other") },
		"no PKCE":         func(v url.Values) { v.Del("code_challenge") },
		"plain PKCE":      func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"remote redirect": func(v url.Values) { v.Set("redirect_uri", "https://evil.example/cb") },
		"implicit flow":   func(v url.Values) { v.Set("response_type", "token") },
	}
	for name, m := range mutate {
		v := url.Values{}
		for k, vals := range base {
			v[k] = vals
		}
		m(v)
		resp, err := http.Get(f.p.Issuer() + "/authorize?" + v.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: got %d", name, resp.StatusCode)
		}
	}
	if _, _, status := f.authorize("0", "v", "https://evil.example/cb"); status != http.StatusBadRequest {
		t.Errorf("remote redirect on sign-in: %d", status)
	}
	if _, _, status := f.authorize("99", "v", redirect); status != http.StatusBadRequest {
		t.Errorf("unknown user: %d", status)
	}
}

func TestTokenEndpointEnforcesPKCEAndSingleUse(t *testing.T) {
	f := newFlow(t)

	code, _, _ := f.authorize("0", "right-verifier", redirect)
	if _, status := f.exchange(code, "wrong-verifier", redirect); status != 400 {
		t.Errorf("wrong verifier: %d", status)
	}
	// The failed attempt burned the code.
	if _, status := f.exchange(code, "right-verifier", redirect); status != 400 {
		t.Errorf("a code must not survive a failed exchange: %d", status)
	}

	code, _, _ = f.authorize("0", "right-verifier", redirect)
	if _, status := f.exchange(code, "right-verifier", "http://127.0.0.1:9999/other"); status != 400 {
		t.Errorf("redirect mismatch: %d", status)
	}

	code, _, _ = f.authorize("0", "right-verifier", redirect)
	if _, status := f.exchange(code, "right-verifier", redirect); status != 200 {
		t.Fatalf("valid exchange: %d", status)
	}
	if _, status := f.exchange(code, "right-verifier", redirect); status != 400 {
		t.Errorf("a code must work only once: %d", status)
	}
	if _, status := f.exchange("made-up", "x", redirect); status != 400 {
		t.Errorf("unknown code: %d", status)
	}
}

func TestBrowserCanReadDiscoveryAndKeys(t *testing.T) {
	f := newFlow(t)
	for _, p := range []string{"/.well-known/openid-configuration", "/keys"} {
		resp, err := http.Get(f.p.Issuer() + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("Access-Control-Allow-Origin") == "" {
			t.Errorf("%s lacks CORS headers; a browser app could not read it", p)
		}
		// Browser OIDC libraries refuse discovery documents that are not JSON.
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s has Content-Type %q, want application/json", p, ct)
		}
	}
}

// A provider behind a service name advertises that name in discovery and in
// the tokens it issues, so a verifier configured with it accepts them.
func TestPublicURLIsWhatTokensAndDiscoveryAdvertise(t *testing.T) {
	f := newFlow(t)
	f.p.SetPublicURL("http://devidp.idp.svc:9100/")

	resp, err := http.Get(f.p.Server.URL + "/.well-known/openid-configuration")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	for _, k := range []string{"issuer", "jwks_uri", "authorization_endpoint", "token_endpoint"} {
		if !strings.HasPrefix(doc[k].(string), "http://devidp.idp.svc:9100") {
			t.Errorf("%s = %v, want the public address", k, doc[k])
		}
	}
	if doc["issuer"] != "http://devidp.idp.svc:9100" {
		t.Errorf("issuer must not keep a trailing slash: %v", doc["issuer"])
	}
	if f.p.Issuer() != "http://devidp.idp.svc:9100" {
		t.Errorf("Issuer() = %s", f.p.Issuer())
	}
	// Tokens carry the public issuer.
	tok := f.p.Token(Claims{Sub: "x"})
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
	if !strings.Contains(string(payload), `"iss":"http://devidp.idp.svc:9100"`) {
		t.Errorf("token issuer: %s", payload)
	}
}
