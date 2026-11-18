package onboarding

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getplinth/plinth/internal/orchestrator"
	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/quota"
	"github.com/getplinth/plinth/internal/request"
)

// ---- fakes -------------------------------------------------------------------

type base struct {
	kind provider.Kind
	typ  string
}

func (b base) Kind() provider.Kind            { return b.kind }
func (b base) Type() string                   { return b.typ }
func (b base) Validate(context.Context) error { return nil }

type fakeGit struct {
	base
	mu     sync.Mutex
	groups []string
	repos  []string
	calls  map[string]int
	vars   map[string]provider.VariableSpec // "repo/KEY/scope"
}

func newGit() *fakeGit {
	return &fakeGit{base: base{provider.KindGitSource, "fake"}, calls: map[string]int{}, vars: map[string]provider.VariableSpec{}}
}
func (g *fakeGit) EnsureGroup(_ context.Context, s provider.GroupSpec) (provider.Outputs, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls["group"]++
	g.groups = append(g.groups, s.Name)
	return provider.Outputs{"name": s.Name}, nil
}
func (g *fakeGit) EnsureRepository(_ context.Context, s provider.RepoSpec) (provider.Outputs, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls["repo/"+s.Name]++
	g.repos = append(g.repos, s.Group.Name+"/"+s.Name)
	return provider.Outputs{"full_name": s.Group.Name + "/" + s.Name}, nil
}
func (g *fakeGit) SetVariable(_ context.Context, r provider.RepoSpec, v provider.VariableSpec) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.vars[r.Name+"/"+v.Key+"/"+v.Scope] = v
	return nil
}
func (g *fakeGit) DeleteRepository(context.Context, provider.RepoSpec) error { return nil }
func (g *fakeGit) v(repo, key, scope string) (provider.VariableSpec, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	v, ok := g.vars[repo+"/"+key+"/"+scope]
	return v, ok
}

type fakeRegistry struct {
	base
	mu      sync.Mutex
	issued  int
	rotates []bool
	exists  map[string]bool
}

func (r *fakeRegistry) EnsureRepository(_ context.Context, s provider.RegistrySpec) (provider.Outputs, error) {
	return provider.Outputs{"project": s.Repo}, nil
}
func (r *fakeRegistry) EnsureCredential(_ context.Context, s provider.RegistrySpec) (provider.Issued, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rotates = append(r.rotates, s.Rotate)
	if r.exists == nil {
		r.exists = map[string]bool{}
	}
	if r.exists[s.Repo] && !s.Rotate {
		return provider.Issued{Outputs: provider.Outputs{"project": s.Repo}}, nil
	}
	r.exists[s.Repo] = true
	r.issued++
	return provider.Issued{
		Outputs:    provider.Outputs{"project": s.Repo},
		Credential: provider.Credential{"server": "reg.example", "username": "robot$" + s.Repo + "+ci", "password": fmt.Sprintf("reg-secret-%d", r.issued)},
	}, nil
}
func (r *fakeRegistry) DeleteRepository(context.Context, provider.RegistrySpec) error { return nil }

type fakeCluster struct {
	base
	mu          sync.Mutex
	name        string
	ns          map[string]provider.NamespaceSpec
	secrets     map[string]provider.SecretSpec // "ns/name"
	nsCalls     int
	failNS      bool
	failSecrets int // number of EnsureSecret calls that fail
}

func newCluster(name string) *fakeCluster {
	return &fakeCluster{base: base{provider.KindCluster, "fake"}, name: name,
		ns: map[string]provider.NamespaceSpec{}, secrets: map[string]provider.SecretSpec{}}
}
func (c *fakeCluster) EnsureNamespace(_ context.Context, s provider.NamespaceSpec) (provider.Outputs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nsCalls++
	if c.failNS {
		return nil, errors.New("cluster unreachable")
	}
	c.ns[s.Name] = s
	return provider.Outputs{"namespace": s.Name}, nil
}
func (c *fakeCluster) EnsureSecret(_ context.Context, s provider.SecretSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failSecrets > 0 {
		c.failSecrets--
		return errors.New("api server timeout")
	}
	c.secrets[s.Namespace+"/"+s.Name] = s
	return nil
}
func (c *fakeCluster) DeleteNamespace(context.Context, string) error { return nil }

type fakeDB struct {
	base
	mu      sync.Mutex
	issued  int
	rotates []bool
	exists  map[string]bool
}

func (d *fakeDB) EnsureDatabase(_ context.Context, s provider.DatabaseSpec) (provider.Issued, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rotates = append(d.rotates, s.Rotate)
	out := provider.Outputs{"name": s.Name}
	if d.exists == nil {
		d.exists = map[string]bool{}
	}
	if d.exists[s.Name] && !s.Rotate {
		return provider.Issued{Outputs: out}, nil
	}
	d.exists[s.Name] = true
	d.issued++
	return provider.Issued{Outputs: out, Credential: provider.Credential{
		"host": "db.example", "port": "5432", "name": s.Name, "user": s.Name, "password": fmt.Sprintf("db-secret-%d", d.issued),
	}}, nil
}
func (d *fakeDB) DeleteDatabase(context.Context, provider.DatabaseSpec) error { return nil }

type memSaver struct {
	mu    sync.Mutex
	saved []string // JSON snapshots
}

func (m *memSaver) Save(_ context.Context, _ string, s *orchestrator.State) error {
	b, _ := json.Marshal(s)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved = append(m.saved, string(b))
	return nil
}

// ---- fixtures ------------------------------------------------------------------

type rig struct {
	git       *fakeGit
	reg       *fakeRegistry
	dev, prod *fakeCluster
	devDB     *fakeDB
	bp        Blueprint
}

func newRig() *rig {
	r := &rig{
		git:   newGit(),
		reg:   &fakeRegistry{base: base{provider.KindRegistry, "fake"}},
		dev:   newCluster("dev"),
		prod:  newCluster("prod"),
		devDB: &fakeDB{base: base{provider.KindDatabase, "postgres"}},
	}
	r.bp = Blueprint{
		Git: r.git, GitParent: "tenants", TemplateRepo: "platform/app-template", ExtraBranches: []string{"develop"},
		Registry: r.reg, Tiers: quota.Default(),
		Envs: []Environment{
			{Name: "dev", Cluster: r.dev, Database: r.devDB, AccessPrefix: "oidc:", IngressNS: "ingress"},
			{Name: "prod", Cluster: r.prod, AccessPrefix: "oidc:", IngressNS: "ingress"},
		},
	}
	return r
}

func parse(t *testing.T, s string) request.Request {
	t.Helper()
	r, err := request.Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const shopReq = `{"tenant":"Team-A","projects":[
  {"name":"shop","quota":"silver","use_db":"true","db_type":"postgresql","host_url":"https://shop.example.com","application_port":"8080"}]}`

var fast = orchestrator.Options{MaxAttempts: 3, Backoff: time.Millisecond}

func names(steps []orchestrator.Step) []string {
	var n []string
	for _, s := range steps {
		n = append(n, s.Name)
	}
	return n
}

// ---- tests ---------------------------------------------------------------------

func TestPlanOrderAndNames(t *testing.T) {
	r := newRig()
	steps, err := Plan(r.bp, parse(t, shopReq))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"git/group", "git/repo/shop", "git/vars/shop", "registry/repo/shop",
		"cluster/shop/dev", "cluster/shop/prod",
		"registry/credential/shop", "database/shop/dev",
	}
	got := names(steps)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps:\n got %v\nwant %v", got, want)
	}
}

func TestHappyPathProvisionsAndDeliversEverywhere(t *testing.T) {
	r := newRig()
	st, sv := orchestrator.NewState(), &memSaver{}
	if err := Execute(context.Background(), "req1", r.bp, parse(t, shopReq), st, sv, fast); err != nil {
		t.Fatal(err)
	}

	if len(r.git.groups) != 1 || r.git.groups[0] != "team-a" || r.git.repos[0] != "team-a/shop" {
		t.Fatalf("git: groups=%v repos=%v", r.git.groups, r.git.repos)
	}
	// HOST has the protocol stripped; PORT is set.
	if v, ok := r.git.v("shop", "HOST", ""); !ok || v.Value != "shop.example.com" {
		t.Errorf("HOST: %+v", v)
	}
	if v, ok := r.git.v("shop", "PORT", ""); !ok || v.Value != "8080" {
		t.Errorf("PORT: %+v", v)
	}

	// Namespace in both clusters, quota from the request, access group, same name.
	for _, c := range []*fakeCluster{r.dev, r.prod} {
		ns, ok := c.ns["team-a-shop"]
		if !ok {
			t.Fatalf("namespace missing in %s", c.name)
		}
		if ns.Quota.Pods != 10 || len(ns.AccessGroups) != 1 || ns.AccessGroups[0] != "oidc:tenants/team-a" {
			t.Errorf("%s namespace spec: %+v", c.name, ns)
		}
	}

	// Registry credential: masked CI variables + a pull secret in EVERY environment.
	pw, _ := r.git.v("shop", "REGISTRY_PASSWORD", "")
	if pw.Value != "reg-secret-1" || !pw.Masked {
		t.Errorf("REGISTRY_PASSWORD: %+v", pw)
	}
	da, _ := r.git.v("shop", "DOCKER_AUTH", "")
	raw, _ := base64.StdEncoding.DecodeString(da.Value)
	if !da.File || !da.Masked || !strings.Contains(string(raw), `"reg.example"`) {
		t.Errorf("DOCKER_AUTH: %+v / %s", da, raw)
	}
	for _, c := range []*fakeCluster{r.dev, r.prod} {
		s, ok := c.secrets["team-a-shop/regcred"]
		if !ok || s.Type != "kubernetes.io/dockerconfigjson" || !strings.Contains(string(s.Data[DockerConfigKey]), "reg.example") {
			t.Errorf("pull secret in %s: %+v", c.name, s)
		}
	}

	// Database: env-scoped variables (password masked) and a secret only where the DB lives.
	if v, ok := r.git.v("shop", "DB_PASS_DEV", "DEV"); !ok || v.Value != "db-secret-1" || !v.Masked {
		t.Errorf("DB_PASS_DEV: %+v", v)
	}
	if v, _ := r.git.v("shop", "DB_NAME_DEV", "DEV"); v.Value != "team-a_shop_dev" {
		t.Errorf("DB_NAME_DEV: %+v", v)
	}
	if s, ok := r.dev.secrets["team-a-shop/db-credentials"]; !ok || string(s.Data["db-password"]) != "db-secret-1" {
		t.Errorf("db secret in dev: %+v", s)
	}
	if _, ok := r.prod.secrets["team-a-shop/db-credentials"]; ok {
		t.Error("prod has no database, so it must not get a db secret")
	}
}

func TestSecretsNeverEnterTheRequestState(t *testing.T) {
	r := newRig()
	st, sv := orchestrator.NewState(), &memSaver{}
	if err := Execute(context.Background(), "req1", r.bp, parse(t, shopReq), st, sv, fast); err != nil {
		t.Fatal(err)
	}
	if len(sv.saved) == 0 {
		t.Fatal("state was never saved")
	}
	for _, snap := range append(sv.saved, mustJSON(st)) {
		for _, secret := range []string{"reg-secret", "db-secret"} {
			if strings.Contains(snap, secret) {
				t.Fatalf("a secret leaked into persisted state:\n%s", snap)
			}
		}
	}
	if st.Outputs["registry/credential/shop.issued"] != "true" || st.Outputs["cluster/shop/dev.namespace"] != "team-a-shop" {
		t.Errorf("outputs: %v", st.Outputs)
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestFailedDeliveryIsRetriedWithRotation(t *testing.T) {
	r := newRig()
	r.prod.failSecrets = 1 // first pull-secret write to prod fails after dev already got secret #1
	st := orchestrator.NewState()
	if err := Execute(context.Background(), "req1", r.bp, parse(t, shopReq), st, &memSaver{}, fast); err != nil {
		t.Fatal(err)
	}
	// Attempt 1 issued secret 1 and lost it; attempt 2 must rotate and redeliver secret 2 EVERYWHERE.
	if got := r.reg.rotates; len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("rotate flags = %v, want [false true]", got)
	}
	for _, c := range []*fakeCluster{r.dev, r.prod} {
		if s := string(c.secrets["team-a-shop/regcred"].Data[DockerConfigKey]); !strings.Contains(s, base64.StdEncoding.EncodeToString([]byte("robot$team-a-shop+ci:reg-secret-2"))) {
			t.Errorf("%s still holds a stale credential: %s", c.name, s)
		}
	}
	if v, _ := r.git.v("shop", "REGISTRY_PASSWORD", ""); v.Value != "reg-secret-2" {
		t.Errorf("CI variable not updated to the rotated secret: %+v", v)
	}
}

func TestResumeSkipsFinishedStepsAndDoesNotRotateAgain(t *testing.T) {
	r := newRig()
	r.prod.failNS = true
	st, sv := orchestrator.NewState(), &memSaver{}
	opt := orchestrator.Options{MaxAttempts: 2, Backoff: time.Millisecond}
	if err := Execute(context.Background(), "req1", r.bp, parse(t, shopReq), st, sv, opt); err == nil {
		t.Fatal("expected the unreachable prod cluster to fail the request")
	}
	if st.Steps["cluster/shop/prod"].Status != orchestrator.Failed || st.Steps["git/repo/shop"].Status != orchestrator.Succeeded {
		t.Fatalf("states: %+v", st.Steps)
	}
	if r.reg.issued != 0 {
		t.Fatal("no credential may be issued before every namespace exists")
	}
	reposBefore := r.git.calls["repo/shop"]

	// The operator fixes the cluster and resumes with the saved state.
	r.prod.failNS = false
	if err := Execute(context.Background(), "req1", r.bp, parse(t, shopReq), st, sv, opt); err != nil {
		t.Fatal(err)
	}
	if r.git.calls["repo/shop"] != reposBefore || r.git.calls["group"] != 1 {
		t.Errorf("finished steps ran again: %v", r.git.calls)
	}
	if len(r.reg.rotates) != 1 || r.reg.rotates[0] {
		t.Errorf("credential step is on its first attempt and must not rotate: %v", r.reg.rotates)
	}
}

func TestMultipleProjectsAreSeparate(t *testing.T) {
	r := newRig()
	two := `{"tenant":"team-a","projects":[{"name":"shop"},{"name":"blog","quota":"gold"}]}`
	st := orchestrator.NewState()
	if err := Execute(context.Background(), "req1", r.bp, parse(t, two), st, &memSaver{}, fast); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.dev.ns["team-a-shop"]; !ok {
		t.Error("shop namespace missing")
	}
	if r.dev.ns["team-a-blog"].Quota.Pods != 15 {
		t.Errorf("blog should use the gold tier: %+v", r.dev.ns["team-a-blog"].Quota)
	}
	if r.git.calls["group"] != 1 {
		t.Errorf("the tenant group must be ensured once, got %d", r.git.calls["group"])
	}
	// Each project has its own credential.
	if r.reg.issued != 2 {
		t.Errorf("credentials issued: %d", r.reg.issued)
	}
	if _, ok := r.git.v("shop", "REGISTRY_PASSWORD", ""); !ok {
		t.Error("shop credential missing")
	}
	if a, _ := r.git.v("shop", "REGISTRY_PASSWORD", ""); a.Value == "" {
		t.Error("empty")
	} else if b, _ := r.git.v("blog", "REGISTRY_PASSWORD", ""); a.Value == b.Value {
		t.Error("projects must not share a credential")
	}
}

func TestPlanRejectsWhatTheBlueprintCannotDo(t *testing.T) {
	r := newRig()
	cases := map[string]string{
		"mysql not offered": `{"tenant":"t","projects":[{"name":"a","use_db":true,"db_type":"mysql"}]}`,
		"unknown quota":     `{"tenant":"t","projects":[{"name":"a","quota":"platinum"}]}`,
		"invalid name":      `{"tenant":"t","projects":[{"name":"a_b"}]}`,
		"no projects":       `{"tenant":"t","projects":[]}`,
	}
	for name, body := range cases {
		if _, err := Plan(r.bp, parse(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	bad := r.bp
	bad.Envs = nil
	if _, err := Plan(bad, parse(t, shopReq)); err == nil {
		t.Error("a blueprint without environments must be rejected")
	}
	bad = r.bp
	bad.Git = nil
	if _, err := Plan(bad, parse(t, shopReq)); err == nil {
		t.Error("a blueprint without a git source must be rejected")
	}
	// Both spellings of the database type are accepted.
	for _, tp := range []string{"postgresql", "postgres"} {
		body := fmt.Sprintf(`{"tenant":"t","projects":[{"name":"a","use_db":true,"db_type":%q}]}`, tp)
		if _, err := Plan(r.bp, parse(t, body)); err != nil {
			t.Errorf("db_type %q should be accepted: %v", tp, err)
		}
	}
}

func TestSkipsRegistryAndDatabaseWhenNotConfiguredOrRequested(t *testing.T) {
	r := newRig()
	r.bp.Registry = nil
	steps, err := Plan(r.bp, parse(t, `{"tenant":"t","projects":[{"name":"a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names(steps) {
		if strings.HasPrefix(n, "registry/") || strings.HasPrefix(n, "database/") {
			t.Errorf("unexpected step %s", n)
		}
	}
}

// The UI shows steps that have not started yet, so StepNames must agree with
// what Plan really produces.
func TestStepNamesMatchPlan(t *testing.T) {
	bodies := []string{
		shopReq,
		`{"tenant":"t","projects":[{"name":"a"},{"name":"b","use_db":true,"db_type":"postgres"}]}`,
	}
	for _, withRegistry := range []bool{true, false} {
		for _, body := range bodies {
			r := newRig()
			if !withRegistry {
				r.bp.Registry = nil
			}
			req := parse(t, body)
			steps, err := Plan(r.bp, req)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Join(names(steps), ",")
			got := strings.Join(StepNames(r.bp.shape(), req), ",")
			if got != want {
				t.Errorf("registry=%v\n StepNames: %s\n Plan:      %s", withRegistry, got, want)
			}
		}
	}
}
