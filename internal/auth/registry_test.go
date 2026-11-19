package auth

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getplinth/plinth/internal/auth/authtest"
)

type recorder struct{}

func (recorder) Warn(string, ...any) {}

func registry(t *testing.T, src func() []Provider, static *Provider) *Registry {
	t.Helper()
	r := &Registry{AllowLoopback: true, Static: static, Log: recorder{}}
	r.Source = func(context.Context) ([]Provider, error) { return src(), nil }
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSeveralProvidersAreToldApartByIssuerAndNamespaced(t *testing.T) {
	a, b := authtest.New("client-a"), authtest.New("client-b")
	defer a.Close()
	defer b.Close()
	r := registry(t, func() []Provider {
		return []Provider{
			{ID: "id-a", Name: "a", Issuer: a.Issuer(), ClientID: "client-a", AdminGroup: "admins-a"},
			{ID: "id-b", Name: "b", Issuer: b.Issuer(), ClientID: "client-b", AdminGroup: "admins-b", TrustUnverifiedEmail: true},
		}
	}, nil)
	ctx := context.Background()

	ida, err := r.Verify(ctx, a.Token(authtest.Claims{Sub: "same", Email: "x@example.test", Groups: []string{"admins-a"}}))
	if err != nil {
		t.Fatal(err)
	}
	idb, err := r.Verify(ctx, b.Token(authtest.Claims{Sub: "same", Email: "x@example.test", Groups: []string{"admins-a"}, Unverified: true}))
	if err != nil {
		t.Fatal(err)
	}
	if ida.Sub == idb.Sub || ida.Sub != "oidc:id-a:same" || idb.Sub != "oidc:id-b:same" {
		t.Errorf("the same subject at two providers must be two people: %q %q", ida.Sub, idb.Sub)
	}
	if ida.Role != RoleAdmin || idb.Role != RoleUser {
		t.Errorf("each provider has its own administrator group: %s %s", ida.Role, idb.Role)
	}
	if !ida.EmailVerified || !idb.EmailVerified {
		t.Errorf("provider b is trusted without email_verified: %v %v", ida.EmailVerified, idb.EmailVerified)
	}
	if id, _ := r.Verify(ctx, a.Token(authtest.Claims{Sub: "u", Email: "x@example.test", Unverified: true})); id.EmailVerified {
		t.Error("provider a is not trusted without the claim")
	}
}

func TestOnlyConfiguredIssuersAndAudiencesAreAccepted(t *testing.T) {
	a, rogue := authtest.New("client-a"), authtest.New("client-a")
	defer a.Close()
	defer rogue.Close()
	r := registry(t, func() []Provider {
		return []Provider{{ID: "id-a", Name: "a", Issuer: a.Issuer(), ClientID: "client-a"}}
	}, nil)
	ctx := context.Background()
	for name, tok := range map[string]string{
		"unknown issuer":        rogue.Token(authtest.Claims{Sub: "u"}),
		"wrong audience":        a.Token(authtest.Claims{Sub: "u", Audience: "someone-else"}),
		"expired":               a.Token(authtest.Claims{Sub: "u", Expires: -time.Minute}),
		"signed by another key": a.ForeignToken("u"),
		"issuer claim forged":   rogue.Token(authtest.Claims{Sub: "u", Issuer: a.Issuer()}),
		"not a token":           "plt_abc",
		"two parts":             "a.b",
		"empty":                 "",
		"garbage payload":       "a.!!!.c",
	} {
		if id, err := r.Verify(ctx, tok); err == nil {
			t.Errorf("%s was accepted: %+v", name, id)
		}
	}
	if _, err := r.Verify(ctx, a.Token(authtest.Claims{Sub: "u"})); err != nil {
		t.Errorf("a good token: %v", err)
	}
}

func TestTheEnvironmentProviderKeepsItsSubjectAndIsReadOnly(t *testing.T) {
	env := authtest.New("plinth")
	defer env.Close()
	r := registry(t, func() []Provider { return nil }, &Provider{Issuer: env.Issuer(), ClientID: "plinth", AdminGroup: "plinth-admins"})
	id, err := r.Verify(context.Background(), env.Token(authtest.Claims{Sub: "alice", Email: "alice@example.test", Groups: []string{"plinth-admins"}}))
	if err != nil || id.Sub != "alice" || id.Role != RoleAdmin {
		t.Fatalf("people who signed in before must keep their identity: %+v %v", id, err)
	}
	list := r.Providers(context.Background())
	if len(list) != 1 || !list[0].ReadOnly {
		t.Errorf("%+v", list)
	}
}

func TestProvidersCanBeAddedWhileRunning(t *testing.T) {
	a, late := authtest.New("client-a"), authtest.New("client-late")
	defer a.Close()
	defer late.Close()
	var mu sync.Mutex
	list := []Provider{{ID: "id-a", Name: "a", Issuer: a.Issuer(), ClientID: "client-a"}}
	r := registry(t, func() []Provider { mu.Lock(); defer mu.Unlock(); return append([]Provider(nil), list...) }, nil)
	ctx := context.Background()
	tok := late.Token(authtest.Claims{Sub: "u"})
	if _, err := r.Verify(ctx, tok); err == nil {
		t.Fatal("not configured yet")
	}
	mu.Lock()
	list = append(list, Provider{ID: "id-late", Name: "late", Issuer: late.Issuer(), ClientID: "client-late"})
	mu.Unlock()
	time.Sleep(1100 * time.Millisecond) // a token from an unknown issuer triggers a reload, at most once a second
	if id, err := r.Verify(ctx, tok); err != nil || id.Sub != "oidc:id-late:u" {
		t.Fatalf("a provider added elsewhere (another replica) must be picked up: %+v %v", id, err)
	}
	mu.Lock()
	list = list[:1]
	mu.Unlock()
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Verify(ctx, tok); err == nil {
		t.Error("a removed provider must stop working")
	}
}

func TestAProviderThatIsDownDoesNotStopTheOthers(t *testing.T) {
	up, down := authtest.New("client-up"), authtest.New("client-down")
	defer up.Close()
	downIssuer := down.Issuer()
	down.Close()
	r := registry(t, func() []Provider {
		return []Provider{
			{ID: "id-down", Name: "down", Issuer: downIssuer, ClientID: "client-down"},
			{ID: "id-up", Name: "up", Issuer: up.Issuer(), ClientID: "client-up"},
		}
	}, nil)
	if _, err := r.Verify(context.Background(), up.Token(authtest.Claims{Sub: "u"})); err != nil {
		t.Fatalf("one dead provider must not break sign-in at another: %v", err)
	}
}

func TestIssuerValidationAndCheck(t *testing.T) {
	for issuer, allowLoopback := range map[string]bool{
		"https://login.example.com/realms/plinth": false, "https://login.example.com": false, "http://127.0.0.1:9000": true,
	} {
		if err := ValidateIssuer(issuer, allowLoopback); err != nil {
			t.Errorf("%s: %v", issuer, err)
		}
	}
	for issuer, allowLoopback := range map[string]bool{
		"": false, "login.example.com": false, "http://login.example.com": false, "ftp://x.example.com": false,
		"https://user:pw@login.example.com": false, "https://login.example.com?x=1": false, "https://login.example.com#f": false,
		"https://169.254.169.254/": true, "https://[fe80::1]/": true,
		"https://127.0.0.1/": false, "http://127.0.0.1:9000": false, "https://0.0.0.0/": true,
	} {
		if err := ValidateIssuer(issuer, allowLoopback); err == nil {
			t.Errorf("%q (loopback allowed: %v) must be refused", issuer, allowLoopback)
		}
	}
	idp := authtest.New("x")
	defer idp.Close()
	r := &Registry{AllowLoopback: true}
	if err := r.Check(context.Background(), idp.Issuer()); err != nil {
		t.Errorf("a working provider: %v", err)
	}
	if err := (&Registry{}).Check(context.Background(), idp.Issuer()); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("a local http provider must be refused unless local testing is allowed: %v", err)
	}
	if err := r.Check(context.Background(), "http://127.0.0.1:1"); err == nil {
		t.Error("nothing listens there")
	}
}
