//go:build integration

package postgres

// Runs the driver against disposable PostgreSQL containers (needs Docker).
// Versions come from PLINTH_TEST_PG_VERSIONS (default "17,16,15"). Each
// container is removed when its test finishes.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getplinth/plinth/internal/naming"
	"github.com/getplinth/plinth/internal/provider"
)

const adminPassword = "itest-admin-password"

func versions() []string {
	v := os.Getenv("PLINTH_TEST_PG_VERSIONS")
	if v == "" {
		v = "17,16,15"
	}
	return strings.Split(v, ",")
}

// startPostgres launches a throwaway server bound to a random localhost port.
func startPostgres(t *testing.T, version string) (host string, port int) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("plinth-itest-pg%s-%06d", version, rand.Intn(1_000_000))
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD="+adminPassword,
		"-p", "127.0.0.1::5432", "postgres:"+version).CombinedOutput()
	if err != nil {
		t.Skipf("cannot start postgres:%s: %v: %s", version, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	pout, err := exec.Command("docker", "port", name, "5432/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	// "127.0.0.1:49153" (docker may print an extra IPv6 line)
	first := strings.TrimSpace(strings.Split(string(pout), "\n")[0])
	port, err = strconv.Atoi(first[strings.LastIndex(first, ":")+1:])
	if err != nil {
		t.Fatalf("parse port from %q: %v", pout, err)
	}

	d, _ := FromConfig(map[string]string{"host": "127.0.0.1", "port": strconv.Itoa(port),
		"admin_user": "postgres", "admin_password": adminPassword, "sslmode": "disable"})
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := d.connect(ctx)
		cancel()
		if err == nil {
			c.Close(context.Background())
			// The image restarts once during init; require two consecutive successes.
			time.Sleep(2 * time.Second)
			if c, err = d.connect(context.Background()); err == nil {
				c.Close(context.Background())
				return "127.0.0.1", port
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres:%s not ready: %v", version, err)
		}
		time.Sleep(time.Second)
	}
}

func newDriver(t *testing.T, host string, port int, adopt bool) *Driver {
	t.Helper()
	d, err := FromConfig(map[string]string{
		"host": host, "port": strconv.Itoa(port),
		"admin_user": "postgres", "admin_password": adminPassword,
		"sslmode": "disable", "adopt_existing": strconv.FormatBool(adopt),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// asTenant connects with an issued credential.
func asTenant(ctx context.Context, host string, port int, db, user, password string) (*pgx.Conn, error) {
	cfg, _ := pgx.ParseConfig("")
	cfg.Host, cfg.Port, cfg.Database, cfg.User, cfg.Password = host, uint16(port), db, user, password
	cfg.TLSConfig = nil
	cfg.Fallbacks = nil
	return pgx.ConnectConfig(ctx, cfg)
}

func TestPostgresDriver(t *testing.T) {
	for _, v := range versions() {
		v := v
		t.Run("postgres"+v, func(t *testing.T) {
			t.Parallel()
			host, port := startPostgres(t, v)
			runSuite(t, host, port)
		})
	}
}

func runSuite(t *testing.T, host string, port int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d := newDriver(t, host, port, false)
	// The legacy naming produces dashes and mixed case input.
	dbA := naming.DBName("Team-A", "Shop", "DEV")
	dbB := naming.DBName("team-b", "blog", "dev")

	if err := d.Validate(ctx); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	var credA provider.Credential
	t.Run("first run issues a working credential", func(t *testing.T) {
		is, err := d.EnsureDatabase(ctx, provider.DatabaseSpec{Name: dbA})
		if err != nil {
			t.Fatal(err)
		}
		if is.Outputs["created"] != "true" || is.Credential == nil {
			t.Fatalf("expected created+credential, got %v %v", is.Outputs, is.Credential != nil)
		}
		credA = is.Credential
		c, err := asTenant(ctx, host, port, dbA, dbA, credA["password"])
		if err != nil {
			t.Fatalf("tenant cannot log in: %v", err)
		}
		defer c.Close(ctx)
		// PostgreSQL 15+ no longer lets ordinary roles create in "public";
		// ownership of the database must make this work.
		if _, err := c.Exec(ctx, `CREATE TABLE items (id int primary key, v text)`); err != nil {
			t.Fatalf("tenant cannot create tables: %v", err)
		}
		if _, err := c.Exec(ctx, `INSERT INTO items VALUES (1, 'x')`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("re-run does not change the password", func(t *testing.T) {
		is, err := d.EnsureDatabase(ctx, provider.DatabaseSpec{Name: dbA})
		if err != nil {
			t.Fatal(err)
		}
		if is.Credential != nil || is.Outputs["created"] != "false" {
			t.Fatalf("second run must not issue a credential: %v", is.Outputs)
		}
		c, err := asTenant(ctx, host, port, dbA, dbA, credA["password"])
		if err != nil {
			t.Fatalf("old password stopped working: %v", err)
		}
		c.Close(ctx)
	})

	t.Run("rotate replaces the password", func(t *testing.T) {
		is, err := d.EnsureDatabase(ctx, provider.DatabaseSpec{Name: dbA, Rotate: true})
		if err != nil || is.Credential == nil {
			t.Fatalf("rotate: %v", err)
		}
		if _, err := asTenant(ctx, host, port, dbA, dbA, credA["password"]); err == nil {
			t.Fatal("old password must stop working after rotation")
		}
		c, err := asTenant(ctx, host, port, dbA, dbA, is.Credential["password"])
		if err != nil {
			t.Fatalf("new password: %v", err)
		}
		defer c.Close(ctx)
		var n int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM items`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("data lost across rotation: %d %v", n, err)
		}
		credA = is.Credential
	})

	t.Run("tenants are isolated and unprivileged", func(t *testing.T) {
		isB, err := d.EnsureDatabase(ctx, provider.DatabaseSpec{Name: dbB})
		if err != nil {
			t.Fatal(err)
		}
		// B cannot connect to A's database even with a valid login.
		if _, err := asTenant(ctx, host, port, dbA, dbB, isB.Credential["password"]); err == nil {
			t.Fatal("tenant B must not be able to connect to tenant A's database")
		}
		admin, _ := d.connect(ctx)
		defer admin.Close(ctx)
		var super, cdb, crole, repl, bypass bool
		if err := admin.QueryRow(ctx, `SELECT rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls
			FROM pg_roles WHERE rolname = $1`, dbA).Scan(&super, &cdb, &crole, &repl, &bypass); err != nil {
			t.Fatal(err)
		}
		if super || cdb || crole || repl || bypass {
			t.Fatalf("tenant role is over-privileged: %v %v %v %v %v", super, cdb, crole, repl, bypass)
		}
	})

	t.Run("hostile names are rejected before any SQL runs", func(t *testing.T) {
		for _, n := range []string{`x"; DROP DATABASE postgres; --`, "postgres", "pg_monitor", "Upper"} {
			if _, err := d.EnsureDatabase(ctx, provider.DatabaseSpec{Name: n}); err == nil {
				t.Errorf("%q should be rejected", n)
			}
		}
		admin, _ := d.connect(ctx)
		defer admin.Close(ctx)
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE datname = 'postgres'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("postgres database damaged: %d %v", n, err)
		}
	})

	t.Run("pre-existing objects are refused unless adopted", func(t *testing.T) {
		admin, _ := d.connect(ctx)
		defer admin.Close(ctx)
		legacy := "legacy_app_dev"
		for _, q := range []string{
			`CREATE ROLE ` + legacy + ` LOGIN PASSWORD 'legacy-pw'`,
			`CREATE DATABASE ` + legacy,
			`GRANT ALL PRIVILEGES ON DATABASE ` + legacy + ` TO ` + legacy,
		} {
			if _, err := admin.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.EnsureDatabase(ctx, provider.DatabaseSpec{Name: legacy}); err == nil || !strings.Contains(err.Error(), "not managed") {
			t.Fatalf("expected refusal, got %v", err)
		}
		if err := d.DeleteDatabase(ctx, provider.DatabaseSpec{Name: legacy}); err == nil {
			t.Fatal("must not drop an unmanaged database")
		}

		is, err := newDriver(t, host, port, true).EnsureDatabase(ctx, provider.DatabaseSpec{Name: legacy})
		if err != nil {
			t.Fatalf("adopt: %v", err)
		}
		if is.Credential != nil {
			t.Fatal("adopting must keep the existing password")
		}
		c, err := asTenant(ctx, host, port, legacy, legacy, "legacy-pw")
		if err != nil {
			t.Fatalf("legacy password must still work after adoption: %v", err)
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, `CREATE TABLE t (id int)`); err != nil {
			t.Fatalf("adopted role must be able to use public schema: %v", err)
		}
	})

	t.Run("a retry finishes a half-created role and still returns the credential", func(t *testing.T) {
		admin, _ := d.connect(ctx)
		defer admin.Close(ctx)
		name := "crashed_midway"
		// What an interrupted first attempt leaves behind.
		for _, q := range []string{
			`CREATE ROLE ` + name + ` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE`,
			`COMMENT ON ROLE ` + name + ` IS '` + markerPending + `'`,
		} {
			if _, err := admin.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		is, err := d.EnsureDatabase(ctx, provider.DatabaseSpec{Name: name})
		if err != nil || is.Credential == nil {
			t.Fatalf("retry should issue the credential: %v", err)
		}
		c, err := asTenant(ctx, host, port, name, name, is.Credential["password"])
		if err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
	})

	t.Run("errors and logs never contain passwords", func(t *testing.T) {
		bad, _ := FromConfig(map[string]string{"host": host, "port": strconv.Itoa(port),
			"admin_user": "postgres", "admin_password": "wrong-pw-xyz", "sslmode": "disable"})
		err := bad.Validate(ctx)
		if err == nil || strings.Contains(err.Error(), "wrong-pw-xyz") {
			t.Fatalf("expected an auth error without the password, got %v", err)
		}
		if s := fmt.Sprint(credA); strings.Contains(s, credA["password"]) {
			t.Fatal("Credential prints its password")
		}
	})

	t.Run("delete removes managed objects and is idempotent", func(t *testing.T) {
		for _, n := range []string{dbA, dbB, "crashed_midway"} {
			if err := d.DeleteDatabase(ctx, provider.DatabaseSpec{Name: n}); err != nil {
				t.Fatalf("delete %s: %v", n, err)
			}
			if err := d.DeleteDatabase(ctx, provider.DatabaseSpec{Name: n}); err != nil {
				t.Fatalf("second delete %s: %v", n, err)
			}
		}
		admin, _ := d.connect(ctx)
		defer admin.Close(ctx)
		var n int
		if err := admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_database WHERE datname = $1)
			+ (SELECT count(*) FROM pg_roles WHERE rolname = $1)`, dbA).Scan(&n); err != nil || n != 0 {
			t.Fatalf("leftovers: %d %v", n, err)
		}
	})
}
