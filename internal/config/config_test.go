package config

import (
	"strings"
	"testing"
)

func load2(get func(string) string) (Config, error) { return load(get, true) }

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
	c, err := load2(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.AdminGroup != "plinth-admins" || c.GroupsClaim != "groups" || c.Workers != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestAllProblemsReportedTogether(t *testing.T) {
	_, err := load2(env(map[string]string{}))
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
	if _, err := load2(env(m)); err == nil {
		t.Fatal("http issuer must be rejected by default")
	}
	m["PLINTH_OIDC_ALLOW_HTTP"] = "true"
	if _, err := load2(env(m)); err != nil {
		t.Fatalf("explicit opt-in should work: %v", err)
	}
}

func TestWorkersBounds(t *testing.T) {
	for _, v := range []string{"0", "33", "abc", "-1"} {
		m := valid()
		m["PLINTH_WORKERS"] = v
		if _, err := load2(env(m)); err == nil {
			t.Errorf("PLINTH_WORKERS=%q should be rejected", v)
		}
	}
	m := valid()
	m["PLINTH_WORKERS"] = "8"
	if c, err := load2(env(m)); err != nil || c.Workers != 8 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	m := valid()
	m["PLINTH_MASTER_KEY"] = "super-secret-key"
	m["PLINTH_DATABASE_URL"] = "postgres://u:pw-secret@h/db"
	c, _ := load2(env(m))
	if s := c.Redacted(); strings.Contains(s, "super-secret-key") || strings.Contains(s, "pw-secret") {
		t.Fatalf("leak: %s", s)
	}
}

func TestTrustUnverifiedEmailIsOffByDefaultAndMustBeABoolean(t *testing.T) {
	c, err := load2(env(valid()))
	if err != nil || c.TrustUnverifiedEmail {
		t.Fatalf("default: %+v %v", c, err)
	}
	m := valid()
	m["PLINTH_TRUST_UNVERIFIED_EMAIL"] = "true"
	if c, err := load2(env(m)); err != nil || !c.TrustUnverifiedEmail {
		t.Fatalf("on: %+v %v", c, err)
	}
	m["PLINTH_TRUST_UNVERIFIED_EMAIL"] = "maybe"
	if _, err := load2(env(m)); err == nil {
		t.Fatal("a non-boolean value must be rejected, not treated as off")
	}
	m["PLINTH_TRUST_UNVERIFIED_EMAIL"] = "true"
	c, _ = load2(env(m))
	if !strings.Contains(c.Redacted(), "trust_unverified_email=true") {
		t.Error("the setting should be visible in the startup log")
	}
}

func TestPreviousMasterKeysAndStorageOnlyLoading(t *testing.T) {
	m := valid()
	m["PLINTH_MASTER_KEY_PREVIOUS"] = " old1, old2\nold3 "
	c, err := load2(env(m))
	if err != nil || len(c.MasterKeyPrevious) != 3 || c.MasterKeyPrevious[2] != "old3" {
		t.Fatalf("%+v %v", c.MasterKeyPrevious, err)
	}
	if strings.Contains(c.Redacted(), "old1") || !strings.Contains(c.Redacted(), "previous_master_keys=3") {
		t.Errorf("previous keys must be counted, never printed: %s", c.Redacted())
	}
	// Maintenance commands need the database and keys, not the identity provider.
	if _, err := load(env(map[string]string{"PLINTH_DATABASE_URL": "postgres://x", "PLINTH_MASTER_KEY": "k"}), false); err != nil {
		t.Errorf("storage-only: %v", err)
	}
	if _, err := load(env(map[string]string{"PLINTH_MASTER_KEY": "k"}), false); err == nil {
		t.Error("the database is still required")
	}
}

func TestNotificationsAreOffByDefault(t *testing.T) {
	c, err := load2(env(valid()))
	if err != nil || c.EmailEnabled() || c.WebhookEnabled() {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestSMTPSettings(t *testing.T) {
	m := valid()
	m["PLINTH_SMTP_HOST"] = "smtp.example.com"
	m["PLINTH_SMTP_FROM"] = "Plinth <plinth@example.com>"
	m["PLINTH_SMTP_USERNAME"], m["PLINTH_SMTP_PASSWORD"] = "u", "p-secret"
	c, err := load2(env(m))
	if err != nil || !c.EmailEnabled() || c.SMTPPort != 587 || c.SMTPTLS != "starttls" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if strings.Contains(c.Redacted(), "p-secret") || !strings.Contains(c.Redacted(), "email=true") {
		t.Errorf("the password must never be printed: %s", c.Redacted())
	}
	m["PLINTH_SMTP_TLS"] = "tls"
	if c, _ := load2(env(m)); c.SMTPPort != 465 {
		t.Errorf("implicit TLS defaults to port 465, got %d", c.SMTPPort)
	}
	for name, change := range map[string]map[string]string{
		"no sender":           {"PLINTH_SMTP_FROM": ""},
		"bad sender":          {"PLINTH_SMTP_FROM": "not an address"},
		"user without pass":   {"PLINTH_SMTP_PASSWORD": ""},
		"bad port":            {"PLINTH_SMTP_PORT": "99999"},
		"bad tls mode":        {"PLINTH_SMTP_TLS": "sometimes"},
		"unencrypted refused": {"PLINTH_SMTP_TLS": "none"},
		"newline in sender":   {"PLINTH_SMTP_FROM": "a@b.c\nBcc: x@y.z"},
	} {
		bad := valid()
		for k, v := range m {
			bad[k] = v
		}
		for k, v := range change {
			bad[k] = v
		}
		if _, err := load2(env(bad)); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
	m["PLINTH_SMTP_TLS"] = "none"
	m["PLINTH_NOTIFY_ALLOW_INSECURE"] = "true"
	if _, err := load2(env(m)); err != nil {
		t.Errorf("unencrypted is allowed when explicitly accepted: %v", err)
	}
	orphan := valid()
	orphan["PLINTH_SMTP_FROM"] = "plinth@example.com"
	if _, err := load2(env(orphan)); err == nil {
		t.Error("SMTP settings without a host are almost certainly a mistake")
	}
}

func TestWebhookAndPublicURLSettings(t *testing.T) {
	m := valid()
	m["PLINTH_WEBHOOK_URL"] = "https://hooks.example.com/abc"
	m["PLINTH_WEBHOOK_SECRET"] = "whsec-secret"
	m["PLINTH_PUBLIC_URL"] = "https://plinth.example.com/"
	c, err := load2(env(m))
	if err != nil || !c.WebhookEnabled() || c.PublicURL != "https://plinth.example.com" {
		t.Fatalf("%+v %v", c, err)
	}
	if strings.Contains(c.Redacted(), "whsec-secret") || strings.Contains(c.Redacted(), "hooks.example.com") {
		t.Errorf("the webhook address and secret must not be printed: %s", c.Redacted())
	}
	for name, kv := range map[string][2]string{
		"http webhook":     {"PLINTH_WEBHOOK_URL", "http://hooks.example.com"},
		"not a url":        {"PLINTH_WEBHOOK_URL", "hooks"},
		"ftp":              {"PLINTH_WEBHOOK_URL", "ftp://x.example.com"},
		"http public url":  {"PLINTH_PUBLIC_URL", "http://plinth.example.com"},
		"public no scheme": {"PLINTH_PUBLIC_URL", "plinth.example.com"},
	} {
		bad := valid()
		for k, v := range m {
			bad[k] = v
		}
		bad[kv[0]] = kv[1]
		if _, err := load2(env(bad)); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
	secretOnly := valid()
	secretOnly["PLINTH_WEBHOOK_SECRET"] = "x"
	if _, err := load2(env(secretOnly)); err == nil {
		t.Error("a secret without a URL is a mistake")
	}
	// Maintenance commands do not care about notification settings.
	if _, err := load(env(map[string]string{"PLINTH_DATABASE_URL": "postgres://x", "PLINTH_MASTER_KEY": "k", "PLINTH_SMTP_FROM": "junk"}), false); err != nil {
		t.Errorf("storage-only loading must ignore them: %v", err)
	}
}
