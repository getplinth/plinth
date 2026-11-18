//go:build integration

package gitea

// Runs the driver against a disposable Gitea container (needs Docker and git).
// Versions come from PLINTH_TEST_GITEA_VERSIONS (default "1.23"). The
// container is removed when the test finishes.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/getplinth/plinth/internal/provider"
)

func versions() []string {
	v := os.Getenv("PLINTH_TEST_GITEA_VERSIONS")
	if v == "" {
		v = "1.23"
	}
	return strings.Split(v, ",")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// startGitea launches a throwaway instance and returns its URL and an admin token.
func startGitea(t *testing.T, version string) (baseURL, token string) {
	t.Helper()
	for _, bin := range []string{"docker", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	port := freePort(t)
	baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	name := fmt.Sprintf("plinth-itest-gitea-%06d", rand.Intn(1_000_000))
	out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:3000", port),
		"-e", "GITEA__security__INSTALL_LOCK=true",
		"-e", "GITEA__database__DB_TYPE=sqlite3",
		"-e", "GITEA__server__ROOT_URL="+baseURL+"/",
		"-e", "GITEA__server__DOMAIN=127.0.0.1",
		"-e", "GITEA__actions__ENABLED=true",
		"-e", "GITEA__service__DISABLE_REGISTRATION=true",
		"gitea/gitea:"+version).CombinedOutput()
	if err != nil {
		t.Skipf("cannot start gitea:%s: %v: %s", version, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	deadline := time.Now().Add(150 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/api/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("gitea:%s not ready", version)
		}
		time.Sleep(2 * time.Second)
	}
	docker(t, "exec", "-u", "git", name, "gitea", "admin", "user", "create", "--admin",
		"--username", "plinth-admin", "--password", "itest-Admin-pw-123", "--email", "admin@example.test",
		"--must-change-password=false")
	token = docker(t, "exec", "-u", "git", name, "gitea", "admin", "user", "generate-access-token",
		"--username", "plinth-admin", "--token-name", "itest", "--scopes", "all", "--raw")
	if token == "" {
		t.Fatal("no token generated")
	}
	return baseURL, token
}

func TestGiteaDriver(t *testing.T) {
	for _, v := range versions() {
		v := v
		t.Run("gitea"+v, func(t *testing.T) {
			base, token := startGitea(t, v)
			runSuite(t, base, token)
		})
	}
}

// ---- helpers over the raw API ------------------------------------------------

type env struct {
	t   *testing.T
	ctx context.Context
	d   *Driver
}

func (e *env) api(method, path string, body, out any) error {
	return e.d.c.do(e.ctx, method, path, body, out)
}

func (e *env) must(method, path string, body, out any) {
	e.t.Helper()
	if err := e.api(method, path, body, out); err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
}

func (e *env) raw(repo, ref, file string) (string, int) {
	e.t.Helper()
	u := fmt.Sprintf("%s/api/v1/repos/%s/raw/%s?ref=%s", e.d.c.base, repo, file, ref)
	req, _ := http.NewRequestWithContext(e.ctx, "GET", u, nil)
	req.Header.Set("Authorization", "token "+e.d.c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func (e *env) putFile(repo, file, branch, content string) {
	e.t.Helper()
	e.must("POST", "/repos/"+repo+"/contents/"+file, map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte(content)), "message": "add " + file, "branch": branch,
	}, nil)
}

func runSuite(t *testing.T, base, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	mk := func(adopt bool) *Driver {
		d, err := FromConfig(map[string]string{"base_url": base, "token": token,
			"allow_http": "true", "adopt_existing": fmt.Sprint(adopt)})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := mk(false)
	e := &env{t: t, ctx: ctx, d: d}

	if err := d.Validate(ctx); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// The golden template: two branches, a tag, a text placeholder, a binary file.
	e.must("POST", "/orgs", map[string]any{"username": "platform"}, nil)
	e.must("POST", "/orgs/platform/repos", map[string]any{"name": "app-template", "auto_init": true, "default_branch": "main"}, nil)
	e.putFile("platform/app-template", "README.md.tpl", "main", "# PROJECT_NAME\n")
	e.putFile("platform/app-template", "k8s/deployment.yaml", "main", "name: PROJECT_NAME\n")
	e.putFile("platform/app-template", "logo.bin", "main", "PROJECT_NAME\x00\x01\x02")
	e.must("POST", "/repos/platform/app-template/branches", map[string]any{"new_branch_name": "develop", "old_branch_name": "main"}, nil)
	e.putFile("platform/app-template", "develop-only.txt", "develop", "dev PROJECT_NAME\n")
	e.must("POST", "/repos/platform/app-template/tags", map[string]any{"tag_name": "v1.0.0", "target": "main"}, nil)

	group := provider.GroupSpec{Name: "team-a"}
	repo := provider.RepoSpec{
		Group: group, Name: "shop", Description: "Shop", Private: true,
		TemplateRepo: "platform/app-template", TemplateVars: map[string]string{"PROJECT_NAME": "shop"},
		ExtraBranches: []string{"staging"},
	}

	t.Run("group is created once", func(t *testing.T) {
		o, err := d.EnsureGroup(ctx, group)
		if err != nil || o["created"] != "true" {
			t.Fatalf("%v %v", o, err)
		}
		o, err = d.EnsureGroup(ctx, group)
		if err != nil || o["created"] != "false" {
			t.Fatalf("second run: %v %v", o, err)
		}
	})

	t.Run("repository is created and populated from the template", func(t *testing.T) {
		o, err := d.EnsureRepository(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if o["created"] != "true" || o["populated"] != "true" || o["default_branch"] != "main" {
			t.Fatalf("outputs: %v", o)
		}
		if body, code := e.raw("team-a/shop", "main", "k8s/deployment.yaml"); code != 200 || body != "name: shop\n" {
			t.Errorf("main substitution: %d %q", code, body)
		}
		if body, _ := e.raw("team-a/shop", "develop", "develop-only.txt"); body != "dev shop\n" {
			t.Errorf("develop branch not copied/substituted: %q", body)
		}
		if body, _ := e.raw("team-a/shop", "main", "logo.bin"); body != "PROJECT_NAME\x00\x01\x02" {
			t.Errorf("binary file modified: %q", body)
		}
		var tags []struct {
			Name string `json:"name"`
		}
		e.must("GET", "/repos/team-a/shop/tags", nil, &tags)
		if len(tags) != 1 || tags[0].Name != "v1.0.0" {
			t.Errorf("tags not copied: %v", tags)
		}
		if err := e.api("GET", "/repos/team-a/shop/branches/staging", nil, nil); err != nil {
			t.Errorf("extra branch missing: %v", err)
		}
		var r struct {
			Private bool `json:"private"`
		}
		e.must("GET", "/repos/team-a/shop", nil, &r)
		if !r.Private {
			t.Error("repository should be private")
		}
	})

	t.Run("re-run never touches existing content", func(t *testing.T) {
		e.putFile("team-a/shop", "developer-work.txt", "main", "mine\n")
		o, err := d.EnsureRepository(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if o["created"] != "false" || o["populated"] != "false" {
			t.Fatalf("outputs: %v", o)
		}
		if body, code := e.raw("team-a/shop", "main", "developer-work.txt"); code != 200 || body != "mine\n" {
			t.Errorf("developer work lost: %d %q", code, body)
		}
	})

	t.Run("a retry populates a repository left empty by a crash", func(t *testing.T) {
		e.must("POST", "/orgs/team-a/repos", map[string]any{"name": "half-done"}, nil)
		e.must("PUT", "/repos/team-a/half-done/topics/"+managedTopic, nil, nil)
		r := repo
		r.Name = "half-done"
		o, err := d.EnsureRepository(ctx, r)
		if err != nil || o["populated"] != "true" {
			t.Fatalf("%v %v", o, err)
		}
		if body, _ := e.raw("team-a/half-done", "main", "k8s/deployment.yaml"); body != "name: shop\n" {
			t.Errorf("not populated: %q", body)
		}
	})

	t.Run("pre-existing repositories are refused unless adopted", func(t *testing.T) {
		e.must("POST", "/orgs/team-a/repos", map[string]any{"name": "legacy-app"}, nil)
		r := provider.RepoSpec{Group: group, Name: "legacy-app"}
		if _, err := d.EnsureRepository(ctx, r); err == nil || !strings.Contains(err.Error(), "not managed") {
			t.Fatalf("expected refusal, got %v", err)
		}
		if err := d.SetVariable(ctx, r, provider.VariableSpec{Key: "HOST", Value: "x"}); err == nil {
			t.Fatal("must not write variables into an unmanaged repository")
		}
		if err := d.DeleteRepository(ctx, r); err == nil {
			t.Fatal("must not delete an unmanaged repository")
		}
		if _, err := mk(true).EnsureRepository(ctx, r); err != nil {
			t.Fatalf("adopt: %v", err)
		}
		if err := d.SetVariable(ctx, r, provider.VariableSpec{Key: "HOST", Value: "x"}); err != nil {
			t.Fatalf("after adoption: %v", err)
		}
	})

	t.Run("variables and secrets are upserted", func(t *testing.T) {
		set := func(k, v string, masked bool) error {
			return d.SetVariable(ctx, repo, provider.VariableSpec{Key: k, Value: v, Masked: masked, Scope: "DEV"})
		}
		if err := set("DB_HOST_DEV", "db.dev.example", false); err != nil {
			t.Fatal(err)
		}
		if err := set("DB_HOST_DEV", "db2.dev.example", false); err != nil { // update path
			t.Fatal(err)
		}
		var v struct {
			Data string `json:"data"`
		}
		e.must("GET", "/repos/team-a/shop/actions/variables/DB_HOST_DEV", nil, &v)
		if v.Data != "db2.dev.example" {
			t.Errorf("variable = %q", v.Data)
		}
		if err := set("DB_PASS_DEV", "s3cret-value", true); err != nil {
			t.Fatal(err)
		}
		if err := set("DB_PASS_DEV", "rotated-value", true); err != nil { // overwrite
			t.Fatal(err)
		}
		var secrets []struct {
			Name string `json:"name"`
		}
		e.must("GET", "/repos/team-a/shop/actions/secrets", nil, &secrets)
		found := false
		for _, s := range secrets {
			found = found || s.Name == "DB_PASS_DEV"
		}
		if !found {
			t.Errorf("secret missing: %v", secrets)
		}
		for _, k := range []string{"DB_HOST_DEV"} {
			var vv struct {
				Data string `json:"data"`
			}
			_ = e.api("GET", "/repos/team-a/shop/actions/variables/"+k, nil, &vv)
			if strings.Contains(vv.Data, "s3cret") {
				t.Error("secret value leaked into a plain variable")
			}
		}
	})

	t.Run("tokens and secrets never appear in errors", func(t *testing.T) {
		bad, _ := FromConfig(map[string]string{"base_url": base, "token": "bad-token-xyz", "allow_http": "true"})
		err := bad.Validate(ctx)
		if err == nil || strings.Contains(err.Error(), "bad-token-xyz") {
			t.Fatalf("expected an auth error without the token, got %v", err)
		}
		err = d.SetVariable(ctx, provider.RepoSpec{Group: group, Name: "does-not-exist"}, provider.VariableSpec{Key: "K", Value: "leaky-secret-value", Masked: true})
		if err == nil || strings.Contains(err.Error(), "leaky-secret-value") || strings.Contains(err.Error(), token) {
			t.Fatalf("error leaks: %v", err)
		}
	})

	t.Run("delete removes managed repositories and is idempotent", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			if err := d.DeleteRepository(ctx, provider.RepoSpec{Group: group, Name: "half-done"}); err != nil {
				t.Fatalf("delete #%d: %v", i+1, err)
			}
		}
		if err := e.api("GET", "/repos/team-a/half-done", nil, nil); !isStatus(err, 404) {
			t.Fatalf("repository still exists: %v", err)
		}
	})
}
