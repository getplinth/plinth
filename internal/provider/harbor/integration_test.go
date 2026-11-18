//go:build integration

package harbor

// Runs the driver against a real Harbor. By default it starts a disposable
// one with scripts/harbor.sh (needs Docker; slow, amd64 images). To use an
// existing Harbor instead set PLINTH_TEST_HARBOR_URL, PLINTH_TEST_HARBOR_USER
// and PLINTH_TEST_HARBOR_PASSWORD (a system administrator).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/getplinth/plinth/internal/provider"
)

func harborAccess(t *testing.T) (url, user, pass string) {
	t.Helper()
	if u := os.Getenv("PLINTH_TEST_HARBOR_URL"); u != "" {
		return u, os.Getenv("PLINTH_TEST_HARBOR_USER"), os.Getenv("PLINTH_TEST_HARBOR_PASSWORD")
	}
	for _, bin := range []string{"docker"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	var out, errb bytes.Buffer
	cmd := exec.Command("../../../scripts/harbor.sh", "up")
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("harbor.sh up: %v\n%s", err, tail(errb.String(), 1500))
	}
	vals := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			vals[k] = v
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("../../../scripts/harbor.sh", "down", vals["HARBOR_DIR"]).Run()
	})
	return vals["HARBOR_URL"], vals["HARBOR_ADMIN_USER"], vals["HARBOR_ADMIN_PASSWORD"]
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// rawDriver builds an authenticated client for arbitrary API calls as `user`.
func asUser(t *testing.T, base, user, pass string) *Driver {
	t.Helper()
	d, err := FromConfig(map[string]string{"base_url": base, "allow_http": "true", "username": user, "password": pass})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// scopeActions asks Harbor's token service what a robot may do on a repository.
// NOTE: for invalid credentials Harbor still returns 200 with no actions, so
// callers must check the returned actions rather than the status code.
func scopeActions(t *testing.T, base, user, pass, repo string) (granted []string, status int) {
	t.Helper()
	u := fmt.Sprintf("%s/service/token?service=harbor-registry&scope=repository:%s:pull,push,delete", base, repo)
	req, _ := http.NewRequest("GET", u, nil)
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(body.Token, ".")
	if len(parts) < 2 {
		t.Fatalf("not a JWT: %q", body.Token)
	}
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
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	for _, a := range claims.Access {
		if a.Name == repo {
			granted = append(granted, a.Actions...)
		}
	}
	return granted, 200
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestHarborDriver(t *testing.T) {
	base, adminUser, adminPass := harborAccess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin := asUser(t, base, adminUser, adminPass)

	// Dedicated identity for Plinth (system admin), plus an unrelated user.
	mkUser := func(name string, sysadmin bool) {
		err := admin.do(ctx, "POST", "/users", map[string]string{
			"username": name, "email": name + "@example.test", "realname": name, "password": "Itest-" + name + "-Pw1",
		}, nil)
		if err != nil && !isStatus(err, 409) {
			t.Fatalf("create user %s: %v", name, err)
		}
		if sysadmin {
			var u struct {
				ID int `json:"user_id"`
			}
			if err := admin.do(ctx, "GET", "/users/search?username="+name, nil, &[]any{}); err != nil {
				t.Fatal(err)
			}
			var found []struct {
				ID       int    `json:"user_id"`
				Username string `json:"username"`
			}
			if err := admin.do(ctx, "GET", "/users/search?username="+name, nil, &found); err != nil || len(found) == 0 {
				t.Fatalf("find user %s: %v", name, err)
			}
			u.ID = found[0].ID
			if err := admin.do(ctx, "PUT", fmt.Sprintf("/users/%d/sysadmin", u.ID), map[string]bool{"sysadmin_flag": true}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkUser("plinth", true)
	mkUser("alice", false)
	d := asUser(t, base, "plinth", "Itest-plinth-Pw1")

	if err := d.Validate(ctx); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := asUser(t, base, "plinth", "wrong-password-xyz").Validate(ctx); err == nil || strings.Contains(err.Error(), "wrong-password-xyz") {
		t.Fatalf("expected a rejected-credentials error without the password, got %v", err)
	}

	const proj = "team-a-shop"
	var cred provider.Credential

	t.Run("project is created once, private", func(t *testing.T) {
		o, err := d.EnsureRepository(ctx, provider.RegistrySpec{Repo: proj})
		if err != nil || o["created"] != "true" {
			t.Fatalf("%v %v", o, err)
		}
		if !strings.HasSuffix(o["image_prefix"], "/"+proj) {
			t.Errorf("image_prefix: %v", o)
		}
		o, err = d.EnsureRepository(ctx, provider.RegistrySpec{Repo: proj})
		if err != nil || o["created"] != "false" {
			t.Fatalf("second run: %v %v", o, err)
		}
		var p struct {
			Metadata struct {
				Public string `json:"public"`
			} `json:"metadata"`
		}
		if err := d.do(ctx, "GET", "/projects/"+proj, nil, &p); err != nil || p.Metadata.Public != "false" {
			t.Fatalf("project must be private: %+v %v", p, err)
		}
	})

	t.Run("credential is limited to push and pull on its own project", func(t *testing.T) {
		is, err := d.EnsureCredential(ctx, provider.RegistrySpec{Repo: proj})
		if err != nil || is.Credential == nil {
			t.Fatalf("EnsureCredential: %v", err)
		}
		cred = is.Credential
		if cred["username"] != "robot$"+proj+"+ci" {
			t.Errorf("username: %q", cred["username"])
		}
		got, status := scopeActions(t, base, cred["username"], cred["password"], proj+"/app")
		if status != 200 || !has(got, "push") || !has(got, "pull") {
			t.Fatalf("own project: status %d actions %v", status, got)
		}
		if has(got, "delete") {
			t.Errorf("robot must not be able to delete: %v", got)
		}
		// Another tenant's project must be out of reach.
		if _, err := d.EnsureRepository(ctx, provider.RegistrySpec{Repo: "team-b-blog"}); err != nil {
			t.Fatal(err)
		}
		other, _ := scopeActions(t, base, cred["username"], cred["password"], "team-b-blog/app")
		if has(other, "push") || has(other, "pull") {
			t.Errorf("robot reaches another project: %v", other)
		}
	})

	t.Run("bad credentials grant nothing", func(t *testing.T) {
		got, _ := scopeActions(t, base, cred["username"], "totally-wrong-secret", proj+"/app")
		if len(got) != 0 {
			t.Fatalf("wrong secret was granted %v", got)
		}
	})

	t.Run("re-run does not change the secret", func(t *testing.T) {
		is, err := d.EnsureCredential(ctx, provider.RegistrySpec{Repo: proj})
		if err != nil {
			t.Fatal(err)
		}
		if is.Credential != nil {
			t.Fatal("second run must not issue a credential")
		}
		if got, status := scopeActions(t, base, cred["username"], cred["password"], proj+"/app"); status != 200 || !has(got, "push") {
			t.Fatalf("existing secret stopped working: %d %v", status, got)
		}
	})

	t.Run("rotate replaces the secret", func(t *testing.T) {
		is, err := d.EnsureCredential(ctx, provider.RegistrySpec{Repo: proj, Rotate: true})
		if err != nil || is.Credential == nil {
			t.Fatalf("rotate: %v", err)
		}
		// Harbor's token endpoint answers 200 with an empty permission list for
		// bad credentials, so judge by the granted actions, not the status.
		if got, _ := scopeActions(t, base, cred["username"], cred["password"], proj+"/app"); has(got, "push") || has(got, "pull") {
			t.Fatalf("old secret must stop working after rotation, still granted %v", got)
		}
		if got, status := scopeActions(t, base, is.Credential["username"], is.Credential["password"], proj+"/app"); status != 200 || !has(got, "push") {
			t.Fatalf("new secret: %d %v", status, got)
		}
		cred = is.Credential
	})

	t.Run("an over-privileged robot is replaced", func(t *testing.T) {
		var p struct {
			ID int `json:"project_id"`
		}
		if err := d.do(ctx, "GET", "/projects/"+proj, nil, &p); err != nil {
			t.Fatal(err)
		}
		r, err := d.findRobot(ctx, &project{ID: p.ID, Name: proj})
		if err != nil || r == nil {
			t.Fatalf("find robot: %v", err)
		}
		if err := d.do(ctx, "DELETE", fmt.Sprintf("/robots/%d", r.ID), nil, nil); err != nil {
			t.Fatal(err)
		}
		var made struct {
			Secret string `json:"secret"`
		}
		err = d.do(ctx, "POST", "/robots", map[string]any{
			"name": "ci", "duration": -1, "level": "project",
			"permissions": []map[string]any{{"kind": "project", "namespace": proj, "access": []map[string]string{
				{"resource": "repository", "action": "push"}, {"resource": "repository", "action": "pull"},
				{"resource": "repository", "action": "delete"},
			}}},
		}, &made)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := scopeActions(t, base, "robot$"+proj+"+ci", made.Secret, proj+"/app"); !has(got, "delete") {
			t.Skipf("test setup: the broad robot did not get delete (%v)", got)
		}
		is, err := d.EnsureCredential(ctx, provider.RegistrySpec{Repo: proj})
		if err != nil || is.Credential == nil {
			t.Fatalf("an over-privileged robot must be replaced with a new credential: %v", err)
		}
		if got, _ := scopeActions(t, base, is.Credential["username"], is.Credential["password"], proj+"/app"); has(got, "delete") || !has(got, "push") {
			t.Fatalf("replacement has wrong scope: %v", got)
		}
	})

	t.Run("projects of other users are refused unless adopted", func(t *testing.T) {
		alice := asUser(t, base, "alice", "Itest-alice-Pw1")
		// 409 means a previous run on the same Harbor already created it.
		if err := alice.do(ctx, "POST", "/projects", map[string]any{"project_name": "alices-project"}, nil); err != nil && !isStatus(err, 409) {
			t.Fatalf("alice creates project: %v", err)
		}
		spec := provider.RegistrySpec{Repo: "alices-project"}
		if _, err := d.EnsureRepository(ctx, spec); err == nil || !strings.Contains(err.Error(), "adopt_existing") {
			t.Fatalf("expected refusal, got %v", err)
		}
		if _, err := d.EnsureCredential(ctx, spec); err == nil {
			t.Fatal("must not issue credentials for someone else's project")
		}
		if err := d.DeleteRepository(ctx, spec); err == nil {
			t.Fatal("must not delete someone else's project")
		}
		adopting := asUser(t, base, "plinth", "Itest-plinth-Pw1")
		adopting.adopt = true
		if _, err := adopting.EnsureRepository(ctx, spec); err != nil {
			t.Fatalf("adopt: %v", err)
		}
	})

	t.Run("hostile names are rejected before any request", func(t *testing.T) {
		for _, n := range []string{"../library", "A B", "x/y", ""} {
			if _, err := d.EnsureRepository(ctx, provider.RegistrySpec{Repo: n}); err == nil {
				t.Errorf("%q should be rejected", n)
			}
		}
	})

	t.Run("secrets never appear in errors or outputs", func(t *testing.T) {
		is, err := d.EnsureCredential(ctx, provider.RegistrySpec{Repo: proj, Rotate: true})
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range is.Outputs {
			if strings.Contains(v, is.Credential["password"]) {
				t.Errorf("secret leaked into output %q", k)
			}
		}
		if s := fmt.Sprint(is.Credential); strings.Contains(s, is.Credential["password"]) {
			t.Error("Credential prints its secret")
		}
	})

	t.Run("delete removes managed projects and is idempotent", func(t *testing.T) {
		for _, n := range []string{proj, "team-b-blog"} {
			for i := 0; i < 2; i++ {
				if err := d.DeleteRepository(ctx, provider.RegistrySpec{Repo: n}); err != nil {
					t.Fatalf("delete %s #%d: %v", n, i+1, err)
				}
			}
			if _, err := d.getProject(ctx, n); !isStatus(err, 404) {
				t.Errorf("%s still exists: %v", n, err)
			}
		}
	})
}
