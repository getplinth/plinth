package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func valid() map[string]string {
	return map[string]string{
		"PLINTH_DATABASE_URL":  "postgres://x",
		"PLINTH_MASTER_KEY":    "key",
		"PLINTH_OIDC_ISSUER":   "https://idp.example.com",
		"PLINTH_OIDC_AUDIENCE": "plinth",
	}
}

func TestDefaults(t *testing.T) {
	c, err := load(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.AdminGroup != "plinth-admins" || c.GroupsClaim != "groups" || c.Workers != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestAllProblemsReportedTogether(t *testing.T) {
	_, err := load(env(map[string]string{}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"PLINTH_DATABASE_URL", "PLINTH_MASTER_KEY", "PLINTH_OIDC_ISSUER", "PLINTH_OIDC_AUDIENCE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s in %v", want, err)
		}
	}
}

func TestIssuerMustBeHTTPSUnlessExplicitlyAllowed(t *testing.T) {
	m := valid()
	m["PLINTH_OIDC_ISSUER"] = "http://idp.example.com"
	if _, err := load(env(m)); err == nil {
		t.Fatal("http issuer must be rejected by default")
	}
	m["PLINTH_OIDC_ALLOW_HTTP"] = "true"
	if _, err := load(env(m)); err != nil {
		t.Fatalf("explicit opt-in should work: %v", err)
	}
}

func TestWorkersBounds(t *testing.T) {
	for _, v := range []string{"0", "33", "abc", "-1"} {
		m := valid()
		m["PLINTH_WORKERS"] = v
		if _, err := load(env(m)); err == nil {
			t.Errorf("PLINTH_WORKERS=%q should be rejected", v)
		}
	}
	m := valid()
	m["PLINTH_WORKERS"] = "8"
	if c, err := load(env(m)); err != nil || c.Workers != 8 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	m := valid()
	m["PLINTH_MASTER_KEY"] = "super-secret-key"
	m["PLINTH_DATABASE_URL"] = "postgres://u:pw-secret@h/db"
	c, _ := load(env(m))
	if s := c.Redacted(); strings.Contains(s, "super-secret-key") || strings.Contains(s, "pw-secret") {
		t.Fatalf("leak: %s", s)
	}
}

func TestTrustUnverifiedEmailIsOffByDefaultAndMustBeABoolean(t *testing.T) {
	c, err := load(env(valid()))
	if err != nil || c.TrustUnverifiedEmail {
		t.Fatalf("default: %+v %v", c, err)
	}
	m := valid()
	m["PLINTH_TRUST_UNVERIFIED_EMAIL"] = "true"
	if c, err := load(env(m)); err != nil || !c.TrustUnverifiedEmail {
		t.Fatalf("on: %+v %v", c, err)
	}
	m["PLINTH_TRUST_UNVERIFIED_EMAIL"] = "maybe"
	if _, err := load(env(m)); err == nil {
		t.Fatal("a non-boolean value must be rejected, not treated as off")
	}
	m["PLINTH_TRUST_UNVERIFIED_EMAIL"] = "true"
	c, _ = load(env(m))
	if !strings.Contains(c.Redacted(), "trust_unverified_email=true") {
		t.Error("the setting should be visible in the startup log")
	}
}
