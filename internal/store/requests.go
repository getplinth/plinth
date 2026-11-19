package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	// AuthSource is "oidc" for people who sign in through an identity provider
	// and "local" for password accounts.
	AuthSource string `json:"auth_source"`
	// MustChange: a password someone else chose has to be replaced before anything else.
	MustChange bool `json:"must_change_password,omitempty"`
}

// UpsertUser records a login and refreshes the role and the e-mail address
// (with whether the provider verified it) from the token. When the address can
// be trusted, any team invitations waiting for it are claimed.
func (s *Store) UpsertUser(ctx context.Context, sub, email string, emailVerified bool, role string) (User, error) {
	var u User
	// An identity provider chooses the subject it sends. Password accounts are
	// "local:<id>", so a provider must never be able to produce one.
	if strings.HasPrefix(sub, "local:") {
		return u, ErrForbidden
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO users (oidc_sub, email, email_verified, role) VALUES ($1, $2, $3, $4)
		ON CONFLICT (oidc_sub) DO UPDATE SET email = EXCLUDED.email, email_verified = EXCLUDED.email_verified,
		  role = EXCLUDED.role, last_seen = now() WHERE users.auth_source = 'oidc'
		RETURNING id, oidc_sub, email, role`, sub, email, emailVerified, role).Scan(&u.ID, &u.Sub, &u.Email, &u.Role)
	u.AuthSource = "oidc"
	if err != nil {
		return u, mapErr(err)
	}
	if (emailVerified || s.trustUnverifiedEmail.Load()) && strings.Contains(email, "@") {
		if err := s.claimInvitations(ctx, u); err != nil {
			return u, err
		}
	}
	return u, nil
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
	// StatusAbandoned: a failed request an administrator gave up on. Its names
	// were released, but what it had already created was left in place.
	StatusAbandoned = "abandoned"
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
	if needsApproval {
		if err := s.notifyRequest(ctx, tx, EventAwaitingApproval, reqID); err != nil {
			return RequestRecord{}, err
		}
	}
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var before string
	if err := tx.QueryRow(ctx, `SELECT status FROM requests WHERE id = $1 FOR UPDATE`, id).Scan(&before); err != nil {
		return mapErr(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE requests SET status = $2, error = $3, claimed_by = NULL, updated_at = now() WHERE id = $1`, id, status, msg); err != nil {
		return err
	}
	if err := audit(ctx, tx, "system", "request."+status, id, nil); err != nil {
		return err
	}
	if before != status { // a repeated Finish must not notify twice
		event := EventSucceeded
		if status == StatusFailed {
			event = EventFailed
		}
		if err := s.notifyRequest(ctx, tx, event, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
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
	// DecisionRelease gives up a failed request and frees its names.
	DecisionRelease = "release"
)

// Decision is an approve, reject or cancel of a request awaiting approval, or
// the release of a failed one.
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
//
// Release is the one decision on a failed request: administrators only, and
// it needs a note. It ends the request as abandoned and frees its names, but
// removes nothing the request had already created in other systems, and the
// audit entry lists the steps that had run so someone can clean up. Until that
// is done a team that reuses the names would adopt the leftovers.
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
	want := StatusAwaiting
	if d.Action == DecisionRelease {
		want = StatusFailed
	}
	if status != want {
		return RequestRecord{}, ErrConflict
	}

	var next string
	var detail = map[string]any{"note": d.Note}
	switch d.Action {
	case DecisionRelease:
		if !d.ActorAdmin {
			return RequestRecord{}, ErrForbidden
		}
		next = StatusAbandoned
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT state FROM requests WHERE id = $1`, id).Scan(&raw); err != nil {
			return RequestRecord{}, err
		}
		var st struct {
			Steps map[string]struct {
				Status string `json:"status"`
			} `json:"steps"`
		}
		_ = json.Unmarshal(raw, &st)
		var ran []string
		for name, s := range st.Steps {
			if s.Status == "succeeded" || s.Status == "failed" || s.Status == "running" {
				ran = append(ran, name)
			}
		}
		sort.Strings(ran)
		detail["left_in_place"] = ran
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
	if next == StatusRejected || next == StatusCancelled || next == StatusAbandoned {
		if err := freeNames(ctx, tx, id, tenantID); err != nil {
			return RequestRecord{}, err
		}
	}
	switch next {
	case StatusPending:
		err = s.notifyRequest(ctx, tx, EventApproved, id)
	case StatusRejected:
		err = s.notifyRequest(ctx, tx, EventRejected, id)
	case StatusAbandoned:
		err = s.notifyRequest(ctx, tx, EventAbandoned, id)
	}
	if err != nil {
		return RequestRecord{}, err
	}
	if err := audit(ctx, tx, d.ActorEmail, "request."+d.Action, id, detail); err != nil {
		return RequestRecord{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RequestRecord{}, err
	}
	return s.GetRequest(ctx, id)
}

// freeNames deletes the project names a request reserved. Only rows this
// request created go; a project that existed before it stays. A name is kept
// while another live request of the same team asks for it (a team that submits
// again after a failure reuses the same name, and that new request still needs
// the row): live means anything but rejected, cancelled or abandoned. When the
// last such request is given up, the row it kept alive goes too.
func freeNames(ctx context.Context, tx pgx.Tx, requestID, tenantID string) error {
	rows, err := tx.Query(ctx, `SELECT payload FROM requests
		WHERE tenant_id = $1 AND id <> $2 AND status NOT IN ('rejected','cancelled','abandoned')`, tenantID, requestID)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var req request.Request
		if json.Unmarshal(raw, &req) == nil {
			for _, p := range req.Projects {
				wanted[p.Normalized()] = true
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	names := []string{}
	for n := range wanted {
		names = append(names, n)
	}
	// Rows owned by this request, and rows owned by any earlier request that was
	// also given up (a newer request kept them alive until now).
	_, err = tx.Exec(ctx, `DELETE FROM projects p USING requests r
		WHERE p.request_id = r.id AND p.tenant_id = $1 AND NOT (p.name = ANY($2))
		  AND (r.id = $3 OR r.status IN ('rejected','cancelled','abandoned'))`, tenantID, names, requestID)
	return err
}
