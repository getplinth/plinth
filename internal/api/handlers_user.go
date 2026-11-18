package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/getplinth/plinth/internal/onboarding"
	"github.com/getplinth/plinth/internal/request"
	"github.com/getplinth/plinth/internal/store"
)

// ---- blueprints ---------------------------------------------------------------------------

// blueprintSummary is what ordinary users see: enough to choose a blueprint,
// without naming the underlying systems.
type blueprintSummary struct {
	Name             string   `json:"name"`
	Environments     []string `json:"environments"`
	HasDatabase      bool     `json:"has_database"`
	DatabaseTypes    []string `json:"database_types"`
	HasRegistry      bool     `json:"has_registry"`
	RequiresApproval bool     `json:"requires_approval"`
}

func summarize(b store.Blueprint, dbTypes []string) blueprintSummary {
	s := blueprintSummary{Name: b.Name, HasRegistry: b.Registry != "", Environments: []string{}, DatabaseTypes: dbTypes, RequiresApproval: b.RequiresApproval}
	if s.DatabaseTypes == nil {
		s.DatabaseTypes = []string{}
	}
	for _, e := range b.Environments {
		s.Environments = append(s.Environments, e.Name)
		if e.Database != "" {
			s.HasDatabase = true
		}
	}
	return s
}

func (s *Server) listBlueprints(w http.ResponseWriter, r *http.Request, c caller) {
	list, err := s.d.Store.ListBlueprints(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	if c.isAdmin() {
		if list == nil {
			list = []store.Blueprint{}
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	drivers, err := s.integrationDrivers(r)
	if err != nil {
		s.storeError(w, err)
		return
	}
	out := make([]blueprintSummary, 0, len(list))
	for _, b := range list {
		out = append(out, summarize(b, dbTypesOf(b, drivers)))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getBlueprint(w http.ResponseWriter, r *http.Request, c caller) {
	b, err := s.d.Store.GetBlueprint(r.Context(), r.PathValue("name"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	if c.isAdmin() {
		writeJSON(w, http.StatusOK, b)
		return
	}
	drivers, err := s.integrationDrivers(r)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summarize(b, dbTypesOf(b, drivers)))
}

// integrationDrivers maps integration name to driver name.
func (s *Server) integrationDrivers(r *http.Request) (map[string]string, error) {
	ints, err := s.d.Store.ListIntegrations(r.Context())
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, in := range ints {
		m[in.Name] = in.Driver
	}
	return m, nil
}

// dbTypesOf lists the distinct database drivers a blueprint offers.
func dbTypesOf(b store.Blueprint, drivers map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range b.Environments {
		if d := drivers[e.Database]; e.Database != "" && d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) listTiers(w http.ResponseWriter, r *http.Request, c caller) {
	tiers, err := s.d.Store.Tiers(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tiers)
}

func validBlueprint(b *store.Blueprint) string {
	switch {
	case !nameRe.MatchString(b.Name):
		return "name must be lowercase letters, digits and dashes"
	case b.GitSource == "":
		return "git_source is required"
	case len(b.Environments) == 0:
		return "at least one environment is required"
	case len(b.Environments) > 10:
		return "too many environments"
	case len(b.TemplateVars) > 50 || len(b.ExtraBranches) > 20:
		return "too many template_vars or extra_branches"
	}
	seen := map[string]bool{}
	for _, e := range b.Environments {
		if !nameRe.MatchString(e.Name) || e.Cluster == "" {
			return "every environment needs a name (lowercase letters, digits, dashes) and a cluster"
		}
		if seen[e.Name] {
			return "duplicate environment " + e.Name
		}
		seen[e.Name] = true
	}
	return ""
}

func (s *Server) createBlueprint(w http.ResponseWriter, r *http.Request, c caller) {
	var b store.Blueprint
	if !readJSON(w, r, &b) {
		return
	}
	if msg := validBlueprint(&b); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	out, err := s.d.Store.SaveBlueprint(r.Context(), b, true)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "blueprint.create", out.Name, nil)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) replaceBlueprint(w http.ResponseWriter, r *http.Request, c caller) {
	var b store.Blueprint
	if !readJSON(w, r, &b) {
		return
	}
	b.Name = r.PathValue("name")
	if msg := validBlueprint(&b); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	out, err := s.d.Store.SaveBlueprint(r.Context(), b, false)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "blueprint.update", out.Name, nil)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteBlueprint(w http.ResponseWriter, r *http.Request, c caller) {
	name := r.PathValue("name")
	if err := s.d.Store.DeleteBlueprint(r.Context(), name); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "blueprint.delete", name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- requests ---------------------------------------------------------------------------------

type submitBody struct {
	Blueprint string `json:"blueprint"`
	request.Request
}

// shapeOf describes a blueprint for validation without contacting any system.
func (s *Server) shapeOf(r *http.Request, b store.Blueprint) (onboarding.Shape, error) {
	tiers, err := s.d.Store.Tiers(r.Context())
	if err != nil {
		return onboarding.Shape{}, err
	}
	ints, err := s.d.Store.ListIntegrations(r.Context())
	if err != nil {
		return onboarding.Shape{}, err
	}
	driver := map[string]string{}
	for _, in := range ints {
		driver[in.Name] = in.Driver
	}
	sh := onboarding.Shape{Tiers: tiers, HasRegistry: b.Registry != ""}
	for _, e := range b.Environments {
		sh.EnvNames = append(sh.EnvNames, e.Name)
		sh.EnvDBDrivers = append(sh.EnvDBDrivers, driver[e.Database]) // "" when no database
	}
	return sh, nil
}

// withPlans fills in the planned steps of requests. Failures leave Plan
// empty: the UI then shows only the steps that have started.
func (s *Server) withPlans(r *http.Request, recs ...store.RequestRecord) []store.RequestRecord {
	shapes := map[string]*onboarding.Shape{}
	for i := range recs {
		name := recs[i].Blueprint
		sh, seen := shapes[name]
		if !seen {
			if bp, err := s.d.Store.GetBlueprint(r.Context(), name); err == nil {
				if v, err := s.shapeOf(r, bp); err == nil {
					sh = &v
				}
			}
			shapes[name] = sh
		}
		if sh != nil {
			recs[i].Plan = onboarding.StepNames(*sh, recs[i].Payload)
		}
	}
	return recs
}

func (s *Server) submitRequest(w http.ResponseWriter, r *http.Request, c caller) {
	if !s.limiters.allow(c.ID) {
		w.Header().Set("Retry-After", "6")
		writeErr(w, http.StatusTooManyRequests, "too many requests, slow down")
		return
	}
	var b submitBody
	if !readJSON(w, r, &b) {
		return
	}
	bp, err := s.d.Store.GetBlueprint(r.Context(), b.Blueprint)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusUnprocessableEntity, "unknown blueprint")
			return
		}
		s.storeError(w, err)
		return
	}
	sh, err := s.shapeOf(r, bp)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := onboarding.ValidateRequest(sh, b.Request); err != nil {
		s.storeError(w, err)
		return
	}
	rec, err := s.d.Store.SubmitRequest(r.Context(), store.Submit{
		Blueprint: b.Blueprint, UserID: c.ID, UserEmail: c.Email, IsAdmin: c.isAdmin(), Req: b.Request,
	})
	if err != nil {
		s.storeError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v1/requests/"+rec.ID)
	writeJSON(w, http.StatusAccepted, s.withPlans(r, rec)[0])
}

var validStatus = map[string]bool{
	store.StatusAwaiting: true, store.StatusPending: true, store.StatusRunning: true,
	store.StatusSucceeded: true, store.StatusFailed: true, store.StatusRejected: true, store.StatusCancelled: true,
}

// decisionBody is the optional note that goes with an approval decision.
type decisionBody struct {
	Note string `json:"note"`
}

const maxNote = 500

// decide handles approve, reject and cancel of a request awaiting approval.
// Approve and reject are registered as administrator-only routes; cancel is
// for the requester or an administrator, and other people's requests are 404.
func (s *Server) decide(action string) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, c caller) {
		var b decisionBody
		if r.ContentLength != 0 && !readJSON(w, r, &b) {
			return // readJSON has already answered
		}
		note := strings.TrimSpace(b.Note)
		switch {
		case len(note) > maxNote:
			writeErr(w, http.StatusBadRequest, "the note is too long (500 characters at most)")
			return
		case action == store.DecisionReject && len(note) < 3:
			writeErr(w, http.StatusBadRequest, "give a reason for rejecting, so the requester knows what to change")
			return
		}
		rec, err := s.d.Store.GetRequest(r.Context(), r.PathValue("id"))
		if err != nil {
			s.storeError(w, err)
			return
		}
		acc, err := s.accessTo(r, c, rec)
		if err != nil {
			s.storeError(w, err)
			return
		}
		if !acc.view {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		out, err := s.d.Store.DecideRequest(r.Context(), rec.ID, store.Decision{
			Action: action, ActorID: c.ID, ActorEmail: c.Email, ActorAdmin: c.isAdmin(), Note: note,
		})
		if err != nil {
			s.storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.withPlans(r, out)[0])
	}
}

// requestAccess is what a caller may do with a request:
//   - view: administrators, members of its team, and whoever requested it
//     (even after leaving the team);
//   - retry: administrators and members of its team.
type requestAccess struct{ view, retry bool }

func (s *Server) accessTo(r *http.Request, c caller, rec store.RequestRecord) (requestAccess, error) {
	if c.isAdmin() {
		return requestAccess{view: true, retry: true}, nil
	}
	role, err := s.d.Store.TenantRole(r.Context(), rec.Tenant, c.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return requestAccess{}, err
	}
	member := role != ""
	return requestAccess{view: member || rec.UserID == c.ID, retry: member}, nil
}

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request, c caller) {
	uid := c.ID
	if c.isAdmin() && r.URL.Query().Get("all") == "true" {
		uid = ""
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	status := r.URL.Query().Get("status")
	if status != "" && !validStatus[status] {
		writeErr(w, http.StatusBadRequest, "unknown status")
		return
	}
	list, err := s.d.Store.ListRequests(r.Context(), uid, status, limit)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if list == nil {
		list = []store.RequestRecord{}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	writeJSON(w, http.StatusOK, s.withPlans(r, list...))
}

func (s *Server) getRequest(w http.ResponseWriter, r *http.Request, c caller) {
	rec, err := s.d.Store.GetRequest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	acc, err := s.accessTo(r, c, rec)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !acc.view { // do not reveal that other teams' requests exist
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, s.withPlans(r, rec)[0])
}

func (s *Server) retryRequest(w http.ResponseWriter, r *http.Request, c caller) {
	if !s.limiters.allow(c.ID) {
		w.Header().Set("Retry-After", "6")
		writeErr(w, http.StatusTooManyRequests, "too many requests, slow down")
		return
	}
	rec, err := s.d.Store.GetRequest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	acc, err := s.accessTo(r, c, rec)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !acc.view {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if !acc.retry { // saw it as its requester but is no longer in the team
		s.storeError(w, store.ErrNotMember)
		return
	}
	if err := s.d.Store.RetryRequest(r.Context(), rec.ID, c.Email); err != nil {
		s.storeError(w, err)
		return
	}
	rec, err = s.d.Store.GetRequest(r.Context(), rec.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s.withPlans(r, rec)[0])
}
