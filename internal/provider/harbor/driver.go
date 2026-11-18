// Package harbor is the Registry driver for Harbor (https://goharbor.io).
//
// Mapping of the generic concepts:
//
//	RegistrySpec.Repo  -> a Harbor project (a private namespace); images are
//	                      pushed as <host>/<project>/<image>
//	credential         -> a project-level robot account limited to
//	                      push + pull on that project's repositories
//
// Ownership: Harbor projects carry no free-form metadata, so a project counts
// as Plinth-managed when it was created by the identity Plinth authenticates
// as. Use a dedicated account (for example a user named "plinth") for this.
// Projects created by anyone else are refused unless adopt_existing is set.
package harbor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/getplinth/plinth/internal/provider"
)

func init() {
	provider.Register(provider.KindRegistry, "harbor", func(cfg map[string]string) (provider.Provider, error) {
		return FromConfig(cfg)
	})
}

// Driver implements provider.Registry for one Harbor instance.
type Driver struct {
	base       *url.URL
	user, pass string
	hc         *http.Client
	robotName  string
	ttlDays    int
	adopt      bool
}

var _ provider.Registry = (*Driver)(nil)

// FromConfig builds a driver from an integration's decrypted configuration.
//
//	base_url         Harbor URL, https (required)
//	username         account Plinth acts as (required); needs permission to
//	                 create projects and robot accounts
//	password         (required)
//	ca_cert          optional PEM CA bundle for a private CA
//	allow_http       "true" permits http:// (local testing only)
//	robot_name       name of the per-project robot, default "ci"
//	robot_ttl_days   robot lifetime in days, default 0 = never expires
//	adopt_existing   "true" takes over projects created by someone else
func FromConfig(cfg map[string]string) (*Driver, error) {
	for _, k := range []string{"base_url", "username", "password"} {
		if cfg[k] == "" {
			return nil, fmt.Errorf("harbor: config %q is required", k)
		}
	}
	u, err := url.Parse(cfg["base_url"])
	if err != nil || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("harbor: invalid base_url %q", cfg["base_url"])
	}
	allowHTTP, _ := strconv.ParseBool(cfg["allow_http"])
	if u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP) {
		return nil, errors.New("harbor: base_url must be https (set allow_http only for local testing)")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if ca := cfg["ca_cert"]; ca != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, errors.New("harbor: ca_cert is not valid PEM")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	d := &Driver{
		base: u, user: cfg["username"], pass: cfg["password"],
		hc:        &http.Client{Transport: tr, Timeout: 30 * time.Second},
		robotName: valueOr(cfg["robot_name"], "ci"),
	}
	if !robotNameRe.MatchString(d.robotName) {
		return nil, fmt.Errorf("harbor: invalid robot_name %q", d.robotName)
	}
	if v := cfg["robot_ttl_days"]; v != "" {
		if d.ttlDays, err = strconv.Atoi(v); err != nil || d.ttlDays < 0 {
			return nil, fmt.Errorf("harbor: invalid robot_ttl_days %q", v)
		}
	}
	d.adopt, _ = strconv.ParseBool(cfg["adopt_existing"])
	return d, nil
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (*Driver) Kind() provider.Kind { return provider.KindRegistry }
func (*Driver) Type() string        { return "harbor" }

var (
	projectRe   = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)
	robotNameRe = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*$`)
)

func checkProject(name string) error {
	if len(name) > 255 || !projectRe.MatchString(name) {
		return fmt.Errorf("harbor: invalid project name %q", name)
	}
	return nil
}

// ---- HTTP -------------------------------------------------------------------

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("harbor API %d: %s", e.Status, e.Message) }

func isStatus(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == code
}

func (d *Driver) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := *d.base
	rawPath, rawQuery, _ := strings.Cut(path, "?")
	u.Path = strings.TrimRight(d.base.Path, "/") + "/api/v2.0" + rawPath
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.SetBasicAuth(d.user, d.pass)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return fmt.Errorf("harbor: %s %s: %s", method, rawPath, strings.ReplaceAll(err.Error(), d.pass, "***"))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var m struct {
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &m) == nil && len(m.Errors) > 0 {
			msg = m.Errors[0].Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &apiError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ---- Provider ---------------------------------------------------------------

// Validate checks that the credentials work.
func (d *Driver) Validate(ctx context.Context) error {
	var me struct {
		Username string `json:"username"`
	}
	if err := d.do(ctx, "GET", "/users/current", nil, &me); err != nil {
		return fmt.Errorf("harbor: credentials rejected: %w", err)
	}
	return nil
}

type project struct {
	ID        int    `json:"project_id"`
	Name      string `json:"name"`
	OwnerName string `json:"owner_name"`
}

func (d *Driver) getProject(ctx context.Context, name string) (*project, error) {
	var p project
	if err := d.do(ctx, "GET", "/projects/"+url.PathEscape(name), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// managed reports whether the project was created by the identity Plinth uses.
func (d *Driver) managed(p *project) bool { return p.OwnerName == d.user }

func (d *Driver) outputs(p *project, created bool) provider.Outputs {
	return provider.Outputs{
		"project": p.Name, "project_id": strconv.Itoa(p.ID),
		"server":       d.base.Host,
		"image_prefix": d.base.Host + "/" + p.Name,
		"created":      strconv.FormatBool(created),
	}
}

// EnsureRepository creates the private project if it does not exist.
// Outputs: project, project_id, server, image_prefix, created.
func (d *Driver) EnsureRepository(ctx context.Context, spec provider.RegistrySpec) (provider.Outputs, error) {
	if err := checkProject(spec.Repo); err != nil {
		return nil, err
	}
	p, err := d.getProject(ctx, spec.Repo)
	created := false
	switch {
	case isStatus(err, 404):
		err := d.do(ctx, "POST", "/projects", map[string]any{
			"project_name": spec.Repo, "metadata": map[string]string{"public": "false"},
		}, nil)
		if err != nil && !isStatus(err, 409) { // 409: created concurrently
			return nil, fmt.Errorf("harbor: create project %q: %w", spec.Repo, err)
		}
		if p, err = d.getProject(ctx, spec.Repo); err != nil {
			return nil, err
		}
		created = true
	case err != nil:
		return nil, err
	case !d.managed(p) && !d.adopt:
		return nil, fmt.Errorf("harbor: project %q exists but was not created by %q (set adopt_existing to take it over)", spec.Repo, d.user)
	}
	return d.outputs(p, created), nil
}

type robot struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Disable     bool   `json:"disable"`
	Permissions []struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Access    []struct {
			Resource string `json:"resource"`
			Action   string `json:"action"`
			Effect   string `json:"effect"`
		} `json:"access"`
	} `json:"permissions"`
}

// onlyPushPull reports whether the robot has exactly push+pull on the project
// and nothing else. Anything broader (or a disabled robot) is replaced.
func onlyPushPull(r *robot, project string) bool {
	if r.Disable || len(r.Permissions) != 1 {
		return false
	}
	p := r.Permissions[0]
	if p.Kind != "project" || p.Namespace != project {
		return false
	}
	var got []string
	for _, a := range p.Access {
		if a.Resource != "repository" || (a.Effect != "" && a.Effect != "allow") {
			return false
		}
		got = append(got, a.Action)
	}
	sort.Strings(got)
	return len(got) == 2 && got[0] == "pull" && got[1] == "push"
}

func (d *Driver) findRobot(ctx context.Context, p *project) (*robot, error) {
	q := url.QueryEscape(fmt.Sprintf("Level=project,ProjectID=%d", p.ID))
	var list []robot
	if err := d.do(ctx, "GET", "/robots?page_size=100&q="+q, nil, &list); err != nil {
		return nil, err
	}
	full := "robot$" + p.Name + "+" + d.robotName
	for i := range list {
		if list[i].Name == full {
			// The list view omits permissions on some versions; fetch the detail.
			var detail robot
			if err := d.do(ctx, "GET", "/robots/"+strconv.Itoa(list[i].ID), nil, &detail); err != nil {
				return nil, err
			}
			return &detail, nil
		}
	}
	return nil, nil
}

// EnsureCredential issues a robot account limited to push+pull on the
// project. The secret exists only in the returned Credential (keys: server,
// username, password) and only when newly created, rotated, or when an
// existing robot was missing, disabled or over-privileged and had to be replaced.
func (d *Driver) EnsureCredential(ctx context.Context, spec provider.RegistrySpec) (provider.Issued, error) {
	var zero provider.Issued
	if err := checkProject(spec.Repo); err != nil {
		return zero, err
	}
	p, err := d.getProject(ctx, spec.Repo)
	if err != nil {
		return zero, fmt.Errorf("harbor: project %q: %w", spec.Repo, err)
	}
	if !d.managed(p) && !d.adopt {
		return zero, fmt.Errorf("harbor: refusing to issue credentials for %q: project is not managed by Plinth", spec.Repo)
	}
	issued := provider.Issued{Outputs: d.outputs(p, false)}

	existing, err := d.findRobot(ctx, p)
	if err != nil {
		return zero, err
	}
	if existing != nil {
		if !spec.Rotate && onlyPushPull(existing, p.Name) {
			issued.Outputs["username"] = existing.Name
			return issued, nil
		}
		if err := d.do(ctx, "DELETE", "/robots/"+strconv.Itoa(existing.ID), nil, nil); err != nil && !isStatus(err, 404) {
			return zero, fmt.Errorf("harbor: replace robot: %w", err)
		}
	}

	duration := -1 // never expires
	if d.ttlDays > 0 {
		duration = d.ttlDays
	}
	var created struct {
		Name   string `json:"name"`
		Secret string `json:"secret"`
	}
	err = d.do(ctx, "POST", "/robots", map[string]any{
		"name": d.robotName, "description": "Managed by Plinth",
		"duration": duration, "level": "project", "disable": false,
		"permissions": []map[string]any{{
			"kind": "project", "namespace": p.Name,
			"access": []map[string]string{
				{"resource": "repository", "action": "push"},
				{"resource": "repository", "action": "pull"},
			},
		}},
	}, &created)
	if err != nil {
		return zero, fmt.Errorf("harbor: create robot for %q: %w", p.Name, err)
	}
	if created.Secret == "" {
		return zero, errors.New("harbor: robot created without a secret")
	}
	issued.Outputs["username"] = created.Name
	issued.Credential = provider.Credential{
		"server": d.base.Host, "username": created.Name, "password": created.Secret,
	}
	return issued, nil
}

// DeleteRepository deletes a Plinth-managed project. Harbor refuses while the
// project still contains images; that is surfaced as an error, not forced.
func (d *Driver) DeleteRepository(ctx context.Context, spec provider.RegistrySpec) error {
	if err := checkProject(spec.Repo); err != nil {
		return err
	}
	p, err := d.getProject(ctx, spec.Repo)
	if isStatus(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if !d.managed(p) {
		return fmt.Errorf("harbor: refusing to delete %q: project is not managed by Plinth", spec.Repo)
	}
	if err := d.do(ctx, "DELETE", "/projects/"+url.PathEscape(spec.Repo), nil, nil); err != nil && !isStatus(err, 404) {
		return fmt.Errorf("harbor: delete project %q: %w", spec.Repo, err)
	}
	return nil
}
