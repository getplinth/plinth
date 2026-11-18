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
	// ErrUserUnknown: nobody with that e-mail address has signed in yet.
	ErrUserUnknown = errors.New("nobody with that e-mail address has signed in to Plinth yet")
	// ErrAmbiguousEmail: more than one account has that address, so it cannot
	// identify one person.
	ErrAmbiguousEmail = errors.New("more than one account has that e-mail address")
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

// Tenant is a team with its members and projects.
type Tenant struct {
	Name        string    `json:"name"`
	Role        string    `json:"role"` // the viewer's role; empty for an administrator who is not a member
	MemberCount int       `json:"member_count"`
	Members     []Member  `json:"members,omitempty"`
	Projects    []string  `json:"projects"`
	CreatedAt   time.Time `json:"created_at"`
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

// GetTenant returns a team with its members. The caller decides who may see it.
func (s *Store) GetTenant(ctx context.Context, name, viewerID string) (Tenant, error) {
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

// AddMember adds a person who has already signed in, identified by e-mail
// address (compared without regard to case). Owners and administrators only.
func (s *Store) AddMember(ctx context.Context, tenant string, a Actor, email, role string) (Member, error) {
	var m Member
	if !validRole(role) {
		return m, fmt.Errorf("store: unknown role %q", role)
	}
	tx, id, err := s.teamTx(ctx, tenant, a, "")
	if err != nil {
		return m, err
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `SELECT id, email FROM users WHERE lower(email) = lower($1)`, strings.TrimSpace(email))
	if err != nil {
		return m, err
	}
	var found []Member
	for rows.Next() {
		var u Member
		if err := rows.Scan(&u.UserID, &u.Email); err != nil {
			rows.Close()
			return m, err
		}
		found = append(found, u)
	}
	rows.Close()
	switch {
	case len(found) == 0:
		return m, ErrUserUnknown
	case len(found) > 1:
		return m, ErrAmbiguousEmail
	}
	m = found[0]
	m.Role = role
	var addedBy *string
	if a.ID != "" {
		addedBy = &a.ID
	}
	if err := tx.QueryRow(ctx, `INSERT INTO tenant_members (tenant_id, user_id, role, added_by) VALUES ($1, $2, $3, $4) RETURNING added_at`,
		id, m.UserID, role, addedBy).Scan(&m.AddedAt); err != nil {
		return m, mapErr(err) // already a member: ErrConflict
	}
	m.AddedBy = a.Email
	if err := audit(ctx, tx, a.Email, "tenant.member.add", tenant, map[string]any{"member": m.Email, "role": role}); err != nil {
		return m, err
	}
	return m, tx.Commit(ctx)
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
