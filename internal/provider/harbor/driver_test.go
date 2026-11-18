package harbor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/getplinth/plinth/internal/provider"
)

func TestFromConfig(t *testing.T) {
	good := map[string]string{"base_url": "https://harbor.example.com", "username": "plinth", "password": "pw"}
	if _, err := FromConfig(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]string){
		"no password":    func(m map[string]string) { delete(m, "password") },
		"http":           func(m map[string]string) { m["base_url"] = "http://harbor.example.com" },
		"creds in url":   func(m map[string]string) { m["base_url"] = "https://u:p@harbor.example.com" },
		"bad ca":         func(m map[string]string) { m["ca_cert"] = "nope" },
		"bad robot name": func(m map[string]string) { m["robot_name"] = "Bad Name" },
		"bad ttl":        func(m map[string]string) { m["robot_ttl_days"] = "-3" },
	}
	for name, mutate := range cases {
		cfg := map[string]string{}
		for k, v := range good {
			cfg[k] = v
		}
		mutate(cfg)
		if _, err := FromConfig(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestProjectNames(t *testing.T) {
	for _, n := range []string{"team-a-shop", "a", "a.b_c-d", "x1"} {
		if err := checkProject(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	for _, n := range []string{"", "-a", "a-", "A", "a b", "a/b", "../x", "a--b", strings.Repeat("a", 256)} {
		if err := checkProject(n); err == nil {
			t.Errorf("%q should be rejected", n)
		}
	}
}

func rob(project string, actions ...string) *robot {
	r := &robot{Name: "robot$" + project + "+ci"}
	r.Permissions = append(r.Permissions, struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace"`
		Access    []struct {
			Resource string `json:"resource"`
			Action   string `json:"action"`
			Effect   string `json:"effect"`
		} `json:"access"`
	}{Kind: "project", Namespace: project})
	for _, a := range actions {
		r.Permissions[0].Access = append(r.Permissions[0].Access, struct {
			Resource string `json:"resource"`
			Action   string `json:"action"`
			Effect   string `json:"effect"`
		}{Resource: "repository", Action: a, Effect: "allow"})
	}
	return r
}

func TestOnlyPushPull(t *testing.T) {
	if !onlyPushPull(rob("p", "push", "pull"), "p") || !onlyPushPull(rob("p", "pull", "push"), "p") {
		t.Fatal("exact push+pull must be accepted")
	}
	for name, r := range map[string]*robot{
		"pull only":      rob("p", "pull"),
		"extra delete":   rob("p", "push", "pull", "delete"),
		"wrong project":  rob("other", "push", "pull"),
		"no permissions": {Name: "x"},
	} {
		if onlyPushPull(r, "p") {
			t.Errorf("%s must not be accepted", name)
		}
	}
	d := rob("p", "push", "pull")
	d.Disable = true
	if onlyPushPull(d, "p") {
		t.Error("disabled robot must not be accepted")
	}
}

// fakeHarbor is a minimal in-memory Harbor used to test the control flow
// (the integration test covers the real API).
type fakeHarbor struct {
	mu       sync.Mutex
	projects map[string]string // name -> owner
	robots   map[string]bool
	created  int
}

func (f *fakeHarbor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, _, _ := r.BasicAuth()
	path := strings.TrimPrefix(r.URL.Path, "/api/v2.0")
	switch {
	case r.Method == "GET" && strings.HasPrefix(path, "/projects/"):
		name := strings.TrimPrefix(path, "/projects/")
		owner, ok := f.projects[name]
		if !ok {
			http.Error(w, `{"errors":[{"message":"not found"}]}`, 404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"project_id": 7, "name": name, "owner_name": owner})
	case r.Method == "POST" && path == "/projects":
		var b struct {
			Name string `json:"project_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.projects[b.Name] = user
		f.created++
		w.WriteHeader(201)
	case r.Method == "GET" && path == "/robots":
		_ = json.NewEncoder(w).Encode([]any{})
	case r.Method == "POST" && path == "/robots":
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "robot$p+ci", "secret": "fake-secret-value"})
	default:
		http.Error(w, "unexpected "+r.Method+" "+path, 500)
	}
}

func newFake(t *testing.T, owners map[string]string) (*Driver, *fakeHarbor) {
	t.Helper()
	f := &fakeHarbor{projects: owners, robots: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	d, err := FromConfig(map[string]string{"base_url": srv.URL, "allow_http": "true", "username": "plinth", "password": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	return d, f
}

func TestEnsureRepositoryCreatesAndReuses(t *testing.T) {
	d, f := newFake(t, map[string]string{})
	ctx := context.Background()
	o, err := d.EnsureRepository(ctx, provider.RegistrySpec{Repo: "team-a-shop"})
	if err != nil || o["created"] != "true" || !strings.HasSuffix(o["image_prefix"], "/team-a-shop") {
		t.Fatalf("%v %v", o, err)
	}
	o, err = d.EnsureRepository(ctx, provider.RegistrySpec{Repo: "team-a-shop"})
	if err != nil || o["created"] != "false" || f.created != 1 {
		t.Fatalf("second run: %v %v created=%d", o, err, f.created)
	}
}

func TestProjectsOfOthersAreRefusedUnlessAdopted(t *testing.T) {
	d, _ := newFake(t, map[string]string{"theirs": "alice"})
	ctx := context.Background()
	if _, err := d.EnsureRepository(ctx, provider.RegistrySpec{Repo: "theirs"}); err == nil {
		t.Fatal("must refuse a project created by someone else")
	}
	if _, err := d.EnsureCredential(ctx, provider.RegistrySpec{Repo: "theirs"}); err == nil {
		t.Fatal("must not issue credentials for an unmanaged project")
	}
	if err := d.DeleteRepository(ctx, provider.RegistrySpec{Repo: "theirs"}); err == nil {
		t.Fatal("must not delete an unmanaged project")
	}
	d.adopt = true
	if _, err := d.EnsureRepository(ctx, provider.RegistrySpec{Repo: "theirs"}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
}

func TestCredentialSecretNotInOutputsOrErrors(t *testing.T) {
	d, _ := newFake(t, map[string]string{"mine": "plinth"})
	is, err := d.EnsureCredential(context.Background(), provider.RegistrySpec{Repo: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	if is.Credential["password"] != "fake-secret-value" {
		t.Fatalf("credential: %v", is.Credential != nil)
	}
	for k, v := range is.Outputs {
		if strings.Contains(v, "fake-secret-value") {
			t.Errorf("secret leaked into output %q", k)
		}
	}
}

func TestRegisteredAsRegistryDriver(t *testing.T) {
	_, err := provider.New(provider.KindRegistry, "harbor", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("driver should be registered and validate config, got %v", err)
	}
}
