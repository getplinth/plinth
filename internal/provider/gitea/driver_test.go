package gitea

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getplinth/plinth/internal/provider"
)

func TestFromConfig(t *testing.T) {
	good := map[string]string{"base_url": "https://git.example.com", "token": "tok-abc"}
	if _, err := FromConfig(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]map[string]string{
		"no token":        {"base_url": "https://git.example.com"},
		"http by default": {"base_url": "http://git.example.com", "token": "t"},
		"creds in url":    {"base_url": "https://u:p@git.example.com", "token": "t"},
		"not a url":       {"base_url": "::::", "token": "t"},
		"bad ca":          {"base_url": "https://git.example.com", "token": "t", "ca_cert": "nope"},
	}
	for name, cfg := range cases {
		if _, err := FromConfig(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if _, err := FromConfig(map[string]string{"base_url": "http://localhost:3000", "token": "t", "allow_http": "true"}); err != nil {
		t.Errorf("allow_http should permit http: %v", err)
	}
}

func TestTemplateURLNeverSendsTokenToOtherHosts(t *testing.T) {
	base, _ := url.Parse("https://git.example.com")
	cases := []struct {
		ref       string
		wantURL   string
		wantToken bool
		wantErr   bool
	}{
		{"platform/app-template", "https://git.example.com/platform/app-template.git", true, false},
		{"https://git.example.com/platform/app-template.git", "https://git.example.com/platform/app-template.git", true, false},
		{"https://github.com/acme/template.git", "https://github.com/acme/template.git", false, false},
		{"file:///etc", "", false, true},
		{"ext::sh -c evil", "", false, true},
		{"ssh://git@git.example.com/x.git", "", false, true},
		{"https://user:pw@git.example.com/x.git", "", false, true},
		{"a/b/c", "", false, true},
		{"", "", false, true},
	}
	for _, c := range cases {
		u, send, err := templateURL(base, c.ref)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err=%v wantErr=%v", c.ref, err, c.wantErr)
			continue
		}
		if !c.wantErr && (u != c.wantURL || send != c.wantToken) {
			t.Errorf("%q: got (%s, %v) want (%s, %v)", c.ref, u, send, c.wantURL, c.wantToken)
		}
	}
}

func TestVariableNameRules(t *testing.T) {
	d, _ := FromConfig(map[string]string{"base_url": "https://git.example.com", "token": "t"})
	repo := provider.RepoSpec{Group: provider.GroupSpec{Name: "team-a"}, Name: "shop"}
	for _, k := range []string{"lower", "HAS-DASH", "GITEA_X", "GITHUB_TOKEN", "1START", ""} {
		if err := d.SetVariable(nil, repo, provider.VariableSpec{Key: k, Value: "v"}); err == nil {
			t.Errorf("%q should be rejected before any request", k)
		}
	}
	if err := d.SetVariable(nil, repo, provider.VariableSpec{Key: "OK_KEY", Value: ""}); err == nil {
		t.Error("empty value should be rejected")
	}
}

func TestNamesRejectedBeforeAnyRequest(t *testing.T) {
	d, _ := FromConfig(map[string]string{"base_url": "https://git.example.com", "token": "t"})
	for _, n := range []string{"../etc", "a b", "x/y", "", strings.Repeat("a", 41)} {
		if _, err := d.EnsureGroup(nil, provider.GroupSpec{Name: n}); err == nil {
			t.Errorf("org %q should be rejected", n)
		}
	}
	if _, err := d.EnsureRepository(nil, provider.RepoSpec{Group: provider.GroupSpec{Name: "ok"}, Name: "../x"}); err == nil {
		t.Error("repo ../x should be rejected")
	}
	if _, err := d.EnsureRepository(nil, provider.RepoSpec{Group: provider.GroupSpec{Name: "ok"}, Name: "r", TemplateRepo: "file:///x"}); err == nil {
		t.Error("file:// template should be rejected before creating the repo")
	}
}

func TestSubstitute(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# PROJECT_NAME by TEAM\n", 0o644)
	write("k8s/app.yaml", "name: PROJECT_NAME\n", 0o644)
	write("run.sh", "echo PROJECT_NAME\n", 0o755)
	write("logo.bin", "PROJECT_NAME\x00\x01binary", 0o644)
	write("untouched.txt", "nothing here\n", 0o644)
	big := strings.Repeat("PROJECT_NAME ", maxSubstFileSize/10)
	write("big.txt", big, 0o644)
	_ = os.Symlink("README.md", filepath.Join(dir, "link"))

	files := []string{"README.md", "k8s/app.yaml", "run.sh", "logo.bin", "untouched.txt", "big.txt", "link", "missing.txt"}
	changed, err := substitute(dir, files, map[string]string{"PROJECT_NAME": "shop", "TEAM": "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 3 {
		t.Fatalf("changed = %v", changed)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if string(got) != "# shop by team-a\n" {
		t.Fatalf("README: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "logo.bin")); !strings.Contains(string(b), "PROJECT_NAME") {
		t.Error("binary file must not be modified")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "big.txt")); string(b) != big {
		t.Error("oversized file must not be modified")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "run.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("file mode lost: %v", fi.Mode())
	}
	// Values are literal: no regex or shell interpretation.
	write("x.txt", "A\n", 0o644)
	if _, err := substitute(dir, []string{"x.txt"}, map[string]string{"A": "$(touch pwned) \\1 &"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "x.txt")); string(b) != "$(touch pwned) \\1 &\n" {
		t.Fatalf("value not literal: %q", b)
	}
}

func TestGitRunnerKeepsTokenOutOfArgsAndURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	g := &gitRunner{home: t.TempDir(), token: "super-secret-token"}
	env := strings.Join(g.env(true), "\n")
	if !strings.Contains(env, "http.extraHeader") || !strings.Contains(env, "protocol.allow") {
		t.Fatalf("env missing settings:\n%s", env)
	}
	if strings.Contains(strings.Join(g.env(false), "\n"), "super-secret-token") {
		t.Fatal("token must not be sent when auth is disabled")
	}
	_, err := g.run(t.Context(), t.TempDir(), true, "clone", "--quiet", "https://127.0.0.1:1/none.git", t.TempDir()+"/x")
	if err == nil || strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("expected a clone error without the token, got %v", err)
	}
}

func TestRegisteredAsGitSourceDriver(t *testing.T) {
	_, err := provider.New(provider.KindGitSource, "gitea", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("driver should be registered and validate config, got %v", err)
	}
}
