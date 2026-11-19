package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/store"
)

// environmentProvider is the id the control panel uses for the identity
// provider configured through PLINTH_OIDC_* variables.
const environmentProvider = "environment"

var providerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// ---- what the sign-in page needs ---------------------------------------------------------------------

type publicProvider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"client_id"`
	Scopes      string `json:"scopes"`
}

type publicConfig struct {
	LocalLogin bool             `json:"local_login"`
	Providers  []publicProvider `json:"providers"`
}

func providerID(p auth.Provider) string {
	if p.ID == "" {
		return environmentProvider
	}
	return p.ID
}

// localLoginOn says whether password sign-in is on. PLINTH_FORCE_LOCAL_LOGIN
// switches it on regardless of the setting: the way back in when an identity
// provider stopped working after password sign-in was turned off.
func (s *Server) localLoginOn(r *http.Request) bool {
	if s.d.ForceLocalLogin {
		return true
	}
	on, err := s.d.Store.LocalLoginEnabled(r.Context())
	return err == nil && on
}

func (s *Server) providers(r *http.Request) []auth.Provider {
	if s.d.Providers == nil {
		return nil
	}
	return s.d.Providers.Providers(r.Context())
}

func (s *Server) publicConfig(w http.ResponseWriter, r *http.Request) {
	out := publicConfig{Providers: []publicProvider{}}
	if s.d.Store != nil {
		out.LocalLogin = s.localLoginOn(r)
	}
	for _, p := range s.providers(r) {
		scopes := p.Scopes
		if scopes == "" {
			scopes = "openid profile email groups"
		}
		name := p.DisplayName
		if name == "" {
			name = p.Name
		}
		out.Providers = append(out.Providers, publicProvider{ID: providerID(p), Name: p.Name, DisplayName: name, Issuer: p.Issuer, ClientID: p.ClientID, Scopes: scopes})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- password sign-in -------------------------------------------------------------------------------

type loginBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResult struct {
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expires_at"`
	MustChange bool      `json:"must_change_password"`
}

var errBadLogin = "invalid username or password"

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var b loginBody
	if !readJSON(w, r, &b) {
		return
	}
	if !s.localLoginOn(r) {
		writeErr(w, http.StatusForbidden, "password sign-in is turned off: use an identity provider")
		return
	}
	name, _ := store.NormalizeUsername(b.Username)
	if len(b.Password) > auth.MaxPasswordLength*2 {
		b.Password = b.Password[:auth.MaxPasswordLength*2] // no point hashing megabytes
	}
	if !s.logins.allow(name, remoteAddr(r)) {
		w.Header().Set("Retry-After", "10")
		writeErr(w, http.StatusTooManyRequests, "too many sign-in attempts: wait a few seconds and try again")
		return
	}
	fail := func(why string) {
		s.d.Log.Info("sign-in refused", "reason", why)
		writeErr(w, http.StatusUnauthorized, errBadLogin) // the same answer for every reason
	}
	rec, err := s.d.Store.LocalLogin(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		auth.SpendTime(b.Password) // as slow as a real check, so timing does not reveal who exists
		fail("unknown username")
		return
	case err != nil:
		s.storeError(w, err)
		return
	case rec.Disabled || rec.LockedUntil != nil:
		auth.SpendTime(b.Password)
		if rec.Disabled {
			fail("account disabled")
		} else {
			fail("account locked")
		}
		return
	}
	if !auth.CheckPassword(rec.Hash, b.Password) {
		until, err := s.d.Store.LoginFailed(r.Context(), rec.UserID)
		if err != nil {
			s.storeError(w, err)
			return
		}
		if until != nil {
			s.d.Log.Warn("account locked after repeated wrong passwords", "until", until.UTC().Format(time.RFC3339))
			_ = s.d.Store.Audit(r.Context(), name, "auth.lockout", name, map[string]any{"until": until})
		}
		fail("wrong password")
		return
	}
	token, err := auth.NewSessionToken()
	if err != nil {
		s.d.Log.Error("session token", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	exp, err := s.d.Store.CreateSession(r.Context(), rec.UserID, store.HashToken(token))
	if err != nil {
		s.storeError(w, err)
		return
	}
	_ = s.d.Store.LoginSucceeded(r.Context(), rec.UserID)
	_ = s.d.Store.Audit(r.Context(), name, "auth.login", name, nil)
	writeJSON(w, http.StatusOK, loginResult{Token: token, ExpiresAt: exp, MustChange: rec.MustChange})
}

func bearer(r *http.Request) string {
	raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(raw)
}

// logout ends the session behind the token. For people who signed in through
// an identity provider there is nothing to end here.
func (s *Server) logout(w http.ResponseWriter, r *http.Request, c caller) {
	if tok := bearer(r); strings.HasPrefix(tok, auth.SessionPrefix) {
		if err := s.d.Store.DeleteSession(r.Context(), store.HashToken(tok)); err != nil {
			s.storeError(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type passwordBody struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

// changePassword lets a password account replace its own password. The other
// sessions of the account end; this one stays.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, c caller) {
	var b passwordBody
	if !readJSON(w, r, &b) {
		return
	}
	if c.AuthSource != "local" {
		writeErr(w, http.StatusBadRequest, "your password is managed by your identity provider")
		return
	}
	acc, err := s.d.Store.GetLocalAccount(r.Context(), c.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !s.logins.allow(acc.Username, remoteAddr(r)) {
		w.Header().Set("Retry-After", "10")
		writeErr(w, http.StatusTooManyRequests, "too many attempts: wait a few seconds and try again")
		return
	}
	rec, err := s.d.Store.LocalLogin(r.Context(), acc.Username)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !auth.CheckPassword(rec.Hash, b.Current) {
		writeErr(w, http.StatusForbidden, "the current password is not right")
		return
	}
	if b.New == b.Current {
		writeErr(w, http.StatusBadRequest, "choose a password different from the current one")
		return
	}
	if err := auth.ValidatePassword(b.New, acc.Username, acc.Email); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	hash, err := auth.HashPassword(b.New)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.d.Store.SetPassword(r.Context(), c.ID, hash, false, store.HashToken(bearer(r))); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "auth.password_change", acc.Username, nil)
	w.WriteHeader(http.StatusNoContent)
}

// allowedWhilePasswordMustChange: a password somebody else chose has to be
// replaced before anything else works.
func allowedWhilePasswordMustChange(r *http.Request) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/me", "POST /api/v1/auth/password", "POST /api/v1/auth/logout":
		return true
	}
	return false
}

// ---- password accounts (administrators) ----------------------------------------------------------

func (s *Server) hasProvider(r *http.Request) bool { return len(s.providers(r)) > 0 }

func (s *Server) listLocalUsers(w http.ResponseWriter, r *http.Request, c caller) {
	list, err := s.d.Store.ListLocalAccounts(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type localUserBody struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

type localUserResult struct {
	store.LocalAccount
	// TemporaryPassword is shown once, when Plinth chose the password. The
	// person must change it at first sign-in.
	TemporaryPassword string `json:"temporary_password,omitempty"`
}

// choosePassword uses the administrator's password if given (and acceptable),
// else makes a random one. ok is false when a response was already written.
func (s *Server) choosePassword(w http.ResponseWriter, given, username, email string) (hash, generated string, ok bool) {
	pw := given
	if pw == "" {
		var err error
		if pw, err = auth.GeneratePassword(); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return "", "", false
		}
		generated = pw
	} else if err := auth.ValidatePassword(pw, username, email); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return "", "", false
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return "", "", false
	}
	return hash, generated, true
}

func validRole(r string) bool { return r == auth.RoleAdmin || r == auth.RoleUser }

func (s *Server) createLocalUser(w http.ResponseWriter, r *http.Request, c caller) {
	var b localUserBody
	if !readJSON(w, r, &b) {
		return
	}
	username, ok := store.NormalizeUsername(b.Username)
	switch {
	case !ok:
		writeErr(w, http.StatusBadRequest, "the username must be 2 to 63 characters: lowercase letters, digits, dots, dashes, underscores or @")
		return
	case !validRole(b.Role):
		writeErr(w, http.StatusBadRequest, "role must be admin or user")
		return
	case b.Email != "" && !strings.Contains(b.Email, "@"):
		writeErr(w, http.StatusBadRequest, "that is not an e-mail address")
		return
	}
	hash, generated, ok := s.choosePassword(w, b.Password, username, b.Email)
	if !ok {
		return
	}
	acc, err := s.d.Store.CreateLocalAccount(r.Context(), username, b.Email, b.Role, hash, true)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, "that username is already taken")
			return
		}
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "local_user.create", username, map[string]any{"role": b.Role})
	writeJSON(w, http.StatusCreated, localUserResult{LocalAccount: acc, TemporaryPassword: generated})
}

type localUserPatchBody struct {
	Role     *string `json:"role"`
	Disabled *bool   `json:"disabled"`
	Email    *string `json:"email"`
}

func (s *Server) updateLocalUser(w http.ResponseWriter, r *http.Request, c caller) {
	id := r.PathValue("id")
	var b localUserPatchBody
	if !readJSON(w, r, &b) {
		return
	}
	if b.Role != nil && !validRole(*b.Role) {
		writeErr(w, http.StatusBadRequest, "role must be admin or user")
		return
	}
	if b.Email != nil && *b.Email != "" && !strings.Contains(*b.Email, "@") {
		writeErr(w, http.StatusBadRequest, "that is not an e-mail address")
		return
	}
	if id == c.ID && (b.Disabled != nil && *b.Disabled || b.Role != nil && *b.Role != c.Role) {
		writeErr(w, http.StatusConflict, "you cannot disable or demote yourself: ask another administrator")
		return
	}
	acc, err := s.d.Store.UpdateLocalAccount(r.Context(), id, store.LocalAccountPatch{Role: b.Role, Disabled: b.Disabled, Email: b.Email}, s.hasProvider(r))
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "local_user.update", acc.Username, map[string]any{"role": acc.Role, "disabled": acc.Disabled})
	writeJSON(w, http.StatusOK, acc)
}

type resetBody struct {
	Password string `json:"password"`
}

func (s *Server) resetLocalPassword(w http.ResponseWriter, r *http.Request, c caller) {
	id := r.PathValue("id")
	var b resetBody
	if r.ContentLength != 0 && !readJSON(w, r, &b) {
		return
	}
	if id == c.ID {
		writeErr(w, http.StatusConflict, "use Change password for your own account")
		return
	}
	acc, err := s.d.Store.GetLocalAccount(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	hash, generated, ok := s.choosePassword(w, b.Password, acc.Username, acc.Email)
	if !ok {
		return
	}
	if err := s.d.Store.SetPassword(r.Context(), id, hash, true, nil); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "local_user.reset_password", acc.Username, nil)
	writeJSON(w, http.StatusOK, map[string]any{"temporary_password": generated})
}

func (s *Server) deleteLocalUser(w http.ResponseWriter, r *http.Request, c caller) {
	id := r.PathValue("id")
	if id == c.ID {
		writeErr(w, http.StatusConflict, "you cannot delete yourself")
		return
	}
	acc, _ := s.d.Store.GetLocalAccount(r.Context(), id)
	if err := s.d.Store.DeleteLocalAccount(r.Context(), id, s.hasProvider(r)); err != nil {
		if errors.Is(err, store.ErrInUse) {
			writeErr(w, http.StatusConflict, "this account has requests or team memberships on record: disable it instead")
			return
		}
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "local_user.delete", acc.Username, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- identity providers (administrators) --------------------------------------------------------

type providerBody struct {
	Name                 string `json:"name"`
	DisplayName          string `json:"display_name"`
	Issuer               string `json:"issuer"`
	ClientID             string `json:"client_id"`
	Scopes               string `json:"scopes"`
	GroupsClaim          string `json:"groups_claim"`
	AdminGroup           string `json:"admin_group"`
	TrustUnverifiedEmail bool   `json:"trust_unverified_email"`
	Enabled              *bool  `json:"enabled"`
}

type providerView struct {
	store.OIDCProvider
	ReadOnly bool `json:"read_only"`
}

func fromAuth(p auth.Provider) providerView {
	return providerView{ReadOnly: true, OIDCProvider: store.OIDCProvider{
		ID: environmentProvider, Name: p.Name, DisplayName: p.DisplayName, Issuer: p.Issuer, ClientID: p.ClientID, Scopes: p.Scopes,
		GroupsClaim: p.GroupsClaim, AdminGroup: p.AdminGroup, TrustUnverifiedEmail: p.TrustUnverifiedEmail, Enabled: true,
	}}
}

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request, c caller) {
	list, err := s.d.Store.ListOIDCProviders(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	out := []providerView{}
	if s.d.EnvProvider != nil {
		out = append(out, fromAuth(*s.d.EnvProvider))
	}
	for _, p := range list {
		out = append(out, providerView{OIDCProvider: p})
	}
	writeJSON(w, http.StatusOK, out)
}

// normalize fills defaults and validates what an administrator entered. It
// writes the response and returns false when something is wrong.
func (s *Server) normalizeProvider(w http.ResponseWriter, r *http.Request, b providerBody, creating bool) (store.OIDCProvider, bool) {
	p := store.OIDCProvider{
		Name: strings.TrimSpace(b.Name), DisplayName: strings.TrimSpace(b.DisplayName), Issuer: strings.TrimRight(strings.TrimSpace(b.Issuer), "/"),
		ClientID: strings.TrimSpace(b.ClientID), Scopes: strings.Join(strings.Fields(b.Scopes), " "),
		GroupsClaim: strings.TrimSpace(b.GroupsClaim), AdminGroup: strings.TrimSpace(b.AdminGroup),
		TrustUnverifiedEmail: b.TrustUnverifiedEmail, Enabled: b.Enabled == nil || *b.Enabled,
	}
	bad := func(msg string) (store.OIDCProvider, bool) { writeErr(w, http.StatusBadRequest, msg); return p, false }
	if creating && !providerNameRe.MatchString(p.Name) {
		return bad("the name must be lowercase letters, digits and dashes, up to 31 characters")
	}
	if p.DisplayName == "" {
		return bad("give it a name people recognise on the sign-in page, for example Company SSO")
	}
	if len(p.DisplayName) > 60 {
		return bad("the display name is too long (60 characters at most)")
	}
	if p.ClientID == "" || len(p.ClientID) > 255 {
		return bad("the client id is required: the one registered for Plinth at the provider")
	}
	if p.Scopes == "" {
		p.Scopes = "openid profile email groups"
	}
	if !containsWord(p.Scopes, "openid") {
		return bad("the scopes must include openid")
	}
	if p.GroupsClaim == "" {
		p.GroupsClaim = "groups"
	}
	if err := auth.ValidateIssuer(p.Issuer, s.d.AllowLoopbackIssuers); err != nil {
		return bad(err.Error())
	}
	if p.Enabled && s.d.Providers != nil { // contact it now: a wrong address is found here, not at the first sign-in
		if err := s.d.Providers.Check(r.Context(), p.Issuer); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, "the provider cannot be used: "+err.Error())
			return p, false
		}
	}
	return p, true
}

func containsWord(list, word string) bool {
	for _, w := range strings.Fields(list) {
		if w == word {
			return true
		}
	}
	return false
}

func (s *Server) refreshProviders(r *http.Request) {
	if s.d.Providers != nil {
		if err := s.d.Providers.Refresh(r.Context()); err != nil {
			s.d.Log.Warn("could not reload identity providers", "err", err)
		}
	}
}

func (s *Server) createProvider(w http.ResponseWriter, r *http.Request, c caller) {
	var b providerBody
	if !readJSON(w, r, &b) {
		return
	}
	p, ok := s.normalizeProvider(w, r, b, true)
	if !ok {
		return
	}
	out, err := s.d.Store.CreateOIDCProvider(r.Context(), p)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, "a provider with that name, or for that issuer and client id, already exists")
			return
		}
		s.storeError(w, err)
		return
	}
	s.refreshProviders(r)
	s.audit(r.Context(), c, "oidc_provider.create", out.Name, map[string]any{"issuer": out.Issuer})
	writeJSON(w, http.StatusCreated, providerView{OIDCProvider: out})
}

var errOnlyWayIn = errors.New("this is the only way to sign in: turn password sign-in back on first")

// leavesAWayIn reports whether, after removing or disabling the provider with
// the given id, someone could still sign in at all.
func (s *Server) leavesAWayIn(r *http.Request, without string) bool {
	if s.localLoginOn(r) {
		return true
	}
	for _, p := range s.providers(r) {
		if p.ID != without {
			return true
		}
	}
	return false
}

func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request, c caller) {
	id := r.PathValue("id")
	if id == environmentProvider {
		writeErr(w, http.StatusConflict, "this provider is configured through the environment (PLINTH_OIDC_*) and can only be changed there")
		return
	}
	var b providerBody
	if !readJSON(w, r, &b) {
		return
	}
	cur, err := s.d.Store.GetOIDCProvider(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	b.Name = cur.Name // a name never changes
	p, ok := s.normalizeProvider(w, r, b, false)
	if !ok {
		return
	}
	p.ID = id
	if !p.Enabled && cur.Enabled && !s.leavesAWayIn(r, id) {
		writeErr(w, http.StatusConflict, errOnlyWayIn.Error())
		return
	}
	out, err := s.d.Store.UpdateOIDCProvider(r.Context(), p)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, "another provider already uses that issuer and client id")
			return
		}
		s.storeError(w, err)
		return
	}
	s.refreshProviders(r)
	s.audit(r.Context(), c, "oidc_provider.update", out.Name, map[string]any{"issuer": out.Issuer, "enabled": out.Enabled})
	writeJSON(w, http.StatusOK, providerView{OIDCProvider: out})
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request, c caller) {
	id := r.PathValue("id")
	if id == environmentProvider {
		writeErr(w, http.StatusConflict, "this provider is configured through the environment (PLINTH_OIDC_*) and can only be removed there")
		return
	}
	cur, err := s.d.Store.GetOIDCProvider(r.Context(), id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if cur.Enabled && !s.leavesAWayIn(r, id) {
		writeErr(w, http.StatusConflict, errOnlyWayIn.Error())
		return
	}
	if err := s.d.Store.DeleteOIDCProvider(r.Context(), id); err != nil {
		s.storeError(w, err)
		return
	}
	s.refreshProviders(r)
	s.audit(r.Context(), c, "oidc_provider.delete", cur.Name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- sign-in settings -----------------------------------------------------------------------------

type authSettings struct {
	LocalLogin bool `json:"local_login_enabled"`
	// Forced: PLINTH_FORCE_LOCAL_LOGIN keeps password sign-in on whatever is set here.
	Forced bool `json:"forced"`
}

func (s *Server) getAuthSettings(w http.ResponseWriter, r *http.Request, c caller) {
	on, err := s.d.Store.LocalLoginEnabled(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, authSettings{LocalLogin: on, Forced: s.d.ForceLocalLogin})
}

func (s *Server) putAuthSettings(w http.ResponseWriter, r *http.Request, c caller) {
	var b struct {
		LocalLogin *bool `json:"local_login_enabled"`
	}
	if !readJSON(w, r, &b) || b.LocalLogin == nil {
		if b.LocalLogin == nil {
			writeErr(w, http.StatusBadRequest, "local_login_enabled is required")
		}
		return
	}
	if !*b.LocalLogin {
		if !s.hasProvider(r) {
			writeErr(w, http.StatusConflict, "turn password sign-in off only after an identity provider is set up: otherwise nobody could sign in")
			return
		}
		ok, err := s.d.Store.HasOIDCAdmin(r.Context())
		if err != nil {
			s.storeError(w, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusConflict, "no administrator has signed in through an identity provider yet: do that once, and check that the provider's administrator group is set, before turning password sign-in off")
			return
		}
	}
	if err := s.d.Store.SetLocalLoginEnabled(r.Context(), *b.LocalLogin, s.hasProvider(r)); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "auth.settings", "local_login", map[string]any{"enabled": *b.LocalLogin})
	writeJSON(w, http.StatusOK, authSettings{LocalLogin: *b.LocalLogin, Forced: s.d.ForceLocalLogin})
}
