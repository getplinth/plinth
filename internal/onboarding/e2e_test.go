//go:build e2e

package onboarding_test

// End-to-end: a complete request through the REAL drivers (Kubernetes x2,
// PostgreSQL, Gitea, Harbor) against the disposable stack that
// scripts/e2e.sh starts. Run it with `make e2e`.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getplinth/plinth/internal/onboarding"
	"github.com/getplinth/plinth/internal/orchestrator"
	"github.com/getplinth/plinth/internal/provider"
	_ "github.com/getplinth/plinth/internal/provider/gitea"
	_ "github.com/getplinth/plinth/internal/provider/harbor"
	_ "github.com/getplinth/plinth/internal/provider/kubernetes"
	_ "github.com/getplinth/plinth/internal/provider/postgres"
	"github.com/getplinth/plinth/internal/quota"
	"github.com/getplinth/plinth/internal/request"
)

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("%s not set: run scripts/e2e.sh", k)
	}
	return v
}

func mustNew[T provider.Provider](t *testing.T, kind provider.Kind, driver string, cfg map[string]string) T {
	t.Helper()
	p, err := provider.New(kind, driver, cfg)
	if err != nil {
		t.Fatalf("%s/%s: %v", kind, driver, err)
	}
	v, ok := p.(T)
	if !ok {
		t.Fatalf("%s/%s has the wrong type %T", kind, driver, p)
	}
	if err := p.Validate(context.Background()); err != nil {
		t.Fatalf("%s/%s Validate: %v", kind, driver, err)
	}
	return v
}

func k8sConfig(t *testing.T, up string) map[string]string {
	return map[string]string{
		"server":  env(t, "E2E_K8S_"+up+"_SERVER"),
		"ca_cert": env(t, "E2E_K8S_"+up+"_CA"),
		"token":   env(t, "E2E_K8S_"+up+"_TOKEN"),
	}
}

func adminClient(t *testing.T, up string) k8s.Interface {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", env(t, "E2E_K8S_"+up+"_ADMIN_KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := k8s.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// flakyCluster fails the first n pull-secret writes into one namespace, like
// an API server that times out half-way through a credential delivery.
type flakyCluster struct {
	provider.Cluster
	mu        sync.Mutex
	namespace string
	failLeft  int
}

func (f *flakyCluster) EnsureSecret(ctx context.Context, s provider.SecretSpec) error {
	f.mu.Lock()
	fail := s.Namespace == f.namespace && s.Name == "regcred" && f.failLeft > 0
	if fail {
		f.failLeft--
	}
	f.mu.Unlock()
	if fail {
		return fmt.Errorf("injected failure: api server timeout")
	}
	return f.Cluster.EnsureSecret(ctx, s)
}

type memSaver struct {
	mu    sync.Mutex
	snaps []string
}

func (m *memSaver) Save(_ context.Context, _ string, s *orchestrator.State) error {
	b, _ := json.Marshal(s)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps = append(m.snaps, string(b))
	return nil
}

// ---- thin Gitea helper for setup and verification ---------------------------------

type gitea struct {
	t           *testing.T
	base, token string
}

func (g gitea) call(method, path string, body any) (int, []byte) {
	g.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, g.base+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "token "+g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (g gitea) mustOK(method, path string, body any, out any) {
	g.t.Helper()
	code, b := g.call(method, path, body)
	if code >= 300 {
		g.t.Fatalf("gitea %s %s: %d %s", method, path, code, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			g.t.Fatal(err)
		}
	}
}

func (g gitea) raw(repo, ref, file string) string {
	g.t.Helper()
	code, b := g.call("GET", fmt.Sprintf("/repos/%s/raw/%s?ref=%s", repo, file, ref), nil)
	if code != 200 {
		g.t.Fatalf("raw %s@%s:%s -> %d", repo, ref, file, code)
	}
	return string(b)
}

// ---- Harbor token scope check ---------------------------------------------------------

// harborActions asks Harbor what a robot may do on a repository. Wrong
// credentials give 200 with no actions, so judge by the returned actions.
func harborActions(t *testing.T, base, user, pass, repo string) []string {
	t.Helper()
	u := fmt.Sprintf("%s/service/token?service=harbor-registry&scope=repository:%s:pull,push,delete", base, repo)
	req, _ := http.NewRequest("GET", u, nil)
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(body.Token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Access []struct {
			Name    string   `json:"name"`
			Actions []string `json:"actions"`
		} `json:"access"`
	}
	_ = json.Unmarshal(payload, &claims)
	var out []string
	for _, a := range claims.Access {
		if a.Name == repo {
			out = append(out, a.Actions...)
		}
	}
	return out
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// registryLogin reads the delivered pull secret and returns its user and password.
func registryLogin(t *testing.T, cs k8s.Interface, ns string) (user, pass string) {
	t.Helper()
	s, err := cs.CoreV1().Secrets(ns).Get(context.Background(), "regcred", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("regcred in %s: %v", ns, err)
	}
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(s.Data[".dockerconfigjson"], &cfg); err != nil || len(cfg.Auths) != 1 {
		t.Fatalf("bad docker config in %s: %v", ns, err)
	}
	for _, a := range cfg.Auths {
		raw, _ := base64.StdEncoding.DecodeString(a.Auth)
		user, pass, _ = strings.Cut(string(raw), ":")
	}
	return
}

func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	giteaURL, giteaToken := env(t, "E2E_GITEA_URL"), env(t, "E2E_GITEA_TOKEN")
	harborURL := env(t, "E2E_HARBOR_URL")
	g := gitea{t: t, base: giteaURL, token: giteaToken}

	// ---- the platform: real systems behind the generic interfaces ----------------
	git := mustNew[provider.GitSource](t, provider.KindGitSource, "gitea",
		map[string]string{"base_url": giteaURL, "token": giteaToken, "allow_http": "true"})
	reg := mustNew[provider.Registry](t, provider.KindRegistry, "harbor",
		map[string]string{"base_url": harborURL, "username": env(t, "E2E_HARBOR_USER"), "password": env(t, "E2E_HARBOR_PASSWORD"), "allow_http": "true"})
	devCluster := mustNew[provider.Cluster](t, provider.KindCluster, "kubernetes", k8sConfig(t, "DEV"))
	prodCluster := mustNew[provider.Cluster](t, provider.KindCluster, "kubernetes", k8sConfig(t, "PROD"))
	db := mustNew[provider.Database](t, provider.KindDatabase, "postgres", map[string]string{
		"host": env(t, "E2E_PG_HOST"), "port": env(t, "E2E_PG_PORT"),
		"admin_user": "postgres", "admin_password": env(t, "E2E_PG_ADMIN_PASSWORD"), "sslmode": "disable",
	})
	flaky := &flakyCluster{Cluster: prodCluster, namespace: "team-a-blog", failLeft: 1}

	// Golden template with a placeholder and a second branch.
	g.mustOK("POST", "/orgs", map[string]any{"username": "platform"}, nil)
	g.mustOK("POST", "/orgs/platform/repos", map[string]any{"name": "app-template", "auto_init": true, "default_branch": "main"}, nil)
	g.mustOK("POST", "/repos/platform/app-template/contents/k8s/deployment.yaml", map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte("name: PROJECT_NAME\n")), "message": "add", "branch": "main"}, nil)

	bp := onboarding.Blueprint{
		Git: git, GitParent: "tenants", TemplateRepo: "platform/app-template",
		TemplateVars:  func(p string) map[string]string { return map[string]string{"PROJECT_NAME": p} },
		ExtraBranches: []string{"develop", "staging"},
		Registry:      reg, Tiers: quota.Default(),
		Envs: []onboarding.Environment{
			{Name: "dev", Cluster: devCluster, Database: db, AccessPrefix: "oidc:", IngressNS: "kube-system"},
			{Name: "prod", Cluster: flaky, AccessPrefix: "oidc:", IngressNS: "kube-system"},
		},
	}
	req, err := request.Parse([]byte(`{"tenant":"Team-A","projects":[
	  {"name":"shop","quota":"silver","use_db":"true","db_type":"postgresql","host_url":"https://shop.example.com","application_port":"8080"},
	  {"name":"blog"}]}`))
	if err != nil {
		t.Fatal(err)
	}

	// ---- run 1: first onboarding, with one injected delivery failure -----------------
	st, saver := orchestrator.NewState(), &memSaver{}
	opt := orchestrator.Options{MaxAttempts: 3, Backoff: 200 * time.Millisecond}
	if err := onboarding.Execute(ctx, "e2e-1", bp, req, st, saver, opt); err != nil {
		t.Fatalf("request failed: %v\nsteps: %+v", err, st.Steps)
	}
	t.Logf("run 1 finished: %d steps", len(st.Steps))

	dev, prod := adminClient(t, "DEV"), adminClient(t, "PROD")

	t.Run("git: organization, populated private repositories, variables", func(t *testing.T) {
		for _, p := range []string{"shop", "blog"} {
			var r struct {
				Private bool `json:"private"`
				Empty   bool `json:"empty"`
			}
			g.mustOK("GET", "/repos/team-a/"+p, nil, &r)
			if !r.Private || r.Empty {
				t.Errorf("%s: %+v", p, r)
			}
			for _, b := range []string{"develop", "staging"} {
				if code, _ := g.call("GET", "/repos/team-a/"+p+"/branches/"+b, nil); code != 200 {
					t.Errorf("%s: branch %s missing", p, b)
				}
			}
			if body := g.raw("team-a/"+p, "main", "k8s/deployment.yaml"); body != "name: "+p+"\n" {
				t.Errorf("%s: template placeholder not replaced: %q", p, body)
			}
		}
		var host struct {
			Data string `json:"data"`
		}
		g.mustOK("GET", "/repos/team-a/shop/actions/variables/HOST", nil, &host)
		if host.Data != "shop.example.com" {
			t.Errorf("HOST = %q", host.Data)
		}
		var secrets []struct {
			Name string `json:"name"`
		}
		g.mustOK("GET", "/repos/team-a/shop/actions/secrets", nil, &secrets)
		got := map[string]bool{}
		for _, s := range secrets {
			got[s.Name] = true
		}
		for _, want := range []string{"REGISTRY_PASSWORD", "DOCKER_AUTH", "DB_PASS_DEV"} {
			if !got[want] {
				t.Errorf("shop is missing secret %s (have %v)", want, got)
			}
		}
		g.mustOK("GET", "/repos/team-a/blog/actions/secrets", nil, &secrets)
		for _, s := range secrets {
			if strings.HasPrefix(s.Name, "DB_") {
				t.Errorf("blog asked for no database but has %s", s.Name)
			}
		}
	})

	t.Run("kubernetes: namespaces, quota tier and isolation in both clusters", func(t *testing.T) {
		for name, cs := range map[string]k8s.Interface{"dev": dev, "prod": prod} {
			for p, pods := range map[string]int64{"shop": 10, "blog": 5} {
				ns := "team-a-" + p
				rq, err := cs.CoreV1().ResourceQuotas(ns).Get(ctx, ns+"-quota", metav1.GetOptions{})
				if err != nil {
					t.Fatalf("%s/%s: %v", name, ns, err)
				}
				if got := rq.Spec.Hard.Pods().Value(); got != pods {
					t.Errorf("%s/%s pods quota = %d, want %d", name, ns, got, pods)
				}
				if _, err := cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, ns+"-isolation", metav1.GetOptions{}); err != nil {
					t.Errorf("%s/%s: %v", name, ns, err)
				}
			}
		}
		if _, err := prod.CoreV1().Secrets("team-a-shop").Get(ctx, "db-credentials", metav1.GetOptions{}); err == nil {
			t.Error("prod has no database server, so it must not get a db secret")
		}
	})

	var shopReg, shopDBPass string
	t.Run("registry: one credential per project, scoped to its own project", func(t *testing.T) {
		user, pass := registryLogin(t, dev, "team-a-shop")
		shopReg = pass
		if got := harborActions(t, harborURL, user, pass, "team-a-shop/app"); !has(got, "push") || !has(got, "pull") || has(got, "delete") {
			t.Errorf("shop robot on its own project: %v", got)
		}
		if got := harborActions(t, harborURL, user, pass, "team-a-blog/app"); has(got, "push") || has(got, "pull") {
			t.Errorf("shop robot reaches blog's project: %v", got)
		}
		// Same credential in every environment.
		if u2, p2 := registryLogin(t, prod, "team-a-shop"); u2 != user || p2 != pass {
			t.Error("dev and prod hold different registry credentials")
		}
	})

	t.Run("retry after a failed delivery rotates and leaves every environment consistent", func(t *testing.T) {
		step := "registry/credential/blog"
		if a := st.Steps[step].Attempts; a != 2 {
			t.Fatalf("expected the injected failure to cost one retry, attempts = %d", a)
		}
		du, dp := registryLogin(t, dev, "team-a-blog")
		pu, pp := registryLogin(t, prod, "team-a-blog")
		if du != pu || dp != pp {
			t.Fatalf("dev and prod disagree after the retry")
		}
		if got := harborActions(t, harborURL, du, dp, "team-a-blog/app"); !has(got, "push") {
			t.Errorf("the delivered credential does not work: %v", got)
		}
	})

	t.Run("database: tenant role works, is isolated, secret delivered", func(t *testing.T) {
		s, err := dev.CoreV1().Secrets("team-a-shop").Get(ctx, "db-credentials", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		shopDBPass = string(s.Data["db-password"])
		connect := func(dbname string) (*pgx.Conn, error) {
			cfg, _ := pgx.ParseConfig("")
			cfg.Host, cfg.Database, cfg.User, cfg.Password = string(s.Data["db-host"]), dbname, string(s.Data["db-user"]), shopDBPass
			fmt.Sscan(string(s.Data["db-port"]), &cfg.Port)
			cfg.TLSConfig, cfg.Fallbacks = nil, nil
			return pgx.ConnectConfig(ctx, cfg)
		}
		c, err := connect(string(s.Data["db-name"]))
		if err != nil {
			t.Fatalf("tenant cannot log in with the delivered credential: %v", err)
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "CREATE TABLE smoke (id int)"); err != nil {
			t.Errorf("tenant cannot use its database: %v", err)
		}
		// Not a privileged role. (Per-tenant isolation between databases is
		// covered by the Postgres driver tests. PostgreSQL itself lets every
		// role connect to the shared maintenance database; Plinth does not
		// change that, see docs/drivers/postgres.md.)
		var super, createDB, createRole bool
		if err := c.QueryRow(ctx, "SELECT rolsuper, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname = current_user").
			Scan(&super, &createDB, &createRole); err != nil || super || createDB || createRole {
			t.Errorf("tenant role is privileged: super=%v createdb=%v createrole=%v err=%v", super, createDB, createRole, err)
		}
		if string(s.Data["db-name"]) != "team-a_shop_dev" {
			t.Errorf("db name = %q", s.Data["db-name"])
		}
	})

	t.Run("no secret ever reached the persisted request state", func(t *testing.T) {
		secrets := []string{shopReg, shopDBPass}
		for _, snap := range append(saver.snaps, func() string { b, _ := json.Marshal(st); return string(b) }()) {
			for _, s := range secrets {
				if s != "" && strings.Contains(snap, s) {
					t.Fatalf("a secret leaked into persisted state: %.200s", snap)
				}
			}
		}
	})

	// ---- run 2: the same request again must change nothing ---------------------------
	t.Run("submitting the same request again is a no-op", func(t *testing.T) {
		st2 := orchestrator.NewState()
		if err := onboarding.Execute(ctx, "e2e-2", bp, req, st2, &memSaver{}, opt); err != nil {
			t.Fatalf("second run failed: %v", err)
		}
		for _, k := range []string{"registry/credential/shop.issued", "registry/credential/blog.issued", "database/shop/dev.issued"} {
			if st2.Outputs[k] != "false" {
				t.Errorf("%s = %q: a re-run must not issue new credentials", k, st2.Outputs[k])
			}
		}
		if _, pass := registryLogin(t, dev, "team-a-shop"); pass != shopReg {
			t.Error("the shop registry credential changed on re-run")
		}
		s, _ := dev.CoreV1().Secrets("team-a-shop").Get(ctx, "db-credentials", metav1.GetOptions{})
		if string(s.Data["db-password"]) != shopDBPass {
			t.Error("the shop database password changed on re-run")
		}
	})
}
