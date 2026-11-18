package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// BlueprintEnv is one environment of a blueprint. Integrations are referenced
// by name.
type BlueprintEnv struct {
	Name         string `json:"name"`
	Cluster      string `json:"cluster"`
	Database     string `json:"database,omitempty"`
	AccessPrefix string `json:"access_prefix,omitempty"`
	IngressNS    string `json:"ingress_namespace,omitempty"`
}

// Blueprint is a named combination of integrations plus defaults.
type Blueprint struct {
	Name          string            `json:"name"`
	GitSource     string            `json:"git_source"`
	Registry      string            `json:"registry,omitempty"`
	GitParent     string            `json:"git_parent,omitempty"`
	TemplateRepo  string            `json:"template_repo,omitempty"`
	TemplateVars  map[string]string `json:"template_vars,omitempty"`
	ExtraBranches []string          `json:"extra_branches,omitempty"`
	Environments  []BlueprintEnv    `json:"environments"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// ErrBadReference means a blueprint names an integration that does not exist
// or has the wrong kind.
type ErrBadReference struct{ Name, Kind string }

func (e ErrBadReference) Error() string {
	return fmt.Sprintf("integration %q of kind %q does not exist", e.Name, e.Kind)
}

type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func integrationID(ctx context.Context, q queryer, name, kind string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id FROM integrations WHERE name = $1 AND kind = $2`, name, kind).Scan(&id)
	if err != nil {
		if mapErr(err) == ErrNotFound {
			return "", ErrBadReference{Name: name, Kind: kind}
		}
		return "", err
	}
	return id, nil
}

func optionalID(ctx context.Context, q queryer, name, kind string) (*string, error) {
	if name == "" {
		return nil, nil
	}
	id, err := integrationID(ctx, q, name, kind)
	return &id, err
}

// SaveBlueprint creates a blueprint or, when it exists, replaces it.
func (s *Store) SaveBlueprint(ctx context.Context, b Blueprint, create bool) (Blueprint, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return b, err
	}
	defer tx.Rollback(ctx)

	gitID, err := integrationID(ctx, tx, b.GitSource, "gitsource")
	if err != nil {
		return b, err
	}
	regID, err := optionalID(ctx, tx, b.Registry, "registry")
	if err != nil {
		return b, err
	}
	vars, _ := json.Marshal(nonNilMap(b.TemplateVars))
	branches := b.ExtraBranches
	if branches == nil {
		branches = []string{}
	}

	var bpID string
	if create {
		err = tx.QueryRow(ctx, `INSERT INTO blueprints (name, gitsource_id, registry_id, git_parent, template_repo, template_vars, extra_branches)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			b.Name, gitID, regID, b.GitParent, b.TemplateRepo, vars, branches).Scan(&bpID)
	} else {
		err = tx.QueryRow(ctx, `UPDATE blueprints SET gitsource_id = $2, registry_id = $3, git_parent = $4, template_repo = $5,
			template_vars = $6, extra_branches = $7, updated_at = now() WHERE name = $1 RETURNING id`,
			b.Name, gitID, regID, b.GitParent, b.TemplateRepo, vars, branches).Scan(&bpID)
	}
	if err != nil {
		return b, mapErr(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM blueprint_environments WHERE blueprint_id = $1`, bpID); err != nil {
		return b, err
	}
	for i, e := range b.Environments {
		clusterID, err := integrationID(ctx, tx, e.Cluster, "cluster")
		if err != nil {
			return b, err
		}
		dbID, err := optionalID(ctx, tx, e.Database, "database")
		if err != nil {
			return b, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO blueprint_environments (blueprint_id, env, position, cluster_id, database_id, access_prefix, ingress_ns)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, bpID, e.Name, i, clusterID, dbID, e.AccessPrefix, e.IngressNS); err != nil {
			return b, mapErr(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return b, err
	}
	return s.GetBlueprint(ctx, b.Name)
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

const blueprintSelect = `SELECT b.id, b.name, g.name, coalesce(r.name, ''), b.git_parent, b.template_repo,
	b.template_vars, b.extra_branches, b.updated_at
	FROM blueprints b JOIN integrations g ON g.id = b.gitsource_id LEFT JOIN integrations r ON r.id = b.registry_id`

func (s *Store) scanBlueprints(ctx context.Context, where string, args ...any) ([]Blueprint, error) {
	rows, err := s.pool.Query(ctx, blueprintSelect+where+` ORDER BY b.name`, args...)
	if err != nil {
		return nil, err
	}
	type row struct {
		id string
		b  Blueprint
	}
	var list []row
	for rows.Next() {
		var r row
		var vars []byte
		if err := rows.Scan(&r.id, &r.b.Name, &r.b.GitSource, &r.b.Registry, &r.b.GitParent, &r.b.TemplateRepo,
			&vars, &r.b.ExtraBranches, &r.b.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(vars, &r.b.TemplateVars); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Blueprint, 0, len(list))
	for _, r := range list {
		envs, err := s.pool.Query(ctx, `SELECT e.env, c.name, coalesce(d.name, ''), e.access_prefix, e.ingress_ns
			FROM blueprint_environments e JOIN integrations c ON c.id = e.cluster_id LEFT JOIN integrations d ON d.id = e.database_id
			WHERE e.blueprint_id = $1 ORDER BY e.position`, r.id)
		if err != nil {
			return nil, err
		}
		for envs.Next() {
			var e BlueprintEnv
			if err := envs.Scan(&e.Name, &e.Cluster, &e.Database, &e.AccessPrefix, &e.IngressNS); err != nil {
				envs.Close()
				return nil, err
			}
			r.b.Environments = append(r.b.Environments, e)
		}
		envs.Close()
		if err := envs.Err(); err != nil {
			return nil, err
		}
		out = append(out, r.b)
	}
	return out, nil
}

func (s *Store) GetBlueprint(ctx context.Context, name string) (Blueprint, error) {
	list, err := s.scanBlueprints(ctx, ` WHERE b.name = $1`, name)
	if err != nil {
		return Blueprint{}, err
	}
	if len(list) == 0 {
		return Blueprint{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Store) ListBlueprints(ctx context.Context) ([]Blueprint, error) {
	return s.scanBlueprints(ctx, ``)
}

// DeleteBlueprint fails with ErrInUse while requests reference it.
func (s *Store) DeleteBlueprint(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM blueprints WHERE name = $1`, name)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IntegrationSpec is a decrypted integration, for building drivers.
type IntegrationSpec struct {
	Name, Kind, Driver string
	Config             map[string]string
}

// ResolvedBlueprint is a blueprint with the decrypted configuration of every
// integration it references. Only the worker uses it.
type ResolvedBlueprint struct {
	Blueprint
	Integrations map[string]IntegrationSpec
}

func (s *Store) ResolveBlueprint(ctx context.Context, name string) (ResolvedBlueprint, error) {
	bp, err := s.GetBlueprint(ctx, name)
	if err != nil {
		return ResolvedBlueprint{}, err
	}
	rb := ResolvedBlueprint{Blueprint: bp, Integrations: map[string]IntegrationSpec{}}
	names := []string{bp.GitSource, bp.Registry}
	for _, e := range bp.Environments {
		names = append(names, e.Cluster, e.Database)
	}
	for _, n := range names {
		if n == "" {
			continue
		}
		if _, done := rb.Integrations[n]; done {
			continue
		}
		in, cfg, err := s.IntegrationConfig(ctx, n)
		if err != nil {
			return rb, err
		}
		rb.Integrations[n] = IntegrationSpec{Name: n, Kind: in.Kind, Driver: in.Driver, Config: cfg}
	}
	return rb, nil
}
