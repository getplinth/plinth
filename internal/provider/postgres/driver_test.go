package postgres

import (
	"strings"
	"testing"

	"github.com/getplinth/plinth/internal/naming"
	"github.com/getplinth/plinth/internal/provider"
)

func TestCheckName(t *testing.T) {
	ok := []string{"a", "team-a_shop_dev", "x1", naming.DBName("Team-A", "Shop", "DEV")}
	bad := []string{
		"", "-x", "_x", "Upper", "has space", "a.b", `x"; DROP DATABASE postgres; --`,
		"postgres", "public", "template0", "template1", "pg_catalog", "pg_monitor",
		strings.Repeat("a", 64),
	}
	for _, n := range ok {
		if err := CheckName(n, "plinth_admin"); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := CheckName(n, "plinth_admin"); err == nil {
			t.Errorf("%q should be rejected", n)
		}
	}
	if err := CheckName("plinth_admin", "plinth_admin"); err == nil {
		t.Error("the admin role name must be reserved")
	}
}

func TestFromConfigValidation(t *testing.T) {
	const secret = "s3cr3t-admin-pw"
	good := map[string]string{"host": "db", "admin_user": "u", "admin_password": secret}
	if _, err := FromConfig(good); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"no host":     func(m map[string]string) { delete(m, "host") },
		"no password": func(m map[string]string) { delete(m, "admin_password") },
		"bad port":    func(m map[string]string) { m["port"] = "99999" },
		"bad sslmode": func(m map[string]string) { m["sslmode"] = "prefer" },
		"bad ca":      func(m map[string]string) { m["ca_cert"] = "not pem" },
	} {
		cfg := map[string]string{}
		for k, v := range good {
			cfg[k] = v
		}
		mutate(cfg)
		if _, err := FromConfig(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the password: %v", name, err)
		}
	}
}

func TestSecureByDefault(t *testing.T) {
	d, _ := FromConfig(map[string]string{"host": "db", "admin_user": "u", "admin_password": "p"})
	if d.tlsConfig == nil || d.tlsConfig.InsecureSkipVerify {
		t.Fatal("default must be verify-full")
	}
}

func TestNewPassword(t *testing.T) {
	a, _ := newPassword()
	b, _ := newPassword()
	if len(a) != 48 || a == b {
		t.Fatalf("passwords: %q %q", a, b)
	}
}

func TestRegisteredAsDatabaseDriver(t *testing.T) {
	_, err := provider.New(provider.KindDatabase, "postgres", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("driver should be registered and validate config, got %v", err)
	}
}
