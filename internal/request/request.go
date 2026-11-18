// Package request defines the onboarding request a developer submits and
// validates it. Rules are ported from the legacy validator, with a few
// deliberate tightenings (marked "stricter").
package request

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/getplinth/plinth/internal/naming"
	"github.com/getplinth/plinth/internal/quota"
)

// Request onboards one tenant (team) with one or more projects. The
// environments to provision in come from the blueprint, not the request.
type Request struct {
	Tenant   string    `json:"tenant"`
	Projects []Project `json:"projects"`
}

type Project struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Public      FlexBool `json:"public,omitempty"`
	HostURL     string   `json:"host_url,omitempty"`
	Port        FlexInt  `json:"application_port,omitempty"`
	Quota       string   `json:"quota,omitempty"` // default quota.DefaultTier
	UseDB       FlexBool `json:"use_db,omitempty"`
	DBType      string   `json:"db_type,omitempty"`
}

// Normalized returns the project name in its canonical form.
func (p Project) Normalized() string { return naming.Normalize(p.Name) }

// QuotaTier returns the effective tier name.
func (p Project) QuotaTier() string {
	if p.Quota == "" {
		return quota.DefaultTier
	}
	return p.Quota
}

// Host returns HostURL without protocol, path or port (legacy stripped only the protocol).
func (p Project) Host() string { return stripScheme(p.HostURL) }

// Parse decodes a request, rejecting unknown fields.
func Parse(b []byte) (Request, error) {
	var r Request
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("invalid request JSON: %w", err)
	}
	return r, nil
}

// Options carries what validation needs from the platform configuration.
type Options struct {
	Tiers   quota.Tiers
	DBTypes []string // database drivers available in the chosen blueprint
}

// Errors is a list of validation problems.
type Errors []string

func (e Errors) Error() string { return strings.Join(e, "; ") }

var hostLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate checks a request. It returns nil or Errors.
func (r Request) Validate(opt Options) error {
	var errs Errors
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	tenant := naming.Normalize(r.Tenant)
	if tenant == "" {
		add("tenant is required")
	} else if !naming.IsDNSLabel(tenant) { // stricter: tenant becomes part of a namespace name
		add("tenant %q must be a valid DNS label after normalization (got %q)", r.Tenant, tenant)
	}
	if len(r.Projects) == 0 {
		add("at least one project is required")
	}

	seen := map[string]int{}
	for i, p := range r.Projects {
		if p.Name == "" {
			add("project at index %d is missing a name", i)
			continue
		}
		n := p.Normalized()
		switch {
		case n == "":
			add("project %q has an invalid name after normalization", p.Name)
		case !naming.IsDNSLabel(n): // stricter: legacy allowed '_', which Kubernetes rejects
			add("project %q must be a valid DNS label after normalization (got %q)", p.Name, n)
		case tenant != "" && !naming.IsDNSLabel(naming.Namespace(tenant, n)):
			add("project %q: namespace %q exceeds %d characters", p.Name, naming.Namespace(tenant, n), naming.MaxNamespaceLen)
		}
		if n != "" {
			if prev, dup := seen[n]; dup { // stricter: legacy silently re-processed duplicates
				add("project %q duplicates project at index %d", p.Name, prev)
			}
			seen[n] = i
		}

		if _, ok := opt.Tiers[p.QuotaTier()]; !ok {
			add("project %q has invalid quota tier %q (valid: %s)", p.Name, p.QuotaTier(), strings.Join(opt.Tiers.Names(), ", "))
		}

		if p.UseDB {
			switch {
			case p.DBType == "":
				add("project %q requested a database but no db_type", p.Name)
			case !contains(opt.DBTypes, p.DBType):
				add("project %q has unsupported db_type %q (supported: %s)", p.Name, p.DBType, strings.Join(opt.DBTypes, ", "))
			}
		}

		if p.HostURL != "" && !validHost(p.Host()) { // stricter: legacy never checked
			add("project %q has an invalid host_url %q", p.Name, p.HostURL)
		}
		if p.Port != 0 && (p.Port < 1 || p.Port > 65535) {
			add("project %q has an invalid application_port %d", p.Name, p.Port)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func stripScheme(u string) string {
	u = strings.TrimSpace(u)
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	return u
}

func validHost(h string) bool {
	if h == "" || len(h) > 253 || net.ParseIP(h) != nil {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if len(l) > 63 || !hostLabel.MatchString(l) {
			return false
		}
	}
	return true
}

// FlexBool accepts true/false and the strings "true"/"false" (the legacy
// trigger payload sent strings).
type FlexBool bool

func (b *FlexBool) UnmarshalJSON(d []byte) error {
	s := strings.Trim(strings.ToLower(string(d)), `"`)
	switch s {
	case "true":
		*b = true
	case "false", "", "null":
		*b = false
	default:
		return fmt.Errorf("invalid boolean %s", d)
	}
	return nil
}

// FlexInt accepts numbers and numeric strings.
type FlexInt int

func (i *FlexInt) UnmarshalJSON(d []byte) error {
	s := strings.Trim(string(d), `"`)
	if s == "" || s == "null" {
		*i = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("invalid integer %s", d)
	}
	*i = FlexInt(n)
	return nil
}
