package store

import (
	"bytes"
	"context"
	"fmt"

	"github.com/getplinth/plinth/internal/crypto"
)

// KeyUsage counts stored encrypted records by the master key they are
// encrypted with (its fingerprint), so an operator can tell whether the old
// key is still needed. Records from before keys carried an id are counted as
// "legacy". It reads headers only and decrypts nothing.
func (s *Store) KeyUsage(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT config_enc FROM integrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var enc []byte
		if err := rows.Scan(&enc); err != nil {
			return nil, err
		}
		id, ok := crypto.KeyOf(enc)
		switch {
		case !ok:
			out["unrecognised"]++
		case id == "":
			out["legacy"]++
		default:
			out[id]++
		}
	}
	return out, rows.Err()
}

// PrimaryKeyID is the fingerprint of the key that encrypts new records.
func (s *Store) PrimaryKeyID() string { return s.box.PrimaryID() }

// RekeyReport is the outcome of Rekey.
type RekeyReport struct {
	Total     int      // encrypted records seen
	Current   int      // already on the primary key
	Rewrapped int      // moved to the primary key (or, in a dry run, that would be)
	Failed    []string // records that could not be moved, with the reason
}

// Rekey moves every encrypted record to the primary master key. Only each
// record's data key is re-wrapped, the encrypted value is untouched, and a
// record is replaced only if it is unchanged since it was read, so it is safe
// to run while Plinth is serving requests and safe to repeat. A record that
// cannot be read (for example its old key was not supplied) is reported and
// left as it is; the others are still moved. With dryRun nothing is written.
func (s *Store) Rekey(ctx context.Context, dryRun bool) (RekeyReport, error) {
	var rep RekeyReport
	rows, err := s.pool.Query(ctx, `SELECT id::text, name FROM integrations ORDER BY id`)
	if err != nil {
		return rep, err
	}
	type rec struct{ id, name string }
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.name); err != nil {
			rows.Close()
			return rep, err
		}
		recs = append(recs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	for _, r := range recs {
		rep.Total++
		var done bool
		for attempt := 0; attempt < 3 && !done; attempt++ {
			var enc []byte
			if err := s.pool.QueryRow(ctx, `SELECT config_enc FROM integrations WHERE id = $1`, r.id).Scan(&enc); err != nil {
				rep.Failed = append(rep.Failed, fmt.Sprintf("%s: %v", r.name, err))
				break
			}
			out, changed, err := s.box.Rewrap(enc, []byte(r.id))
			if err != nil {
				rep.Failed = append(rep.Failed, fmt.Sprintf("%s: %v", r.name, err))
				break
			}
			if !changed {
				rep.Current++
				done = true
				break
			}
			if dryRun {
				rep.Rewrapped++
				done = true
				break
			}
			// Replace only if nobody changed the record meanwhile; otherwise read it again.
			tag, err := s.pool.Exec(ctx, `UPDATE integrations SET config_enc = $2 WHERE id = $1 AND config_enc = $3`, r.id, out, enc)
			if err != nil {
				return rep, err
			}
			if tag.RowsAffected() == 1 {
				rep.Rewrapped++
				done = true
			}
		}
		if !done && !hasFailure(rep.Failed, r.name) {
			rep.Failed = append(rep.Failed, fmt.Sprintf("%s: changed too often while it was being moved; run again", r.name))
		}
	}
	return rep, nil
}

func hasFailure(failed []string, name string) bool {
	for _, f := range failed {
		if bytes.HasPrefix([]byte(f), []byte(name+": ")) {
			return true
		}
	}
	return false
}
