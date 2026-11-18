// Package gitea is the GitSource driver for Gitea (and compatible forks such
// as Forgejo).
//
// Mapping of the generic concepts:
//
//	GroupSpec        -> organization (Gitea has no nested groups, so
//	                    GroupSpec.Parent is ignored)
//	RepoSpec         -> repository inside that organization
//	VariableSpec     -> Actions variable, or Actions secret when Masked/File
//	                    (Gitea has no environment scoping or file variables;
//	                    Scope is ignored and File values are stored as-is)
//
// Ownership: repositories Plinth creates carry the topic "managed-by-plinth".
// Plinth only populates, writes secrets into, or deletes repositories that
// carry it, unless adopt_existing is set.
package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getplinth/plinth/internal/provider"
)

const managedTopic = "managed-by-plinth"

func init() {
	provider.Register(provider.KindGitSource, "gitea", func(cfg map[string]string) (provider.Provider, error) {
		return FromConfig(cfg)
	})
}

// Driver implements provider.GitSource for one Gitea instance.
type Driver struct {
	c     *client
	caPEM string
	adopt bool
}

var _ provider.GitSource = (*Driver)(nil)

// FromConfig builds a driver from an integration's decrypted configuration.
//
//	base_url         instance URL, https (required)
//	token            access token with write:organization, write:repository (required)
//	ca_cert          optional PEM CA bundle for a private CA
//	allow_http       "true" permits an http:// base_url (local testing only)
//	adopt_existing   "true" takes over repositories created before Plinth
func FromConfig(cfg map[string]string) (*Driver, error) {
	if cfg["base_url"] == "" || cfg["token"] == "" {
		return nil, errors.New(`gitea: config "base_url" and "token" are required`)
	}
	u, err := url.Parse(cfg["base_url"])
	if err != nil || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("gitea: invalid base_url %q", cfg["base_url"])
	}
	allowHTTP, _ := strconv.ParseBool(cfg["allow_http"])
	if u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP) {
		return nil, errors.New("gitea: base_url must be https (set allow_http only for local testing)")
	}
	c, err := newClient(u, cfg["token"], cfg["ca_cert"])
	if err != nil {
		return nil, err
	}
	d := &Driver{c: c, caPEM: cfg["ca_cert"]}
	d.adopt, _ = strconv.ParseBool(cfg["adopt_existing"])
	return d, nil
}

func (*Driver) Kind() provider.Kind { return provider.KindGitSource }
func (*Driver) Type() string        { return "gitea" }

// Validate checks the token works and the server is reachable.
func (d *Driver) Validate(ctx context.Context) error {
	var v struct {
		Version string `json:"version"`
	}
	if err := d.c.do(ctx, "GET", "/version", nil, &v); err != nil {
		return err
	}
	var me struct {
		Login string `json:"login"`
	}
	if err := d.c.do(ctx, "GET", "/user", nil, &me); err != nil {
		return fmt.Errorf("gitea: token rejected: %w", err)
	}
	return nil
}

var (
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,99}$`)
	varRe  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,99}$`)
)

func checkNames(org, repo string) error {
	if len(org) > 40 || !nameRe.MatchString(org) {
		return fmt.Errorf("gitea: invalid organization name %q", org)
	}
	if repo != "" && !nameRe.MatchString(repo) {
		return fmt.Errorf("gitea: invalid repository name %q", repo)
	}
	return nil
}

// EnsureGroup creates the organization if missing. Outputs: name, created.
func (d *Driver) EnsureGroup(ctx context.Context, spec provider.GroupSpec) (provider.Outputs, error) {
	if err := checkNames(spec.Name, ""); err != nil {
		return nil, err
	}
	err := d.c.do(ctx, "GET", "/orgs/"+spec.Name, nil, nil)
	switch {
	case err == nil:
		return provider.Outputs{"name": spec.Name, "created": "false"}, nil
	case !isStatus(err, 404):
		return nil, err
	}
	err = d.c.do(ctx, "POST", "/orgs", map[string]any{"username": spec.Name, "visibility": "private"}, nil)
	if err != nil && !isStatus(err, 422) { // 422: created concurrently
		return nil, fmt.Errorf("gitea: create organization %q: %w", spec.Name, err)
	}
	return provider.Outputs{"name": spec.Name, "created": strconv.FormatBool(err == nil)}, nil
}

type repoInfo struct {
	Empty         bool   `json:"empty"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
	FullName      string `json:"full_name"`
}

func (d *Driver) isManaged(ctx context.Context, org, repo string) (bool, error) {
	var t struct {
		Topics []string `json:"topics"`
	}
	if err := d.c.do(ctx, "GET", "/repos/"+org+"/"+repo+"/topics", nil, &t); err != nil {
		return false, err
	}
	for _, x := range t.Topics {
		if x == managedTopic {
			return true, nil
		}
	}
	return false, nil
}

func (d *Driver) mark(ctx context.Context, org, repo string) error {
	return d.c.do(ctx, "PUT", "/repos/"+org+"/"+repo+"/topics/"+managedTopic, nil, nil)
}

// requireManaged enforces the ownership rule for operations on an existing repo.
func (d *Driver) requireManaged(ctx context.Context, org, repo, what string) error {
	ok, err := d.isManaged(ctx, org, repo)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("gitea: refusing to %s %s/%s: repository is not managed by Plinth", what, org, repo)
	}
	return nil
}

// EnsureRepository creates the repository and, while it is still empty,
// populates it from the template (all branches and tags). An existing repo
// that already has content is never modified. Outputs: created, populated,
// full_name, html_url, clone_url, default_branch.
func (d *Driver) EnsureRepository(ctx context.Context, spec provider.RepoSpec) (provider.Outputs, error) {
	org, name := spec.Group.Name, spec.Name
	if err := checkNames(org, name); err != nil {
		return nil, err
	}
	if spec.TemplateRepo != "" { // fail on bad input before creating anything
		if _, _, err := templateURL(d.c.base, spec.TemplateRepo); err != nil {
			return nil, err
		}
	}
	for _, b := range spec.ExtraBranches {
		if !nameRe.MatchString(b) {
			return nil, fmt.Errorf("gitea: invalid branch name %q", b)
		}
	}

	var info repoInfo
	created := false
	path := "/repos/" + org + "/" + name
	err := d.c.do(ctx, "GET", path, nil, &info)
	switch {
	case isStatus(err, 404):
		body := map[string]any{
			"name": name, "description": spec.Description,
			"private": spec.Private, "auto_init": false, "default_branch": "main",
		}
		if err := d.c.do(ctx, "POST", "/orgs/"+org+"/repos", body, &info); err != nil {
			return nil, fmt.Errorf("gitea: create repository %s/%s: %w", org, name, err)
		}
		if err := d.mark(ctx, org, name); err != nil {
			return nil, err
		}
		created = true
	case err != nil:
		return nil, err
	default:
		managed, err := d.isManaged(ctx, org, name)
		if err != nil {
			return nil, err
		}
		if !managed {
			if !d.adopt {
				return nil, fmt.Errorf("gitea: repository %s/%s exists but is not managed by Plinth (set adopt_existing to take it over)", org, name)
			}
			if err := d.mark(ctx, org, name); err != nil {
				return nil, err
			}
		}
	}

	populated := false
	if info.Empty && spec.TemplateRepo != "" {
		head, err := d.populate(ctx, spec.TemplateRepo, info.CloneURL, spec.TemplateVars)
		if err != nil {
			return nil, err
		}
		if head != "" && head != info.DefaultBranch {
			if err := d.c.do(ctx, "PATCH", path, map[string]any{"default_branch": head}, nil); err != nil {
				return nil, fmt.Errorf("gitea: set default branch: %w", err)
			}
			info.DefaultBranch = head
		}
		// Gitea updates its "empty" flag asynchronously after a push; wait for
		// it, or the branch calls below fail with "repository is empty".
		if err := d.waitNotEmpty(ctx, path, &info); err != nil {
			return nil, err
		}
		populated = true
	}

	if !info.Empty {
		for _, b := range spec.ExtraBranches {
			err := d.c.do(ctx, "GET", path+"/branches/"+b, nil, nil)
			if err == nil {
				continue
			}
			if !isStatus(err, 404) {
				return nil, err
			}
			if err := d.c.do(ctx, "POST", path+"/branches",
				map[string]any{"new_branch_name": b, "old_branch_name": info.DefaultBranch}, nil); err != nil {
				return nil, fmt.Errorf("gitea: create branch %q: %w", b, err)
			}
		}
	}

	return provider.Outputs{
		"created": strconv.FormatBool(created), "populated": strconv.FormatBool(populated),
		"full_name": info.FullName, "html_url": info.HTMLURL, "clone_url": info.CloneURL,
		"default_branch": info.DefaultBranch,
	}, nil
}

// waitNotEmpty polls until the repository reports content (up to 30s).
func (d *Driver) waitNotEmpty(ctx context.Context, path string, info *repoInfo) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		var cur repoInfo
		if err := d.c.do(ctx, "GET", path, nil, &cur); err != nil {
			return err
		}
		if !cur.Empty {
			info.Empty, info.DefaultBranch = false, cur.DefaultBranch
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("gitea: repository still reports empty 30s after the push")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// SetVariable upserts an Actions variable, or an Actions secret when the
// value is Masked or File. Only Plinth-managed repositories are written to.
func (d *Driver) SetVariable(ctx context.Context, repo provider.RepoSpec, v provider.VariableSpec) error {
	org, name := repo.Group.Name, repo.Name
	if err := checkNames(org, name); err != nil {
		return err
	}
	if !varRe.MatchString(v.Key) || strings.HasPrefix(v.Key, "GITEA_") || strings.HasPrefix(v.Key, "GITHUB_") {
		return fmt.Errorf("gitea: invalid variable name %q (A-Z, 0-9, _ ; no GITEA_/GITHUB_ prefix)", v.Key)
	}
	if v.Value == "" {
		return fmt.Errorf("gitea: variable %q has an empty value", v.Key)
	}
	if err := d.requireManaged(ctx, org, name, "write variables to"); err != nil {
		return err
	}
	base := "/repos/" + org + "/" + name + "/actions"
	if v.Masked || v.File {
		return d.wrap(v.Key, d.c.do(ctx, "PUT", base+"/secrets/"+v.Key, map[string]string{"data": v.Value}, nil))
	}
	err := d.c.do(ctx, "POST", base+"/variables/"+v.Key, map[string]string{"value": v.Value}, nil)
	if isStatus(err, 409) {
		err = d.c.do(ctx, "PUT", base+"/variables/"+v.Key, map[string]string{"name": v.Key, "value": v.Value}, nil)
	}
	return d.wrap(v.Key, err)
}

// wrap adds the variable name (never the value) to an API error.
func (d *Driver) wrap(key string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("gitea: set variable %q: %w", key, err)
}

// DeleteRepository deletes a Plinth-managed repository. Idempotent.
func (d *Driver) DeleteRepository(ctx context.Context, spec provider.RepoSpec) error {
	org, name := spec.Group.Name, spec.Name
	if err := checkNames(org, name); err != nil {
		return err
	}
	managed, err := d.isManaged(ctx, org, name)
	if isStatus(err, 404) {
		return nil
	}
	if err != nil {
		return err
	}
	if !managed {
		return fmt.Errorf("gitea: refusing to delete %s/%s: repository is not managed by Plinth", org, name)
	}
	if err := d.c.do(ctx, "DELETE", "/repos/"+org+"/"+name, nil, nil); err != nil && !isStatus(err, 404) {
		return fmt.Errorf("gitea: delete %s/%s: %w", org, name, err)
	}
	return nil
}
