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
	"os"
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

// ---- approvals ------------------------------------------------------------------------------

func TestApprovals(t *testing.T) {
	e := newEnv(t)
	admin2 := e.token("admin2", "plinth-admins")

	mk := func(kind, driver, name string, cfg map[string]string) {
		e.expect(201, e.admin(), "POST", "/api/v1/integrations",
			map[string]any{"kind": kind, "driver": driver, "name": name, "config": cfg})
	}
	mk("gitsource", "fake", "git", map[string]string{"a": "b"})
	mk("registry", "fake", "reg", map[string]string{"a": "b"})
	mk("cluster", "fake", "cl-dev", map[string]string{"name": "dev"})
	mk("database", "postgres", "db", map[string]string{"a": "b"})
	blueprint := func(name string, approval bool) {
		e.expect(201, e.admin(), "POST", "/api/v1/blueprints", map[string]any{
			"name": name, "git_source": "git", "registry": "reg", "requires_approval": approval,
			"environments": []map[string]any{{"name": "dev", "cluster": "cl-dev", "database": "db"}},
		})
	}
	blueprint("open", false)
	blueprint("sensitive", true)

	submit := func(token, bp, tenant, project string) (int, map[string]any) {
		code, v, _ := e.call(token, "POST", "/api/v1/requests",
			fmt.Sprintf(`{"blueprint":%q,"tenant":%q,"projects":[{"name":%q}]}`, bp, tenant, project))
		m, _ := v.(map[string]any)
		return code, m
	}
	idOf := func(m map[string]any) string { return m["id"].(string) }
	decide := func(token, id, action, note string) (int, string) {
		var body any
		if note != "" {
			body = map[string]string{"note": note}
		}
		code, _, raw := e.call(token, "POST", "/api/v1/requests/"+id+"/"+action, body)
		return code, raw
	}
	status := func(token, id string) string {
		v, _ := e.expect(200, token, "GET", "/api/v1/requests/"+id, nil)
		return get(v, "status").(string)
	}

	t.Run("users can see which blueprints need approval", func(t *testing.T) {
		v, _ := e.expect(200, e.alice(), "GET", "/api/v1/blueprints", nil)
		got := map[string]bool{}
		for _, b := range v.([]any) {
			got[get(b, "name").(string)] = get(b, "requires_approval") == true
		}
		if got["open"] || !got["sensitive"] {
			t.Fatalf("requires_approval flags: %v", got)
		}
	})

	t.Run("a blueprint without approval runs straight away", func(t *testing.T) {
		code, m := submit(e.alice(), "open", "team-o", "app")
		if code != 202 || m["status"] == "awaiting_approval" {
			t.Fatalf("%d %v", code, m["status"])
		}
		e.waitStatus(e.alice(), idOf(m), "succeeded")
	})

	var held string
	t.Run("a sensitive request waits and nothing is provisioned", func(t *testing.T) {
		code, m := submit(e.alice(), "sensitive", "team-s", "app")
		if code != 202 || m["status"] != "awaiting_approval" {
			t.Fatalf("%d %v", code, m["status"])
		}
		held = idOf(m)
		time.Sleep(800 * time.Millisecond) // many worker polls
		if status(e.alice(), held) != "awaiting_approval" {
			t.Fatal("the worker must not run an unapproved request")
		}
		if world.count("group/team-s") != 0 || world.count("repo/team-s/app") != 0 {
			t.Fatalf("systems were contacted before approval: %v", world.calls)
		}
		v, _ := e.expect(200, e.alice(), "GET", "/api/v1/requests/"+held, nil)
		if len(get(v, "state", "steps").(map[string]any)) != 0 {
			t.Fatal("no step may have started")
		}
		e.expect(409, e.alice(), "POST", "/api/v1/requests/"+held+"/retry", nil) // not failed
	})

	t.Run("only administrators approve, and never their own request", func(t *testing.T) {
		e.expect(401, "", "POST", "/api/v1/requests/"+held+"/approve", nil)
		if code, _ := decide(e.alice(), held, "approve", ""); code != 403 { // the requester
			t.Errorf("requester approving: %d", code)
		}
		if code, _ := decide(e.bob(), held, "approve", ""); code != 403 {
			t.Errorf("another user approving: %d", code)
		}
		if code, _ := decide(e.bob(), held, "reject", "nope nope"); code != 403 {
			t.Errorf("a user rejecting: %d", code)
		}
		if status(e.alice(), held) != "awaiting_approval" {
			t.Fatal("refused decisions must change nothing")
		}

		// An administrator's own request needs a different administrator.
		code, m := submit(e.admin(), "sensitive", "team-self", "app")
		if code != 202 || m["status"] != "awaiting_approval" {
			t.Fatalf("admin submit: %d %v", code, m["status"])
		}
		self := idOf(m)
		code, raw := decide(e.admin(), self, "approve", "")
		if code != 403 || !strings.Contains(raw, "cannot approve your own") {
			t.Fatalf("self approval: %d %s", code, raw)
		}
		if code, _ := decide(admin2, self, "approve", "second pair of eyes"); code != 200 {
			t.Fatalf("another admin approving: %d", code)
		}
		e.waitStatus(e.admin(), self, "succeeded")
	})

	t.Run("approving releases the request to the queue and records who and why", func(t *testing.T) {
		code, raw := decide(e.admin(), held, "approve", "checked with finance")
		if code != 200 {
			t.Fatalf("approve: %d %s", code, raw)
		}
		done := e.waitStatus(e.alice(), held, "succeeded")
		if done["decided_by"] != "admin@example.test" || done["decision_note"] != "checked with finance" || done["decided_at"] == nil {
			t.Fatalf("decision not recorded: %v %v %v", done["decided_by"], done["decision_note"], done["decided_at"])
		}
		if code, _ := decide(admin2, held, "approve", ""); code != 409 {
			t.Errorf("a decided request cannot be decided again: %d", code)
		}
		if code, _ := decide(e.admin(), held, "reject", "too late now"); code != 409 {
			t.Errorf("reject after approval: %d", code)
		}
	})

	t.Run("two simultaneous approvals: exactly one wins", func(t *testing.T) {
		_, m := submit(e.alice(), "sensitive", "team-race", "app")
		id := idOf(m)
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, tok := range []string{e.admin(), admin2} {
			wg.Add(1)
			go func(i int, tok string) {
				defer wg.Done()
				codes[i], _ = decide(tok, id, "approve", "")
			}(i, tok)
		}
		wg.Wait()
		if !(codes[0] == 200 && codes[1] == 409 || codes[0] == 409 && codes[1] == 200) {
			t.Fatalf("expected one 200 and one 409, got %v", codes)
		}
		e.waitStatus(e.alice(), id, "succeeded")
		if world.count("repo/team-race/app") != 1 {
			t.Errorf("provisioned %d times", world.count("repo/team-race/app"))
		}
	})

	t.Run("rejecting needs a reason, ends the request and frees the names", func(t *testing.T) {
		_, m := submit(e.alice(), "sensitive", "sens-x", "p")
		id := idOf(m)

		// While it waits, its names are reserved against other tenants.
		if code, _ := submit(e.bob(), "sensitive", "sens", "x-p"); code != 409 {
			t.Fatalf("the colliding name must be held while the request waits: %d", code)
		}
		if code, _ := decide(e.admin(), id, "reject", ""); code != 400 {
			t.Errorf("reject without a reason: %d", code)
		}
		if code, _ := decide(e.admin(), id, "reject", "no"); code != 400 {
			t.Errorf("reject with a one-word reason: %d", code)
		}
		if code, _ := decide(e.admin(), id, "reject", strings.Repeat("x", 600)); code != 400 {
			t.Errorf("a note over 500 characters: %d", code)
		}
		if status(e.alice(), id) != "awaiting_approval" {
			t.Fatal("invalid decisions must change nothing")
		}

		if code, _ := decide(e.admin(), id, "reject", "Use the shared database instead."); code != 200 {
			t.Fatalf("reject: %d", code)
		}
		v, _ := e.expect(200, e.alice(), "GET", "/api/v1/requests/"+id, nil)
		if get(v, "status") != "rejected" || get(v, "decision_note") != "Use the shared database instead." {
			t.Fatalf("the requester must see the verdict and the reason: %v", v)
		}
		time.Sleep(300 * time.Millisecond)
		if world.count("group/sens-x") != 0 {
			t.Fatal("a rejected request must never run")
		}
		e.expect(409, e.alice(), "POST", "/api/v1/requests/"+id+"/retry", nil)

		// The reservation is gone: the other tenant can have the name now.
		if code, _ := submit(e.bob(), "sensitive", "sens", "x-p"); code != 202 {
			t.Fatalf("the name must be free after a rejection: %d", code)
		}
	})

	t.Run("requesters can withdraw while it waits; nobody else's business", func(t *testing.T) {
		_, m := submit(e.alice(), "sensitive", "team-c", "app")
		id := idOf(m)
		if code, _ := decide(e.bob(), id, "cancel", ""); code != 404 {
			t.Errorf("another user cancelling: %d", code)
		}
		if code, _ := decide(e.alice(), id, "cancel", ""); code != 200 {
			t.Fatalf("withdrawing: %d", code)
		}
		if status(e.alice(), id) != "cancelled" {
			t.Fatal("not cancelled")
		}
		if code, _ := decide(e.alice(), id, "cancel", ""); code != 409 {
			t.Errorf("cancelling twice: %d", code)
		}
		if code, _ := decide(e.admin(), id, "approve", ""); code != 409 {
			t.Errorf("approving a withdrawn request: %d", code)
		}
		// An administrator can also close someone's waiting request.
		_, m2 := submit(e.alice(), "sensitive", "team-c2", "app")
		if code, _ := decide(e.admin(), idOf(m2), "cancel", "duplicate of another request"); code != 200 {
			t.Errorf("admin cancelling: %d", code)
		}
		// A finished request cannot be cancelled.
		if code, _ := decide(e.alice(), firstSucceeded(t, e), "cancel", ""); code != 409 {
			t.Errorf("cancelling a finished request: %d", code)
		}
	})

	t.Run("administrators can list what is waiting", func(t *testing.T) {
		_, m := submit(e.alice(), "sensitive", "team-q", "app")
		v, _ := e.expect(200, e.admin(), "GET", "/api/v1/requests?all=true&status=awaiting_approval", nil)
		found := false
		for _, r := range v.([]any) {
			if get(r, "status") != "awaiting_approval" {
				t.Fatalf("filter let through %v", get(r, "status"))
			}
			found = found || get(r, "id") == idOf(m)
		}
		if !found {
			t.Fatal("the waiting request is missing from the queue")
		}
		e.expect(400, e.admin(), "GET", "/api/v1/requests?status=bogus", nil)
		// Users only ever get their own, whatever they ask for.
		mine, _ := e.expect(200, e.bob(), "GET", "/api/v1/requests?all=true&status=awaiting_approval", nil)
		for _, r := range mine.([]any) {
			if get(r, "requested_by") != "bob@example.test" {
				t.Fatalf("a user saw someone else's request: %v", r)
			}
		}
	})

	t.Run("every decision is in the audit log with its note", func(t *testing.T) {
		v, raw := e.expect(200, e.admin(), "GET", "/api/v1/audit?limit=500", nil)
		seen := map[string]bool{}
		for _, ent := range v.([]any) {
			seen[get(ent, "action").(string)] = true
		}
		for _, want := range []string{"request.approve", "request.reject", "request.cancel"} {
			if !seen[want] {
				t.Errorf("no %s entry", want)
			}
		}
		if !strings.Contains(raw, "checked with finance") || !strings.Contains(raw, "Use the shared database instead.") {
			t.Error("the decision notes are missing from the audit log")
		}
	})
}

// firstSucceeded finds any finished request, for tests that need one.
func firstSucceeded(t *testing.T, e *env) string {
	t.Helper()
	v, _ := e.expect(200, e.admin(), "GET", "/api/v1/requests?all=true&status=succeeded", nil)
	list := v.([]any)
	if len(list) == 0 {
		t.Fatal("no succeeded request to use")
	}
	return get(list[0], "id").(string)
}

// ---- team members ------------------------------------------------------------------------------

func TestTeams(t *testing.T) {
	e := newEnv(t)
	admin2 := e.token("admin2", "plinth-admins")
	alice, bob, carol, dave := e.alice(), e.bob(), e.token("carol", "devs"), e.token("dave", "devs")

	mk := func(kind, driver, name string, cfg map[string]string) {
		e.expect(201, e.admin(), "POST", "/api/v1/integrations", map[string]any{"kind": kind, "driver": driver, "name": name, "config": cfg})
	}
	mk("gitsource", "fake", "git", map[string]string{"a": "b"})
	mk("cluster", "fake", "cl", map[string]string{"name": "dev"})
	for name, approval := range map[string]bool{"open": false, "sensitive": true} {
		e.expect(201, e.admin(), "POST", "/api/v1/blueprints", map[string]any{
			"name": name, "git_source": "git", "requires_approval": approval,
			"environments": []map[string]any{{"name": "dev", "cluster": "cl"}},
		})
	}

	// Everyone signs in once, so they exist and can be added by e-mail.
	ids := map[string]string{}
	for name, tok := range map[string]string{"alice": alice, "bob": bob, "carol": carol, "dave": dave} {
		v, _ := e.expect(200, tok, "GET", "/api/v1/me", nil)
		ids[name] = get(v, "id").(string)
	}

	submit := func(tok, bp, tenant, project string) (int, map[string]any, string) {
		code, v, raw := e.call(tok, "POST", "/api/v1/requests",
			fmt.Sprintf(`{"blueprint":%q,"tenant":%q,"projects":[{"name":%q}]}`, bp, tenant, project))
		m, _ := v.(map[string]any)
		return code, m, raw
	}
	idOf := func(m map[string]any) string { return m["id"].(string) }
	members := func(tok, team string) map[string]string { // email -> role
		v, _ := e.expect(200, tok, "GET", "/api/v1/tenants/"+team, nil)
		out := map[string]string{}
		for _, m := range get(v, "members").([]any) {
			out[get(m, "email").(string)] = get(m, "role").(string)
		}
		return out
	}
	add := func(tok, team, email, role string) (int, string) {
		code, _, raw := e.call(tok, "POST", "/api/v1/tenants/"+team+"/members", map[string]string{"email": email, "role": role})
		return code, raw
	}
	setRole := func(tok, team, uid, role string) (int, string) {
		code, _, raw := e.call(tok, "PUT", "/api/v1/tenants/"+team+"/members/"+uid, map[string]string{"role": role})
		return code, raw
	}
	remove := func(tok, team, uid string) (int, string) {
		code, _, raw := e.call(tok, "DELETE", "/api/v1/tenants/"+team+"/members/"+uid, nil)
		return code, raw
	}
	listIDs := func(tok string) map[string]bool {
		v, _ := e.expect(200, tok, "GET", "/api/v1/requests", nil)
		out := map[string]bool{}
		for _, r := range v.([]any) {
			out[get(r, "id").(string)] = true
		}
		return out
	}

	t.Run("the first request creates the team and makes you its owner", func(t *testing.T) {
		code, m, _ := submit(alice, "open", "team-t", "app")
		if code != 202 {
			t.Fatalf("submit: %d", code)
		}
		e.waitStatus(alice, idOf(m), "succeeded")

		v, _ := e.expect(200, alice, "GET", "/api/v1/tenants", nil)
		list := v.([]any)
		if len(list) != 1 || get(list[0], "name") != "team-t" || get(list[0], "role") != "owner" || get(list[0], "member_count") != float64(1) {
			t.Fatalf("alice's teams: %v", list)
		}
		if got := members(alice, "team-t"); len(got) != 1 || got["alice@example.test"] != "owner" {
			t.Fatalf("members: %v", got)
		}
		if p := get(list[0], "projects").([]any); len(p) != 1 || p[0] != "app" {
			t.Errorf("projects: %v", p)
		}
	})

	t.Run("outsiders cannot see the team, join in, or learn anything about it", func(t *testing.T) {
		v, _ := e.expect(200, bob, "GET", "/api/v1/tenants", nil)
		if len(v.([]any)) != 0 {
			t.Errorf("bob sees teams he is not in: %v", v)
		}
		e.expect(404, bob, "GET", "/api/v1/tenants/team-t", nil)
		e.expect(404, bob, "GET", "/api/v1/tenants/no-such-team", nil) // same answer as for a team he cannot see
		code, _, raw := submit(bob, "open", "team-t", "sneaky")
		if code != 403 || !strings.Contains(raw, "not a member of this team") || !strings.Contains(raw, "ask one of its owners") {
			t.Errorf("an outsider requesting for the team: %d %s", code, raw)
		}
		if code, _ := add(bob, "team-t", "bob@example.test", "owner"); code != 404 {
			t.Errorf("an outsider adding themselves: %d", code)
		}
		// Administrators see every team, but not as members.
		v, _ = e.expect(200, e.admin(), "GET", "/api/v1/tenants?all=true", nil)
		if len(v.([]any)) != 1 || get(v.([]any)[0], "role") != "" {
			t.Errorf("admin view: %v", v)
		}
		e.expect(200, e.admin(), "GET", "/api/v1/tenants/team-t", nil)
		// ?all=true is ignored for everyone else.
		v, _ = e.expect(200, bob, "GET", "/api/v1/tenants?all=true", nil)
		if len(v.([]any)) != 0 {
			t.Error("a user must not be able to list every team")
		}
	})

	t.Run("owners add people by e-mail address", func(t *testing.T) {
		code, raw := add(alice, "team-t", "BOB@Example.Test", "member") // case does not matter
		if code != 201 || !strings.Contains(raw, `"role":"member"`) {
			t.Fatalf("add: %d %s", code, raw)
		}
		if code, _ := add(alice, "team-t", "bob@example.test", "member"); code != 409 {
			t.Errorf("adding twice: %d", code)
		}
		code, raw = add(alice, "team-t", "nobody@example.test", "member")
		if code != 202 || !strings.Contains(raw, `"expires_at"`) || strings.Contains(raw, `"user_id"`) {
			t.Errorf("a person who never signed in gets an invitation, not a membership: %d %s", code, raw)
		}
		if code, _ := add(alice, "team-t", "not-an-address", "member"); code != 400 {
			t.Errorf("bad address: %d", code)
		}
		if code, _ := add(alice, "team-t", "carol@example.test", "admin"); code != 400 {
			t.Errorf("unknown role: %d", code)
		}
		if code, _, _ := e.call(alice, "POST", "/api/v1/tenants/team-t/members", `{"email":"carol@example.test","role":"member","extra":1}`); code != 400 {
			t.Errorf("unknown field: %d", code)
		}
		if got := members(bob, "team-t"); len(got) != 2 || got["bob@example.test"] != "member" {
			t.Fatalf("bob should now see the team: %v", got)
		}
	})

	t.Run("two accounts with the same address cannot be told apart", func(t *testing.T) {
		twin := e.idp.Token(authtest.Claims{Sub: "twin-1", Email: "twin@example.test"})
		twin2 := e.idp.Token(authtest.Claims{Sub: "twin-2", Email: "twin@example.test"})
		e.expect(200, twin, "GET", "/api/v1/me", nil)
		e.expect(200, twin2, "GET", "/api/v1/me", nil)
		if code, raw := add(alice, "team-t", "twin@example.test", "member"); code != 409 || !strings.Contains(raw, "more than one account") {
			t.Errorf("ambiguous e-mail: %d %s", code, raw)
		}
	})

	var aliceReq, bobReq string
	t.Run("members request projects together and see each other's requests", func(t *testing.T) {
		_, m, _ := submit(bob, "open", "team-t", "web")
		bobReq = idOf(m)
		e.waitStatus(bob, bobReq, "succeeded")
		_, m, _ = submit(alice, "open", "team-t", "api2")
		aliceReq = idOf(m)
		e.waitStatus(alice, aliceReq, "succeeded")

		for who, tok := range map[string]string{"alice": alice, "bob": bob} {
			got := listIDs(tok)
			if !got[aliceReq] || !got[bobReq] {
				t.Errorf("%s does not see the whole team's requests: %v", who, got)
			}
		}
		e.expect(200, bob, "GET", "/api/v1/requests/"+aliceReq, nil)
		e.expect(404, carol, "GET", "/api/v1/requests/"+aliceReq, nil)
		if got := listIDs(carol); got[aliceReq] || got[bobReq] {
			t.Error("an outsider sees the team's requests")
		}
		v, _ := e.expect(200, bob, "GET", "/api/v1/tenants/team-t", nil)
		if p := get(v, "projects").([]any); len(p) != 3 {
			t.Errorf("projects: %v", p)
		}
	})

	t.Run("a teammate can retry a failed request; an outsider cannot", func(t *testing.T) {
		world.mu.Lock()
		world.failNS["team-t-flaky"] = true
		world.mu.Unlock()
		_, m, _ := submit(alice, "open", "team-t", "flaky")
		id := idOf(m)
		e.waitStatus(alice, id, "failed")

		e.expect(404, carol, "POST", "/api/v1/requests/"+id+"/retry", nil)
		world.mu.Lock()
		world.failNS["team-t-flaky"] = false
		world.mu.Unlock()
		e.expect(202, bob, "POST", "/api/v1/requests/"+id+"/retry", nil)
		e.waitStatus(alice, id, "succeeded")
	})

	t.Run("only owners manage the team", func(t *testing.T) {
		if code, raw := add(bob, "team-t", "carol@example.test", "member"); code != 403 || !strings.Contains(raw, "only team owners") {
			t.Errorf("a member adding: %d %s", code, raw)
		}
		if code, _ := setRole(bob, "team-t", ids["bob"], "owner"); code != 403 {
			t.Errorf("a member promoting themselves: %d", code)
		}
		if code, _ := remove(bob, "team-t", ids["alice"]); code != 403 {
			t.Errorf("a member removing the owner: %d", code)
		}
		if got := members(alice, "team-t"); got["alice@example.test"] != "owner" || got["bob@example.test"] != "member" {
			t.Fatalf("refused changes must change nothing: %v", got)
		}
		// Administrators can manage any team.
		if code, _ := add(e.admin(), "team-t", "dave@example.test", "member"); code != 201 {
			t.Errorf("admin adding: %d", code)
		}
	})

	t.Run("roles change, and a team always keeps an owner", func(t *testing.T) {
		if code, _ := setRole(alice, "team-t", ids["bob"], "owner"); code != 200 {
			t.Fatalf("promote: %d", code)
		}
		if code, _ := add(bob, "team-t", "carol@example.test", "member"); code != 201 { // bob can manage now
			t.Errorf("a new owner adding: %d", code)
		}
		if code, _ := setRole(alice, "team-t", ids["bob"], "member"); code != 200 { // two owners: one may step down
			t.Fatalf("demote with two owners: %d", code)
		}
		if code, _ := setRole(alice, "team-t", ids["bob"], "owner"); code != 200 {
			t.Fatalf("promote again: %d", code)
		}
		if code, _ := setRole(alice, "team-t", ids["carol"], "member"); code != 200 { // no-op change is fine
			t.Errorf("same role: %d", code)
		}
		if code, _ := setRole(alice, "team-t", "00000000-0000-0000-0000-000000000000", "owner"); code != 404 {
			t.Errorf("unknown member: %d", code)
		}
		if code, _ := setRole(alice, "team-t", "not-a-uuid", "owner"); code != 404 {
			t.Errorf("malformed id: %d", code)
		}
		if code, _ := remove(alice, "team-t", "not-a-uuid"); code != 404 {
			t.Errorf("malformed id: %d", code)
		}
		if code, _ := setRole(alice, "team-t", ids["bob"], "boss"); code != 400 {
			t.Errorf("unknown role: %d", code)
		}

		// Back to a single owner: nothing may remove or demote them.
		e.expect(204, alice, "DELETE", "/api/v1/tenants/team-t/members/"+ids["bob"], nil)
		for name, f := range map[string]func() (int, string){
			"demote": func() (int, string) { return setRole(alice, "team-t", ids["alice"], "member") },
			"remove": func() (int, string) { return remove(e.admin(), "team-t", ids["alice"]) },
			"leave":  func() (int, string) { return remove(alice, "team-t", ids["alice"]) },
		} {
			if code, raw := f(); code != 409 || !strings.Contains(raw, "at least one owner") {
				t.Errorf("last owner %s: %d %s", name, code, raw)
			}
		}
	})

	t.Run("two owners removing each other at once leaves one", func(t *testing.T) {
		code, m, _ := submit(alice, "open", "team-race", "app")
		if code != 202 {
			t.Fatal(code)
		}
		e.waitStatus(alice, idOf(m), "succeeded")
		add(alice, "team-race", "bob@example.test", "owner")

		var wg sync.WaitGroup
		codes := make([]int, 2)
		wg.Add(2)
		go func() { defer wg.Done(); codes[0], _ = remove(alice, "team-race", ids["bob"]) }()
		go func() { defer wg.Done(); codes[1], _ = remove(bob, "team-race", ids["alice"]) }()
		wg.Wait()

		v, _ := e.expect(200, e.admin(), "GET", "/api/v1/tenants/team-race", nil)
		owners := 0
		for _, mm := range get(v, "members").([]any) {
			if get(mm, "role") == "owner" {
				owners++
			}
		}
		if owners != 1 {
			t.Fatalf("a team must end up with exactly one owner after the race, got %d (codes %v)", owners, codes)
		}
	})

	t.Run("leaving ends access to the team but not to your own old requests", func(t *testing.T) {
		world.mu.Lock()
		world.failNS["team-t-broken"] = true
		world.mu.Unlock()
		_, m, _ := submit(alice, "open", "team-t", "broken")
		broken := idOf(m)
		e.waitStatus(alice, broken, "failed")

		if code, _ := setRole(alice, "team-t", ids["carol"], "owner"); code != 200 { // so alice may leave
			t.Fatalf("promote carol: %d", code)
		}
		e.expect(204, alice, "DELETE", "/api/v1/tenants/team-t/members/"+ids["alice"], nil) // leaving

		e.expect(404, alice, "GET", "/api/v1/tenants/team-t", nil)
		if code, _, raw := submit(alice, "open", "team-t", "again"); code != 403 || !strings.Contains(raw, "not a member") {
			t.Errorf("a former member requesting: %d %s", code, raw)
		}
		// What she asked for stays visible to her, but she cannot act on it any more.
		e.expect(200, alice, "GET", "/api/v1/requests/"+broken, nil)
		if !listIDs(alice)[broken] {
			t.Error("her own request disappeared from her list")
		}
		if code, _, raw := e.call(alice, "POST", "/api/v1/requests/"+broken+"/retry", nil); code != 403 || !strings.Contains(raw, "not a member") {
			t.Errorf("retry by a former member: %d %s", code, raw)
		}
		if listIDs(alice)[bobReq] {
			t.Error("a former member still sees the team's other requests")
		}
		// The remaining owner can retry it.
		world.mu.Lock()
		world.failNS["team-t-broken"] = false
		world.mu.Unlock()
		e.expect(202, carol, "POST", "/api/v1/requests/"+broken+"/retry", nil)
		e.waitStatus(carol, broken, "succeeded")
	})

	t.Run("who may withdraw a waiting request, and who may approve it", func(t *testing.T) {
		if code, _ := add(carol, "team-t", "bob@example.test", "member"); code != 201 { // bob is back, as a member
			t.Fatalf("add bob back: %d", code)
		}
		wait := func(tok, project string) string {
			code, m, raw := submit(tok, "sensitive", "team-t", project)
			if code != 202 || m["status"] != "awaiting_approval" {
				t.Fatalf("submit %s: %d %s", project, code, raw)
			}
			return idOf(m)
		}
		decide := func(tok, id, action string) int {
			code, _, _ := e.call(tok, "POST", "/api/v1/requests/"+id+"/"+action, nil)
			return code
		}

		r1 := wait(dave, "reg1")
		if c := decide(alice, r1, "cancel"); c != 404 { // a former member
			t.Errorf("former member withdrawing: %d", c)
		}
		if c := decide(bob, r1, "cancel"); c != 403 { // a plain member who is not the requester
			t.Errorf("a teammate withdrawing someone else's request: %d", c)
		}
		if c := decide(carol, r1, "cancel"); c != 200 { // an owner
			t.Errorf("an owner withdrawing: %d", c)
		}
		r2 := wait(dave, "reg2")
		if c := decide(dave, r2, "cancel"); c != 200 { // the requester
			t.Errorf("the requester withdrawing: %d", c)
		}

		// Approving is for administrators only: being a team owner is not enough.
		r3 := wait(dave, "reg3")
		if c := decide(carol, r3, "approve"); c != 403 {
			t.Errorf("an owner approving: %d", c)
		}
		if c := decide(admin2, r3, "approve"); c != 200 {
			t.Errorf("an administrator approving: %d", c)
		}
		e.waitStatus(dave, r3, "succeeded")
	})

	t.Run("every membership change is in the audit log", func(t *testing.T) {
		v, raw := e.expect(200, e.admin(), "GET", "/api/v1/audit?limit=500", nil)
		seen := map[string]bool{}
		for _, ent := range v.([]any) {
			seen[get(ent, "action").(string)] = true
		}
		for _, want := range []string{"tenant.create", "tenant.member.add", "tenant.member.role", "tenant.member.remove", "tenant.member.leave"} {
			if !seen[want] {
				t.Errorf("no %s entry in the audit log", want)
			}
		}
		for _, want := range []string{"bob@example.test", "carol@example.test", `"role":"owner"`} {
			if !strings.Contains(raw, want) {
				t.Errorf("the audit log does not say %s", want)
			}
		}
	})
}

// Teams that existed before members were introduced keep their owner.
func TestTeamMembersMigrationKeepsExistingOwners(t *testing.T) {
	url := startPostgres(t)
	key, _ := crypto.GenerateKey()
	box, _ := crypto.NewBox(key)
	ctx := context.Background()
	st, err := store.Open(ctx, url, box)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Bring the schema to the state before this change, with a team in it.
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE TABLE schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"0001_init.sql", "0002_approvals.sql"} {
		sql, err := os.ReadFile("../store/migrations/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, f); err != nil {
			t.Fatal(err)
		}
	}
	var owner string
	if err := conn.QueryRow(ctx, `INSERT INTO users (oidc_sub, email, role) VALUES ('old-owner', 'old@example.test', 'user') RETURNING id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO tenants (name, owner_id) VALUES ('legacy-team', $1)`, owner); err != nil {
		t.Fatal(err)
	}

	if err := st.Migrate(ctx); err != nil { // applies 0003 on top of existing data
		t.Fatal(err)
	}
	var verified bool
	if err := conn.QueryRow(ctx, `SELECT email_verified FROM users WHERE id = $1`, owner).Scan(&verified); err != nil || verified {
		t.Fatalf("existing accounts must not count as verified until they sign in again: %v %v", verified, err)
	}
	role, err := st.TenantRole(ctx, "legacy-team", owner)
	if err != nil || role != store.RoleOwner {
		t.Fatalf("the existing owner must stay an owner: role=%q err=%v", role, err)
	}
	tn, err := st.GetTenant(ctx, "legacy-team", owner, false)
	if err != nil || len(tn.Members) != 1 || tn.Members[0].Email != "old@example.test" {
		t.Fatalf("members after migration: %+v %v", tn.Members, err)
	}
}

// ---- invitations ---------------------------------------------------------------------------------

func TestInvitations(t *testing.T) {
	e := newEnv(t)
	alice, bob, carol := e.alice(), e.bob(), e.token("carol", "devs")

	mk := func(kind, driver, name string, cfg map[string]string) {
		e.expect(201, e.admin(), "POST", "/api/v1/integrations", map[string]any{"kind": kind, "driver": driver, "name": name, "config": cfg})
	}
	mk("gitsource", "fake", "git", map[string]string{"a": "b"})
	mk("cluster", "fake", "cl", map[string]string{"name": "dev"})
	e.expect(201, e.admin(), "POST", "/api/v1/blueprints", map[string]any{
		"name": "open", "git_source": "git", "environments": []map[string]any{{"name": "dev", "cluster": "cl"}}})

	tok := func(sub, email string, mod ...func(*authtest.Claims)) string {
		c := authtest.Claims{Sub: sub, Email: email, Groups: []string{"devs"}}
		for _, m := range mod {
			m(&c)
		}
		return e.idp.Token(c)
	}
	unverified := func(c *authtest.Claims) { c.Unverified = true }
	noClaim := func(c *authtest.Claims) { c.NoVerifiedClaim = true }
	signIn := func(token string) { e.expect(200, token, "GET", "/api/v1/me", nil) }
	submit := func(token, team, project string) int {
		code, _, _ := e.call(token, "POST", "/api/v1/requests", fmt.Sprintf(`{"blueprint":"open","tenant":%q,"projects":[{"name":%q}]}`, team, project))
		return code
	}
	invite := func(token, team, email, role string) (int, string) {
		code, _, raw := e.call(token, "POST", "/api/v1/tenants/"+team+"/members", map[string]string{"email": email, "role": role})
		return code, raw
	}
	invitations := func(token, team string) []any {
		v, _ := e.expect(200, token, "GET", "/api/v1/tenants/"+team, nil)
		list, _ := get(v, "invitations").([]any)
		return list
	}
	roleIn := func(token, team string) string { // "" when not a member
		v, _ := e.expect(200, token, "GET", "/api/v1/tenants", nil)
		for _, tn := range v.([]any) {
			if get(tn, "name") == team {
				return get(tn, "role").(string)
			}
		}
		return ""
	}
	count := func(q string, args ...any) string { return e.rawQuery(q, args...) }

	signIn(alice)
	signIn(bob)
	signIn(carol)
	if code := submit(alice, "inv-team", "app"); code != 202 {
		t.Fatalf("create team: %d", code)
	}
	e.expect(201, alice, "POST", "/api/v1/tenants/inv-team/members", map[string]string{"email": "bob@example.test", "role": "member"})

	t.Run("inviting someone who has not signed in records an invitation", func(t *testing.T) {
		code, raw := invite(alice, "inv-team", "  Newbie@Example.TEST ", "member")
		if code != 202 {
			t.Fatalf("invite: %d %s", code, raw)
		}
		list := invitations(alice, "inv-team")
		if len(list) != 1 || get(list[0], "email") != "newbie@example.test" || get(list[0], "role") != "member" ||
			get(list[0], "invited_by") != "alice@example.test" {
			t.Fatalf("invitations: %v", list)
		}
		// About 30 days to claim it.
		life := count(`SELECT round(extract(epoch FROM (expires_at - created_at)) / 86400)::text FROM tenant_invitations LIMIT 1`)
		if life != "30" {
			t.Errorf("lifetime = %s days", life)
		}
		// Not a member yet, and only owners and administrators see who was invited.
		if got := members(e, alice, "inv-team"); len(got) != 2 {
			t.Errorf("an invitation is not a membership: %v", got)
		}
		if len(invitations(bob, "inv-team")) != 0 {
			t.Error("a plain member must not see open invitations")
		}
		if len(invitations(e.admin(), "inv-team")) != 1 {
			t.Error("administrators see open invitations")
		}
		// Inviting again renews it and can change the role; it stays one invitation.
		if code, _ := invite(alice, "inv-team", "newbie@example.test", "owner"); code != 202 {
			t.Fatalf("re-invite: %d", code)
		}
		list = invitations(alice, "inv-team")
		if len(list) != 1 || get(list[0], "role") != "owner" {
			t.Errorf("re-invitation: %v", list)
		}
	})

	t.Run("it is claimed on the first sign-in with that address", func(t *testing.T) {
		newbie := tok("newbie-sub", "NEWBIE@example.test") // verified by default
		signIn(newbie)
		if got := roleIn(newbie, "inv-team"); got != "owner" {
			t.Fatalf("the invitee should be an owner now, got %q", got)
		}
		if n := len(invitations(alice, "inv-team")); n != 0 {
			t.Errorf("the invitation must be used up: %d left", n)
		}
		if code := submit(newbie, "inv-team", "web"); code != 202 {
			t.Errorf("the new member requesting for the team: %d", code)
		}
		// Nobody else gets in with it, and signing in again changes nothing.
		signIn(newbie)
		if got := members(e, alice, "inv-team"); len(got) != 3 {
			t.Errorf("members: %v", got)
		}
	})

	t.Run("an address the provider did not verify never claims an invitation", func(t *testing.T) {
		invite(alice, "inv-team", "careful@example.test", "member")
		invite(alice, "inv-team", "quiet@example.test", "member")
		careful := tok("careful-sub", "careful@example.test", unverified)
		quiet := tok("quiet-sub", "quiet@example.test", noClaim) // some providers send no claim at all
		signIn(careful)
		signIn(quiet)
		if roleIn(careful, "inv-team") != "" || roleIn(quiet, "inv-team") != "" {
			t.Fatal("an unverified address must not be able to claim an invitation")
		}
		if n := len(invitations(alice, "inv-team")); n != 2 {
			t.Errorf("the invitations must stay open: %d", n)
		}
		// Once the provider verifies the address, the next sign-in joins.
		signIn(tok("careful-sub", "careful@example.test"))
		if roleIn(careful, "inv-team") != "member" {
			t.Error("after verification the same person should be able to join")
		}
	})

	t.Run("an attacker registering someone else's address gets nothing", func(t *testing.T) {
		attacker := tok("attacker-sub", "victim@example.test", unverified)
		signIn(attacker) // an account with the victim's address exists, unverified

		code, raw := invite(alice, "inv-team", "victim@example.test", "owner")
		if code != 202 {
			t.Fatalf("the attacker's unverified account must not match: %d %s", code, raw)
		}
		if roleIn(attacker, "inv-team") != "" {
			t.Fatal("the attacker was added")
		}
		e.expect(404, attacker, "GET", "/api/v1/tenants/inv-team", nil)

		victim := tok("victim-sub", "victim@example.test")
		signIn(victim)
		if roleIn(victim, "inv-team") != "owner" {
			t.Error("the real owner of the address should have joined")
		}
		signIn(attacker)
		if roleIn(attacker, "inv-team") != "" {
			t.Error("the attacker must not get in later either")
		}
	})

	t.Run("with trust switched on, unverified addresses count", func(t *testing.T) {
		e.st.SetTrustUnverifiedEmail(true)
		defer e.st.SetTrustUnverifiedEmail(false)

		invite(alice, "inv-team", "lenient@example.test", "member")
		lenient := tok("lenient-sub", "lenient@example.test", unverified)
		signIn(lenient)
		if roleIn(lenient, "inv-team") != "member" {
			t.Error("with PLINTH_TRUST_UNVERIFIED_EMAIL an unverified address should claim")
		}
		// An existing unverified account can now be added directly too.
		signIn(tok("direct-sub", "direct@example.test", unverified))
		if code, _ := invite(alice, "inv-team", "direct@example.test", "member"); code != 201 {
			t.Errorf("adding an existing unverified account: %d", code)
		}
	})

	t.Run("without trust, an existing unverified account is invited rather than added", func(t *testing.T) {
		signIn(tok("shy-sub", "shy@example.test", unverified))
		if code, _ := invite(alice, "inv-team", "shy@example.test", "member"); code != 202 {
			t.Errorf("unverified account: %d", code)
		}
	})

	t.Run("owners can revoke an invitation; nobody else can", func(t *testing.T) {
		invite(alice, "inv-team", "late@example.test", "member")
		list := invitations(alice, "inv-team")
		var id string
		for _, inv := range list {
			if get(inv, "email") == "late@example.test" {
				id = get(inv, "id").(string)
			}
		}
		if id == "" {
			t.Fatal("invitation not found")
		}
		e.expect(403, bob, "DELETE", "/api/v1/tenants/inv-team/invitations/"+id, nil)   // a member
		e.expect(404, carol, "DELETE", "/api/v1/tenants/inv-team/invitations/"+id, nil) // an outsider
		e.expect(204, alice, "DELETE", "/api/v1/tenants/inv-team/invitations/"+id, nil)
		e.expect(404, alice, "DELETE", "/api/v1/tenants/inv-team/invitations/"+id, nil)
		e.expect(404, alice, "DELETE", "/api/v1/tenants/inv-team/invitations/not-a-uuid", nil)

		signIn(tok("late-sub", "late@example.test"))
		if roleIn(tok("late-sub", "late@example.test"), "inv-team") != "" {
			t.Error("a revoked invitation must not be claimable")
		}
	})

	t.Run("an expired invitation grants nothing and disappears", func(t *testing.T) {
		invite(alice, "inv-team", "stale@example.test", "member")
		e.exec(`UPDATE tenant_invitations SET expires_at = now() - interval '1 minute' WHERE email = 'stale@example.test'`)
		for _, inv := range invitations(alice, "inv-team") {
			if get(inv, "email") == "stale@example.test" {
				t.Error("an expired invitation is still listed")
			}
		}
		stale := tok("stale-sub", "stale@example.test")
		signIn(stale)
		if roleIn(stale, "inv-team") != "" {
			t.Error("an expired invitation must not be claimable")
		}
		if n := count(`SELECT count(*)::text FROM tenant_invitations WHERE email = 'stale@example.test'`); n != "0" {
			t.Errorf("the expired invitation should be gone, %s left", n)
		}
	})

	t.Run("only owners invite", func(t *testing.T) {
		if code, raw := invite(bob, "inv-team", "x@example.test", "member"); code != 403 || !strings.Contains(raw, "only team owners") {
			t.Errorf("a member inviting: %d %s", code, raw)
		}
		if code, _ := invite(carol, "inv-team", "x@example.test", "member"); code != 404 {
			t.Errorf("an outsider inviting: %d", code)
		}
		if code, _ := invite(alice, "inv-team", "not-an-address", "member"); code != 400 {
			t.Errorf("a bad address: %d", code)
		}
	})

	t.Run("a team can only have so many open invitations", func(t *testing.T) {
		if code := submit(alice, "inv-cap", "app"); code != 202 {
			t.Fatal(code)
		}
		for i := 0; i < store.MaxOpenInvitations; i++ {
			if code, raw := invite(alice, "inv-cap", fmt.Sprintf("person%d@example.test", i), "member"); code != 202 {
				t.Fatalf("invitation %d: %d %s", i, code, raw)
			}
		}
		code, raw := invite(alice, "inv-cap", "one-too-many@example.test", "member")
		if code != 409 || !strings.Contains(raw, "50 open invitations") {
			t.Errorf("past the cap: %d %s", code, raw)
		}
		if code, _ := invite(alice, "inv-cap", "person0@example.test", "owner"); code != 202 {
			t.Errorf("renewing an existing invitation at the cap: %d", code)
		}
	})

	t.Run("simultaneous sign-ins claim it exactly once", func(t *testing.T) {
		invite(alice, "inv-team", "twice@example.test", "member")
		twice := tok("twice-sub", "twice@example.test")
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if code, _, _ := e.call(twice, "GET", "/api/v1/me", nil); code != 200 {
					t.Errorf("sign-in: %d", code)
				}
			}()
		}
		wg.Wait()
		if roleIn(twice, "inv-team") != "member" {
			t.Fatal("not a member")
		}
		if n := count(`SELECT count(*)::text FROM audit_log WHERE action = 'tenant.invitation.accept' AND actor = 'twice@example.test'`); n != "1" {
			t.Errorf("claimed %s times", n)
		}
		if n := count(`SELECT count(*)::text FROM tenant_members m JOIN users u ON u.id = m.user_id WHERE u.email = 'twice@example.test'`); n != "1" {
			t.Errorf("memberships: %s", n)
		}
	})

	t.Run("invitations are in the audit log", func(t *testing.T) {
		v, raw := e.expect(200, e.admin(), "GET", "/api/v1/audit?limit=500", nil)
		seen := map[string]bool{}
		for _, ent := range v.([]any) {
			seen[get(ent, "action").(string)] = true
		}
		for _, want := range []string{"tenant.invitation.create", "tenant.invitation.accept", "tenant.invitation.revoke"} {
			if !seen[want] {
				t.Errorf("no %s entry", want)
			}
		}
		if !strings.Contains(raw, "late@example.test") || !strings.Contains(raw, "newbie@example.test") {
			t.Error("the audit log should say whom it concerned")
		}
	})
}

// members lists a team's members as e-mail -> role, as seen by token.
func members(e *env, token, team string) map[string]string {
	v, _ := e.expect(200, token, "GET", "/api/v1/tenants/"+team, nil)
	out := map[string]string{}
	for _, m := range get(v, "members").([]any) {
		out[get(m, "email").(string)] = get(m, "role").(string)
	}
	return out
}
