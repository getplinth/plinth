//go:build integration

package api_test

// The whole application on a real PostgreSQL (disposable Docker container):
// store + API + OIDC authentication + background worker, with fake drivers
// standing in for the external systems. The drivers themselves are covered by
// their own integration tests and by `make e2e`.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getplinth/plinth/internal/api"
	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/auth/authtest"
	"github.com/getplinth/plinth/internal/crypto"
	"github.com/getplinth/plinth/internal/orchestrator"
	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/store"
	"github.com/getplinth/plinth/internal/worker"
)

// ---- fake drivers --------------------------------------------------------------------------

const (
	regSecret = "REG-SECRET-do-not-leak"
	dbSecret  = "DB-SECRET-do-not-leak"
)

type fakeWorld struct {
	mu         sync.Mutex
	calls      map[string]int
	failNS     map[string]bool     // namespace -> EnsureNamespace fails
	namespaces map[string][]string // cluster -> namespaces
	issued     map[string]int
}

var world = &fakeWorld{calls: map[string]int{}, failNS: map[string]bool{}, namespaces: map[string][]string{}, issued: map[string]int{}}

func (w *fakeWorld) count(key string) int { w.mu.Lock(); defer w.mu.Unlock(); return w.calls[key] }
func (w *fakeWorld) bump(key string)      { w.mu.Lock(); defer w.mu.Unlock(); w.calls[key]++ }

type base struct {
	kind provider.Kind
	cfg  map[string]string
}

func (b base) Kind() provider.Kind { return b.kind }
func (b base) Type() string        { return "fake" }
func (b base) Validate(context.Context) error {
	if b.cfg["fail_validate"] == "true" {
		return fmt.Errorf("fake system unreachable")
	}
	return nil
}

type fGit struct{ base }

func (g fGit) EnsureGroup(_ context.Context, s provider.GroupSpec) (provider.Outputs, error) {
	world.bump("group/" + s.Name)
	return provider.Outputs{"name": s.Name}, nil
}
func (g fGit) EnsureRepository(_ context.Context, s provider.RepoSpec) (provider.Outputs, error) {
	world.bump("repo/" + s.Group.Name + "/" + s.Name)
	return provider.Outputs{"full_name": s.Group.Name + "/" + s.Name}, nil
}
func (g fGit) SetVariable(context.Context, provider.RepoSpec, provider.VariableSpec) error {
	return nil
}
func (g fGit) DeleteRepository(context.Context, provider.RepoSpec) error { return nil }

type fReg struct{ base }

func (r fReg) EnsureRepository(_ context.Context, s provider.RegistrySpec) (provider.Outputs, error) {
	return provider.Outputs{"project": s.Repo}, nil
}
func (r fReg) EnsureCredential(_ context.Context, s provider.RegistrySpec) (provider.Issued, error) {
	world.mu.Lock()
	defer world.mu.Unlock()
	if world.issued["reg/"+s.Repo] > 0 && !s.Rotate {
		return provider.Issued{Outputs: provider.Outputs{"project": s.Repo}}, nil
	}
	world.issued["reg/"+s.Repo]++
	return provider.Issued{Outputs: provider.Outputs{"project": s.Repo},
		Credential: provider.Credential{"server": "reg.fake", "username": "robot", "password": regSecret}}, nil
}
func (r fReg) DeleteRepository(context.Context, provider.RegistrySpec) error { return nil }

type fCluster struct{ base }

func (c fCluster) EnsureNamespace(_ context.Context, s provider.NamespaceSpec) (provider.Outputs, error) {
	world.mu.Lock()
	defer world.mu.Unlock()
	if world.failNS[s.Name] {
		return nil, fmt.Errorf("cluster %s unreachable", c.cfg["name"])
	}
	world.namespaces[c.cfg["name"]] = append(world.namespaces[c.cfg["name"]], s.Name)
	return provider.Outputs{"namespace": s.Name}, nil
}
func (c fCluster) EnsureSecret(context.Context, provider.SecretSpec) error { return nil }
func (c fCluster) DeleteNamespace(context.Context, string) error           { return nil }

type fDB struct{ base }

// Type reports the real driver's name so requests asking for "postgresql" match.
func (fDB) Type() string { return "postgres" }

func (d fDB) EnsureDatabase(_ context.Context, s provider.DatabaseSpec) (provider.Issued, error) {
	world.mu.Lock()
	defer world.mu.Unlock()
	if world.issued["db/"+s.Name] > 0 && !s.Rotate {
		return provider.Issued{Outputs: provider.Outputs{"name": s.Name}}, nil
	}
	world.issued["db/"+s.Name]++
	return provider.Issued{Outputs: provider.Outputs{"name": s.Name}, Credential: provider.Credential{
		"host": "db.fake", "port": "5432", "name": s.Name, "user": s.Name, "password": dbSecret}}, nil
}
func (d fDB) DeleteDatabase(context.Context, provider.DatabaseSpec) error { return nil }

func init() {
	reg := func(kind provider.Kind, mk func(base) provider.Provider) {
		provider.Register(kind, "fake", func(cfg map[string]string) (provider.Provider, error) {
			return mk(base{kind: kind, cfg: cfg}), nil
		})
	}
	reg(provider.KindGitSource, func(b base) provider.Provider { return fGit{b} })
	reg(provider.KindRegistry, func(b base) provider.Provider { return fReg{b} })
	reg(provider.KindCluster, func(b base) provider.Provider { return fCluster{b} })
	provider.Register(provider.KindDatabase, "postgres", func(cfg map[string]string) (provider.Provider, error) {
		return fDB{base{kind: provider.KindDatabase, cfg: cfg}}, nil
	})
}

// ---- harness -------------------------------------------------------------------------------

func startPostgres(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("plinth-itest-app-%06d", rand.Intn(1_000_000))
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_PASSWORD=itest", "-p", "127.0.0.1::5432", "postgres:17").CombinedOutput()
	if err != nil {
		t.Skipf("cannot start postgres: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	pout, err := exec.Command("docker", "port", name, "5432/tcp").Output()
	if err != nil {
		t.Fatal(err)
	}
	first := strings.TrimSpace(strings.Split(string(pout), "\n")[0])
	port, _ := strconv.Atoi(first[strings.LastIndex(first, ":")+1:])
	url := fmt.Sprintf("postgres://postgres:itest@127.0.0.1:%d/postgres?sslmode=disable", port)

	deadline := time.Now().Add(90 * time.Second)
	for ok := 0; ok < 2; {
		c, err := pgx.Connect(context.Background(), url)
		if err == nil {
			c.Close(context.Background())
			ok++
			time.Sleep(2 * time.Second)
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres not ready: %v", err)
		}
		time.Sleep(time.Second)
	}
	return url
}

type env struct {
	t          *testing.T
	url        string
	st         *store.Store
	srv        *httptest.Server
	limited    *httptest.Server // same app with the default rate limit
	idp        *authtest.Provider
	worker     *worker.Worker
	stopWorker context.CancelFunc
	workerDone chan struct{}
}

func (e *env) token(sub string, groups ...string) string {
	return e.idp.Token(authtest.Claims{Sub: sub, Email: sub + "@example.test", Groups: groups})
}

func (e *env) admin() string { return e.token("admin", "plinth-admins") }
func (e *env) alice() string { return e.token("alice", "devs") }
func (e *env) bob() string   { return e.token("bob", "devs") }

// call performs a request and decodes the JSON response.
func (e *env) call(token, method, path string, body any) (int, any, string) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var v any
	_ = json.Unmarshal(raw, &v)
	return resp.StatusCode, v, string(raw)
}

func (e *env) expect(want int, token, method, path string, body any) (any, string) {
	e.t.Helper()
	code, v, raw := e.call(token, method, path, body)
	if code != want {
		e.t.Fatalf("%s %s: got %d, want %d: %s", method, path, code, want, raw)
	}
	return v, raw
}

func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func (e *env) waitStatus(token, id string, want string) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, _ := e.expect(200, token, "GET", "/api/v1/requests/"+id, nil)
		m := v.(map[string]any)
		if m["status"] == want {
			return m
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("request %s: status %v, wanted %s (error %v)", id, m["status"], want, m["error"])
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (e *env) startWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	e.stopWorker = cancel
	e.workerDone = make(chan struct{})
	go func() {
		defer close(e.workerDone)
		e.worker.Run(ctx, 1)
	}()
}

func (e *env) pauseWorker() {
	e.stopWorker()
	<-e.workerDone
}

func newEnv(t *testing.T) *env {
	t.Helper()
	url := startPostgres(t)
	key, _ := crypto.GenerateKey()
	box, _ := crypto.NewBox(key)
	ctx := context.Background()
	st, err := store.Open(ctx, url, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	for i := 0; i < 2; i++ { // migrating twice must be harmless
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	idp := authtest.New("plinth")
	t.Cleanup(idp.Close)
	v, err := auth.New(ctx, auth.Config{Issuer: idp.Issuer(), Audience: "plinth", AdminGroup: "plinth-admins"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(api.Deps{Store: st, Verifier: v, RateBurst: 1000, RatePerMinute: 6000}))
	t.Cleanup(srv.Close)
	limited := httptest.NewServer(api.New(api.Deps{Store: st, Verifier: v})) // default limits
	t.Cleanup(limited.Close)
	e := &env{t: t, url: url, st: st, srv: srv, limited: limited, idp: idp}
	e.worker = &worker.Worker{Store: st, Poll: 50 * time.Millisecond,
		Options: orchestrator.Options{MaxAttempts: 2, Backoff: 10 * time.Millisecond}}
	e.startWorker()
	t.Cleanup(func() { e.stopWorker() })
	return e
}

func (e *env) rawQuery(q string, args ...any) string {
	e.t.Helper()
	c, err := pgx.Connect(context.Background(), e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close(context.Background())
	var out string
	if err := c.QueryRow(context.Background(), q, args...).Scan(&out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) exec(q string, args ...any) {
	e.t.Helper()
	c, err := pgx.Connect(context.Background(), e.url)
	if err != nil {
		e.t.Fatal(err)
	}
	defer c.Close(context.Background())
	if _, err := c.Exec(context.Background(), q, args...); err != nil {
		e.t.Fatal(err)
	}
}

// ---- the tests -------------------------------------------------------------------------------

const shopRequest = `{"blueprint":"standard","tenant":"%s","projects":[
  {"name":"%s","quota":"silver","use_db":"true","db_type":"postgresql","host_url":"https://%s.example.com","application_port":"8080"}]}`

func TestApplication(t *testing.T) {
	e := newEnv(t)

	t.Run("health, identity and roles", func(t *testing.T) {
		e.expect(200, "", "GET", "/healthz", nil)
		e.expect(200, "", "GET", "/readyz", nil)
		e.expect(401, "", "GET", "/api/v1/me", nil)
		e.expect(401, "garbage", "GET", "/api/v1/me", nil)
		e.expect(401, e.idp.Token(authtest.Claims{Sub: "x", Audience: "other"}), "GET", "/api/v1/me", nil)
		e.expect(401, e.idp.Token(authtest.Claims{Sub: "x", Expires: -time.Minute}), "GET", "/api/v1/me", nil)

		v, _ := e.expect(200, e.alice(), "GET", "/api/v1/me", nil)
		if get(v, "role") != "user" || get(v, "email") != "alice@example.test" {
			t.Fatalf("alice: %v", v)
		}
		v, _ = e.expect(200, e.admin(), "GET", "/api/v1/me", nil)
		if get(v, "role") != "admin" {
			t.Fatalf("admin: %v", v)
		}
	})

	t.Run("users cannot use admin endpoints", func(t *testing.T) {
		for _, c := range [][2]string{
			{"GET", "/api/v1/integrations"}, {"POST", "/api/v1/integrations"}, {"DELETE", "/api/v1/integrations/x"},
			{"POST", "/api/v1/blueprints"}, {"DELETE", "/api/v1/blueprints/x"}, {"GET", "/api/v1/audit"},
			{"POST", "/api/v1/integrations/x/validate"},
		} {
			e.expect(403, e.alice(), c[0], c[1], `{}`)
		}
	})

	t.Run("integrations: validation, encryption at rest, no secret in responses", func(t *testing.T) {
		secret := "super-secret-git-token"
		create := func(kind, name string, cfg map[string]string) (int, string) {
			driver := "fake"
			if kind == "database" {
				driver = "postgres"
			}
			code, _, raw := e.call(e.admin(), "POST", "/api/v1/integrations",
				map[string]any{"kind": kind, "driver": driver, "name": name, "config": cfg})
			return code, raw
		}
		code, raw := create("gitsource", "git-main", map[string]string{"token": secret, "base_url": "https://git.example.com"})
		if code != 201 || strings.Contains(raw, secret) {
			t.Fatalf("create: %d %s", code, raw)
		}
		for _, in := range [][2]string{{"registry", "registry-main"}, {"cluster", "cluster-dev"}, {"cluster", "cluster-prod"}, {"database", "db-dev"}} {
			if code, raw := create(in[0], in[1], map[string]string{"name": strings.TrimPrefix(in[1], "cluster-"), "token": "t-" + in[1]}); code != 201 {
				t.Fatalf("create %s: %d %s", in[1], code, raw)
			}
		}
		e.expect(409, e.admin(), "POST", "/api/v1/integrations", map[string]any{"kind": "gitsource", "driver": "fake", "name": "git-main", "config": map[string]string{"a": "b"}})
		e.expect(422, e.admin(), "POST", "/api/v1/integrations", map[string]any{"kind": "gitsource", "driver": "nonexistent", "name": "x", "config": map[string]string{"a": "b"}})
		e.expect(400, e.admin(), "POST", "/api/v1/integrations", map[string]any{"kind": "toaster", "driver": "fake", "name": "x", "config": map[string]string{"a": "b"}})
		e.expect(400, e.admin(), "POST", "/api/v1/integrations", map[string]any{"kind": "gitsource", "driver": "fake", "name": "Bad Name", "config": map[string]string{"a": "b"}})
		e.expect(400, e.admin(), "POST", "/api/v1/integrations", map[string]any{"kind": "gitsource", "driver": "fake", "name": "empty", "config": map[string]string{}})
		e.expect(400, e.admin(), "POST", "/api/v1/integrations", `{"kind":"gitsource","surprise":1}`)
		e.expect(400, e.admin(), "POST", "/api/v1/integrations", `{"kind":"gitsource"} {"again":1}`)

		// Responses never include configuration values; only key names.
		list, raw := e.expect(200, e.admin(), "GET", "/api/v1/integrations", nil)
		if strings.Contains(raw, secret) || len(list.([]any)) != 5 {
			t.Fatalf("list leaks or is wrong: %s", raw)
		}
		one, raw := e.expect(200, e.admin(), "GET", "/api/v1/integrations/git-main", nil)
		keys := get(one, "config_keys").([]any)
		if strings.Contains(raw, secret) || len(keys) != 2 || keys[0] != "base_url" || keys[1] != "token" {
			t.Fatalf("get: %s", raw)
		}

		// Encrypted in the database, bound to the row.
		stored := e.rawQuery(`SELECT encode(config_enc, 'escape') FROM integrations WHERE name = 'git-main'`)
		if strings.Contains(stored, secret) || strings.Contains(stored, "git.example.com") {
			t.Fatal("integration configuration is stored in clear text")
		}
		e.exec(`UPDATE integrations SET config_enc = (SELECT config_enc FROM integrations WHERE name = 'cluster-dev') WHERE name = 'cluster-prod'`)
		if _, _, err := e.st.IntegrationConfig(context.Background(), "cluster-prod"); err == nil {
			t.Fatal("a ciphertext copied to another row must not decrypt")
		}
		// restore a valid config for the later scenarios
		e.expect(200, e.admin(), "PUT", "/api/v1/integrations/cluster-prod", map[string]any{"config": map[string]string{"name": "prod", "token": "t-prod"}})

		// Validate hits the system and reports only ok / error.
		v, _ := e.expect(200, e.admin(), "POST", "/api/v1/integrations/git-main/validate", nil)
		if get(v, "ok") != true {
			t.Fatalf("validate: %v", v)
		}
		create("registry", "broken", map[string]string{"fail_validate": "true"})
		v, raw = e.expect(200, e.admin(), "POST", "/api/v1/integrations/broken/validate", nil)
		if get(v, "ok") != false || !strings.Contains(raw, "unreachable") {
			t.Fatalf("validate of a broken system: %s", raw)
		}
		e.expect(204, e.admin(), "DELETE", "/api/v1/integrations/broken", nil)
		e.expect(404, e.admin(), "DELETE", "/api/v1/integrations/broken", nil)
	})

	t.Run("blueprints: references are checked, users see a summary", func(t *testing.T) {
		bp := func(m map[string]any) map[string]any {
			base := map[string]any{
				"name": "standard", "git_source": "git-main", "registry": "registry-main", "git_parent": "tenants",
				"template_repo": "platform/app-template", "template_vars": map[string]string{"PROJECT_NAME": "{project}"},
				"extra_branches": []string{"develop"},
				"environments": []map[string]any{
					{"name": "dev", "cluster": "cluster-dev", "database": "db-dev", "access_prefix": "oidc:", "ingress_namespace": "ingress"},
					{"name": "prod", "cluster": "cluster-prod", "access_prefix": "oidc:", "ingress_namespace": "ingress"},
				},
			}
			for k, v := range m {
				base[k] = v
			}
			return base
		}
		e.expect(422, e.admin(), "POST", "/api/v1/blueprints", bp(map[string]any{"git_source": "no-such-git"}))
		e.expect(422, e.admin(), "POST", "/api/v1/blueprints", bp(map[string]any{"git_source": "cluster-dev"})) // wrong kind
		e.expect(422, e.admin(), "POST", "/api/v1/blueprints", bp(map[string]any{"registry": "git-main"}))
		e.expect(400, e.admin(), "POST", "/api/v1/blueprints", bp(map[string]any{"name": "Bad"}))
		e.expect(400, e.admin(), "POST", "/api/v1/blueprints", bp(map[string]any{"environments": []map[string]any{}}))
		e.expect(400, e.admin(), "POST", "/api/v1/blueprints", bp(map[string]any{"environments": []map[string]any{
			{"name": "dev", "cluster": "cluster-dev"}, {"name": "dev", "cluster": "cluster-prod"}}}))
		e.expect(201, e.admin(), "POST", "/api/v1/blueprints", bp(nil))
		e.expect(409, e.admin(), "POST", "/api/v1/blueprints", bp(nil))

		// Admin sees everything, a user sees only a summary.
		_, raw := e.expect(200, e.admin(), "GET", "/api/v1/blueprints/standard", nil)
		if !strings.Contains(raw, "git-main") {
			t.Fatalf("admin view: %s", raw)
		}
		v, raw := e.expect(200, e.alice(), "GET", "/api/v1/blueprints", nil)
		if strings.Contains(raw, "git-main") || strings.Contains(raw, "cluster-dev") || strings.Contains(raw, "template") {
			t.Fatalf("user view exposes infrastructure: %s", raw)
		}
		first := v.([]any)[0]
		if get(first, "name") != "standard" || get(first, "has_database") != true {
			t.Fatalf("summary: %v", first)
		}
		e.expect(200, e.alice(), "GET", "/api/v1/quota-tiers", nil)

		// In use: cannot be deleted.
		e.expect(409, e.admin(), "DELETE", "/api/v1/integrations/git-main", nil)
	})

	t.Run("submitting: validation", func(t *testing.T) {
		e.expect(401, "", "POST", "/api/v1/requests", `{}`)
		e.expect(422, e.bob(), "POST", "/api/v1/requests", `{"blueprint":"nope","tenant":"t","projects":[{"name":"a"}]}`)
		for name, body := range map[string]string{
			"bad project name":  `{"blueprint":"standard","tenant":"t","projects":[{"name":"a_b"}]}`,
			"unknown tier":      `{"blueprint":"standard","tenant":"t","projects":[{"name":"a","quota":"platinum"}]}`,
			"mysql not offered": `{"blueprint":"standard","tenant":"t","projects":[{"name":"a","use_db":true,"db_type":"mysql"}]}`,
			"no projects":       `{"blueprint":"standard","tenant":"t","projects":[]}`,
		} {
			v, raw := e.expect(422, e.bob(), "POST", "/api/v1/requests", body)
			if get(v, "details") == nil {
				t.Errorf("%s: no details: %s", name, raw)
			}
		}
		e.expect(400, e.bob(), "POST", "/api/v1/requests", `{"blueprint":"standard","tenant":"t","projects":[],"admin":true}`)
	})

	var firstID string
	t.Run("a request runs in the background and succeeds", func(t *testing.T) {
		v, _ := e.expect(202, e.alice(), "POST", "/api/v1/requests", fmt.Sprintf(shopRequest, "team-a", "shop", "shop"))
		firstID = get(v, "id").(string)
		done := e.waitStatus(e.alice(), firstID, "succeeded")

		steps := get(done, "state", "steps").(map[string]any)
		for _, s := range []string{"git/group", "git/repo/shop", "cluster/shop/dev", "cluster/shop/prod", "registry/credential/shop", "database/shop/dev"} {
			if get(steps[s], "status") != "succeeded" {
				t.Errorf("step %s: %v", s, steps[s])
			}
		}
		if get(done, "state", "outputs", "cluster/shop/prod.namespace") != "team-a-shop" {
			t.Errorf("outputs: %v", get(done, "state", "outputs"))
		}
		if world.count("group/team-a") != 1 || world.count("repo/team-a/shop") != 1 {
			t.Errorf("provider calls: %v", world.calls)
		}

		// Visibility: owner and admin yes, other users no (404, not 403).
		e.expect(200, e.admin(), "GET", "/api/v1/requests/"+firstID, nil)
		e.expect(404, e.bob(), "GET", "/api/v1/requests/"+firstID, nil)
		e.expect(404, e.alice(), "GET", "/api/v1/requests/00000000-0000-0000-0000-000000000000", nil)
		e.expect(404, e.alice(), "GET", "/api/v1/requests/not-a-uuid", nil)
		mine, _ := e.expect(200, e.alice(), "GET", "/api/v1/requests", nil)
		theirs, _ := e.expect(200, e.bob(), "GET", "/api/v1/requests", nil)
		all, _ := e.expect(200, e.admin(), "GET", "/api/v1/requests?all=true", nil)
		if len(mine.([]any)) != 1 || len(theirs.([]any)) != 0 || len(all.([]any)) < 1 {
			t.Errorf("listing: mine=%d theirs=%d all=%d", len(mine.([]any)), len(theirs.([]any)), len(all.([]any)))
		}
	})

	t.Run("provisioned secrets never reach the database, API or audit log", func(t *testing.T) {
		_, raw := e.expect(200, e.alice(), "GET", "/api/v1/requests/"+firstID, nil)
		state := e.rawQuery(`SELECT string_agg(state::text || payload::text || error, ' ') FROM requests`)
		audit := e.rawQuery(`SELECT coalesce(string_agg(detail::text, ' '), '') FROM audit_log`)
		for _, s := range []string{regSecret, dbSecret, "super-secret-git-token"} {
			for where, text := range map[string]string{"api": raw, "requests table": state, "audit log": audit} {
				if strings.Contains(text, s) {
					t.Errorf("%q found in %s", s, where)
				}
			}
		}
	})

	t.Run("tenant ownership", func(t *testing.T) {
		e.expect(403, e.bob(), "POST", "/api/v1/requests", fmt.Sprintf(shopRequest, "team-a", "other", "other"))
		v, _ := e.expect(202, e.admin(), "POST", "/api/v1/requests", fmt.Sprintf(shopRequest, "team-a", "adminwork", "adminwork"))
		e.waitStatus(e.admin(), get(v, "id").(string), "succeeded")
		e.expect(202, e.alice(), "POST", "/api/v1/requests", fmt.Sprintf(shopRequest, "team-a", "second", "second"))
	})

	t.Run("two tenants cannot end up with the same name", func(t *testing.T) {
		e.expect(202, e.alice(), "POST", "/api/v1/requests", `{"blueprint":"standard","tenant":"a-b","projects":[{"name":"c"}]}`)
		v, raw := e.expect(409, e.bob(), "POST", "/api/v1/requests", `{"blueprint":"standard","tenant":"a","projects":[{"name":"b-c"}]}`)
		if !strings.Contains(raw, "a-b-c") {
			t.Errorf("conflict message should name the colliding name: %v", v)
		}
		// Bob's tenant must not have been half-created.
		if n := e.rawQuery(`SELECT count(*)::text FROM tenants WHERE name = 'a'`); n != "0" {
			t.Errorf("the rejected request left a tenant behind (%s)", n)
		}
		// Re-submitting your own project is fine.
		e.expect(202, e.alice(), "POST", "/api/v1/requests", `{"blueprint":"standard","tenant":"a-b","projects":[{"name":"c"}]}`)
	})

	t.Run("failure is reported, retry resumes without repeating finished steps", func(t *testing.T) {
		world.mu.Lock()
		world.failNS["team-f-app"] = true
		world.mu.Unlock()

		v, _ := e.expect(202, e.alice(), "POST", "/api/v1/requests", `{"blueprint":"standard","tenant":"team-f","projects":[{"name":"app"}]}`)
		id := get(v, "id").(string)
		failed := e.waitStatus(e.alice(), id, "failed")
		if !strings.Contains(fmt.Sprint(failed["error"]), "cluster/app/dev") {
			t.Errorf("error should name the failing step: %v", failed["error"])
		}
		steps := get(failed, "state", "steps").(map[string]any)
		if get(steps["git/repo/app"], "status") != "succeeded" || get(steps["cluster/app/dev"], "status") != "failed" {
			t.Errorf("steps: %v", steps)
		}
		repoCalls := world.count("repo/team-f/app")

		e.expect(404, e.bob(), "POST", "/api/v1/requests/"+id+"/retry", nil) // not bob's
		world.mu.Lock()
		world.failNS["team-f-app"] = false
		world.mu.Unlock()
		e.expect(202, e.alice(), "POST", "/api/v1/requests/"+id+"/retry", nil)
		e.waitStatus(e.alice(), id, "succeeded")
		if world.count("repo/team-f/app") != repoCalls {
			t.Errorf("a finished step ran again after the retry")
		}
		e.expect(409, e.alice(), "POST", "/api/v1/requests/"+id+"/retry", nil) // not failed any more
	})

	t.Run("request rate is limited per user", func(t *testing.T) {
		post := func(srv *httptest.Server, token string) int {
			req, _ := http.NewRequest("POST", srv.URL+"/api/v1/requests", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		carol := e.token("carol", "devs")
		limited := 0
		for i := 0; i < 8; i++ {
			if post(e.limited, carol) == 429 {
				limited++
			}
		}
		if limited < 2 || limited > 4 {
			t.Errorf("with a burst of 5, 8 rapid requests should see 3 refusals, got %d", limited)
		}
		if post(e.limited, e.token("dave", "devs")) == 429 {
			t.Error("one user's requests must not use up another user's allowance")
		}
		if code, _, _ := e.call(carol, "GET", "/api/v1/me", nil); code != 200 {
			t.Errorf("read-only calls are not rate limited, got %d", code)
		}
	})

	t.Run("oversized bodies are refused", func(t *testing.T) {
		big := `{"kind":"gitsource","driver":"fake","name":"big","config":{"a":"` + strings.Repeat("x", 2<<20) + `"}}`
		e.expect(413, e.admin(), "POST", "/api/v1/integrations", big)
	})

	t.Run("the audit log records administration and requests", func(t *testing.T) {
		v, raw := e.expect(200, e.admin(), "GET", "/api/v1/audit?limit=500", nil)
		actions := map[string]bool{}
		for _, ent := range v.([]any) {
			actions[get(ent, "action").(string)] = true
		}
		for _, want := range []string{"integration.create", "integration.update", "integration.validate", "integration.delete",
			"blueprint.create", "tenant.create", "request.submit", "request.succeeded", "request.failed", "request.retry"} {
			if !actions[want] {
				t.Errorf("audit log has no %q entry", want)
			}
		}
		if strings.Contains(raw, "super-secret") || strings.Contains(raw, regSecret) {
			t.Error("audit log contains a secret")
		}
	})

	// The remaining scenarios need to control the worker themselves.
	e.pauseWorker()

	t.Run("a request whose worker died is picked up again", func(t *testing.T) {
		ctx := context.Background()
		v, _ := e.expect(202, e.alice(), "POST", "/api/v1/requests", `{"blueprint":"standard","tenant":"team-z","projects":[{"name":"zed"}]}`)
		id := get(v, "id").(string)

		// A worker claimed it a while ago, saved nothing, and died.
		e.exec(`UPDATE requests SET status = 'running', claimed_by = 'dead-worker', claimed_at = now() - interval '2 hours' WHERE id = $1`, id)
		// A live lease is respected.
		e.exec(`UPDATE requests SET claimed_at = now() WHERE id = $1`, id)
		if worked, err := e.worker.RunOne(ctx, "w1"); err != nil || worked {
			t.Fatalf("a request with a fresh lease must not be taken: worked=%v err=%v", worked, err)
		}
		e.exec(`UPDATE requests SET claimed_at = now() - interval '2 hours' WHERE id = $1`, id)
		if worked, err := e.worker.RunOne(ctx, "w2"); err != nil || !worked {
			t.Fatalf("an expired lease must be taken over: worked=%v err=%v", worked, err)
		}
		e.waitStatus(e.alice(), id, "succeeded")
	})

	t.Run("only one worker runs a request", func(t *testing.T) {
		ctx := context.Background()
		v, _ := e.expect(202, e.alice(), "POST", "/api/v1/requests", `{"blueprint":"standard","tenant":"team-y","projects":[{"name":"why"}]}`)
		id := get(v, "id").(string)

		var wg sync.WaitGroup
		var mu sync.Mutex
		worked := 0
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ok, err := e.worker.RunOne(ctx, fmt.Sprintf("racer-%d", i))
				if err != nil {
					t.Error(err)
				}
				if ok {
					mu.Lock()
					worked++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()
		if worked != 1 {
			t.Fatalf("%d workers processed the same request", worked)
		}
		e.waitStatus(e.alice(), id, "succeeded")
		if world.count("repo/team-y/why") != 1 {
			t.Errorf("provisioning ran %d times", world.count("repo/team-y/why"))
		}
	})

	t.Run("a worker that loses its lease stops without recording an outcome", func(t *testing.T) {
		v, _ := e.expect(202, e.alice(), "POST", "/api/v1/requests", `{"blueprint":"standard","tenant":"team-x","projects":[{"name":"ex"}]}`)
		id := get(v, "id").(string)
		rec, err := e.st.ClaimNext(context.Background(), "slow-worker", time.Minute)
		if err != nil || rec.ID != id {
			t.Fatalf("claim: %v %v", rec.ID, err)
		}
		// Another worker finishes it in the meantime.
		e.exec(`UPDATE requests SET status = 'succeeded' WHERE id = $1`, id)
		err = e.st.Save(context.Background(), id, orchestrator.NewState())
		if err == nil || err != store.ErrLeaseLost {
			t.Fatalf("saving after losing the lease must fail with ErrLeaseLost, got %v", err)
		}
	})
}
