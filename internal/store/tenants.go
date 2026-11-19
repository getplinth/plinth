package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Team roles.
const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// Errors for team management. The API turns them into plain sentences.
var (
	// ErrNotMember: the person is not in the team they are acting on.
	ErrNotMember = errors.New("you are not a member of this team")
	// ErrNotOwner: members cannot manage the team.
	ErrNotOwner = errors.New("only team owners can do this")
	// ErrLastOwner: a team must always keep an owner.
	ErrLastOwner = errors.New("a team needs at least one owner")
	// ErrAmbiguousEmail: more than one account has that address, so it cannot
	// identify one person.
	ErrAmbiguousEmail = errors.New("more than one account has that e-mail address")
	// ErrTooManyInvitations: a team may have only so many open invitations.
	ErrTooManyInvitations = errors.New("this team has too many open invitations")
)

const (
	// InvitationLifetime is how long an invitation can be claimed.
	InvitationLifetime = 30 * 24 * time.Hour
	// MaxOpenInvitations caps the open invitations of one team.
	MaxOpenInvitations = 50
)

// Actor is who performs a team action.
type Actor struct {
	ID    string
	Email string
	Admin bool
}

// Member is one person in a team.
type Member struct {
	UserID  string    `json:"user_id"`
	Email   string    `json:"email"`
	Role    string    `json:"role"`
	AddedBy string    `json:"added_by,omitempty"`
	AddedAt time.Time `json:"added_at"`
}

// Invitation lets someone who has not signed in yet join a team when they do.
type Invitation struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InvitedBy string    `json:"invited_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AddResult is what adding a person by e-mail produced: either a member
// (they already have an account) or an invitation (they do not yet).
type AddResult struct {
	Member     *Member
	Invitation *Invitation
}

// Tenant is a team with its members and projects.
type Tenant struct {
	Name        string   `json:"name"`
	Role        string   `json:"role"` // the viewer's role; empty for an administrator who is not a member
	MemberCount int      `json:"member_count"`
	Members     []Member `json:"members,omitempty"`
	// Invitations are visible to owners and administrators only.
	Invitations []Invitation `json:"invitations,omitempty"`
	Projects    []string     `json:"projects"`
	CreatedAt   time.Time    `json:"created_at"`
}

func validRole(r string) bool { return r == RoleOwner || r == RoleMember }

// memberRole returns the user's role in a tenant, or "" when not a member.
func memberRole(ctx context.Context, q queryer, tenantID, userID string) (string, error) {
	var role string
	err := q.QueryRow(ctx, `SELECT role FROM tenant_members WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func ownerCount(ctx context.Context, q queryer, tenantID string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM tenant_members WHERE tenant_id = $1 AND role = 'owner'`, tenantID).Scan(&n)
	return n, err
}

// TenantRole returns the user's role in the named tenant ("" for none).
// ErrNotFound when the tenant does not exist.
func (s *Store) TenantRole(ctx context.Context, tenant, userID string) (string, error) {
	var role string
	err := s.pool.QueryRow(ctx, `SELECT coalesce(m.role, '') FROM tenants t
		LEFT JOIN tenant_members m ON m.tenant_id = t.id AND m.user_id = $2 WHERE t.name = $1`, tenant, userID).Scan(&role)
	return role, mapErr(err)
}

func (s *Store) loadProjects(ctx context.Context, byTenant map[string]*Tenant) error {
	rows, err := s.pool.Query(ctx, `SELECT t.name, p.name FROM projects p JOIN tenants t ON t.id = p.tenant_id ORDER BY p.name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t, p string
		if err := rows.Scan(&t, &p); err != nil {
			return err
		}
		if tn := byTenant[t]; tn != nil {
			tn.Projects = append(tn.Projects, p)
		}
	}
	return rows.Err()
}

// ListTenants lists the teams the user belongs to, or every team when all is
// true (administrators).
func (s *Store) ListTenants(ctx context.Context, userID string, all bool) ([]Tenant, error) {
	q := `SELECT t.name, coalesce(m.role, ''), t.created_at, (SELECT count(*) FROM tenant_members x WHERE x.tenant_id = t.id)
		FROM tenants t LEFT JOIN tenant_members m ON m.tenant_id = t.id AND m.user_id = $1`
	if !all {
		q += ` WHERE m.user_id IS NOT NULL`
	}
	rows, err := s.pool.Query(ctx, q+` ORDER BY t.name`, userID)
	if err != nil {
		return nil, err
	}
	var out []Tenant
	by := map[string]*Tenant{}
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.Name, &t.Role, &t.CreatedAt, &t.MemberCount); err != nil {
			rows.Close()
			return nil, err
		}
		t.Projects = []string{}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		by[out[i].Name] = &out[i]
	}
	return out, s.loadProjects(ctx, by)
}

// GetTenant returns a team with its members, and its open invitations when
// withInvitations is set (owners and administrators). The caller decides who
// may see the team at all.
func (s *Store) GetTenant(ctx context.Context, name, viewerID string, withInvitations bool) (Tenant, error) {
	var t Tenant
	var id string
	err := s.pool.QueryRow(ctx, `SELECT t.id, t.name, coalesce(m.role, ''), t.created_at FROM tenants t
		LEFT JOIN tenant_members m ON m.tenant_id = t.id AND m.user_id = $2 WHERE t.name = $1`, name, viewerID).
		Scan(&id, &t.Name, &t.Role, &t.CreatedAt)
	if err != nil {
		return t, mapErr(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT m.user_id, u.email, m.role, coalesce(a.email, ''), m.added_at
		FROM tenant_members m JOIN users u ON u.id = m.user_id LEFT JOIN users a ON a.id = m.added_by
		WHERE m.tenant_id = $1 ORDER BY (m.role = 'owner') DESC, u.email`, id)
	if err != nil {
		return t, err
	}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role, &m.AddedBy, &m.AddedAt); err != nil {
			rows.Close()
			return t, err
		}
		t.Members = append(t.Members, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return t, err
	}
	t.MemberCount = len(t.Members)
	t.Projects = []string{}
	if withInvitations {
		inv, err := s.pool.Query(ctx, `SELECT i.id, i.email, i.role, coalesce(u.email, ''), i.created_at, i.expires_at
			FROM tenant_invitations i LEFT JOIN users u ON u.id = i.invited_by
			WHERE i.tenant_id = $1 AND i.expires_at > now() ORDER BY i.created_at`, id)
		if err != nil {
			return t, err
		}
		for inv.Next() {
			var v Invitation
			if err := inv.Scan(&v.ID, &v.Email, &v.Role, &v.InvitedBy, &v.CreatedAt, &v.ExpiresAt); err != nil {
				inv.Close()
				return t, err
			}
			t.Invitations = append(t.Invitations, v)
		}
		inv.Close()
		if err := inv.Err(); err != nil {
			return t, err
		}
	}
	return t, s.loadProjects(ctx, map[string]*Tenant{t.Name: &t})
}

// teamTx locks the tenant and checks that the actor may manage it. Someone
// outside the team gets ErrNotFound (its existence is not revealed), a plain
// member gets ErrNotOwner. selfOK lets a member act on their own membership.
func (s *Store) teamTx(ctx context.Context, tenant string, a Actor, selfTarget string) (pgx.Tx, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE name = $1 FOR UPDATE`, tenant).Scan(&id); err != nil {
		_ = tx.Rollback(ctx)
		return nil, "", mapErr(err)
	}
	role, err := memberRole(ctx, tx, id, a.ID)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, "", err
	}
	switch {
	case a.Admin, role == RoleOwner:
	case role == RoleMember && selfTarget != "" && selfTarget == a.ID:
	case role == RoleMember:
		_ = tx.Rollback(ctx)
		return nil, "", ErrNotOwner
	default:
		_ = tx.Rollback(ctx)
		return nil, "", ErrNotFound
	}
	return tx, id, nil
}

// AddMember adds a person to a team by e-mail address (compared without
// regard to case). Owners and administrators only.
//
//   - If exactly one account has that address, and the identity provider
//     verified it, that person becomes a member now (ErrConflict if they
//     already are one).
//   - If nobody does, an invitation is recorded instead (renewed if one is
//     already open). It is claimed automatically when someone signs in with
//     that address, verified. Plinth sends no e-mail: the owner tells them.
//   - Accounts whose address the provider did not verify do not count, so
//     nobody can get matched to another person's address by registering it
//     unverified. (Unless SetTrustUnverifiedEmail is on.)
//   - More than one matching account is ErrAmbiguousEmail.
func (s *Store) AddMember(ctx context.Context, tenant string, a Actor, email, role string) (AddResult, error) {
	var res AddResult
	if !validRole(role) {
		return res, fmt.Errorf("store: unknown role %q", role)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	tx, id, err := s.teamTx(ctx, tenant, a, "")
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `SELECT id, email FROM users WHERE lower(email) = $1 AND (email_verified OR $2)`,
		email, s.trustUnverifiedEmail.Load())
	if err != nil {
		return res, err
	}
	var found []Member
	for rows.Next() {
		var u Member
		if err := rows.Scan(&u.UserID, &u.Email); err != nil {
			rows.Close()
			return res, err
		}
		found = append(found, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	var addedBy *string
	if a.ID != "" {
		addedBy = &a.ID
	}

	switch {
	case len(found) > 1:
		return res, ErrAmbiguousEmail

	case len(found) == 1:
		m := found[0]
		m.Role = role
		if err := tx.QueryRow(ctx, `INSERT INTO tenant_members (tenant_id, user_id, role, added_by) VALUES ($1, $2, $3, $4) RETURNING added_at`,
			id, m.UserID, role, addedBy).Scan(&m.AddedAt); err != nil {
			return res, mapErr(err) // already a member: ErrConflict
		}
		// Their open invitation to this team, if any, is now pointless.
		if _, err := tx.Exec(ctx, `DELETE FROM tenant_invitations WHERE tenant_id = $1 AND email = $2`, id, email); err != nil {
			return res, err
		}
		m.AddedBy = a.Email
		if err := audit(ctx, tx, a.Email, "tenant.member.add", tenant, map[string]any{"member": m.Email, "role": role}); err != nil {
			return res, err
		}
		res.Member = &m

	default:
		// Expired invitations do not count towards the cap.
		if _, err := tx.Exec(ctx, `DELETE FROM tenant_invitations WHERE tenant_id = $1 AND expires_at <= now()`, id); err != nil {
			return res, err
		}
		var open int
		var already bool
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(bool_or(email = $2), false) FROM tenant_invitations WHERE tenant_id = $1`, id, email).Scan(&open, &already); err != nil {
			return res, err
		}
		if open >= MaxOpenInvitations && !already {
			return res, ErrTooManyInvitations
		}
		inv := Invitation{Email: email, Role: role, InvitedBy: a.Email}
		if err := tx.QueryRow(ctx, `INSERT INTO tenant_invitations (tenant_id, email, role, invited_by, expires_at)
			VALUES ($1, $2, $3, $4, now() + $5::interval)
			ON CONFLICT (tenant_id, email) DO UPDATE SET role = EXCLUDED.role, invited_by = EXCLUDED.invited_by,
			  created_at = now(), expires_at = EXCLUDED.expires_at
			RETURNING id, created_at, expires_at`, id, email, role, addedBy, fmt.Sprintf("%d seconds", int(InvitationLifetime.Seconds()))).
			Scan(&inv.ID, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
			return res, mapErr(err)
		}
		if err := s.enqueue(ctx, tx, EventInvitation, map[string]any{
			"tenant": tenant, "email": email, "role": role, "invited_by": a.Email, "expires_at": inv.ExpiresAt,
		}, email); err != nil {
			return res, err
		}
		if err := audit(ctx, tx, a.Email, "tenant.invitation.create", tenant, map[string]any{"email": email, "role": role}); err != nil {
			return res, err
		}
		res.Invitation = &inv
	}
	return res, tx.Commit(ctx)
}

// RevokeInvitation withdraws an open invitation. Owners and administrators only.
func (s *Store) RevokeInvitation(ctx context.Context, tenant string, a Actor, invitationID string) error {
	if !isUUID(invitationID) {
		return ErrNotFound
	}
	tx, id, err := s.teamTx(ctx, tenant, a, "")
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var email string
	if err := tx.QueryRow(ctx, `DELETE FROM tenant_invitations WHERE id = $1 AND tenant_id = $2 RETURNING email`, invitationID, id).Scan(&email); err != nil {
		return mapErr(err)
	}
	if err := audit(ctx, tx, a.Email, "tenant.invitation.revoke", tenant, map[string]any{"email": email}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// claimInvitations turns the open invitations for a signed-in person's address
// into memberships. It is atomic: DELETE ... RETURNING takes each invitation
// once even if two requests of the same person arrive together. An existing
// membership is never lowered; an invitation as owner promotes a member.
func (s *Store) claimInvitations(ctx context.Context, u User) error {
	var pending bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_invitations WHERE email = lower($1))`, u.Email).Scan(&pending); err != nil || !pending {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `DELETE FROM tenant_invitations i USING tenants t WHERE t.id = i.tenant_id AND i.email = lower($1)
		RETURNING i.tenant_id::text, t.name, i.role, i.invited_by::text, i.expires_at > now()`, u.Email)
	if err != nil {
		return err
	}
	type claim struct {
		tenantID, name, role string
		invitedBy            *string
		live                 bool
	}
	var claims []claim
	for rows.Next() {
		var c claim
		if err := rows.Scan(&c.tenantID, &c.name, &c.role, &c.invitedBy, &c.live); err != nil {
			rows.Close()
			return err
		}
		claims = append(claims, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range claims {
		if !c.live { // expired: removed, but it grants nothing
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_members (tenant_id, user_id, role, added_by) VALUES ($1, $2, $3, $4)
			ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = CASE
			  WHEN tenant_members.role = 'owner' OR EXCLUDED.role = 'owner' THEN 'owner' ELSE 'member' END`,
			c.tenantID, u.ID, c.role, c.invitedBy); err != nil {
			return err
		}
		if err := audit(ctx, tx, u.Email, "tenant.invitation.accept", c.name, map[string]any{"role": c.role}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SetMemberRole changes a member's role. Owners and administrators only, and
// the last owner cannot be demoted.
func (s *Store) SetMemberRole(ctx context.Context, tenant string, a Actor, userID, role string) (Member, error) {
	var m Member
	if !validRole(role) {
		return m, fmt.Errorf("store: unknown role %q", role)
	}
	if !isUUID(userID) {
		return m, ErrNotFound
	}
	tx, id, err := s.teamTx(ctx, tenant, a, "")
	if err != nil {
		return m, err
	}
	defer tx.Rollback(ctx)

	cur, err := memberRole(ctx, tx, id, userID)
	if err != nil {
		return m, err
	}
	if cur == "" {
		return m, ErrNotFound
	}
	if cur == RoleOwner && role == RoleMember {
		if n, err := ownerCount(ctx, tx, id); err != nil {
			return m, err
		} else if n <= 1 {
			return m, ErrLastOwner
		}
	}
	if err := tx.QueryRow(ctx, `UPDATE tenant_members m SET role = $3 FROM users u WHERE m.tenant_id = $1 AND m.user_id = $2 AND u.id = m.user_id
		RETURNING m.user_id, u.email, m.role, m.added_at`, id, userID, role).Scan(&m.UserID, &m.Email, &m.Role, &m.AddedAt); err != nil {
		return m, mapErr(err)
	}
	if err := audit(ctx, tx, a.Email, "tenant.member.role", tenant, map[string]any{"member": m.Email, "from": cur, "to": role}); err != nil {
		return m, err
	}
	return m, tx.Commit(ctx)
}

// RemoveMember removes a person from a team. Owners and administrators can
// remove anyone; anybody can remove themselves (leave). The last owner cannot
// be removed.
func (s *Store) RemoveMember(ctx context.Context, tenant string, a Actor, userID string) error {
	if !isUUID(userID) {
		return ErrNotFound
	}
	tx, id, err := s.teamTx(ctx, tenant, a, userID)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	cur, err := memberRole(ctx, tx, id, userID)
	if err != nil {
		return err
	}
	if cur == "" {
		return ErrNotFound
	}
	if cur == RoleOwner {
		if n, err := ownerCount(ctx, tx, id); err != nil {
			return err
		} else if n <= 1 {
			return ErrLastOwner
		}
	}
	var email string
	if err := tx.QueryRow(ctx, `DELETE FROM tenant_members m USING users u WHERE m.tenant_id = $1 AND m.user_id = $2 AND u.id = m.user_id RETURNING u.email`,
		id, userID).Scan(&email); err != nil {
		return mapErr(err)
	}
	action := "tenant.member.remove"
	if userID == a.ID {
		action = "tenant.member.leave"
	}
	if err := audit(ctx, tx, a.Email, action, tenant, map[string]any{"member": email, "role": cur}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
