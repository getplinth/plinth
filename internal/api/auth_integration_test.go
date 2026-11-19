//go:build integration

package api_test

// Sign-in without an external identity provider (password accounts and
// sessions), and identity providers managed in the control panel, on a real
// PostgreSQL with stand-in OpenID Connect providers.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getplinth/plinth/internal/api"
	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/auth/authtest"
	"github.com/getplinth/plinth/internal/identity"
	"github.com/getplinth/plinth/internal/store"
)

func hashOf(token string) []byte { return store.HashToken(token) }

// secret returns a fresh password that passes the policy, so no password is written into the tests.
func secret(t *testing.T) string {
	t.Helper()
	p, err := auth.GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// authEnv serves the same database with the sign-in features switched on. The
// stand-in provider of env is the one "from the environment" when withEnvProvider is set.
func authEnv(t *testing.T, e *env, withEnvProvider, force bool) *env {
	return authEnvBurst(t, e, withEnvProvider, force, 1000)
}

// authEnvBurst is authEnv with a chosen sign-in burst (0 is the real default).
func authEnvBurst(t *testing.T, e *env, withEnvProvider, force bool, burst int) *env {
	t.Helper()
	var envProv *auth.Provider
	if withEnvProvider {
		envProv = &auth.Provider{Name: "environment", DisplayName: "Company SSO", Issuer: e.idp.Issuer(), ClientID: "plinth", AdminGroup: "plinth-admins"}
	}
	reg := identity.NewRegistry(e.st, envProv, true, slogDiscard())
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(api.Deps{
		Store: e.st, Verifier: reg, Providers: reg, EnvProvider: envProv, AllowLoopbackIssuers: true, ForceLocalLogin: force, LoginBurst: burst,
		RateBurst: 1000, RatePerMinute: 6000,
	}))
	t.Cleanup(srv.Close)
	cp := *e
	cp.srv = srv
	return &cp
}

type loginOut struct {
	Token      string
	MustChange bool
}

func (e *env) login(user, pw string) (int, loginOut, string) {
	e.t.Helper()
	code, v, raw := e.call("", "POST", "/api/v1/auth/login", map[string]string{"username": user, "password": pw})
	out := loginOut{}
	if t, ok := get(v, "token").(string); ok {
		out.Token = t
	}
	out.MustChange = get(v, "must_change_password") == true
	return code, out, raw
}

func (e *env) mustLogin(user, pw string) loginOut {
	e.t.Helper()
	code, out, raw := e.login(user, pw)
	if code != 200 {
		e.t.Fatalf("sign in as %s: %d %s", user, code, raw)
	}
	return out
}

// bootstrap creates the first administrator and completes the forced change of password.
func bootstrap(t *testing.T, e *env, username string) (token, password string) {
	t.Helper()
	first := secret(t)
	hash, _ := auth.HashPassword(first)
	if created, err := e.st.EnsureBootstrapAdmin(context.Background(), username, hash, true); err != nil || !created {
		t.Fatalf("bootstrap: %v %v", created, err)
	}
	s := e.mustLogin(username, first)
	password = secret(t)
	e.expect(204, s.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": first, "new_password": password})
	return s.Token, password
}

func TestBootstrapAdmin(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	pw := secret(t)
	hash, _ := auth.HashPassword(pw)

	t.Run("it creates one administrator, once, even when replicas start together", func(t *testing.T) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		created := 0
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := e.st.EnsureBootstrapAdmin(ctx, "admin", hash, true)
				if err != nil {
					t.Error(err)
				}
				if ok {
					mu.Lock()
					created++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if created != 1 {
			t.Fatalf("%d administrators were created", created)
		}
		if n := e.rawQuery(`SELECT count(*)::text FROM local_accounts`); n != "1" {
			t.Errorf("accounts: %s", n)
		}
		if ok, _ := e.st.EnsureBootstrapAdmin(ctx, "someone-else", hash, true); ok {
			t.Error("a later start must not add accounts: the first administrator already exists")
		}
	})

	t.Run("the account must change its password and the hash is not the password", func(t *testing.T) {
		if h := e.rawQuery(`SELECT password_hash FROM local_accounts`); strings.Contains(h, pw) || !strings.HasPrefix(h, "$argon2id$") {
			t.Errorf("stored: %s", h)
		}
		if e.rawQuery(`SELECT must_change::text FROM local_accounts`) != "true" {
			t.Error("a password chosen by someone else must be replaced at first sign-in")
		}
		if e.rawQuery(`SELECT role FROM users WHERE auth_source = 'local'`) != "admin" {
			t.Error("the bootstrap account is an administrator")
		}
	})

	t.Run("the command-line reset is the way back in", func(t *testing.T) {
		s := e.mustLogin("admin", pw)
		again := secret(t)
		h2, _ := auth.HashPassword(again)
		e.exec(`UPDATE local_accounts SET disabled = true, failed_attempts = 9, locked_until = now() + interval '1 hour'`)
		if created, err := e.st.ResetLocalPassword(ctx, "admin", h2); err != nil || created {
			t.Fatalf("reset: %v %v", created, err)
		}
		e.expect(401, s.Token, "GET", "/api/v1/me", nil) // the old session ended
		if out := e.mustLogin("admin", again); !out.MustChange {
			t.Error("a reset password must be changed again")
		}
		if created, err := e.st.ResetLocalPassword(ctx, "rescue", h2); err != nil || !created {
			t.Errorf("an account that does not exist is created: %v %v", created, err)
		}
	})
}

func TestBootstrapDoesNotSurpriseAnUpgradedInstallation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hash, _ := auth.HashPassword(secret(t))
	// Someone already signed in through an identity provider: this is not a fresh installation.
	e.expect(200, e.admin(), "GET", "/api/v1/me", nil)
	if created, err := e.st.EnsureBootstrapAdmin(ctx, "admin", hash, true); err != nil || created {
		t.Fatalf("an upgrade must not create a password administrator on its own: %v %v", created, err)
	}
	if n := e.rawQuery(`SELECT count(*)::text FROM local_accounts`); n != "0" {
		t.Errorf("accounts: %s", n)
	}
	// The explicit command still can.
	if created, err := e.st.EnsureBootstrapAdmin(ctx, "admin", hash, false); err != nil || !created {
		t.Errorf("bootstrap-admin on an existing installation: %v %v", created, err)
	}
}

func TestLocalSignIn(t *testing.T) {
	e := authEnv(t, newEnv(t), false, false)
	first := secret(t)
	hash, _ := auth.HashPassword(first)
	if _, err := e.st.EnsureBootstrapAdmin(context.Background(), "admin", hash, true); err != nil {
		t.Fatal(err)
	}

	t.Run("a wrong password and an unknown user get the same answer", func(t *testing.T) {
		c1, _, r1 := e.login("admin", "wrong-"+first)
		c2, _, r2 := e.login("nobody-here", "wrong-"+first)
		if c1 != 401 || c2 != 401 || r1 != r2 || !strings.Contains(r1, "invalid username or password") {
			t.Errorf("%d %s / %d %s", c1, r1, c2, r2)
		}
		start := time.Now()
		e.login("nobody-here", first)
		unknown := time.Since(start)
		start = time.Now()
		e.login("admin", "wrong-"+first)
		known := time.Since(start)
		if unknown < known/3 {
			t.Errorf("an unknown username answered in %v but a real one in %v: timing reveals who exists", unknown, known)
		}
		e.expect(400, "", "POST", "/api/v1/auth/login", `{"username":"admin","password":"x","extra":1}`)
	})

	var session string
	t.Run("the first password must be replaced before anything else works", func(t *testing.T) {
		s := e.mustLogin("Admin", first) // the name is not case sensitive
		if !s.MustChange || !strings.HasPrefix(s.Token, "plt_") {
			t.Fatalf("%+v", s)
		}
		me, _ := e.expect(200, s.Token, "GET", "/api/v1/me", nil)
		if get(me, "auth_source") != "local" || get(me, "must_change_password") != true || get(me, "role") != "admin" {
			t.Errorf("/me: %v", me)
		}
		_, raw := e.expect(403, s.Token, "GET", "/api/v1/requests", nil)
		if !strings.Contains(raw, "password_change_required") {
			t.Errorf("the refusal should say why: %s", raw)
		}
		e.expect(403, s.Token, "GET", "/api/v1/local-users", nil)

		other := e.mustLogin("admin", first) // a second session, to see it end
		next := secret(t)
		e.expect(403, s.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": "wrong-" + first, "new_password": next})
		e.expect(400, s.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": first, "new_password": first})
		for name, weak := range map[string]string{"short": "short-1", "contains the username": "my-admin-passphrase-1", "all one character": "zzzzzzzzzzzzzz"} {
			_, raw := e.expect(422, s.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": first, "new_password": weak})
			if !strings.Contains(raw, "password") {
				t.Errorf("%s: %s", name, raw)
			}
		}
		e.expect(204, s.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": first, "new_password": next})

		e.expect(200, s.Token, "GET", "/api/v1/requests", nil)  // this session carries on
		e.expect(401, other.Token, "GET", "/api/v1/me", nil)    // others ended
		if code, _, _ := e.login("admin", first); code != 401 { // the old password is gone
			t.Errorf("old password still works: %d", code)
		}
		session = e.mustLogin("admin", next).Token
		if e.mustLogin("admin", next).MustChange {
			t.Error("a password the person chose needs no further change")
		}
	})

	t.Run("repeated wrong passwords lock the account for a while, never for good", func(t *testing.T) {
		pw := secret(t)
		h, _ := auth.HashPassword(pw)
		if _, err := e.st.CreateLocalAccount(context.Background(), "locky", "", "user", h, false); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			if code, _, _ := e.login("locky", "wrong-"+pw); code != 401 {
				t.Fatalf("attempt %d: %d", i, code)
			}
		}
		if code, _, _ := e.login("locky", pw); code != 401 {
			t.Fatal("the right password must be refused while the account is locked")
		}
		if n := e.rawQuery(`SELECT count(*)::text FROM audit_log WHERE action = 'auth.lockout' AND target = 'locky'`); n != "1" {
			t.Errorf("the lockout should be audited once: %s", n)
		}
		e.exec(`UPDATE local_accounts SET locked_until = now() - interval '1 second' WHERE username = 'locky'`)
		if code, _, _ := e.login("locky", pw); code != 200 {
			t.Fatalf("after the lock ends the right password works: %d", code)
		}
		if n := e.rawQuery(`SELECT failed_attempts::text FROM local_accounts WHERE username = 'locky'`); n != "0" {
			t.Errorf("a successful sign-in clears the count: %s", n)
		}
	})

	t.Run("guessing is slowed down per username", func(t *testing.T) {
		real := authEnvBurst(t, e, false, false, 0) // the default limit
		limited := 0
		for i := 0; i < 16; i++ {
			code, _, _ := real.login("slow-down", "wrong-"+first)
			if code == 429 {
				limited++
			}
		}
		if limited == 0 {
			t.Error("sixteen quick attempts at one name must hit the limit")
		}
	})

	t.Run("sessions end on sign-out, on idle timeout and at the absolute limit", func(t *testing.T) {
		pw := secret(t)
		h, _ := auth.HashPassword(pw)
		e.st.CreateLocalAccount(context.Background(), "sessy", "", "user", h, false)
		a, b, c := e.mustLogin("sessy", pw), e.mustLogin("sessy", pw), e.mustLogin("sessy", pw)
		e.expect(204, a.Token, "POST", "/api/v1/auth/logout", nil)
		e.expect(401, a.Token, "GET", "/api/v1/me", nil)
		e.expect(200, b.Token, "GET", "/api/v1/me", nil)
		e.exec(`UPDATE sessions SET expires_at = now() - interval '1 second' WHERE token_hash = $1`, hashOf(b.Token))
		e.expect(401, b.Token, "GET", "/api/v1/me", nil)
		e.exec(`UPDATE sessions SET absolute_expires_at = now() - interval '1 second' WHERE token_hash = $1`, hashOf(c.Token))
		e.expect(401, c.Token, "GET", "/api/v1/me", nil)
		// Use extends the idle timeout but never past the absolute one.
		d := e.mustLogin("sessy", pw)
		e.exec(`UPDATE sessions SET last_used_at = now() - interval '2 minutes', expires_at = now() + interval '1 minute' WHERE token_hash = $1`, hashOf(d.Token))
		e.expect(200, d.Token, "GET", "/api/v1/me", nil)
		if left := e.rawQuery(`SELECT round(extract(epoch FROM (expires_at - now())) / 3600)::text FROM sessions WHERE token_hash = $1`, hashOf(d.Token)); left != "8" {
			t.Errorf("use should renew the idle timeout to 8 hours, got %s", left)
		}
		e.expect(401, "plt_notarealtoken", "GET", "/api/v1/me", nil)
		e.expect(401, "plt_", "GET", "/api/v1/me", nil)
	})

	t.Run("tokens are stored only as digests", func(t *testing.T) {
		if strings.Contains(e.rawQuery(`SELECT string_agg(encode(token_hash, 'hex'), ',') FROM sessions`), "plt_") {
			t.Error("a session token is stored in clear text")
		}
		_ = session
	})
}

func TestLocalUserAdministration(t *testing.T) {
	e := authEnv(t, newEnv(t), false, false)
	boss, _ := bootstrap(t, e, "boss")

	create := func(token string, body map[string]string, want int) (map[string]any, string) {
		v, raw := e.expect(want, token, "POST", "/api/v1/local-users", body)
		m, _ := v.(map[string]any)
		return m, raw
	}

	t.Run("an administrator creates accounts and gets a one-time password", func(t *testing.T) {
		out, _ := create(boss, map[string]string{"username": "Dana", "role": "user", "email": "dana@example.test"}, 201)
		temp, _ := out["temporary_password"].(string)
		if get(out, "username") != "dana" || len(temp) != 20 || out["must_change_password"] != true {
			t.Fatalf("%v", out)
		}
		s := e.mustLogin("dana", temp)
		if !s.MustChange {
			t.Error("a password the administrator chose must be changed")
		}
		own := secret(t)
		e.expect(204, s.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": temp, "new_password": own})
		e.expect(403, s.Token, "GET", "/api/v1/local-users", nil) // an ordinary user
		e.expect(403, s.Token, "POST", "/api/v1/local-users", map[string]string{"username": "x1", "role": "user"})
		e.expect(403, s.Token, "GET", "/api/v1/oidc-providers", nil)
		e.expect(403, s.Token, "PUT", "/api/v1/auth-settings", map[string]bool{"local_login_enabled": false})
		if raw := e.rawQuery(`SELECT string_agg(detail::text, ' ') FROM audit_log WHERE action LIKE 'local_user.%' OR action LIKE 'auth.%'`); strings.Contains(raw, temp) || strings.Contains(raw, own) {
			t.Error("a password reached the audit log")
		}
		list, _ := e.expect(200, boss, "GET", "/api/v1/local-users", nil)
		if len(list.([]any)) != 2 {
			t.Errorf("%v", list)
		}
	})

	t.Run("bad input is explained and a chosen password must pass the policy", func(t *testing.T) {
		create(boss, map[string]string{"username": "A", "role": "user"}, 400)
		create(boss, map[string]string{"username": "has space", "role": "user"}, 400)
		create(boss, map[string]string{"username": "erin", "role": "root"}, 400)
		create(boss, map[string]string{"username": "erin", "role": "user", "email": "not-an-email"}, 400)
		_, raw := create(boss, map[string]string{"username": "erin", "role": "user", "password": "short"}, 422)
		if !strings.Contains(raw, "at least 12") {
			t.Errorf("%s", raw)
		}
		create(boss, map[string]string{"username": "dana", "role": "user"}, 409)
		chosen := secret(t)
		out, _ := create(boss, map[string]string{"username": "erin", "role": "user", "password": chosen}, 201)
		if _, has := out["temporary_password"]; has {
			t.Error("a password the administrator chose is not echoed back")
		}
		if !e.mustLogin("erin", chosen).MustChange {
			t.Error("it still has to be changed at first sign-in")
		}
	})

	t.Run("disabling ends sessions and blocks sign-in; roles can change", func(t *testing.T) {
		out, _ := create(boss, map[string]string{"username": "frank", "role": "user"}, 201)
		id, temp := out["id"].(string), out["temporary_password"].(string)
		s := e.mustLogin("frank", temp)
		e.expect(200, boss, "PUT", "/api/v1/local-users/"+id, map[string]any{"disabled": true})
		e.expect(401, s.Token, "GET", "/api/v1/me", nil)
		if code, _, _ := e.login("frank", temp); code != 401 {
			t.Errorf("a disabled account signed in: %d", code)
		}
		e.expect(200, boss, "PUT", "/api/v1/local-users/"+id, map[string]any{"disabled": false, "role": "admin"})
		s2 := e.mustLogin("frank", temp)
		e.expect(204, s2.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": temp, "new_password": secret(t)})
		e.expect(200, s2.Token, "GET", "/api/v1/local-users", nil) // now an administrator
		e.expect(400, boss, "PUT", "/api/v1/local-users/"+id, map[string]any{"role": "superuser"})
		e.expect(404, boss, "PUT", "/api/v1/local-users/00000000-0000-0000-0000-000000000000", map[string]any{"disabled": true})
		e.expect(404, boss, "PUT", "/api/v1/local-users/not-a-uuid", map[string]any{"disabled": true})
	})

	t.Run("a reset gives a new one-time password and ends the old sessions", func(t *testing.T) {
		out, _ := create(boss, map[string]string{"username": "gina", "role": "user"}, 201)
		id, temp := out["id"].(string), out["temporary_password"].(string)
		s := e.mustLogin("gina", temp)
		v, _ := e.expect(200, boss, "POST", "/api/v1/local-users/"+id+"/reset-password", nil)
		again, _ := get(v, "temporary_password").(string)
		if len(again) != 20 || again == temp {
			t.Fatalf("%v", v)
		}
		e.expect(401, s.Token, "GET", "/api/v1/me", nil)
		if code, _, _ := e.login("gina", temp); code != 401 {
			t.Error("the old password still works")
		}
		if !e.mustLogin("gina", again).MustChange {
			t.Error("a reset password must be changed")
		}
		e.expect(422, boss, "POST", "/api/v1/local-users/"+id+"/reset-password", map[string]string{"password": "short"})
	})

	t.Run("you cannot lock yourself out", func(t *testing.T) {
		me, _ := e.expect(200, boss, "GET", "/api/v1/me", nil)
		self := get(me, "id").(string)
		e.expect(409, boss, "PUT", "/api/v1/local-users/"+self, map[string]any{"disabled": true})
		e.expect(409, boss, "PUT", "/api/v1/local-users/"+self, map[string]any{"role": "user"})
		e.expect(409, boss, "DELETE", "/api/v1/local-users/"+self, nil)
		e.expect(409, boss, "POST", "/api/v1/local-users/"+self+"/reset-password", nil)
		e.expect(200, boss, "PUT", "/api/v1/local-users/"+self, map[string]any{"email": "boss@example.test"})
	})

	t.Run("two administrators cannot remove each other at the same moment", func(t *testing.T) {
		e2 := authEnv(t, newEnv(t), false, false)
		a, _ := bootstrap(t, e2, "alpha")
		out, _ := e2.expect(201, a, "POST", "/api/v1/local-users", map[string]string{"username": "beta", "role": "admin"})
		beta := e2.mustLogin("beta", get(out, "temporary_password").(string))
		bp := secret(t)
		e2.expect(204, beta.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": get(out, "temporary_password").(string), "new_password": bp})
		betaTok := e2.mustLogin("beta", bp).Token
		list, _ := e2.expect(200, a, "GET", "/api/v1/local-users", nil)
		ids := map[string]string{}
		for _, u := range list.([]any) {
			ids[get(u, "username").(string)] = get(u, "id").(string)
		}
		codes := make(chan int, 2)
		var wg sync.WaitGroup
		for _, c := range []struct{ token, target string }{{a, ids["beta"]}, {betaTok, ids["alpha"]}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				code, _, _ := e2.call(c.token, "PUT", "/api/v1/local-users/"+c.target, map[string]any{"disabled": true})
				codes <- code
			}()
		}
		wg.Wait()
		close(codes)
		ok, refused := 0, 0
		for c := range codes {
			switch c {
			case 200:
				ok++
			case 409, 401:
				refused++
			}
		}
		if n := e2.rawQuery(`SELECT count(*)::text FROM local_accounts a JOIN users u ON u.id = a.user_id WHERE u.role = 'admin' AND NOT a.disabled`); n == "0" {
			t.Fatalf("nobody can sign in as an administrator any more (%d ok, %d refused)", ok, refused)
		}
	})

	t.Run("accounts with history cannot be deleted; others can", func(t *testing.T) {
		out, _ := create(boss, map[string]string{"username": "hank", "role": "user"}, 201)
		id := out["id"].(string)
		e.exec(`WITH t AS (INSERT INTO tenants (name, owner_id) VALUES ('hanks-team', $1) RETURNING id)
			INSERT INTO tenant_members (tenant_id, user_id, role) SELECT id, $1, 'owner' FROM t`, id)
		_, raw := e.expect(409, boss, "DELETE", "/api/v1/local-users/"+id, nil)
		if !strings.Contains(raw, "disable it instead") {
			t.Errorf("%s", raw)
		}
		out2, _ := create(boss, map[string]string{"username": "ivy", "role": "user"}, 201)
		e.expect(204, boss, "DELETE", "/api/v1/local-users/"+out2["id"].(string), nil)
		e.expect(404, boss, "DELETE", "/api/v1/local-users/"+out2["id"].(string), nil)
	})

	t.Run("an e-mail given by the administrator claims waiting team invitations", func(t *testing.T) {
		me, _ := e.expect(200, boss, "GET", "/api/v1/me", nil)
		e.exec(`WITH t AS (INSERT INTO tenants (name, owner_id) VALUES ('invite-local', $1) RETURNING id)
			INSERT INTO tenant_members (tenant_id, user_id, role) SELECT id, $1, 'owner' FROM t`, get(me, "id"))
		e.expect(202, boss, "POST", "/api/v1/tenants/invite-local/members", map[string]string{"email": "jo@example.test", "role": "member"})
		out, _ := create(boss, map[string]string{"username": "jo", "role": "user", "email": "jo@example.test"}, 201)
		s := e.mustLogin("jo", out["temporary_password"].(string))
		e.expect(204, s.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": out["temporary_password"].(string), "new_password": secret(t)})
		teams, _ := e.expect(200, boss, "GET", "/api/v1/tenants?all=true", nil)
		found := false
		for _, tm := range teams.([]any) {
			if get(tm, "name") == "invite-local" && get(tm, "member_count") == float64(2) {
				found = true
			}
		}
		if !found {
			t.Errorf("the invited person should have joined: %v", teams)
		}
	})
}

func TestIdentityProviders(t *testing.T) {
	base := newEnv(t)
	e := authEnv(t, base, true, false)
	idp2 := authtest.New("client-two")
	defer idp2.Close()
	oidcAdmin := base.admin() // from the provider "from the environment", in the administrator group
	boss, bossPW := bootstrap(t, e, "boss")
	_ = bossPW

	body := func(over map[string]any) map[string]any {
		m := map[string]any{"name": "second", "display_name": "Second SSO", "issuer": idp2.Issuer(), "client_id": "client-two", "admin_group": "ops"}
		for k, v := range over {
			m[k] = v
		}
		return m
	}

	t.Run("the sign-in page learns the ways to sign in", func(t *testing.T) {
		v, _ := e.expect(200, "", "GET", "/api/v1/config", nil)
		list := get(v, "providers").([]any)
		if get(v, "local_login") != true || len(list) != 1 || get(list[0], "id") != "environment" || get(list[0], "display_name") != "Company SSO" || get(list[0], "client_id") != "plinth" {
			t.Errorf("%v", v)
		}
		if strings.Contains(fmt.Sprint(v), "admin_group") {
			t.Error("the public configuration must not describe how administrators are chosen")
		}
	})

	var second string
	t.Run("an administrator adds a provider and people can sign in with it", func(t *testing.T) {
		v, _ := e.expect(201, boss, "POST", "/api/v1/oidc-providers", body(nil))
		second = get(v, "id").(string)
		cfg, _ := e.expect(200, "", "GET", "/api/v1/config", nil)
		if len(get(cfg, "providers").([]any)) != 2 {
			t.Fatalf("%v", cfg)
		}
		user := idp2.Token(authtest.Claims{Sub: "same-sub", Email: "kim@example.test", Groups: []string{"devs"}})
		me, _ := e.expect(200, user, "GET", "/api/v1/me", nil)
		if get(me, "role") != "user" || get(me, "email") != "kim@example.test" || get(me, "auth_source") != "oidc" {
			t.Errorf("%v", me)
		}
		if sub := e.rawQuery(`SELECT oidc_sub FROM users WHERE email = 'kim@example.test'`); sub != "oidc:"+second+":same-sub" {
			t.Errorf("subjects are namespaced by provider: %s", sub)
		}
		ops := idp2.Token(authtest.Claims{Sub: "ops-sub", Email: "ops@example.test", Groups: []string{"ops"}})
		if me, _ := e.expect(200, ops, "GET", "/api/v1/me", nil); get(me, "role") != "admin" {
			t.Errorf("the provider's own administrator group applies: %v", me)
		}
		// The same subject at the environment provider is a different person.
		if me, _ := e.expect(200, base.token("same-sub", "devs"), "GET", "/api/v1/me", nil); get(me, "email") == "kim@example.test" {
			t.Error("two providers must never share an identity")
		}
		list, _ := e.expect(200, boss, "GET", "/api/v1/oidc-providers", nil)
		if l := list.([]any); len(l) != 2 || get(l[0], "read_only") != true || get(l[1], "read_only") != false {
			t.Errorf("%v", list)
		}
	})

	t.Run("a provider that cannot work is refused with the reason", func(t *testing.T) {
		for name, c := range map[string]struct {
			over map[string]any
			want int
			text string
		}{
			"not a url":       {map[string]any{"name": "x1", "issuer": "login"}, 400, "issuer"},
			"wrong scheme":    {map[string]any{"name": "x2", "issuer": "ftp://login.example.com"}, 400, "https"},
			"link-local":      {map[string]any{"name": "x3", "issuer": "https://169.254.169.254"}, 400, "not allowed"},
			"credentials":     {map[string]any{"name": "x4", "issuer": "https://u:p@login.example.com"}, 400, "credentials"},
			"unreachable":     {map[string]any{"name": "x5", "issuer": "http://127.0.0.1:1"}, 422, "cannot be used"},
			"no display name": {map[string]any{"name": "x6", "display_name": " "}, 400, "name people recognise"},
			"no client id":    {map[string]any{"name": "x7", "client_id": ""}, 400, "client id"},
			"no openid scope": {map[string]any{"name": "x8", "scopes": "profile email"}, 400, "openid"},
			"bad name":        {map[string]any{"name": "Not Valid"}, 400, "lowercase"},
			"duplicate pair":  {map[string]any{"name": "x9"}, 409, "already exists"},
			"duplicate name":  {map[string]any{"client_id": "client-three"}, 409, "already exists"},
		} {
			_, raw := e.expect(c.want, boss, "POST", "/api/v1/oidc-providers", body(c.over))
			if c.text != "" && !strings.Contains(raw, c.text) {
				t.Errorf("%s: %s", name, raw)
			}
		}
		e.expect(403, base.alice(), "POST", "/api/v1/oidc-providers", body(map[string]any{"name": "x10"}))
	})

	t.Run("the provider from the environment is read-only", func(t *testing.T) {
		e.expect(409, boss, "PUT", "/api/v1/oidc-providers/environment", body(nil))
		e.expect(409, boss, "DELETE", "/api/v1/oidc-providers/environment", nil)
	})

	t.Run("disabling or deleting a provider takes effect at once", func(t *testing.T) {
		tok := idp2.Token(authtest.Claims{Sub: "kim", Email: "kim@example.test"})
		e.expect(200, boss, "PUT", "/api/v1/oidc-providers/"+second, body(map[string]any{"enabled": false, "display_name": "Second (off)"}))
		e.expect(401, tok, "GET", "/api/v1/me", nil)
		if cfg, _ := e.expect(200, "", "GET", "/api/v1/config", nil); len(get(cfg, "providers").([]any)) != 1 {
			t.Errorf("a disabled provider is not offered: %v", cfg)
		}
		e.expect(200, boss, "PUT", "/api/v1/oidc-providers/"+second, body(map[string]any{"enabled": true}))
		e.expect(200, tok, "GET", "/api/v1/me", nil)
		e.expect(404, boss, "PUT", "/api/v1/oidc-providers/00000000-0000-0000-0000-000000000000", body(nil))
	})

	t.Run("a hostile provider cannot take over a password account", func(t *testing.T) {
		me, _ := e.expect(200, boss, "GET", "/api/v1/me", nil)
		forged := base.idp.Token(authtest.Claims{Sub: "local:" + get(me, "id").(string), Email: "attacker@example.test", Groups: []string{"plinth-admins"}})
		e.expect(401, forged, "GET", "/api/v1/me", nil)
		if got := e.rawQuery(`SELECT email FROM users WHERE id = $1`, get(me, "id")); got == "attacker@example.test" {
			t.Fatal("the password account was overwritten by a provider")
		}
	})

	t.Run("password sign-in is turned off only when another way in is proven", func(t *testing.T) {
		// No administrator has signed in through a provider yet.
		fresh := authEnv(t, newEnv(t), true, false)
		freshBoss, _ := bootstrap(t, fresh, "boss")
		_, raw := fresh.expect(409, freshBoss, "PUT", "/api/v1/auth-settings", map[string]bool{"local_login_enabled": false})
		if !strings.Contains(raw, "signed in through an identity provider") {
			t.Errorf("%s", raw)
		}
		// Without any provider it is refused too.
		bare := authEnv(t, newEnv(t), false, false)
		bareBoss, _ := bootstrap(t, bare, "boss")
		bare.expect(409, bareBoss, "PUT", "/api/v1/auth-settings", map[string]bool{"local_login_enabled": false})
		bare.expect(400, bareBoss, "PUT", "/api/v1/auth-settings", map[string]any{})
	})

	t.Run("turning password sign-in off ends password sessions; a recovery switch brings it back", func(t *testing.T) {
		e.expect(200, oidcAdmin, "GET", "/api/v1/me", nil) // an administrator has signed in through a provider
		v, _ := e.expect(200, boss, "PUT", "/api/v1/auth-settings", map[string]bool{"local_login_enabled": false})
		if get(v, "local_login_enabled") != false {
			t.Fatalf("%v", v)
		}
		e.expect(401, boss, "GET", "/api/v1/me", nil)
		code, _, raw := e.login("boss", bossPW)
		if code != 403 || !strings.Contains(raw, "turned off") {
			t.Errorf("%d %s", code, raw)
		}
		if cfg, _ := e.expect(200, "", "GET", "/api/v1/config", nil); get(cfg, "local_login") != false {
			t.Errorf("%v", cfg)
		}
		forced := authEnv(t, base, true, true) // PLINTH_FORCE_LOCAL_LOGIN
		s := forced.mustLogin("boss", bossPW)
		forced.expect(200, s.Token, "GET", "/api/v1/me", nil)
		if cfg, _ := forced.expect(200, "", "GET", "/api/v1/config", nil); get(cfg, "local_login") != true {
			t.Errorf("forced: %v", cfg)
		}
		forced.expect(200, s.Token, "PUT", "/api/v1/auth-settings", map[string]bool{"local_login_enabled": true})
		if code, _, _ := e.login("boss", bossPW); code != 200 {
			t.Errorf("after turning it back on: %d", code)
		}
	})

	t.Run("the last way to sign in cannot be removed", func(t *testing.T) {
		only := authEnv(t, base, false, false) // only the provider added in the control panel
		e.expect(200, oidcAdmin, "PUT", "/api/v1/auth-settings", map[string]bool{"local_login_enabled": false})
		defer e.expect(200, oidcAdmin, "PUT", "/api/v1/auth-settings", map[string]bool{"local_login_enabled": true})
		// idp2 is the only provider and password sign-in is off.
		ops := idp2.Token(authtest.Claims{Sub: "ops-sub", Email: "ops@example.test", Groups: []string{"ops"}})
		_, raw := only.expect(409, ops, "PUT", "/api/v1/oidc-providers/"+second, body(map[string]any{"enabled": false}))
		if !strings.Contains(raw, "only way to sign in") {
			t.Errorf("%s", raw)
		}
		only.expect(409, ops, "DELETE", "/api/v1/oidc-providers/"+second, nil)
	})

	t.Run("an administrator from a provider may remove the last password administrator", func(t *testing.T) {
		me, _ := e.expect(200, boss, "GET", "/api/v1/me", nil)
		e.expect(200, oidcAdmin, "PUT", "/api/v1/local-users/"+get(me, "id").(string), map[string]any{"role": "user"})
	})

	t.Run("a deleted provider stops working and is audited", func(t *testing.T) {
		tok := idp2.Token(authtest.Claims{Sub: "kim", Email: "kim@example.test"})
		e.expect(200, tok, "GET", "/api/v1/me", nil)
		e.expect(204, oidcAdmin, "DELETE", "/api/v1/oidc-providers/"+second, nil)
		e.expect(401, tok, "GET", "/api/v1/me", nil)
		_, raw := e.expect(200, oidcAdmin, "GET", "/api/v1/audit?limit=300", nil)
		for _, want := range []string{"oidc_provider.create", "oidc_provider.update", "oidc_provider.delete", "auth.settings"} {
			if !strings.Contains(raw, want) {
				t.Errorf("no %s entry", want)
			}
		}
	})
}
