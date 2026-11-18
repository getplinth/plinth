package api

import (
	"net/http"
	"strings"

	"github.com/getplinth/plinth/internal/store"
)

func actorOf(c caller) store.Actor { return store.Actor{ID: c.ID, Email: c.Email, Admin: c.isAdmin()} }

// listTenants lists the caller's teams. Administrators can ask for every team.
func (s *Server) listTenants(w http.ResponseWriter, r *http.Request, c caller) {
	all := c.isAdmin() && r.URL.Query().Get("all") == "true"
	list, err := s.d.Store.ListTenants(r.Context(), c.ID, all)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if list == nil {
		list = []store.Tenant{}
	}
	writeJSON(w, http.StatusOK, list)
}

// getTenant shows a team and its members to its members and to administrators.
// Everyone else is told it does not exist.
func (s *Server) getTenant(w http.ResponseWriter, r *http.Request, c caller) {
	name := r.PathValue("name")
	role, err := s.d.Store.TenantRole(r.Context(), name, c.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if role == "" && !c.isAdmin() {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// Who has been invited is for owners and administrators only.
	t, err := s.d.Store.GetTenant(r.Context(), name, c.ID, c.isAdmin() || role == store.RoleOwner)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

type memberBody struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

func roleOrDefault(role string) (string, bool) {
	if role == "" {
		return store.RoleMember, true
	}
	return role, role == store.RoleOwner || role == store.RoleMember
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request, c caller) {
	var b memberBody
	if !readJSON(w, r, &b) {
		return
	}
	email := strings.TrimSpace(b.Email)
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") {
		writeErr(w, http.StatusBadRequest, "enter the person's e-mail address")
		return
	}
	role, ok := roleOrDefault(b.Role)
	if !ok {
		writeErr(w, http.StatusBadRequest, `role must be "owner" or "member"`)
		return
	}
	res, err := s.d.Store.AddMember(r.Context(), r.PathValue("name"), actorOf(c), email, role)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if res.Member != nil { // they already had an account: added now
		writeJSON(w, http.StatusCreated, res.Member)
		return
	}
	// No account yet: invited. 202 because it takes effect when they sign in.
	writeJSON(w, http.StatusAccepted, res.Invitation)
}

// revokeInvitation withdraws an open invitation.
func (s *Server) revokeInvitation(w http.ResponseWriter, r *http.Request, c caller) {
	if err := s.d.Store.RevokeInvitation(r.Context(), r.PathValue("name"), actorOf(c), r.PathValue("id")); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setMemberRole(w http.ResponseWriter, r *http.Request, c caller) {
	var b memberBody
	if !readJSON(w, r, &b) {
		return
	}
	role, ok := roleOrDefault(b.Role)
	if !ok || b.Role == "" {
		writeErr(w, http.StatusBadRequest, `role must be "owner" or "member"`)
		return
	}
	m, err := s.d.Store.SetMemberRole(r.Context(), r.PathValue("name"), actorOf(c), r.PathValue("user"), role)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// removeMember removes someone, or lets the caller leave when it is themselves.
func (s *Server) removeMember(w http.ResponseWriter, r *http.Request, c caller) {
	if err := s.d.Store.RemoveMember(r.Context(), r.PathValue("name"), actorOf(c), r.PathValue("user")); err != nil {
		s.storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
