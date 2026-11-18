// Package onboarding turns a validated request plus a blueprint into the
// ordered, idempotent steps that provision everything, and delivers the
// credentials those steps create.
//
// Per project the steps run in dependency order:
//
//	git/group (once per tenant)
//	git/repo/<project>            repository, populated from the template
//	git/vars/<project>            HOST / PORT variables
//	registry/repo/<project>
//	cluster/<project>/<env>       namespace, RBAC, quota, isolation (per env)
//	registry/credential/<project> issue credential, deliver everywhere
//	database/<project>/<env>      database + role, deliver everywhere (per env)
//
// Credential handling: a provider returns a secret at most once. The step that
// receives it delivers it to every destination before it finishes and never
// stores it in the request state. If delivery fails the secret would be lost,
// so any retry of a credential-issuing step rotates (orchestrator.Attempt > 1)
// and delivers the fresh secret again.
package onboarding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/getplinth/plinth/internal/naming"
	"github.com/getplinth/plinth/internal/orchestrator"
	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/quota"
	"github.com/getplinth/plinth/internal/request"
)

// Variable and secret names used in the repositories and namespaces.
const (
	VarHost             = "HOST"
	VarPort             = "PORT"
	VarRegistryUser     = "REGISTRY_USER"
	VarRegistryPassword = "REGISTRY_PASSWORD"
	DockerConfigKey     = ".dockerconfigjson"
)

// Environment is one deployment target: a cluster and, optionally, a
// database server.
type Environment struct {
	Name         string // e.g. "dev"; upper-cased in variable names
	Cluster      provider.Cluster
	Database     provider.Database // nil: no databases in this environment
	AccessPrefix string            // OIDC group prefix on that cluster
	IngressNS    string            // where the ingress controller runs
}

// Blueprint is the resolved combination of systems a request is built on.
type Blueprint struct {
	Git           provider.GitSource
	GitParent     string // org / parent group for tenants
	TemplateRepo  string
	TemplateVars  func(project string) map[string]string // optional
	ExtraBranches []string

	Registry provider.Registry // nil: no registry
	Tiers    quota.Tiers
	Envs     []Environment
}

// dbAliases maps names developers use to the driver type.
var dbAliases = map[string]string{"postgresql": "postgres", "pg": "postgres"}

func canonicalDBType(t string) string {
	if c, ok := dbAliases[t]; ok {
		return c
	}
	return t
}

// Shape is what request validation needs to know about a blueprint, without
// live drivers: the quota tiers and, per environment, the database driver type
// ("" when the environment has no database server).
type Shape struct {
	Tiers        quota.Tiers
	EnvNames     []string // parallel to EnvDBDrivers
	EnvDBDrivers []string
	HasRegistry  bool
}

func (b Blueprint) shape() Shape {
	sh := Shape{Tiers: b.Tiers, HasRegistry: b.Registry != nil}
	for _, e := range b.Envs {
		sh.EnvNames = append(sh.EnvNames, e.Name)
		d := ""
		if e.Database != nil {
			d = e.Database.Type()
		}
		sh.EnvDBDrivers = append(sh.EnvDBDrivers, d)
	}
	return sh
}

// acceptedDBTypes lists the db_type values requests may use.
func (sh Shape) acceptedDBTypes() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, t := range sh.EnvDBDrivers {
		if t == "" {
			continue
		}
		add(t)
		for alias, canon := range dbAliases {
			if canon == t {
				add(alias)
			}
		}
	}
	return out
}

// ValidateRequest checks a request against a blueprint's shape: the generic
// request rules plus quota tiers and whether a requested database type is
// offered by at least one environment.
func ValidateRequest(sh Shape, req request.Request) error {
	if len(sh.EnvDBDrivers) == 0 {
		return fmt.Errorf("onboarding: blueprint has no environments")
	}
	if err := req.Validate(request.Options{Tiers: sh.Tiers, DBTypes: sh.acceptedDBTypes()}); err != nil {
		return err
	}
	var errs request.Errors
	for _, p := range req.Projects {
		if !p.UseDB {
			continue
		}
		want := canonicalDBType(p.DBType)
		ok := false
		for _, d := range sh.EnvDBDrivers {
			if d == want {
				ok = true
			}
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("project %q wants a %s database but no environment offers one", p.Name, p.DBType))
		}
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// StepNames lists, in execution order, the step names a request will have
// against a blueprint of this shape, without contacting any system. It must
// match Plan exactly (a test enforces that); the UI uses it to show steps that
// have not started yet.
func StepNames(sh Shape, req request.Request) []string {
	names := []string{orchestrator.StepName("git", "group")}
	for _, p := range req.Projects {
		n := p.Normalized()
		names = append(names, orchestrator.StepName("git", "repo", n), orchestrator.StepName("git", "vars", n))
		if sh.HasRegistry {
			names = append(names, orchestrator.StepName("registry", "repo", n))
		}
		for _, env := range sh.EnvNames {
			names = append(names, orchestrator.StepName("cluster", n, env))
		}
		if sh.HasRegistry {
			names = append(names, orchestrator.StepName("registry", "credential", n))
		}
		if p.UseDB {
			want := canonicalDBType(p.DBType)
			for i, env := range sh.EnvNames {
				if i < len(sh.EnvDBDrivers) && sh.EnvDBDrivers[i] == want {
					names = append(names, orchestrator.StepName("database", n, env))
				}
			}
		}
	}
	return names
}

// Plan validates the request against the blueprint and returns its steps.
func Plan(bp Blueprint, req request.Request) ([]orchestrator.Step, error) {
	if bp.Git == nil {
		return nil, fmt.Errorf("onboarding: blueprint has no git source")
	}
	if err := ValidateRequest(bp.shape(), req); err != nil {
		return nil, err
	}

	tenant := naming.Normalize(req.Tenant)
	group := provider.GroupSpec{Name: tenant, Parent: bp.GitParent}
	steps := []orchestrator.Step{{
		Name: orchestrator.StepName("git", "group"),
		Run: func(ctx context.Context, st *orchestrator.State) error {
			out, err := bp.Git.EnsureGroup(ctx, group)
			record(st, "git/group", out)
			return err
		},
	}}
	for _, p := range req.Projects {
		steps = append(steps, projectSteps(bp, tenant, group, p)...)
	}
	return steps, nil
}

// Execute plans and runs a request.
func Execute(ctx context.Context, requestID string, bp Blueprint, req request.Request,
	st *orchestrator.State, saver orchestrator.Saver, opt orchestrator.Options) error {
	steps, err := Plan(bp, req)
	if err != nil {
		return err
	}
	return orchestrator.Run(ctx, requestID, steps, st, saver, opt)
}

func projectSteps(bp Blueprint, tenant string, group provider.GroupSpec, p request.Project) []orchestrator.Step {
	name := p.Normalized()
	ns := naming.Namespace(tenant, name)
	repoName := naming.RegistryRepo(tenant, name)
	repo := provider.RepoSpec{
		Group: group, Name: name, Description: p.Description,
		Private:       !bool(p.Public),
		TemplateRepo:  bp.TemplateRepo,
		ExtraBranches: bp.ExtraBranches,
	}
	if bp.TemplateVars != nil {
		repo.TemplateVars = bp.TemplateVars(name)
	}

	var steps []orchestrator.Step
	add := func(stepName string, run func(context.Context, *orchestrator.State) error) {
		steps = append(steps, orchestrator.Step{Name: stepName, Run: run})
	}

	repoStep := orchestrator.StepName("git", "repo", name)
	add(repoStep, func(ctx context.Context, st *orchestrator.State) error {
		out, err := bp.Git.EnsureRepository(ctx, repo)
		record(st, repoStep, out)
		return err
	})

	varsStep := orchestrator.StepName("git", "vars", name)
	add(varsStep, func(ctx context.Context, st *orchestrator.State) error {
		if h := p.Host(); h != "" {
			if err := bp.Git.SetVariable(ctx, repo, provider.VariableSpec{Key: VarHost, Value: h}); err != nil {
				return err
			}
		}
		if p.Port != 0 {
			return bp.Git.SetVariable(ctx, repo, provider.VariableSpec{Key: VarPort, Value: strconv.Itoa(int(p.Port))})
		}
		return nil
	})

	if bp.Registry != nil {
		regStep := orchestrator.StepName("registry", "repo", name)
		add(regStep, func(ctx context.Context, st *orchestrator.State) error {
			out, err := bp.Registry.EnsureRepository(ctx, provider.RegistrySpec{Repo: repoName})
			record(st, regStep, out)
			return err
		})
	}

	for _, env := range bp.Envs {
		env := env
		step := orchestrator.StepName("cluster", name, env.Name)
		add(step, func(ctx context.Context, st *orchestrator.State) error {
			out, err := env.Cluster.EnsureNamespace(ctx, provider.NamespaceSpec{
				Name:             ns,
				Quota:            bp.Tiers[p.QuotaTier()],
				Labels:           map[string]string{"plinth/tenant": tenant, "plinth/project": name},
				AccessGroups:     []string{naming.RBACGroup(env.AccessPrefix, bp.GitParent, tenant)},
				IngressNamespace: env.IngressNS,
			})
			record(st, step, out)
			return err
		})
	}

	if bp.Registry != nil {
		step := orchestrator.StepName("registry", "credential", name)
		add(step, func(ctx context.Context, st *orchestrator.State) error {
			is, err := bp.Registry.EnsureCredential(ctx, provider.RegistrySpec{
				Repo: repoName, Rotate: orchestrator.Attempt(st, step) > 1,
			})
			if err != nil {
				return err
			}
			record(st, step, is.Outputs)
			if is.Credential == nil {
				st.Outputs[step+".issued"] = "false"
				return nil
			}
			st.Outputs[step+".issued"] = "true"
			return deliverRegistry(ctx, bp, repo, ns, is.Credential)
		})
	}

	if p.UseDB {
		want := canonicalDBType(p.DBType)
		for _, env := range bp.Envs {
			env := env
			if env.Database == nil || env.Database.Type() != want {
				continue
			}
			step := orchestrator.StepName("database", name, env.Name)
			dbName := naming.DBName(tenant, name, env.Name)
			add(step, func(ctx context.Context, st *orchestrator.State) error {
				is, err := env.Database.EnsureDatabase(ctx, provider.DatabaseSpec{
					Name: dbName, Rotate: orchestrator.Attempt(st, step) > 1,
				})
				if err != nil {
					return err
				}
				record(st, step, is.Outputs)
				if is.Credential == nil {
					st.Outputs[step+".issued"] = "false"
					return nil
				}
				st.Outputs[step+".issued"] = "true"
				return deliverDatabase(ctx, bp, repo, ns, env, is.Credential)
			})
		}
	}
	return steps
}

// record stores non-secret outputs in the request state under "<step>.<key>".
func record(st *orchestrator.State, step string, out provider.Outputs) {
	for k, v := range out {
		st.Outputs[step+"."+k] = v
	}
}

// ---- credential delivery ----------------------------------------------------

// dockerConfig builds the docker client config for a registry credential.
func dockerConfig(c provider.Credential) []byte {
	auth := base64.StdEncoding.EncodeToString([]byte(c["username"] + ":" + c["password"]))
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{c["server"]: map[string]string{"auth": auth}}})
	return b
}

// deliverRegistry puts a registry credential where it is consumed: CI
// variables in the repository, and a pull secret in every environment.
func deliverRegistry(ctx context.Context, bp Blueprint, repo provider.RepoSpec, ns string, c provider.Credential) error {
	cfg := dockerConfig(c)
	vars := []provider.VariableSpec{
		{Key: VarRegistryUser, Value: c["username"], Masked: true},
		{Key: VarRegistryPassword, Value: c["password"], Masked: true},
		// Same shape as the legacy DOCKER_AUTH variable: base64 of the docker config.
		{Key: naming.CIDockerAuthVar, Value: base64.StdEncoding.EncodeToString(cfg), Masked: true, File: true},
	}
	for _, v := range vars {
		if err := bp.Git.SetVariable(ctx, repo, v); err != nil {
			return fmt.Errorf("deliver %s: %w", v.Key, err)
		}
	}
	for _, env := range bp.Envs {
		err := env.Cluster.EnsureSecret(ctx, provider.SecretSpec{
			Namespace: ns, Name: naming.RegistrySecret, Type: "kubernetes.io/dockerconfigjson",
			Data: map[string][]byte{DockerConfigKey: cfg},
		})
		if err != nil {
			return fmt.Errorf("deliver pull secret to %s: %w", env.Name, err)
		}
	}
	return nil
}

// deliverDatabase puts a database credential into the repository (scoped to
// the environment) and into that environment's namespace.
func deliverDatabase(ctx context.Context, bp Blueprint, repo provider.RepoSpec, ns string, env Environment, c provider.Credential) error {
	e := strings.ToUpper(env.Name)
	vars := []provider.VariableSpec{
		{Key: naming.CIVarDB("host", e), Value: c["host"], Scope: e},
		{Key: naming.CIVarDB("port", e), Value: c["port"], Scope: e},
		{Key: naming.CIVarDB("name", e), Value: c["name"], Scope: e},
		{Key: naming.CIVarDB("user", e), Value: c["user"], Scope: e},
		{Key: naming.CIVarDB("pass", e), Value: c["password"], Scope: e, Masked: true},
	}
	for _, v := range vars {
		if err := bp.Git.SetVariable(ctx, repo, v); err != nil {
			return fmt.Errorf("deliver %s: %w", v.Key, err)
		}
	}
	err := env.Cluster.EnsureSecret(ctx, provider.SecretSpec{
		Namespace: ns, Name: naming.DBSecret,
		Data: map[string][]byte{
			"db-host": []byte(c["host"]), "db-port": []byte(c["port"]), "db-name": []byte(c["name"]),
			"db-user": []byte(c["user"]), "db-password": []byte(c["password"]),
		},
	})
	if err != nil {
		return fmt.Errorf("deliver database secret to %s: %w", env.Name, err)
	}
	return nil
}
