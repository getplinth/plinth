package authtest

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// User is someone the development login page offers.
type User struct {
	Sub    string
	Email  string
	Name   string
	Groups []string
}

type pendingCode struct {
	user        User
	challenge   string
	redirectURI string
	nonce       string
	expires     time.Time
}

type loginState struct {
	mu    sync.Mutex
	users []User
	codes map[string]pendingCode
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "content-type")
}

// EnableLogin adds the browser sign-in flow (authorization code with PKCE) so
// single-page apps can be developed and tested without a real identity
// provider. The "sign in" page simply lists the given users.
//
// DEVELOPMENT ONLY: there are no passwords, anyone who can reach the server
// can become any listed user. Redirects are limited to localhost.
func (p *Provider) EnableLogin(users []User) {
	st := &loginState{users: users, codes: map[string]pendingCode{}}
	p.login = st
	p.mux.HandleFunc("GET /authorize", p.authorizePage)
	p.mux.HandleFunc("POST /authorize", p.authorizeSelect)
	p.mux.HandleFunc("OPTIONS /token", func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.WriteHeader(http.StatusNoContent)
	})
	p.mux.HandleFunc("POST /token", p.token)
}

func localRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func (p *Provider) authorizePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != p.Audience || q.Get("response_type") != "code" ||
		q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" || !localRedirect(q.Get("redirect_uri")) {
		http.Error(w, "invalid authorization request (needs: response_type=code, this client_id, PKCE S256, a localhost redirect_uri)", http.StatusBadRequest)
		return
	}
	var b strings.Builder
	b.WriteString(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Development sign-in</title>
<style>body{font:16px system-ui;max-width:28rem;margin:3rem auto;padding:0 1rem}
button{display:block;width:100%;text-align:left;padding:.8rem 1rem;margin:.5rem 0;font:inherit;border:1px solid #888;border-radius:6px;background:#fff;cursor:pointer}
button:hover,button:focus-visible{outline:2px solid #1f7a6d}small{color:#555}</style>
<h1>Development sign-in</h1><p>This is a stand-in identity provider for local use. Choose who to sign in as.</p>
<form method="post" action="/authorize">`)
	for _, k := range []string{"client_id", "redirect_uri", "state", "nonce", "code_challenge", "scope"} {
		b.WriteString(`<input type="hidden" name="` + k + `" value="` + html.EscapeString(q.Get(k)) + `">`)
	}
	for i, u := range p.login.users {
		b.WriteString(`<button name="user" value="` + strconv.Itoa(i) + `">` + html.EscapeString(u.Name) +
			`<br><small>` + html.EscapeString(u.Email) + ` &middot; ` + html.EscapeString(strings.Join(u.Groups, ", ")) + `</small></button>`)
	}
	b.WriteString(`</form>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (p *Provider) authorizeSelect(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f := r.PostForm
	i, err := strconv.Atoi(f.Get("user"))
	if err != nil || i < 0 || i >= len(p.login.users) || f.Get("client_id") != p.Audience ||
		f.Get("code_challenge") == "" || !localRedirect(f.Get("redirect_uri")) {
		http.Error(w, "invalid sign-in", http.StatusBadRequest)
		return
	}
	code := randomHex(16)
	p.login.mu.Lock()
	p.login.codes[code] = pendingCode{
		user: p.login.users[i], challenge: f.Get("code_challenge"), redirectURI: f.Get("redirect_uri"),
		nonce: f.Get("nonce"), expires: time.Now().Add(time.Minute),
	}
	p.login.mu.Unlock()

	u, _ := url.Parse(f.Get("redirect_uri"))
	q := u.Query()
	q.Set("code", code)
	if s := f.Get("state"); s != "" {
		q.Set("state", s)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func tokenError(w http.ResponseWriter, msg string) {
	cors(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"` + msg + `"}`))
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		tokenError(w, "unsupported grant")
		return
	}
	code := r.PostForm.Get("code")
	p.login.mu.Lock()
	pc, ok := p.login.codes[code]
	delete(p.login.codes, code) // single use, also on failure
	p.login.mu.Unlock()
	if !ok || time.Now().After(pc.expires) {
		tokenError(w, "unknown or expired code")
		return
	}
	if r.PostForm.Get("redirect_uri") != pc.redirectURI || r.PostForm.Get("client_id") != p.Audience {
		tokenError(w, "redirect_uri or client_id mismatch")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(want), []byte(pc.challenge)) != 1 {
		tokenError(w, "PKCE verification failed")
		return
	}
	idToken := p.Token(Claims{Sub: pc.user.Sub, Email: pc.user.Email, Groups: pc.user.Groups, Nonce: pc.nonce})
	cors(w)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(`{"access_token":"` + randomHex(16) + `","token_type":"Bearer","expires_in":3600,"id_token":"` + idToken + `"}`))
}
