// Package postgres is the Database driver for PostgreSQL servers.
//
// Design rules (all fixing problems in the legacy script):
//   - SQL identifiers and literals are quoted by the server with format(%I, %L),
//     never by string concatenation.
//   - A password is generated only when the role is first created or when the
//     caller asks for rotation; a re-run never changes it.
//   - The tenant role owns its database (so it can use the public schema on
//     PostgreSQL 15+) and no other tenant can connect to it.
//   - Plinth only modifies or drops roles and databases that carry its
//     comment marker, unless adopt_existing is set.
package postgres

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/getplinth/plinth/internal/provider"
)

const (
	markerManaged = "managed-by:plinth"
	// markerPending is set on a role that was created but has not yet been
	// given a password. It lets a retry finish the job and still return the
	// credential after a crash between the two steps.
	markerPending = "managed-by:plinth;credential-pending"
)

func init() {
	provider.Register(provider.KindDatabase, "postgres", func(cfg map[string]string) (provider.Provider, error) {
		return FromConfig(cfg)
	})
}

// Driver implements provider.Database for one PostgreSQL server.
type Driver struct {
	host, adminUser, adminPassword, adminDB string
	port                                    uint16
	tlsConfig                               *tls.Config
	adopt                                   bool
}

var _ provider.Database = (*Driver)(nil)

// FromConfig builds a driver from an integration's decrypted configuration.
//
//	host              server hostname (required)
//	port              default 5432
//	admin_user        role with CREATEDB and CREATEROLE (required)
//	admin_password    (required)
//	admin_database    maintenance database, default "postgres"
//	sslmode           verify-full (default) | require (encrypt, no verification) | disable
//	ca_cert           optional PEM CA bundle for verify-full
//	adopt_existing    "true" to take over roles/databases created before Plinth
func FromConfig(cfg map[string]string) (*Driver, error) {
	for _, k := range []string{"host", "admin_user", "admin_password"} {
		if cfg[k] == "" {
			return nil, fmt.Errorf("postgres: config %q is required", k)
		}
	}
	port := uint64(5432)
	if p := cfg["port"]; p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("postgres: invalid port %q", p)
		}
		port = n
	}
	d := &Driver{
		host: cfg["host"], port: uint16(port),
		adminUser: cfg["admin_user"], adminPassword: cfg["admin_password"],
		adminDB: valueOr(cfg["admin_database"], "postgres"),
	}
	d.adopt, _ = strconv.ParseBool(cfg["adopt_existing"])

	switch mode := valueOr(cfg["sslmode"], "verify-full"); mode {
	case "disable":
	case "require":
		d.tlsConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit opt-in: encrypts but does not authenticate the server
	case "verify-full":
		t := &tls.Config{ServerName: d.host, MinVersion: tls.VersionTLS12}
		if ca := cfg["ca_cert"]; ca != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(ca)) {
				return nil, errors.New("postgres: ca_cert is not valid PEM")
			}
			t.RootCAs = pool
		}
		d.tlsConfig = t
	default:
		return nil, fmt.Errorf("postgres: unsupported sslmode %q", mode)
	}
	return d, nil
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (*Driver) Kind() provider.Kind { return provider.KindDatabase }
func (*Driver) Type() string        { return "postgres" }

func (d *Driver) connect(ctx context.Context) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		return nil, err
	}
	// Overwrite everything explicitly so PG* environment variables on the
	// Plinth host can never influence which server or role is used.
	cfg.Host, cfg.Port = d.host, d.port
	cfg.User, cfg.Password, cfg.Database = d.adminUser, d.adminPassword, d.adminDB
	cfg.TLSConfig = d.tlsConfig
	cfg.Fallbacks = nil
	cfg.RuntimeParams = map[string]string{"application_name": "plinth"}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect to %s:%d: %w", d.host, d.port, err)
	}
	return conn, nil
}

// Validate checks connectivity, server version and the admin role's privileges.
func (d *Driver) Validate(ctx context.Context) error {
	conn, err := d.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	var version int
	var canDB, canRole bool
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int,
		       rolsuper OR rolcreatedb, rolsuper OR rolcreaterole
		FROM pg_roles WHERE rolname = current_user`).Scan(&version, &canDB, &canRole); err != nil {
		return fmt.Errorf("postgres: validate: %w", err)
	}
	if version < 130000 {
		return fmt.Errorf("postgres: server version %d is too old (need 13+)", version)
	}
	if !canDB || !canRole {
		return fmt.Errorf("postgres: admin role %q needs CREATEDB and CREATEROLE", d.adminUser)
	}
	return nil
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var reservedNames = map[string]bool{"postgres": true, "public": true, "template0": true, "template1": true}

// CheckName validates a database/role name. Dashes are allowed because the
// legacy naming produced them; they are safe because every use is quoted.
func CheckName(name, adminUser string) error {
	switch {
	case !nameRe.MatchString(name):
		return fmt.Errorf("postgres: invalid database name %q (allowed: a-z 0-9 _ -, max 63, no leading _ or -)", name)
	case reservedNames[name] || strings.HasPrefix(name, "pg_") || name == adminUser:
		return fmt.Errorf("postgres: name %q is reserved", name)
	}
	return nil
}

// execf formats a statement on the server (%I identifiers, %L literals) and runs it.
func execf(ctx context.Context, conn *pgx.Conn, format string, args ...string) error {
	var stmt string
	if err := conn.QueryRow(ctx, `SELECT format($1::text, VARIADIC $2::text[])`, format, args).Scan(&stmt); err != nil {
		return err
	}
	_, err := conn.Exec(ctx, stmt) // errors never include the statement, so literals do not leak
	return err
}

func newPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// EnsureDatabase creates the database and its owning login role.
// Outputs: host, port, name, user, created. Issued.Credential (host, port,
// name, user, password) is set only when a password was just generated.
func (d *Driver) EnsureDatabase(ctx context.Context, spec provider.DatabaseSpec) (provider.Issued, error) {
	var zero provider.Issued
	if err := CheckName(spec.Name, d.adminUser); err != nil {
		return zero, err
	}
	conn, err := d.connect(ctx)
	if err != nil {
		return zero, err
	}
	defer conn.Close(ctx)
	name := spec.Name
	created := false

	// --- role -----------------------------------------------------------
	var roleComment *string
	var super, createDB, createRole bool
	err = conn.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_authid'), rolsuper, rolcreatedb, rolcreaterole
		FROM pg_roles WHERE rolname = $1`, name).Scan(&roleComment, &super, &createDB, &createRole)
	roleExists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return zero, fmt.Errorf("postgres: look up role %q: %w", name, err)
	}
	pending := false
	switch {
	case !roleExists:
		if err := execf(ctx, conn, `CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`, name); err != nil {
			return zero, fmt.Errorf("postgres: create role %q: %w", name, err)
		}
		if err := execf(ctx, conn, `COMMENT ON ROLE %I IS %L`, name, markerPending); err != nil {
			return zero, fmt.Errorf("postgres: mark role %q: %w", name, err)
		}
		pending, created = true, true
	case comment(roleComment) == markerPending:
		pending = true // a previous attempt created the role but never set a password
	case comment(roleComment) != markerManaged:
		if !d.adopt {
			return zero, fmt.Errorf("postgres: role %q exists but is not managed by Plinth (set adopt_existing to take it over)", name)
		}
		if super || createDB || createRole {
			return zero, fmt.Errorf("postgres: refusing to adopt privileged role %q", name)
		}
		if err := execf(ctx, conn, `COMMENT ON ROLE %I IS %L`, name, markerManaged); err != nil {
			return zero, fmt.Errorf("postgres: adopt role %q: %w", name, err)
		}
	}

	// --- database -------------------------------------------------------
	var dbComment *string
	err = conn.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, name).Scan(&dbComment)
	dbExists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return zero, fmt.Errorf("postgres: look up database %q: %w", name, err)
	}
	switch {
	case !dbExists:
		if err := execf(ctx, conn, `CREATE DATABASE %I OWNER %I`, name, name); err != nil {
			return zero, fmt.Errorf("postgres: create database %q: %w", name, err)
		}
		if err := execf(ctx, conn, `COMMENT ON DATABASE %I IS %L`, name, markerManaged); err != nil {
			return zero, fmt.Errorf("postgres: mark database %q: %w", name, err)
		}
		created = true
	case comment(dbComment) != markerManaged:
		if !d.adopt {
			return zero, fmt.Errorf("postgres: database %q exists but is not managed by Plinth (set adopt_existing to take it over)", name)
		}
		// Legacy databases were owned by the admin with GRANT ALL; ownership
		// is what gives the role the public schema on PostgreSQL 15+.
		if err := execf(ctx, conn, `ALTER DATABASE %I OWNER TO %I`, name, name); err != nil {
			return zero, fmt.Errorf("postgres: adopt database %q: %w", name, err)
		}
		if err := execf(ctx, conn, `COMMENT ON DATABASE %I IS %L`, name, markerManaged); err != nil {
			return zero, fmt.Errorf("postgres: mark database %q: %w", name, err)
		}
	}
	// Tenant isolation: other roles cannot connect to this database.
	if err := execf(ctx, conn, `REVOKE ALL ON DATABASE %I FROM PUBLIC`, name); err != nil {
		return zero, fmt.Errorf("postgres: restrict database %q: %w", name, err)
	}
	if err := execf(ctx, conn, `GRANT CONNECT ON DATABASE %I TO %I`, name, name); err != nil {
		return zero, fmt.Errorf("postgres: grant connect on %q: %w", name, err)
	}

	// --- password (last, so a failure above never loses a credential) -----
	issued := provider.Issued{Outputs: provider.Outputs{
		"host": d.host, "port": strconv.Itoa(int(d.port)), "name": name, "user": name,
		"created": strconv.FormatBool(created),
	}}
	if pending || spec.Rotate {
		pw, err := newPassword()
		if err != nil {
			return zero, fmt.Errorf("postgres: generate password: %w", err)
		}
		if err := execf(ctx, conn, `ALTER ROLE %I PASSWORD %L`, name, pw); err != nil {
			return zero, fmt.Errorf("postgres: set password for %q: %w", name, err)
		}
		if err := execf(ctx, conn, `COMMENT ON ROLE %I IS %L`, name, markerManaged); err != nil {
			return zero, fmt.Errorf("postgres: mark role %q: %w", name, err)
		}
		issued.Credential = provider.Credential{
			"host": d.host, "port": strconv.Itoa(int(d.port)),
			"name": name, "user": name, "password": pw,
		}
	}
	return issued, nil
}

// DeleteDatabase drops the database and role, but only ones Plinth manages.
func (d *Driver) DeleteDatabase(ctx context.Context, spec provider.DatabaseSpec) error {
	if err := CheckName(spec.Name, d.adminUser); err != nil {
		return err
	}
	conn, err := d.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	name := spec.Name

	var dbComment *string
	switch err := conn.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1`, name).Scan(&dbComment); {
	case err == nil:
		if comment(dbComment) != markerManaged {
			return fmt.Errorf("postgres: refusing to drop %q: database not managed by Plinth", name)
		}
		if err := execf(ctx, conn, `DROP DATABASE %I WITH (FORCE)`, name); err != nil {
			return fmt.Errorf("postgres: drop database %q: %w", name, err)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("postgres: look up database %q: %w", name, err)
	}

	var roleComment *string
	switch err := conn.QueryRow(ctx, `SELECT shobj_description(oid, 'pg_authid') FROM pg_roles WHERE rolname = $1`, name).Scan(&roleComment); {
	case err == nil:
		if c := comment(roleComment); c != markerManaged && c != markerPending {
			return fmt.Errorf("postgres: refusing to drop %q: role not managed by Plinth", name)
		}
		if err := execf(ctx, conn, `DROP ROLE %I`, name); err != nil {
			return fmt.Errorf("postgres: drop role %q: %w", name, err)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("postgres: look up role %q: %w", name, err)
	}
	return nil
}

func comment(c *string) string {
	if c == nil {
		return ""
	}
	return *c
}
