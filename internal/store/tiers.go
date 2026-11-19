package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/getplinth/plinth/internal/quota"
)

// ErrDefaultTier: the tier that projects get when they name none cannot be removed.
var ErrDefaultTier = errors.New("the default tier cannot be deleted")

// ErrTierInUse: requests that are not finished still name the tier.
type ErrTierInUse struct{ Requests int }

func (e ErrTierInUse) Error() string {
	return fmt.Sprintf("%d unfinished request(s) still use this tier: wait for them to finish or be withdrawn", e.Requests)
}

// PutTier creates or replaces a tier and reports whether it is new. A tier
// applies to requests processed from now on; namespaces that exist keep what
// they were given until a request for them runs again.
func (s *Store) PutTier(ctx context.Context, name string, t quota.Tier) (created bool, err error) {
	b, err := json.Marshal(t)
	if err != nil {
		return false, err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO quota_tiers (name, spec) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET spec = EXCLUDED.spec
		RETURNING (xmax = 0)`, name, b).Scan(&created)
	return created, mapErr(err)
}

// DeleteTier removes a tier unless it is the default or unfinished requests use it.
func (s *Store) DeleteTier(ctx context.Context, name string) error {
	if name == quota.DefaultTier {
		return ErrDefaultTier
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialise with other tier changes; requests are checked against what is committed.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM quota_tiers WHERE name = $1 FOR UPDATE`, name).Scan(&exists); err != nil {
		return mapErr(err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM requests
		WHERE status IN ('pending','running','failed','awaiting_approval')
		  AND payload->'projects' @> jsonb_build_array(jsonb_build_object('quota', $1::text))`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrTierInUse{Requests: n}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM quota_tiers WHERE name = $1`, name); err != nil {
		return mapErr(err)
	}
	return tx.Commit(ctx)
}
