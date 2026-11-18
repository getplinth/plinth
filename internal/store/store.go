// Package store persists Plinth's state in PostgreSQL.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getplinth/plinth/internal/crypto"
	"github.com/getplinth/plinth/internal/quota"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Errors the API maps to HTTP statuses.
var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("already exists or state conflict")
	ErrInUse     = errors.New("still referenced")
	ErrForbidden = errors.New("not allowed for this user")
	// ErrLeaseLost means another worker took over the request; the caller
	// must stop working on it and must not record an outcome.
	ErrLeaseLost = errors.New("request lease lost")
)

// ErrNamespaceTaken means another tenant already owns the derived name.
type ErrNamespaceTaken struct{ Namespace string }

func (e ErrNamespaceTaken) Error() string {
	return fmt.Sprintf("the name %q is already used by a project of another tenant", e.Namespace)
}

// Store is the database handle.
type Store struct {
	pool *pgxpool.Pool
	box  *crypto.Box
}

// Open connects and verifies the connection. Call Migrate before use.
func Open(ctx context.Context, url string, box *crypto.Box) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: cannot reach the database: %w", err)
	}
	return &Store{pool: pool, box: box}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies pending migrations in order, each in its own transaction,
// holding an advisory lock so concurrent starts do not race.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	const lockID = 7_251_042_001
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return s.EnsureDefaultTiers(ctx)
}

// ---- quota tiers ----------------------------------------------------------------

// EnsureDefaultTiers seeds the built-in tiers when none exist.
func (s *Store) EnsureDefaultTiers(ctx context.Context) error {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM quota_tiers`).Scan(&n); err != nil || n > 0 {
		return err
	}
	for name, t := range quota.Default() {
		b, _ := json.Marshal(t)
		if _, err := s.pool.Exec(ctx, `INSERT INTO quota_tiers (name, spec) VALUES ($1, $2) ON CONFLICT DO NOTHING`, name, b); err != nil {
			return err
		}
	}
	return nil
}

// Tiers returns all quota tiers.
func (s *Store) Tiers(ctx context.Context) (quota.Tiers, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, spec FROM quota_tiers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := quota.Tiers{}
	for rows.Next() {
		var name string
		var raw []byte
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var t quota.Tier
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, rows.Err()
}

// ---- audit ----------------------------------------------------------------------

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type AuditEntry struct {
	ID     int64           `json:"id"`
	At     time.Time       `json:"at"`
	Actor  string          `json:"actor"`
	Action string          `json:"action"`
	Target string          `json:"target"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

func audit(ctx context.Context, e execer, actor, action, target string, detail any) error {
	var raw []byte
	if detail != nil {
		raw, _ = json.Marshal(detail)
	}
	_, err := e.Exec(ctx, `INSERT INTO audit_log (actor, action, target, detail) VALUES ($1, $2, $3, $4)`, actor, action, target, raw)
	return err
}

// Audit records who did what. Detail must never contain secrets.
func (s *Store) Audit(ctx context.Context, actor, action, target string, detail any) error {
	return audit(ctx, s.pool, actor, action, target, detail)
}

// ListAudit returns the newest entries first.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, at, actor, action, target, coalesce(detail, 'null'::jsonb) FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- errors ---------------------------------------------------------------------

func pgCode(err error) (code, constraint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

// mapErr turns common database errors into this package's errors.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	}
	switch code, _ := pgCode(err); code {
	case "23505":
		return ErrConflict
	case "23503":
		return ErrInUse
	}
	return err
}

func isUniqueOn(err error, constraintSubstr string) bool {
	code, c := pgCode(err)
	return code == "23505" && strings.Contains(c, constraintSubstr)
}
