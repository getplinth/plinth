package auth

import (
	"context"
	"testing"
	"time"

	"github.com/getplinth/plinth/internal/auth/authtest"
)

func newVerifier(t *testing.T, p *authtest.Provider) *Verifier {
	t.Helper()
	v, err := New(context.Background(), Config{Issuer: p.Issuer(), Audience: p.Audience, AdminGroup: "plinth-admins"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVerifyMapsRoles(t *testing.T) {
	p := authtest.New("plinth")
	defer p.Close()
	v := newVerifier(t, p)

	user, err := v.Verify(context.Background(), p.Token(authtest.Claims{Sub: "u1", Email: "u1@example.test", Groups: []string{"devs"}}))
	if err != nil || user.Role != RoleUser || user.Email != "u1@example.test" || user.Sub != "u1" {
		t.Fatalf("user: %+v %v", user, err)
	}
	admin, err := v.Verify(context.Background(), p.Token(authtest.Claims{Sub: "a1", Email: "a1@example.test", Groups: []string{"devs", "plinth-admins"}}))
	if err != nil || admin.Role != RoleAdmin {
		t.Fatalf("admin: %+v %v", admin, err)
	}
	// No groups claim at all: ordinary user.
	if id, err := v.Verify(context.Background(), p.Token(authtest.Claims{Sub: "u2"})); err != nil || id.Role != RoleUser || id.Email != "u2" {
		t.Fatalf("no groups: %+v %v", id, err)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	p := authtest.New("plinth")
	defer p.Close()
	v := newVerifier(t, p)
	ctx := context.Background()

	cases := map[string]string{
		"empty":           "",
		"garbage":         "not-a-jwt",
		"expired":         p.Token(authtest.Claims{Sub: "u", Expires: -time.Minute}),
		"wrong audience":  p.Token(authtest.Claims{Sub: "u", Audience: "someone-else"}),
		"wrong issuer":    p.Token(authtest.Claims{Sub: "u", Issuer: "https://evil.example"}),
		"foreign key":     p.ForeignToken("u"),
		"missing subject": p.Token(authtest.Claims{}),
	}
	for name, tok := range cases {
		if _, err := v.Verify(ctx, tok); err == nil {
			t.Errorf("%s: token must be rejected", name)
		}
	}
	// Tampering with the payload breaks the signature.
	good := p.Token(authtest.Claims{Sub: "u", Groups: []string{"devs"}})
	bad := good[:len(good)-4] + "AAAA"
	if _, err := v.Verify(ctx, bad); err == nil {
		t.Error("a token with a damaged signature must be rejected")
	}
}

func TestEmptyAdminGroupMakesNobodyAdmin(t *testing.T) {
	p := authtest.New("plinth")
	defer p.Close()
	v, err := New(context.Background(), Config{Issuer: p.Issuer(), Audience: "plinth"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Verify(context.Background(), p.Token(authtest.Claims{Sub: "u", Groups: []string{""}}))
	if err != nil || id.Role != RoleUser {
		t.Fatalf("%+v %v", id, err)
	}
}

func TestNewRequiresIssuerAndAudience(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := New(context.Background(), Config{Issuer: "http://127.0.0.1:1", Audience: "x"}); err == nil {
		t.Fatal("an unreachable issuer must fail at startup")
	}
}
