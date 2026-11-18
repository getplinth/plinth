package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getplinth/plinth/internal/naming"
	"github.com/getplinth/plinth/internal/orchestrator"
	"github.com/getplinth/plinth/internal/request"
)

// User is a person known from their identity token.
type User struct {
	ID    string `json:"id"`
	Sub   string `json:"-"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

// UpsertUser records a login and refreshes the role from the token.
func (s *Store) UpsertUser(ctx context.Context, sub, email, role string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `INSERT INTO users (oidc_sub, email, role) VALUES ($1, $2, $3)
		ON CONFLICT (oidc_sub) DO UPDATE SET email = EXCLUDED.email, role = EXCLUDED.role, last_seen = now()
		RETURNING id, oidc_sub, email, role`, sub, email, role).Scan(&u.ID, &u.Sub, &u.Email, &u.Role)
	return u, mapErr(err)
}

// Request statuses.
const (
	StatusAwaiting  = "awaiting_approval"
	StatusRejected  = "rejected"
	StatusCancelled = "cancelled"
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// RequestRecord is a stored onboarding request.
type RequestRecord struct {
	ID          string             `json:"id"`
	Blueprint   string             `json:"blueprint"`
	Tenant      string             `json:"tenant"`
	RequestedBy string             `json:"requested_by"`
	UserID      string             `json:"-"`
	Payload     request.Request    `json:"payload"`
	Status      string             `json:"status"`
	Error       string             `json:"error,omitempty"`
	State       orchestrator.State `json:"state"`
	// Plan lists every step in execution order, including ones not started
	// yet. Filled in by the API from the current blueprint; not stored.
	Plan []string `json:"plan,omitempty"`
	// Decision, for requests that needed approval: who approved, rejected or
	// cancelled, when, and the note they gave.
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	DecisionNote string     `json:"decision_note,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

const requestSelect = `SELECT r.id, b.name, t.name, u.email, u.id, r.payload, r.status, r.error, r.state,
	coalesce(d.email, ''), r.decided_at, r.decision_note, r.created_at, r.updated_at
	FROM requests r JOIN blueprints b ON b.id = r.blueprint_id JOIN tenants t ON t.id = r.tenant_id
	JOIN users u ON u.id = r.requested_by LEFT JOIN users d ON d.id = r.decided_by`

func scanRequest(row pgx.Row) (RequestRecord, error) {
	var r RequestRecord
	var payload, state []byte
	if err := row.Scan(&r.ID, &r.Blueprint, &r.Tenant, &r.RequestedBy, &r.UserID, &payload, &r.Status, &r.Error, &state, &r.DecidedBy, &r.DecidedAt, &r.DecisionNote, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return r, mapErr(err)
	}
	if err := json.Unmarshal(payload, &r.Payload); err != nil {
		return r, err
	}
	r.State = *orchestrator.NewState()
	if err := json.Unmarshal(state, &r.State); err != nil {
		return r, err
	}
	if r.State.Steps == nil {
		r.State.Steps = map[string]orchestrator.StepResult{}
	}
	if r.State.Outputs == nil {
		r.State.Outputs = map[string]string{}
	}
	return r, nil
}

// Submit describes a new request. Req must already be validated.
type Submit struct {
	Blueprint string
	UserID    string
	UserEmail string
	IsAdmin   bool
	Req       request.Request
}

// SubmitRequest stores a request after checking tenant ownership and
// reserving the derived names, atomically:
//   - the first request for a tenant makes the requester its owner;
//   - later requests need the owner or an admin (ErrForbidden);
//   - a project whose derived name belongs to another tenant is refused with
//     ErrNamespaceTaken (for example tenant "a-b"/project "c" vs "a"/"b-c").
func (s *Store) SubmitRequest(ctx context.Context, p Submit) (RequestRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RequestRecord{}, err
	}
	defer tx.Rollback(ctx)

	var bpID string
	var needsApproval bool
	if err := tx.QueryRow(ctx, `SELECT id, requires_approval FROM blueprints WHERE name = $1`, p.Blueprint).Scan(&bpID, &needsApproval); err != nil {
		return RequestRecord{}, mapErr(err)
	}
	status := StatusPending
	if needsApproval {
		status = StatusAwaiting
	}

	tenant := naming.Normalize(p.Req.Tenant)
	var tenantID string
	err = tx.QueryRow(ctx, `SELECT id FROM tenants WHERE name = $1 FOR UPDATE`, tenant).Scan(&tenantID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The first request for a team creates it and makes the requester its owner.
		if err := tx.QueryRow(ctx, `INSERT INTO tenants (name, owner_id) VALUES ($1, $2) RETURNING id`, tenant, p.UserID).Scan(&tenantID); err != nil {
			return RequestRecord{}, mapErr(err) // a concurrent first request: conflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_members (tenant_id, user_id, role, added_by) VALUES ($1, $2, 'owner', $2)`, tenantID, p.UserID); err != nil {
			return RequestRecord{}, err
		}
		_ = audit(ctx, tx, p.UserEmail, "tenant.create", tenant, nil)
	case err != nil:
		return RequestRecord{}, err
	default:
		role, err := memberRole(ctx, tx, tenantID, p.UserID)
		if err != nil {
			return RequestRecord{}, err
		}
		if role == "" && !p.IsAdmin {
			return RequestRecord{}, ErrNotMember
		}
	}

	payload, _ := json.Marshal(p.Req)
	var reqID string
	if err := tx.QueryRow(ctx, `INSERT INTO requests (blueprint_id, tenant_id, requested_by, payload, status) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		bpID, tenantID, p.UserID, payload, status).Scan(&reqID); err != nil {
		return RequestRecord{}, mapErr(err)
	}

	for _, pr := range p.Req.Projects {
		name := pr.Normalized()
		ns := naming.Namespace(tenant, name)
		spec, _ := json.Marshal(pr)
		if _, err := tx.Exec(ctx, `INSERT INTO projects (tenant_id, name, namespace, request_id, spec) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, name) DO UPDATE SET spec = EXCLUDED.spec`, tenantID, name, ns, reqID, spec); err != nil {
			if isUniqueOn(err, "namespace") {
				return RequestRecord{}, ErrNamespaceTaken{Namespace: ns}
			}
			return RequestRecord{}, mapErr(err)
		}
	}
	_ = audit(ctx, tx, p.UserEmail, "request.submit", reqID, map[string]any{"tenant": tenant, "blueprint": p.Blueprint, "projects": len(p.Req.Projects), "needs_approval": needsApproval})
	if err := tx.Commit(ctx); err != nil {
		return RequestRecord{}, err
	}
	return s.GetRequest(ctx, reqID)
}

func (s *Store) GetRequest(ctx context.Context, id string) (RequestRecord, error) {
	if !isUUID(id) {
		return RequestRecord{}, ErrNotFound
	}
	return scanRequest(s.pool.QueryRow(ctx, requestSelect+` WHERE r.id = $1`, id))
}

// ListRequests returns newest first. userID "" lists everyone's requests;
// status "" lists every status.
func (s *Store) ListRequests(ctx context.Context, userID, status string, limit int) ([]RequestRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := requestSelect + ` WHERE true`
	args := []any{limit}
	if userID != "" {
		args = append(args, userID)
		// Their own requests (even after leaving a team) and everything in teams they belong to.
		q += fmt.Sprintf(` AND (r.requested_by = $%[1]d OR r.tenant_id IN (SELECT tenant_id FROM tenant_members WHERE user_id = $%[1]d))`, len(args))
	}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(` AND r.status = $%d`, len(args))
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY r.created_at DESC LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RequestRecord
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RetryRequest puts a failed request back in the queue; finished steps are
// kept and not repeated.
func (s *Store) RetryRequest(ctx context.Context, id, actor string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE requests SET status = 'pending', error = '', updated_at = now() WHERE id = $1 AND status = 'failed'`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetRequest(ctx, id); err != nil {
			return err
		}
		return ErrConflict
	}
	return s.Audit(ctx, actor, "request.retry", id, nil)
}

// ClaimNext leases the oldest runnable request: pending, or running with an
// expired lease (its worker died). It returns ErrNotFound when idle.
func (s *Store) ClaimNext(ctx context.Context, worker string, lease time.Duration) (RequestRecord, error) {
	var id string
	err := s.pool.QueryRow(ctx, `UPDATE requests SET status = 'running', claimed_by = $1, claimed_at = now(), updated_at = now()
		WHERE id = (SELECT id FROM requests
		            WHERE status = 'pending' OR (status = 'running' AND claimed_at < now() - $2::interval)
		            ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id`, worker, fmt.Sprintf("%d seconds", int(lease.Seconds()))).Scan(&id)
	if err != nil {
		return RequestRecord{}, mapErr(err)
	}
	return s.GetRequest(ctx, id)
}

// Save implements orchestrator.Saver. It also refreshes the lease, and fails
// when the request is no longer running (the lease was lost to another
// worker), which stops this worker from continuing.
func (s *Store) Save(ctx context.Context, requestID string, st *orchestrator.State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE requests SET state = $2, claimed_at = now(), updated_at = now() WHERE id = $1 AND status = 'running'`, requestID, b)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Release puts a request this worker is running back in the queue (used on
// shutdown, so another worker continues from the saved state immediately
// instead of waiting for the lease to expire).
func (s *Store) Release(ctx context.Context, id, worker string) error {
	_, err := s.pool.Exec(ctx, `UPDATE requests SET status = 'pending', claimed_by = NULL, updated_at = now()
		WHERE id = $1 AND status = 'running' AND claimed_by = $2`, id, worker)
	return err
}

// Finish records the outcome of a run.
func (s *Store) Finish(ctx context.Context, id string, runErr error) error {
	status, msg := StatusSucceeded, ""
	if runErr != nil {
		status, msg = StatusFailed, runErr.Error()
	}
	if _, err := s.pool.Exec(ctx, `UPDATE requests SET status = $2, error = $3, claimed_by = NULL, updated_at = now() WHERE id = $1`, id, status, msg); err != nil {
		return err
	}
	return s.Audit(ctx, "system", "request."+status, id, nil)
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
			return false
		}
	}
	return true
}

// Decision actions.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
	DecisionCancel  = "cancel"
)

// Decision is an approve, reject or cancel of a request awaiting approval.
type Decision struct {
	Action     string
	ActorID    string
	ActorEmail string
	ActorAdmin bool
	Note       string
}

// DecideRequest records a decision on a request that is awaiting approval,
// atomically (a request can be decided once):
//   - approve: administrators only, and never the requester (ErrSelfApproval);
//     the request goes to the queue;
//   - reject: administrators only; the request ends as rejected;
//   - cancel: the requester or an administrator; the request ends as cancelled.
//
// Rejecting or cancelling releases the names the request reserved, so another
// tenant can use them. The decision and its note are written to the audit log.
func (s *Store) DecideRequest(ctx context.Context, id string, d Decision) (RequestRecord, error) {
	if !isUUID(id) {
		return RequestRecord{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RequestRecord{}, err
	}
	defer tx.Rollback(ctx)

	var status, requester, tenantID string
	if err := tx.QueryRow(ctx, `SELECT status, requested_by::text, tenant_id::text FROM requests WHERE id = $1 FOR UPDATE`, id).Scan(&status, &requester, &tenantID); err != nil {
		return RequestRecord{}, mapErr(err)
	}
	if status != StatusAwaiting {
		return RequestRecord{}, ErrConflict
	}

	var next string
	switch d.Action {
	case DecisionApprove:
		if !d.ActorAdmin {
			return RequestRecord{}, ErrForbidden
		}
		if requester == d.ActorID {
			return RequestRecord{}, ErrSelfApproval
		}
		next = StatusPending
	case DecisionReject:
		if !d.ActorAdmin {
			return RequestRecord{}, ErrForbidden
		}
		next = StatusRejected
	case DecisionCancel:
		// The requester, an owner of the team, or an administrator.
		if !d.ActorAdmin && requester != d.ActorID {
			role, err := memberRole(ctx, tx, tenantID, d.ActorID)
			if err != nil {
				return RequestRecord{}, err
			}
			if role != RoleOwner {
				return RequestRecord{}, ErrForbidden
			}
		}
		next = StatusCancelled
	default:
		return RequestRecord{}, fmt.Errorf("store: unknown decision %q", d.Action)
	}

	if _, err := tx.Exec(ctx, `UPDATE requests SET status = $2, decided_by = $3, decided_at = now(), decision_note = $4, updated_at = now() WHERE id = $1`,
		id, next, d.ActorID, d.Note); err != nil {
		return RequestRecord{}, err
	}
	if next == StatusRejected || next == StatusCancelled {
		// Free the reserved names. Only rows this request created are removed;
		// a project that existed before it stays.
		if _, err := tx.Exec(ctx, `DELETE FROM projects WHERE request_id = $1`, id); err != nil {
			return RequestRecord{}, err
		}
	}
	if err := audit(ctx, tx, d.ActorEmail, "request."+d.Action, id, map[string]any{"note": d.Note}); err != nil {
		return RequestRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RequestRecord{}, err
	}
	return s.GetRequest(ctx, id)
}
