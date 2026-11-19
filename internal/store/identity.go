package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Session lifetimes: a session ends after SessionIdle without use, and never
// lasts longer than SessionMax.
const (
	SessionIdle = 8 * time.Hour
	SessionMax  = 7 * 24 * time.Hour
)

// Lockout: after LockoutAfter failed password attempts in a row the account
// refuses sign-in for a minute, doubling with every further failure up to LockoutMax.
const (
	LockoutAfter = 5
	LockoutMax   = 15 * time.Minute
)

// ErrLastAdmin: the change would leave nobody able to sign in as an administrator.
var ErrLastAdmin = errors.New("this is the last administrator who can sign in")

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{1,62}$`)

// NormalizeUsername lower-cases and trims a username and reports whether it is acceptable.
func NormalizeUsername(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	return s, usernameRe.MatchString(s)
}

// HashToken is how session tokens are stored: only this digest is kept.
func HashToken(token string) []byte { h := sha256.Sum256([]byte(token)); return h[:] }

// ---- local accounts -----------------------------------------------------------------------------------

// LocalAccount is a person who signs in with a password.
type LocalAccount struct {
	UserID      string     `json:"id"`
	Username    string     `json:"username"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`
	Disabled    bool       `json:"disabled"`
	MustChange  bool       `json:"must_change_password"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	LastLogin   *time.Time `json:"last_login,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

const localCols = `u.id::text, a.username, u.email, u.role, a.disabled, a.must_change,
	CASE WHEN a.locked_until > now() THEN a.locked_until END, a.last_login, a.created_at`

func scanLocal(row pgx.Row) (LocalAccount, error) {
	var a LocalAccount
	err := row.Scan(&a.UserID, &a.Username, &a.Email, &a.Role, &a.Disabled, &a.MustChange, &a.LockedUntil, &a.LastLogin, &a.CreatedAt)
	return a, mapErr(err)
}

// CreateLocalAccount adds a password account. The hash comes from the auth
// package. With an e-mail address the administrator vouches for it, so team
// invitations waiting for it are claimed at once; without one the username
// stands in and is never matched against invitations.
func (s *Store) CreateLocalAccount(ctx context.Context, username, email, role, hash string, mustChange bool) (LocalAccount, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LocalAccount{}, err
	}
	defer tx.Rollback(ctx)
	a, err := createLocal(ctx, tx, username, email, role, hash, mustChange)
	if err != nil {
		return a, err
	}
	if err := tx.Commit(ctx); err != nil {
		return a, err
	}
	if email = strings.ToLower(strings.TrimSpace(email)); strings.Contains(email, "@") {
		if err := s.claimInvitations(ctx, User{ID: a.UserID, Email: email, Role: role}); err != nil {
			return a, err
		}
	}
	return a, nil
}

func createLocal(ctx context.Context, tx pgx.Tx, username, email, role, hash string, mustChange bool) (LocalAccount, error) {
	username, ok := NormalizeUsername(username)
	if !ok {
		return LocalAccount{}, ErrConflict
	}
	email = strings.ToLower(strings.TrimSpace(email))
	verified := email != ""
	if email == "" {
		email = username
	}
	var id string
	if err := tx.QueryRow(ctx, `WITH n AS (SELECT gen_random_uuid() AS id)
		INSERT INTO users (id, oidc_sub, email, email_verified, role, auth_source)
		SELECT id, 'local:' || id, $1, $2, $3, 'local' FROM n RETURNING id::text`, email, verified, role).Scan(&id); err != nil {
		return LocalAccount{}, mapErr(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO local_accounts (user_id, username, password_hash, must_change) VALUES ($1, $2, $3, $4)`,
		id, username, hash, mustChange); err != nil {
		return LocalAccount{}, mapErr(err)
	}
	return scanLocal(tx.QueryRow(ctx, `SELECT `+localCols+` FROM local_accounts a JOIN users u ON u.id = a.user_id WHERE a.user_id = $1`, id))
}

// ListLocalAccounts returns every password account.
func (s *Store) ListLocalAccounts(ctx context.Context) ([]LocalAccount, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+localCols+` FROM local_accounts a JOIN users u ON u.id = a.user_id ORDER BY a.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LocalAccount{}
	for rows.Next() {
		a, err := scanLocal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetLocalAccount returns one account by user id.
func (s *Store) GetLocalAccount(ctx context.Context, userID string) (LocalAccount, error) {
	if !isUUID(userID) {
		return LocalAccount{}, ErrNotFound
	}
	return scanLocal(s.pool.QueryRow(ctx, `SELECT `+localCols+` FROM local_accounts a JOIN users u ON u.id = a.user_id WHERE a.user_id = $1`, userID))
}

// LoginRecord is what checking a password needs.
type LoginRecord struct {
	UserID      string
	Hash        string
	Disabled    bool
	LockedUntil *time.Time
	MustChange  bool
}

// LocalLogin looks up an account for sign-in. ErrNotFound when there is none.
func (s *Store) LocalLogin(ctx context.Context, username string) (LoginRecord, error) {
	username, _ = NormalizeUsername(username)
	var r LoginRecord
	err := s.pool.QueryRow(ctx, `SELECT user_id::text, password_hash, disabled, CASE WHEN locked_until > now() THEN locked_until END, must_change
		FROM local_accounts WHERE username = $1`, username).Scan(&r.UserID, &r.Hash, &r.Disabled, &r.LockedUntil, &r.MustChange)
	return r, mapErr(err)
}

// LoginFailed counts a wrong password and locks the account after too many in a row.
// It reports the time until which the account is now locked, if it is.
func (s *Store) LoginFailed(ctx context.Context, userID string) (*time.Time, error) {
	var until *time.Time
	err := s.pool.QueryRow(ctx, `UPDATE local_accounts SET failed_attempts = failed_attempts + 1,
		locked_until = CASE WHEN failed_attempts + 1 >= $2
		  THEN now() + least($3::interval, interval '1 minute' * power(2, failed_attempts + 1 - $2)) ELSE locked_until END
		WHERE user_id = $1 RETURNING CASE WHEN locked_until > now() THEN locked_until END`,
		userID, LockoutAfter, fmt.Sprintf("%d seconds", int(LockoutMax.Seconds()))).Scan(&until)
	return until, err
}

// LoginSucceeded clears the failure count.
func (s *Store) LoginSucceeded(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE local_accounts SET failed_attempts = 0, locked_until = NULL, last_login = now() WHERE user_id = $1`, userID)
	return err
}

// SetPassword replaces a password, ends every session of the account except
// the one given (the person changing their own password stays signed in), and
// clears any lockout. mustChange is true when someone else chose the password.
func (s *Store) SetPassword(ctx context.Context, userID, hash string, mustChange bool, keep []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE local_accounts SET password_hash = $2, must_change = $3, failed_attempts = 0, locked_until = NULL,
		password_changed_at = now() WHERE user_id = $1`, userID, hash, mustChange)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND token_hash IS DISTINCT FROM $2`, userID, keep); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LocalAccountPatch changes an account. Nil fields stay as they are.
type LocalAccountPatch struct {
	Role     *string
	Disabled *bool
	Email    *string
}

// UpdateLocalAccount changes the role, the disabled flag or the e-mail address.
// Disabling ends the account's sessions. It refuses to leave nobody able to
// sign in as an administrator, unless anotherWay says an identity provider
// (where administrators come from the provider's group) is available.
func (s *Store) UpdateLocalAccount(ctx context.Context, userID string, p LocalAccountPatch, anotherWay bool) (LocalAccount, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return LocalAccount{}, err
	}
	defer tx.Rollback(ctx)
	cur, err := lockAccount(ctx, tx, userID)
	if err != nil {
		return LocalAccount{}, err
	}
	role, disabled := cur.Role, cur.Disabled
	if p.Role != nil {
		role = *p.Role
	}
	if p.Disabled != nil {
		disabled = *p.Disabled
	}
	if cur.Role == "admin" && !cur.Disabled && (role != "admin" || disabled) {
		if err := requireOtherAdmin(ctx, tx, userID, anotherWay); err != nil {
			return LocalAccount{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, userID, role); err != nil {
		return LocalAccount{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE local_accounts SET disabled = $2 WHERE user_id = $1`, userID, disabled); err != nil {
		return LocalAccount{}, err
	}
	if p.Email != nil {
		email := strings.ToLower(strings.TrimSpace(*p.Email))
		verified := email != ""
		if email == "" {
			email = cur.Username
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET email = $2, email_verified = $3 WHERE id = $1`, userID, email, verified); err != nil {
			return LocalAccount{}, err
		}
	}
	if disabled || role != cur.Role {
		// A changed role takes effect at the next sign-in for tokens already issued.
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
			return LocalAccount{}, err
		}
	}
	out, err := scanLocal(tx.QueryRow(ctx, `SELECT `+localCols+` FROM local_accounts a JOIN users u ON u.id = a.user_id WHERE a.user_id = $1`, userID))
	if err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	if p.Email != nil && strings.Contains(out.Email, "@") && verifiedEmail(out) {
		if err := s.claimInvitations(ctx, User{ID: out.UserID, Email: out.Email, Role: out.Role}); err != nil {
			return out, err
		}
	}
	return out, nil
}

func verifiedEmail(a LocalAccount) bool { return a.Email != a.Username }

// DeleteLocalAccount removes an account that never did anything. One with
// history (requests, team membership records) cannot be deleted: disable it.
func (s *Store) DeleteLocalAccount(ctx context.Context, userID string, anotherWay bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	cur, err := lockAccount(ctx, tx, userID)
	if err != nil {
		return err
	}
	if cur.Role == "admin" && !cur.Disabled {
		if err := requireOtherAdmin(ctx, tx, userID, anotherWay); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return mapErr(err) // a foreign key means the account has history: ErrInUse
	}
	return tx.Commit(ctx)
}

func lockAccount(ctx context.Context, tx pgx.Tx, userID string) (LocalAccount, error) {
	if !isUUID(userID) {
		return LocalAccount{}, ErrNotFound
	}
	return scanLocal(tx.QueryRow(ctx, `SELECT `+localCols+` FROM local_accounts a JOIN users u ON u.id = a.user_id WHERE a.user_id = $1 FOR UPDATE OF a, u`, userID))
}

// requireOtherAdmin checks that an administrator other than userID can still sign in.
func requireOtherAdmin(ctx context.Context, tx pgx.Tx, userID string, anotherWay bool) error {
	if anotherWay {
		return nil
	}
	// Lock the administrators so two of them cannot remove each other at once.
	rows, err := tx.Query(ctx, `SELECT a.user_id FROM local_accounts a JOIN users u ON u.id = a.user_id
		WHERE u.role = 'admin' AND NOT a.disabled FOR UPDATE OF a`)
	if err != nil {
		return err
	}
	others := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if id != userID {
			others++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if others == 0 {
		return ErrLastAdmin
	}
	return nil
}

// ---- sessions -----------------------------------------------------------------------------------------

// CreateSession records a session for a token digest and returns when it expires if unused.
func (s *Store) CreateSession(ctx context.Context, userID string, tokenHash []byte) (time.Time, error) {
	var exp time.Time
	err := s.pool.QueryRow(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at, absolute_expires_at)
		VALUES ($1, $2, now() + $3::interval, now() + $4::interval) RETURNING expires_at`,
		tokenHash, userID, fmt.Sprintf("%d seconds", int(SessionIdle.Seconds())), fmt.Sprintf("%d seconds", int(SessionMax.Seconds()))).Scan(&exp)
	return exp, err
}

// SessionUser returns the user a session token belongs to, and renews its idle
// timeout. ErrNotFound when the token is unknown, expired, its account is
// disabled, or password sign-in has been switched off (unless force says it
// stays on whatever the setting is).
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte, force bool) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `WITH hit AS (
		  UPDATE sessions SET
		    last_used_at = CASE WHEN last_used_at < now() - interval '1 minute' THEN now() ELSE last_used_at END,
		    expires_at   = CASE WHEN last_used_at < now() - interval '1 minute'
		                        THEN least(now() + $2::interval, absolute_expires_at) ELSE expires_at END
		  WHERE token_hash = $1 AND expires_at > now() AND absolute_expires_at > now()
		  RETURNING user_id)
		SELECT u.id::text, u.oidc_sub, u.email, u.role, u.auth_source, a.must_change
		FROM hit JOIN users u ON u.id = hit.user_id JOIN local_accounts a ON a.user_id = u.id
		WHERE NOT a.disabled AND ($3 OR (SELECT local_login_enabled FROM auth_settings))`, tokenHash, fmt.Sprintf("%d seconds", int(SessionIdle.Seconds())), force).
		Scan(&u.ID, &u.Sub, &u.Email, &u.Role, &u.AuthSource, &u.MustChange)
	return u, mapErr(err)
}

// DeleteSession ends one session (sign out).
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	return err
}

// PurgeSessions removes sessions that can no longer be used.
func (s *Store) PurgeSessions(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now() OR absolute_expires_at < now()`)
	return err
}

// ---- bootstrap ----------------------------------------------------------------------------------------

// EnsureBootstrapAdmin creates the first administrator when no password
// account exists yet, and does nothing otherwise, so it is safe at every
// start and with several replicas starting together. The account must change
// its password at first sign-in.
//
// With onlyFresh it also does nothing when anybody has signed in before: an
// existing installation that was upgraded must not suddenly gain a password
// administrator. Starting Plinth passes onlyFresh; the explicit
// `plinth bootstrap-admin` command does not.
func (s *Store) EnsureBootstrapAdmin(ctx context.Context, username, hash string, onlyFresh bool) (created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7305001)`); err != nil {
		return false, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM local_accounts`).Scan(&n); err != nil || n > 0 {
		return false, err
	}
	if onlyFresh {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n > 0 {
			return false, err
		}
	}
	if _, err := createLocal(ctx, tx, username, "", "admin", hash, true); err != nil {
		return false, err
	}
	if err := audit(ctx, tx, "system", "auth.bootstrap_admin", username, nil); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ResetLocalPassword sets the password of a password account by username, for
// the command-line recovery path (someone with database access). The account
// is enabled and unlocked, its sessions end, and it must change the password.
// A missing account is created as an administrator.
func (s *Store) ResetLocalPassword(ctx context.Context, username, hash string) (created bool, err error) {
	username, ok := NormalizeUsername(username)
	if !ok {
		return false, ErrConflict
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT user_id::text FROM local_accounts WHERE username = $1 FOR UPDATE`, username).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := createLocal(ctx, tx, username, "", "admin", hash, true); err != nil {
			return false, err
		}
		created = true
	case err != nil:
		return false, err
	default:
		if _, err := tx.Exec(ctx, `UPDATE local_accounts SET password_hash = $2, must_change = true, disabled = false,
			failed_attempts = 0, locked_until = NULL, password_changed_at = now() WHERE user_id = $1`, id, hash); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
			return false, err
		}
	}
	if err := audit(ctx, tx, "system", "auth.password_reset_cli", username, map[string]any{"created": created}); err != nil {
		return false, err
	}
	return created, tx.Commit(ctx)
}

// ---- settings and providers ---------------------------------------------------------------------------

// LocalLoginEnabled says whether password sign-in is switched on.
func (s *Store) LocalLoginEnabled(ctx context.Context) (bool, error) {
	var on bool
	err := s.pool.QueryRow(ctx, `SELECT local_login_enabled FROM auth_settings`).Scan(&on)
	return on, err
}

// SetLocalLoginEnabled switches password sign-in on or off. Turning it off
// needs an enabled identity provider (anotherWay), or nobody could sign in.
func (s *Store) SetLocalLoginEnabled(ctx context.Context, on, anotherWay bool) error {
	if !on && !anotherWay {
		return ErrLastAdmin
	}
	_, err := s.pool.Exec(ctx, `UPDATE auth_settings SET local_login_enabled = $1`, on)
	return err
}

// OIDCProvider is an identity provider an administrator configured.
type OIDCProvider struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	DisplayName          string    `json:"display_name"`
	Issuer               string    `json:"issuer"`
	ClientID             string    `json:"client_id"`
	Scopes               string    `json:"scopes"`
	GroupsClaim          string    `json:"groups_claim"`
	AdminGroup           string    `json:"admin_group"`
	TrustUnverifiedEmail bool      `json:"trust_unverified_email"`
	Enabled              bool      `json:"enabled"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

const providerCols = `id::text, name, display_name, issuer, client_id, scopes, groups_claim, admin_group, trust_unverified_email, enabled, created_at, updated_at`

func scanProvider(row pgx.Row) (OIDCProvider, error) {
	var p OIDCProvider
	err := row.Scan(&p.ID, &p.Name, &p.DisplayName, &p.Issuer, &p.ClientID, &p.Scopes, &p.GroupsClaim, &p.AdminGroup, &p.TrustUnverifiedEmail, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	return p, mapErr(err)
}

// ListOIDCProviders returns the providers, by name.
func (s *Store) ListOIDCProviders(ctx context.Context) ([]OIDCProvider, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+providerCols+` FROM oidc_providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OIDCProvider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetOIDCProvider returns one provider by id.
func (s *Store) GetOIDCProvider(ctx context.Context, id string) (OIDCProvider, error) {
	if !isUUID(id) {
		return OIDCProvider{}, ErrNotFound
	}
	return scanProvider(s.pool.QueryRow(ctx, `SELECT `+providerCols+` FROM oidc_providers WHERE id = $1`, id))
}

// CreateOIDCProvider adds a provider.
func (s *Store) CreateOIDCProvider(ctx context.Context, p OIDCProvider) (OIDCProvider, error) {
	return scanProvider(s.pool.QueryRow(ctx, `INSERT INTO oidc_providers (name, display_name, issuer, client_id, scopes, groups_claim, admin_group, trust_unverified_email, enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+providerCols,
		p.Name, p.DisplayName, p.Issuer, p.ClientID, p.Scopes, p.GroupsClaim, p.AdminGroup, p.TrustUnverifiedEmail, p.Enabled))
}

// UpdateOIDCProvider replaces everything but the name and id.
func (s *Store) UpdateOIDCProvider(ctx context.Context, p OIDCProvider) (OIDCProvider, error) {
	if !isUUID(p.ID) {
		return OIDCProvider{}, ErrNotFound
	}
	return scanProvider(s.pool.QueryRow(ctx, `UPDATE oidc_providers SET display_name = $2, issuer = $3, client_id = $4, scopes = $5,
		groups_claim = $6, admin_group = $7, trust_unverified_email = $8, enabled = $9, updated_at = now()
		WHERE id = $1 RETURNING `+providerCols,
		p.ID, p.DisplayName, p.Issuer, p.ClientID, p.Scopes, p.GroupsClaim, p.AdminGroup, p.TrustUnverifiedEmail, p.Enabled))
}

// DeleteOIDCProvider removes a provider. People who signed in through it keep
// their records but can no longer sign in until it is configured again.
func (s *Store) DeleteOIDCProvider(ctx context.Context, id string) error {
	if !isUUID(id) {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM oidc_providers WHERE id = $1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// HasOIDCAdmin says whether anyone with the administrator role has signed in
// through an identity provider, which shows that the provider and its
// administrator group are set up in a way that works.
func (s *Store) HasOIDCAdmin(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE auth_source = 'oidc' AND role = 'admin')`).Scan(&ok)
	return ok, err
}
