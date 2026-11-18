package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// Integration describes a configured external system. The configuration
// values are never part of this type: only key names are exposed.
type Integration struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Driver     string    `json:"driver"`
	Name       string    `json:"name"`
	ConfigKeys []string  `json:"config_keys"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

// CreateIntegration stores a new integration with its configuration
// encrypted. The ciphertext is bound to the row id.
func (s *Store) CreateIntegration(ctx context.Context, kind, driver, name string, cfg map[string]string) (Integration, error) {
	var in Integration
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return in, err
	}
	defer tx.Rollback(ctx)

	var id string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		return in, err
	}
	enc, err := s.sealConfig(id, cfg)
	if err != nil {
		return in, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO integrations (id, kind, driver, name, config_enc, config_keys)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, kind, driver, name, config_keys, created_at, updated_at`,
		id, kind, driver, name, enc, sortedKeys(cfg)).
		Scan(&in.ID, &in.Kind, &in.Driver, &in.Name, &in.ConfigKeys, &in.CreatedAt, &in.UpdatedAt)
	if err != nil {
		return in, mapErr(err)
	}
	return in, tx.Commit(ctx)
}

func (s *Store) sealConfig(id string, cfg map[string]string) ([]byte, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return s.box.Seal(raw, []byte(id))
}

func (s *Store) openConfig(id string, enc []byte) (map[string]string, error) {
	raw, err := s.box.Open(enc, []byte(id))
	if err != nil {
		return nil, fmt.Errorf("integration %s: %w", id, err)
	}
	var cfg map[string]string
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

const integrationCols = `id, kind, driver, name, config_keys, created_at, updated_at`

func scanIntegration(row pgx.Row) (Integration, error) {
	var in Integration
	err := row.Scan(&in.ID, &in.Kind, &in.Driver, &in.Name, &in.ConfigKeys, &in.CreatedAt, &in.UpdatedAt)
	return in, mapErr(err)
}

func (s *Store) ListIntegrations(ctx context.Context) ([]Integration, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+integrationCols+` FROM integrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Integration
	for rows.Next() {
		in, err := scanIntegration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) GetIntegration(ctx context.Context, name string) (Integration, error) {
	return scanIntegration(s.pool.QueryRow(ctx, `SELECT `+integrationCols+` FROM integrations WHERE name = $1`, name))
}

// IntegrationConfig returns the decrypted configuration. Only the worker and
// the validate endpoint call this; it is never returned over the API.
func (s *Store) IntegrationConfig(ctx context.Context, name string) (Integration, map[string]string, error) {
	var in Integration
	var enc []byte
	err := s.pool.QueryRow(ctx, `SELECT `+integrationCols+`, config_enc FROM integrations WHERE name = $1`, name).
		Scan(&in.ID, &in.Kind, &in.Driver, &in.Name, &in.ConfigKeys, &in.CreatedAt, &in.UpdatedAt, &enc)
	if err != nil {
		return in, nil, mapErr(err)
	}
	cfg, err := s.openConfig(in.ID, enc)
	return in, cfg, err
}

// ReplaceIntegrationConfig overwrites the whole configuration.
func (s *Store) ReplaceIntegrationConfig(ctx context.Context, name string, cfg map[string]string) (Integration, error) {
	cur, err := s.GetIntegration(ctx, name)
	if err != nil {
		return cur, err
	}
	enc, err := s.sealConfig(cur.ID, cfg)
	if err != nil {
		return cur, err
	}
	return scanIntegration(s.pool.QueryRow(ctx, `UPDATE integrations SET config_enc = $2, config_keys = $3, updated_at = now()
		WHERE id = $1 RETURNING `+integrationCols, cur.ID, enc, sortedKeys(cfg)))
}

// DeleteIntegration fails with ErrInUse while a blueprint references it.
func (s *Store) DeleteIntegration(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM integrations WHERE name = $1`, name)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
