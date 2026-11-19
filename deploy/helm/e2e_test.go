//go:build helme2e

package helm

// Checks the chart installed on a real cluster. Run it through
// scripts/helm-e2e.sh (`make helm-e2e`), which creates the cluster, the
// database and a stand-in identity provider and sets the HELM_E2E_* variables.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("%s not set: run scripts/helm-e2e.sh", k)
	}
	return v
}

// run executes a command against the test cluster and returns its output.
func run(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+env(t, "HELM_E2E_KUBECONFIG"))
	// Data comes from stdout only: kubectl sometimes prints harmless warnings
	// to stderr. On failure both are returned so the cause is visible.
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String() + "\n" + errb.String()), err
	}
	return strings.TrimSpace(out.String()), nil
}

func must(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := run(t, name, args...)
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

func kubectl(t *testing.T, args ...string) string {
	return must(t, "kubectl", append([]string{"-n", env(t, "HELM_E2E_NS")}, args...)...)
}

func helmArgs(t *testing.T, extra ...string) []string {
	return append([]string{"-n", env(t, "HELM_E2E_NS"), "-f", env(t, "HELM_E2E_VALUES")}, extra...)
}

// ---- reaching the application ------------------------------------------------------

// forward is a port-forward to the Service. kubectl pins it to one pod, so it
// is restarted after pods are replaced.
type forward struct {
	t    *testing.T
	cmd  *exec.Cmd
	base string
}

func (f *forward) start() {
	f.t.Helper()
	f.stop()
	cmd := exec.Command("kubectl", "-n", env(f.t, "HELM_E2E_NS"), "port-forward", "svc/plinth", ":80")
	cmd.Env = append(os.Environ(), "KUBECONFIG="+env(f.t, "HELM_E2E_KUBECONFIG"))
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.cmd = cmd
	re := regexp.MustCompile(`127\.0\.0\.1:(\d+)`)
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := re.FindStringSubmatch(sc.Text()); m != nil {
				line <- m[1]
				return
			}
		}
	}()
	select {
	case port := <-line:
		f.base = "http://127.0.0.1:" + port
	case <-time.After(20 * time.Second):
		f.t.Fatal("port-forward did not start")
	}
}

func (f *forward) stop() {
	if f.cmd != nil && f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
		_, _ = f.cmd.Process.Wait()
	}
	f.cmd = nil
}

// Do sends a request, restarting the forward once if the pod it was pinned to is gone.
func (f *forward) Do(token, method, path string, body any) (int, []byte) {
	f.t.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		if f.base == "" || attempt == 1 {
			f.start()
		}
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, f.base+path, rd)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	f.t.Fatalf("%s %s: unreachable", method, path)
	return 0, nil
}

// ---- signing in through the real protocol --------------------------------------------

func login(t *testing.T, user int) string {
	t.Helper()
	idp := env(t, "HELM_E2E_IDP")
	verifier := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("verifier-%d-%d-padding-padding", user, time.Now().UnixNano())))
	sum := sha256.Sum256([]byte(verifier))
	redirect := "http://127.0.0.1:8080/callback"
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(idp+"/authorize", url.Values{
		"client_id": {"plinth-ui"}, "redirect_uri": {redirect}, "state": {"s"}, "nonce": {"n"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "scope": {"openid"}, "user": {fmt.Sprint(user)},
	})
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %v %v", err, resp)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	tr, err := c.PostForm(idp+"/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")}, "code_verifier": {verifier},
		"redirect_uri": {redirect}, "client_id": {"plinth-ui"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
	}
	_ = json.NewDecoder(tr.Body).Decode(&tok)
	if tok.IDToken == "" {
		t.Fatalf("no token (status %d)", tr.StatusCode)
	}
	return tok.IDToken
}

// ---- cluster facts ------------------------------------------------------------------------

func pods(t *testing.T) []string {
	t.Helper()
	out := kubectl(t, "get", "pods", "-l", "app.kubernetes.io/name=plinth,app.kubernetes.io/component=server",
		"--field-selector=status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}")
	return strings.Fields(out)
}

func readyReplicas(t *testing.T) int {
	t.Helper()
	var n int
	fmt.Sscan(kubectl(t, "get", "deploy", "plinth", "-o", "jsonpath={.status.readyReplicas}"), &n)
	return n
}

func waitReady(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var spec, updated, ready, total int
		fmt.Sscan(kubectl(t, "get", "deploy", "plinth", "-o", "jsonpath={.spec.replicas} {.status.updatedReplicas} {.status.readyReplicas} {.status.replicas}"), &spec, &updated, &ready, &total)
		if ready == want && total == want && updated == want {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("deployment did not reach %d ready replicas", want)
}

func psql(t *testing.T, query string) string {
	t.Helper()
	return must(t, "kubectl", "-n", "db", "exec", "deploy/postgres", "--", "psql", "-U", "plinth", "-d", "plinth", "-tAc", query)
}

func TestInstalledChart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Minute)
	defer cancel()
	_ = ctx
	masterKey := env(t, "HELM_E2E_MASTER_KEY")
	f := &forward{t: t}
	defer f.stop()

	root, alice, bob := login(t, 2), login(t, 0), login(t, 1)

	t.Run("every replica is ready and the database answers", func(t *testing.T) {
		waitReady(t, 2)
		code, body := f.Do("", "GET", "/readyz", nil)
		if code != 200 || strings.TrimSpace(string(body)) != "ready" {
			t.Fatalf("readyz: %d %s", code, body)
		}
		code, body = f.Do("", "GET", "/api/v1/config", nil)
		if code != 200 || !strings.Contains(string(body), "devidp.idp.svc.cluster.local:9100") {
			t.Fatalf("config: %d %s", code, body)
		}
		// Both replicas started at the same moment and migrated safely.
		files, err := filepath.Glob("../../internal/store/migrations/*.sql")
		if err != nil || len(files) == 0 {
			t.Fatalf("cannot count the migrations: %v", err)
		}
		if n, want := psql(t, "select count(*) from schema_migrations"), fmt.Sprint(len(files)); n != want {
			t.Errorf("applied migrations = %s, want %s (every migration exactly once)", n, want)
		}
		for _, p := range pods(t) {
			logs := kubectl(t, "logs", p)
			if strings.Contains(logs, `"level":"ERROR"`) {
				t.Errorf("errors in the log of %s:\n%s", p, logs)
			}
		}
	})

	t.Run("helm test passes", func(t *testing.T) {
		out, err := run(t, "helm", "test", "plinth", "-n", env(t, "HELM_E2E_NS"), "--timeout", "2m")
		if err != nil || !strings.Contains(out, "Phase:") {
			t.Fatalf("helm test: %v\n%s", err, out)
		}
	})

	t.Run("people can sign in, and administrators are told apart", func(t *testing.T) {
		for tok, want := range map[string]string{root: `"role":"admin"`, alice: `"role":"user"`} {
			code, body := f.Do(tok, "GET", "/api/v1/me", nil)
			if code != 200 || !strings.Contains(string(body), want) {
				t.Errorf("/me: %d %s, want %s", code, body, want)
			}
		}
		if code, _ := f.Do(bob, "GET", "/api/v1/integrations", nil); code != 403 {
			t.Errorf("a developer reached administration: %d", code)
		}
		if code, _ := f.Do("", "GET", "/api/v1/integrations", nil); code != 401 {
			t.Errorf("anonymous access: %d", code)
		}
	})

	t.Run("the first administrator signs in with the generated password and must change it", func(t *testing.T) {
		enc := kubectl(t, "get", "secret", "plinth-admin", "-o", "jsonpath={.data.password}")
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
		if err != nil || len(raw) != 20 {
			t.Fatalf("the generated password: %q %v", raw, err)
		}
		first := string(raw)
		code, body := f.Do("", "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": first})
		var login struct {
			Token      string `json:"token"`
			MustChange bool   `json:"must_change_password"`
		}
		_ = json.Unmarshal(body, &login)
		if code != 200 || !login.MustChange || !strings.HasPrefix(login.Token, "plt_") {
			t.Fatalf("sign in: %d %s", code, body)
		}
		if code, _ := f.Do(login.Token, "GET", "/api/v1/requests", nil); code != 403 {
			t.Errorf("until the password is changed nothing else may work: %d", code)
		}
		own := first + "-changed-by-the-test"
		if code, body := f.Do(login.Token, "POST", "/api/v1/auth/password", map[string]string{"current_password": first, "new_password": own}); code != 204 {
			t.Fatalf("change: %d %s", code, body)
		}
		if code, _ := f.Do(login.Token, "GET", "/api/v1/local-users", nil); code != 200 {
			t.Errorf("the bootstrap account is an administrator: %d", code)
		}
		if code, _ := f.Do("", "POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": first}); code != 401 {
			t.Errorf("the starting password still works after it was changed: %d", code)
		}
		// Both replicas start together and look for a first administrator: one account exists.
		if n := psql(t, "select count(*) from local_accounts"); n != "1" {
			t.Errorf("local accounts = %s", n)
		}
		// The provider from the environment and password sign-in are both offered.
		code, body = f.Do("", "GET", "/api/v1/config", nil)
		if code != 200 || !strings.Contains(string(body), `"local_login":true`) || !strings.Contains(string(body), "devidp.idp.svc.cluster.local:9100") {
			t.Errorf("sign-in options: %d %s", code, body)
		}
		for _, p := range pods(t) {
			if strings.Contains(kubectl(t, "logs", p), first) {
				t.Error("the starting password is in a pod's log")
			}
		}
	})

	secret := "super-secret-chart-test-password"
	t.Run("administration works and the settings are encrypted in the database", func(t *testing.T) {
		code, body := f.Do(root, "POST", "/api/v1/integrations", map[string]any{
			"kind": "database", "driver": "postgres", "name": "e2e-db",
			"config": map[string]string{"host": "db.example.com", "admin_user": "plinth_admin", "admin_password": secret},
		})
		if code != 201 || strings.Contains(string(body), secret) {
			t.Fatalf("create: %d %s", code, body)
		}
		stored := psql(t, "select encode(config_enc, 'escape') from integrations where name = 'e2e-db'")
		if stored == "" || strings.Contains(stored, secret) || strings.Contains(stored, "db.example.com") {
			t.Fatal("the integration is not encrypted at rest")
		}
		// The served web interface and the API share the one server.
		code, body = f.Do("", "GET", "/", nil)
		if code != 200 && code != 503 {
			t.Errorf("web app: %d", code)
		}
		if code == 200 && !strings.Contains(string(body), `<div id="root">`) {
			t.Errorf("the app shell is missing: %.200s", body)
		}
	})

	t.Run("the pods are hardened on a real cluster", func(t *testing.T) {
		list := pods(t)
		if len(list) != 2 {
			t.Fatalf("pods: %v", list)
		}
		for _, p := range list {
			if uid := kubectl(t, "exec", p, "--", "id", "-u"); uid != "10001" {
				t.Errorf("%s runs as uid %s", p, uid)
			}
			if _, err := run(t, "kubectl", "-n", env(t, "HELM_E2E_NS"), "exec", p, "--", "touch", "/not-allowed"); err == nil {
				t.Errorf("%s can write to its root filesystem", p)
			}
			if out, err := run(t, "kubectl", "-n", env(t, "HELM_E2E_NS"), "exec", p, "--", "touch", "/tmp/scratch"); err != nil {
				t.Errorf("%s cannot write to /tmp (git needs it): %s", p, out)
			}
			if _, err := run(t, "kubectl", "-n", env(t, "HELM_E2E_NS"), "exec", p, "--", "ls", "/var/run/secrets/kubernetes.io/serviceaccount"); err == nil {
				t.Errorf("%s has a Kubernetes API token mounted", p)
			}
			spec := kubectl(t, "get", "pod", p, "-o", "json")
			if strings.Contains(spec, masterKey) {
				t.Errorf("the master key appears in the spec of %s", p)
			}
			if strings.Contains(kubectl(t, "logs", p), masterKey) {
				t.Errorf("the master key appears in the logs of %s", p)
			}
		}
		if got := kubectl(t, "get", "pdb", "plinth", "-o", "jsonpath={.status.disruptionsAllowed}"); got != "1" {
			t.Errorf("disruptions allowed = %s, want 1 with two replicas", got)
		}
	})

	t.Run("an upgrade changes configuration and scale without interrupting service", func(t *testing.T) {
		ns := env(t, "HELM_E2E_NS")
		// A client inside the cluster calls the Service every 100 ms throughout.
		must(t, "kubectl", "-n", ns, "run", "probe", "--image=busybox:1.37", "--image-pull-policy=IfNotPresent", "--restart=Never", "--command", "--",
			"sh", "-c", "while true; do if wget -q -T 2 -O /dev/null http://plinth/healthz; then echo ok >> /tmp/r; else echo fail >> /tmp/r; fi; sleep 0.1; done")
		kubectl(t, "wait", "--for=condition=Ready", "pod/probe", "--timeout=90s")
		time.Sleep(3 * time.Second)

		must(t, "helm", append([]string{"upgrade", "plinth", env(t, "HELM_E2E_CHART")}, helmArgs(t, "--set", "replicaCount=3", "--set", "config.workers=4", "--wait", "--timeout", "4m")...)...)
		waitReady(t, 3)
		time.Sleep(3 * time.Second)

		counts := kubectl(t, "exec", "probe", "--", "sh", "-c", "echo $(grep -c fail /tmp/r || true) $(grep -c ok /tmp/r)")
		var failed, ok int
		fmt.Sscan(counts, &failed, &ok)
		kubectl(t, "delete", "pod", "probe", "--now", "--wait=false")
		if failed != 0 || ok < 50 {
			t.Errorf("during the upgrade %d requests failed and %d succeeded: service was interrupted", failed, ok)
		}
		t.Logf("upgrade: %d requests succeeded, %d failed", ok, failed)

		if w := kubectl(t, "get", "deploy", "plinth", "-o", "jsonpath={.spec.template.spec.containers[0].env[?(@.name=='PLINTH_WORKERS')].value}"); w != "4" {
			t.Errorf("PLINTH_WORKERS = %s after the upgrade", w)
		}
		f.start()
		code, body := f.Do(root, "GET", "/api/v1/integrations", nil)
		if code != 200 || !strings.Contains(string(body), "e2e-db") {
			t.Errorf("state did not survive replacing every pod: %d %s", code, body)
		}
	})

	t.Run("a rollback restores the earlier release", func(t *testing.T) {
		must(t, "helm", "rollback", "plinth", "1", "-n", env(t, "HELM_E2E_NS"), "--wait", "--timeout", "4m")
		waitReady(t, 2)
		if w := kubectl(t, "get", "deploy", "plinth", "-o", "jsonpath={.spec.template.spec.containers[0].env[?(@.name=='PLINTH_WORKERS')].value}"); w != "2" {
			t.Errorf("PLINTH_WORKERS = %s after the rollback", w)
		}
		f.start()
		if code, _ := f.Do(root, "GET", "/api/v1/me", nil); code != 200 {
			t.Errorf("after rollback: %d", code)
		}
	})

	t.Run("losing a replica does not lose the service", func(t *testing.T) {
		victim := pods(t)[0]
		kubectl(t, "delete", "pod", victim, "--wait=false")
		waitReady(t, 2)
		f.start()
		if code, _ := f.Do(alice, "GET", "/api/v1/me", nil); code != 200 {
			t.Errorf("after losing a pod: %d", code)
		}
	})

	t.Run("uninstalling removes the chart's objects and keeps your Secrets and data", func(t *testing.T) {
		f.stop()
		must(t, "helm", "uninstall", "plinth", "-n", env(t, "HELM_E2E_NS"), "--wait")
		// Pods finish their graceful shutdown a few seconds after the release is gone.
		var left string
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
			if left = kubectl(t, "get", "deploy,svc,pdb,sa,pods", "-l", "app.kubernetes.io/instance=plinth", "-o", "name"); left == "" {
				break
			}
		}
		if left != "" {
			t.Errorf("objects left behind:\n%s", left)
		}
		if out, err := run(t, "kubectl", "-n", env(t, "HELM_E2E_NS"), "get", "secret", "plinth-master-key", "plinth-database", "-o", "name"); err != nil {
			t.Errorf("the Secrets you manage must survive an uninstall: %s", out)
		}
		if n := psql(t, "select count(*) from integrations"); n != "1" {
			t.Errorf("the database must keep its data, integrations = %s", n)
		}
	})
}
