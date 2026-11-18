package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/request"
	"github.com/getplinth/plinth/internal/store"
)

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	kinds  = map[string]bool{"gitsource": true, "registry": true, "cluster": true, "database": true}
)

const (
	maxConfigKeys  = 50
	maxConfigValue = 16 << 10
)

// storeError maps store and validation errors to HTTP responses. Unknown
// errors are logged and reported generically so internals do not leak.
func (s *Server) storeError(w http.ResponseWriter, err error) {
	var taken store.ErrNamespaceTaken
	var badRef store.ErrBadReference
	var verrs request.Errors
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrForbidden):
		writeErr(w, http.StatusForbidden, "this tenant belongs to another user")
	case errors.Is(err, store.ErrInUse):
		writeErr(w, http.StatusConflict, "still referenced by other objects")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "already exists or the object is in a different state")
	case errors.As(err, &taken):
		writeErr(w, http.StatusConflict, taken.Error())
	case errors.As(err, &badRef):
		writeErr(w, http.StatusUnprocessableEntity, badRef.Error())
	case errors.As(err, &verrs):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "details": []string(verrs)})
	default:
		s.d.Log.Error("internal error", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) audit(ctx context.Context, c caller, action, target string, detail any) {
	if err := s.d.Store.Audit(ctx, c.Email, action, target, detail); err != nil {
		s.d.Log.Error("audit write failed", "action", action, "err", err)
	}
}

// ---- integrations ----------------------------------------------------------------------

type integrationBody struct {
	Kind   string            `json:"kind"`
	Driver string            `json:"driver"`
	Name   string            `json:"name"`
	Config map[string]string `json:"config"`
}

func checkConfig(cfg map[string]string) string {
	if len(cfg) == 0 {
		return "config is required"
	}
	if len(cfg) > maxConfigKeys {
		return "config has too many keys"
	}
	for k, v := range cfg {
		if k == "" || len(k) > 100 || len(v) > maxConfigValue {
			return "config key or value is too large"
		}
	}
	return ""
}

// tryBuild constructs the driver without contacting the system, to catch
// unknown drivers and malformed configuration at the time they are entered.
func tryBuild(kind, driver string, cfg map[string]string) error {
	_, err := provider.New(provider.Kind(kind), driver, cfg)
	return err
}

func (s *Server) listIntegrations(w http.ResponseWriter, r *http.Request, c caller) {
	list, err := s.d.Store.ListIntegrations(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	if list == nil {
		list = []store.Integration{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) getIntegration(w http.ResponseWriter, r *http.Request, c caller) {
	in, err := s.d.Store.GetIntegration(r.Context(), r.PathValue("name"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (s *Server) createIntegration(w http.ResponseWriter, r *http.Request, c caller) {
	var b integrationBody
	if !readJSON(w, r, &b) {
		return
	}
	switch {
	case !nameRe.MatchString(b.Name):
		writeErr(w, http.StatusBadRequest, "name must be lowercase letters, digits and dashes")
		return
	case !kinds[b.Kind]:
		writeErr(w, http.StatusBadRequest, "kind must be one of gitsource, registry, cluster, database")
		return
	}
	if msg := checkConfig(b.Config); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if err := tryBuild(b.Kind, b.Driver, b.Config); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := s.d.Store.CreateIntegration(r.Context(), b.Kind, b.Driver, b.Name, b.Config)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "integration.create", in.Name, map[string]string{"kind": in.Kind, "driver": in.Driver})
	writeJSON(w, http.StatusCreated, in)
}

func (s *Server) replaceIntegration(w http.ResponseWriter, r *http.Request, c caller) {
	var b struct {
		Config map[string]string `json:"config"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if msg := checkConfig(b.Config); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	cur, err := s.d.Store.GetIntegration(r.Context(), r.PathValue("name"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := tryBuild(cur.Kind, cur.Driver, b.Config); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	in, err := s.d.Store.ReplaceIntegrationConfig(r.Context(), cur.Name, b.Config)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "integration.update", in.Name, map[string]any{"config_keys": in.ConfigKeys})
	writeJSON(w, http.StatusOK, in)
}

func (s *Server) deleteIntegration(w http.ResponseWriter, r *http.Request, c caller) {
	name := r.PathValue("name")
	if err := s.d.Store.DeleteIntegration(r.Context(), name); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r.Context(), c, "integration.delete", name, nil)
	w.WriteHeader(http.StatusNoContent)
}

// validateIntegration contacts the external system with the stored
// configuration. The response carries only ok/error: drivers never put
// credentials in their errors, and the config is never returned.
func (s *Server) validateIntegration(w http.ResponseWriter, r *http.Request, c caller) {
	in, cfg, err := s.d.Store.IntegrationConfig(r.Context(), r.PathValue("name"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res := map[string]any{"ok": true}
	p, err := provider.New(provider.Kind(in.Kind), in.Driver, cfg)
	if err == nil {
		err = p.Validate(ctx)
	}
	if err != nil {
		res = map[string]any{"ok": false, "error": err.Error()}
	}
	s.audit(r.Context(), c, "integration.validate", in.Name, map[string]any{"ok": res["ok"]})
	writeJSON(w, http.StatusOK, res)
}

// ---- audit ------------------------------------------------------------------------------

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, c caller) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.d.Store.ListAudit(r.Context(), limit)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if list == nil {
		list = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, list)
}

// listDrivers lists the drivers compiled into this server, so the UI can
// offer them when an administrator adds an integration.
func (s *Server) listDrivers(w http.ResponseWriter, r *http.Request, c caller) {
	writeJSON(w, http.StatusOK, provider.Registered())
}
