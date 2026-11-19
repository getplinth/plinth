package helm

// Renders the chart with `helm template` and checks the Kubernetes objects it
// produces: what is created, how it is hardened, how values flow through, and
// that bad configuration is refused with a sentence that says what to set.
// Skipped when helm is not installed. `make helm-test` runs it.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	minimal = "plinth/ci/minimal-values.yaml"
	full    = "plinth/ci/full-values.yaml"
)

type rendered map[string][]*unstructured.Unstructured // by kind

func helmTemplate(t *testing.T, args ...string) (rendered, string, error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}
	cmd := exec.Command("helm", append([]string{"template", "plinth", "./plinth", "--namespace", "platform"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, errb.String(), err
	}
	r := rendered{}
	dec := k8syaml.NewYAMLOrJSONDecoder(&out, 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			break
		}
		if len(u.Object) == 0 {
			continue
		}
		r[u.GetKind()] = append(r[u.GetKind()], u)
	}
	return r, "", nil
}

func render(t *testing.T, args ...string) rendered {
	t.Helper()
	r, stderr, err := helmTemplate(t, args...)
	if err != nil {
		t.Fatalf("helm template %v: %v\n%s", args, err, stderr)
	}
	return r
}

func one[T any](t *testing.T, r rendered, kind string) *T {
	t.Helper()
	list := r[kind]
	if len(list) != 1 {
		t.Fatalf("want exactly one %s, got %d", kind, len(list))
	}
	var v T
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list[0].Object, &v); err != nil {
		t.Fatal(err)
	}
	return &v
}

// chartSecret is the Secret holding inline database and master key values
// (the first administrator's is separate).
func chartSecret(t *testing.T, r rendered) *corev1.Secret {
	t.Helper()
	var list []*unstructured.Unstructured
	for _, u := range r["Secret"] {
		if u.GetName() != "plinth-admin" {
			list = append(list, u)
		}
	}
	return one[corev1.Secret](t, rendered{"Secret": list}, "Secret")
}

func deployment(t *testing.T, r rendered) *appsv1.Deployment {
	return one[appsv1.Deployment](t, r, "Deployment")
}

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		m[e.Name] = e
	}
	return m
}

func TestMinimalInstallCreatesWhatIsNeededAndNothingElse(t *testing.T) {
	r := render(t, "-f", minimal)
	want := map[string]int{"ServiceAccount": 1, "Service": 1, "Deployment": 1, "PodDisruptionBudget": 1, "Pod": 1 /* helm test */}
	for kind, n := range want {
		if len(r[kind]) != n {
			t.Errorf("%s: got %d, want %d", kind, len(r[kind]), n)
		}
	}
	// The only Secret is the first administrator's starting password.
	if secrets := r["Secret"]; len(secrets) != 1 || secrets[0].GetName() != "plinth-admin" {
		t.Errorf("a minimal install creates only the admin Secret, got %d", len(secrets))
	}
	for _, kind := range []string{"Ingress", "NetworkPolicy", "ConfigMap", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"} {
		if len(r[kind]) != 0 {
			t.Errorf("a minimal install must not create a %s", kind)
		}
	}
	if got := deployment(t, r).Name; got != "plinth" {
		t.Errorf("object name = %q", got)
	}
	if got := deployment(t, r).Namespace; got != "platform" {
		t.Errorf("namespace = %q", got)
	}
}

func TestPodIsHardened(t *testing.T) {
	d := deployment(t, render(t, "-f", minimal))
	spec := d.Spec.Template.Spec
	if len(spec.Containers) != 1 {
		t.Fatalf("containers: %d", len(spec.Containers))
	}
	c := spec.Containers[0]

	ps := spec.SecurityContext
	if ps == nil || ps.RunAsNonRoot == nil || !*ps.RunAsNonRoot || ps.RunAsUser == nil || *ps.RunAsUser == 0 {
		t.Errorf("the pod must run as a non-root user: %+v", ps)
	}
	if ps == nil || ps.SeccompProfile == nil || ps.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("the pod needs the RuntimeDefault seccomp profile")
	}
	cs := c.SecurityContext
	if cs == nil || cs.AllowPrivilegeEscalation == nil || *cs.AllowPrivilegeEscalation {
		t.Error("privilege escalation must be off")
	}
	if cs == nil || cs.ReadOnlyRootFilesystem == nil || !*cs.ReadOnlyRootFilesystem {
		t.Error("the root filesystem must be read-only")
	}
	if cs == nil || cs.Capabilities == nil || len(cs.Capabilities.Drop) != 1 || cs.Capabilities.Drop[0] != "ALL" {
		t.Error("all capabilities must be dropped")
	}
	if cs != nil && cs.Privileged != nil && *cs.Privileged {
		t.Error("never privileged")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the pod does not need this cluster's API token")
	}
	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Error("service links only leak environment variables")
	}
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		t.Error("no host namespaces")
	}

	// A read-only root filesystem still needs scratch space for git.
	var tmp bool
	for _, m := range c.VolumeMounts {
		tmp = tmp || (m.Name == "tmp" && m.MountPath == "/tmp")
	}
	if !tmp {
		t.Error("/tmp must be a writable volume")
	}
	if c.Resources.Requests.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
		t.Errorf("default resources: %+v", c.Resources)
	}
}

func TestSecretsAreReferencedNeverWrittenIntoThePod(t *testing.T) {
	env := envOf(deployment(t, render(t, "-f", minimal)).Spec.Template.Spec.Containers[0])
	for name, wantSecret := range map[string][2]string{
		"PLINTH_DATABASE_URL": {"plinth-database", "database-url"},
		"PLINTH_MASTER_KEY":   {"plinth-master-key", "master-key"},
	} {
		e, ok := env[name]
		if !ok || e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s must come from a secret, got %+v", name, e)
		}
		if ref := e.ValueFrom.SecretKeyRef; ref.Name != wantSecret[0] || ref.Key != wantSecret[1] {
			t.Errorf("%s -> %s/%s, want %s/%s", name, ref.Name, ref.Key, wantSecret[0], wantSecret[1])
		}
	}
}

func TestNoIdentityProviderIsNeededAtInstall(t *testing.T) {
	bare := []string{"--set-string", "database.existingSecret=plinth-database", "--set-string", "masterKey.existingSecret=plinth-master-key"}
	r := render(t, bare...)
	env := envOf(deployment(t, r).Spec.Template.Spec.Containers[0])
	for _, k := range []string{"PLINTH_OIDC_ISSUER", "PLINTH_OIDC_AUDIENCE", "PLINTH_OIDC_NAME"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must not be set when no provider is configured at install", k)
		}
	}
	if env["PLINTH_BOOTSTRAP_ADMIN_USERNAME"].Value != "admin" {
		t.Errorf("the first administrator: %+v", env["PLINTH_BOOTSTRAP_ADMIN_USERNAME"])
	}
	// A provider given at install is still passed on, with its display name.
	env = envOf(deployment(t, render(t, "-f", minimal, "--set-string", "config.oidc.name=Company SSO")).Spec.Template.Spec.Containers[0])
	if env["PLINTH_OIDC_ISSUER"].Value != "https://idp.example.com" || env["PLINTH_OIDC_NAME"].Value != "Company SSO" {
		t.Errorf("%+v %+v", env["PLINTH_OIDC_ISSUER"], env["PLINTH_OIDC_NAME"])
	}
}

func TestTheFirstAdministratorsPasswordIsGeneratedIntoASecretAndNeverInThePod(t *testing.T) {
	r := render(t, "-f", minimal)
	secrets := r["Secret"]
	var admin map[string]any
	for _, s := range secrets {
		if s.GetName() == "plinth-admin" {
			admin = s.Object
		}
	}
	if admin == nil {
		t.Fatalf("no admin Secret among %d secrets", len(secrets))
	}
	data, _ := admin["data"].(map[string]any)
	enc, _ := data["password"].(string)
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) != 20 {
		t.Fatalf("a random 20 character password, got %q (%v)", raw, err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "admin") {
		t.Error("a generated password must not contain the username")
	}
	meta, _ := admin["metadata"].(map[string]any)
	if ann, _ := meta["annotations"].(map[string]any); ann["helm.sh/resource-policy"] != "keep" {
		t.Errorf("the Secret must survive an uninstall: %v", ann)
	}
	env := envOf(deployment(t, r).Spec.Template.Spec.Containers[0])["PLINTH_BOOTSTRAP_ADMIN_PASSWORD"]
	if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef.Name != "plinth-admin" || env.ValueFrom.SecretKeyRef.Key != "password" {
		t.Errorf("the password must come from the Secret, never inline: %+v", env)
	}
	if strings.Contains(fmt.Sprint(deployment(t, r).Spec), string(raw)) {
		t.Error("the password is written into the pod spec")
	}
	// Two renders differ (random), but a password you provide is used as it is.
	own := envOf(deployment(t, render(t, "-f", minimal, "--set-string", "admin.existingSecret=my-admin", "--set-string", "admin.existingSecretKey=pw")).Spec.Template.Spec.Containers[0])["PLINTH_BOOTSTRAP_ADMIN_PASSWORD"]
	if own.ValueFrom.SecretKeyRef.Name != "my-admin" || own.ValueFrom.SecretKeyRef.Key != "pw" {
		t.Errorf("%+v", own)
	}
	for _, s := range render(t, "-f", minimal, "--set-string", "admin.existingSecret=my-admin")["Secret"] {
		if s.GetName() == "plinth-admin" {
			t.Error("no Secret is generated when one is provided")
		}
	}
	// Switched off: no account is bootstrapped (use `plinth bootstrap-admin`).
	off := envOf(deployment(t, render(t, "-f", minimal, "--set", "admin.create=false", "--set-string", "config.oidc.issuer=https://idp.example.com", "--set-string", "config.oidc.audience=plinth")).Spec.Template.Spec.Containers[0])
	if _, ok := off["PLINTH_BOOTSTRAP_ADMIN_PASSWORD"]; ok {
		t.Error("admin.create=false must not pass a password")
	}
}

func TestRecoverySwitch(t *testing.T) {
	if _, ok := envOf(deployment(t, render(t, "-f", minimal)).Spec.Template.Spec.Containers[0])["PLINTH_FORCE_LOCAL_LOGIN"]; ok {
		t.Error("password sign-in is not forced by default")
	}
	if envOf(deployment(t, render(t, "-f", minimal, "--set", "config.forceLocalLogin=true")).Spec.Template.Spec.Containers[0])["PLINTH_FORCE_LOCAL_LOGIN"].Value != "true" {
		t.Error("config.forceLocalLogin must reach the server")
	}
}

func TestSettingsReachTheServer(t *testing.T) {
	env := envOf(deployment(t, render(t, "-f", minimal)).Spec.Template.Spec.Containers[0])
	want := map[string]string{
		"PLINTH_LISTEN_ADDR":   ":8080",
		"PLINTH_OIDC_ISSUER":   "https://idp.example.com",
		"PLINTH_OIDC_AUDIENCE": "plinth",
		"PLINTH_ADMIN_GROUP":   "plinth-admins",
		"PLINTH_GROUPS_CLAIM":  "groups",
		"PLINTH_WORKERS":       "2",
	}
	for k, v := range want {
		if env[k].Value != v {
			t.Errorf("%s = %q, want %q", k, env[k].Value, v)
		}
	}
	if _, ok := env["PLINTH_OIDC_ALLOW_HTTP"]; ok {
		t.Error("plain-http identity providers must be off by default")
	}
}

func TestHealthChecksAndRollingUpdates(t *testing.T) {
	d := deployment(t, render(t, "-f", minimal))
	c := d.Spec.Template.Spec.Containers[0]
	if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" ||
		c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" ||
		c.StartupProbe == nil || c.StartupProbe.HTTPGet.Path != "/healthz" {
		t.Errorf("probes: live=%+v ready=%+v startup=%+v", c.LivenessProbe, c.ReadinessProbe, c.StartupProbe)
	}
	if got := c.StartupProbe.FailureThreshold * c.StartupProbe.PeriodSeconds; got < 60 {
		t.Errorf("startup may take %ds: migrations and identity provider discovery need more room", got)
	}
	ru := d.Spec.Strategy.RollingUpdate
	if ru == nil || ru.MaxUnavailable == nil || ru.MaxUnavailable.IntValue() != 0 {
		t.Errorf("an upgrade must never remove a ready pod before its replacement is ready: %+v", ru)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 2 {
		t.Errorf("replicas: %v", d.Spec.Replicas)
	}
	if len(d.Spec.Template.Spec.TopologySpreadConstraints) != 1 {
		t.Error("replicas should be spread across nodes")
	}
	pre := c.Lifecycle
	if pre == nil || pre.PreStop == nil || pre.PreStop.Exec == nil || strings.Join(pre.PreStop.Exec.Command, " ") != "sleep 5" {
		t.Errorf("a stopping pod must keep serving briefly while the Service drops it: %+v", pre)
	}
	// delay + the server's own shutdown (up to 20s) must fit in the grace period
	if tg := d.Spec.Template.Spec.TerminationGracePeriodSeconds; tg == nil || *tg < 5+20 {
		t.Errorf("termination grace period %v is shorter than the drain plus the server's shutdown", tg)
	}
}

func TestImageDefaultsToTheChartVersionAndADigestWins(t *testing.T) {
	img := func(args ...string) string {
		return deployment(t, render(t, append([]string{"-f", minimal}, args...)...)).Spec.Template.Spec.Containers[0].Image
	}
	if got := img(); got != "ghcr.io/getplinth/plinth:0.1.0" {
		t.Errorf("default image %q", got)
	}
	if got := img("--set", "image.tag=9.9.9"); got != "ghcr.io/getplinth/plinth:9.9.9" {
		t.Errorf("tag: %q", got)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	if got := img("--set", "image.tag=9.9.9", "--set", "image.digest="+digest); got != "ghcr.io/getplinth/plinth@"+digest {
		t.Errorf("digest must win over the tag: %q", got)
	}
}

func TestLabelsAndSelectorsLineUp(t *testing.T) {
	r := render(t, "-f", minimal)
	d := deployment(t, r)
	svc := one[corev1.Service](t, r, "Service")
	for k, v := range d.Spec.Selector.MatchLabels {
		if d.Spec.Template.Labels[k] != v {
			t.Errorf("the pod lacks selector label %s=%s", k, v)
		}
		if svc.Spec.Selector[k] != v {
			t.Errorf("the service does not select on %s=%s", k, v)
		}
	}
	for kind, list := range r {
		for _, o := range list {
			l := o.GetLabels()
			if l["app.kubernetes.io/name"] != "plinth" || l["app.kubernetes.io/instance"] != "plinth" || l["app.kubernetes.io/managed-by"] != "Helm" {
				t.Errorf("%s %s has incomplete labels: %v", kind, o.GetName(), l)
			}
		}
	}
	// The service targets the container's named port.
	port := svc.Spec.Ports[0]
	var found bool
	for _, p := range d.Spec.Template.Spec.Containers[0].Ports {
		found = found || p.Name == port.TargetPort.StrVal
	}
	if port.Port != 80 || !found {
		t.Errorf("service port %+v does not reach a named container port", port)
	}
}

func TestInlineSecretsCreateASecretAndRollThePodsWhenTheyChange(t *testing.T) {
	inline := func(url string) rendered {
		return render(t, "--set-string", "config.oidc.issuer=https://idp.example.com", "--set-string", "config.oidc.audience=plinth",
			"--set-string", "database.url="+url, "--set-string", "masterKey.value=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	}
	r := inline("postgres://u:p@db/plinth")
	s := chartSecret(t, r)
	if s.StringData["database-url"] != "postgres://u:p@db/plinth" || s.StringData["master-key"] == "" {
		t.Errorf("secret data: %v", s.StringData)
	}
	d := deployment(t, r)
	env := envOf(d.Spec.Template.Spec.Containers[0])
	if ref := env["PLINTH_DATABASE_URL"].ValueFrom.SecretKeyRef; ref.Name != "plinth" || ref.Key != "database-url" {
		t.Errorf("database ref: %+v", ref)
	}
	if env["PLINTH_MASTER_KEY"].ValueFrom == nil || env["PLINTH_MASTER_KEY"].Value != "" {
		t.Error("even inline, the pod reads the key from the Secret, not from its own spec")
	}
	before := d.Spec.Template.Annotations["checksum/secret"]
	after := deployment(t, inline("postgres://u:other@db/plinth")).Spec.Template.Annotations["checksum/secret"]
	if before == "" || before == after {
		t.Errorf("changing a chart-managed secret must change the pod template: %q -> %q", before, after)
	}
}

func TestMixingInlineAndExistingSecretsIsPossible(t *testing.T) {
	r := render(t, "-f", minimal, "--set-string", "masterKey.existingSecret=", "--set-string", "masterKey.value=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	s := chartSecret(t, r)
	if _, ok := s.StringData["database-url"]; ok || s.StringData["master-key"] == "" {
		t.Errorf("only the inline value belongs in the chart's Secret: %v", s.StringData)
	}
	env := envOf(deployment(t, r).Spec.Template.Spec.Containers[0])
	if env["PLINTH_DATABASE_URL"].ValueFrom.SecretKeyRef.Name != "plinth-database" {
		t.Error("the database URL should still come from the existing Secret")
	}
}

func TestFullConfigurationFlowsThrough(t *testing.T) {
	r := render(t, "-f", full)
	d := deployment(t, r)
	spec := d.Spec.Template.Spec
	c := spec.Containers[0]

	if *d.Spec.Replicas != 3 || c.Image != "registry.example.com/platform/plinth:1.2.3" ||
		len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "registry-pull" ||
		spec.PriorityClassName != "platform-critical" || spec.NodeSelector["kubernetes.io/os"] != "linux" ||
		len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "dedicated" ||
		d.Spec.Template.Annotations["example.com/owner"] != "platform-team" {
		t.Errorf("scheduling and image settings were not applied: %+v", spec)
	}
	env := envOf(c)
	if env["PLINTH_WORKERS"].Value != "4" || env["PLINTH_ADMIN_GROUP"].Value != "platform-admins" ||
		env["PLINTH_GROUPS_CLAIM"].Value != "roles" || env["TZ"].Value != "UTC" ||
		env["PLINTH_OIDC_ISSUER"].Value != "https://login.example.com/realms/platform" {
		t.Errorf("environment: %v", env)
	}
	if got := c.Resources.Limits.Memory().String(); got != "1Gi" {
		t.Errorf("memory limit %s", got)
	}

	ing := one[networkingv1.Ingress](t, r, "Ingress")
	if *ing.Spec.IngressClassName != "nginx" || ing.Spec.Rules[0].Host != "plinth.example.com" ||
		ing.Spec.TLS[0].SecretName != "plinth-tls" || ing.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("ingress: %+v", ing.Spec)
	}
	be := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
	if be.Name != "plinth" || be.Port.Name != "http" {
		t.Errorf("ingress backend %+v must be the chart's service", be)
	}

	pdb := one[policyv1.PodDisruptionBudget](t, r, "PodDisruptionBudget")
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntValue() != 2 {
		t.Errorf("pdb: %+v", pdb.Spec)
	}
	np := one[networkingv1.NetworkPolicy](t, r, "NetworkPolicy")
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || len(np.Spec.Ingress) != 1 {
		t.Errorf("network policy: %+v", np.Spec)
	}
	if len(np.Spec.Egress) != 0 {
		t.Error("egress must stay open: Plinth reaches many systems")
	}
}

func TestSingleReplicaHasNoDisruptionBudget(t *testing.T) {
	r := render(t, "-f", minimal, "--set", "replicaCount=1")
	if len(r["PodDisruptionBudget"]) != 0 {
		t.Error("a budget of minAvailable=1 on one replica would block every node drain")
	}
	if *deployment(t, r).Spec.Replicas != 1 {
		t.Error("replicas")
	}
}

func TestNotificationsOffByDefaultAndSecretsOnlyFromSecrets(t *testing.T) {
	env := func(args ...string) map[string]corev1.EnvVar {
		return envOf(deployment(t, render(t, append([]string{"-f", minimal}, args...)...)).Spec.Template.Spec.Containers[0])
	}
	for k := range env() {
		if strings.HasPrefix(k, "PLINTH_SMTP_") || strings.HasPrefix(k, "PLINTH_WEBHOOK_") || k == "PLINTH_PUBLIC_URL" || k == "PLINTH_NOTIFY_ALLOW_INSECURE" {
			t.Errorf("%s is set by default", k)
		}
	}
	e := env("--set-string", "notifications.smtp.host=smtp.example.com", "--set-string", "notifications.smtp.from=Plinth <plinth@example.com>",
		"--set-string", "notifications.smtp.existingSecret=mail-login", "--set-string", "notifications.webhook.existingSecret=hook",
		"--set-string", "config.publicUrl=https://plinth.example.com/")
	for k, want := range map[string]string{"PLINTH_SMTP_HOST": "smtp.example.com", "PLINTH_SMTP_PORT": "587", "PLINTH_SMTP_TLS": "starttls",
		"PLINTH_SMTP_FROM": "Plinth <plinth@example.com>", "PLINTH_PUBLIC_URL": "https://plinth.example.com"} {
		if e[k].Value != want {
			t.Errorf("%s = %q, want %q", k, e[k].Value, want)
		}
	}
	for k, secret := range map[string]string{"PLINTH_SMTP_USERNAME": "mail-login", "PLINTH_SMTP_PASSWORD": "mail-login", "PLINTH_WEBHOOK_URL": "hook", "PLINTH_WEBHOOK_SECRET": "hook"} {
		ref := e[k].ValueFrom
		if e[k].Value != "" || ref == nil || ref.SecretKeyRef.Name != secret {
			t.Errorf("%s must come from the Secret %s, never inline: %+v", k, secret, e[k])
		}
	}
	if !*e["PLINTH_WEBHOOK_SECRET"].ValueFrom.SecretKeyRef.Optional {
		t.Error("the signing key is optional: a Secret without it means unsigned requests")
	}
	if e["PLINTH_SMTP_USERNAME"].ValueFrom.SecretKeyRef.Key != "username" || e["PLINTH_WEBHOOK_URL"].ValueFrom.SecretKeyRef.Key != "url" {
		t.Error("default key names")
	}
	// The public address falls back to a TLS ingress host.
	e = env("--set", "ingress.enabled=true", "--set", "ingress.hosts={plinth.example.com}", "--set", "ingress.tls[0].secretName=tls", "--set", "ingress.tls[0].hosts={plinth.example.com}")
	if e["PLINTH_PUBLIC_URL"].Value != "https://plinth.example.com" {
		t.Errorf("public url from ingress: %q", e["PLINTH_PUBLIC_URL"].Value)
	}
	if _, ok := env("--set", "ingress.enabled=true", "--set", "ingress.hosts={plinth.example.com}")["PLINTH_PUBLIC_URL"]; ok {
		t.Error("an ingress without TLS must not claim an https address")
	}
}

func TestNotificationSettingsAreChecked(t *testing.T) {
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"host without sender":  {[]string{"--set-string", "notifications.smtp.host=smtp.example.com"}, "notifications.smtp.from is required"},
		"sender without host":  {[]string{"--set-string", "notifications.smtp.from=plinth@example.com"}, "notifications.smtp.host is required"},
		"unencrypted":          {[]string{"--set-string", "notifications.smtp.host=h", "--set-string", "notifications.smtp.from=a@b.c", "--set-string", "notifications.smtp.tls=none"}, "allowInsecure"},
		"unknown tls mode":     {[]string{"--set-string", "notifications.smtp.tls=sometimes"}, "tls"},
		"bad port":             {[]string{"--set", "notifications.smtp.port=70000"}, "port"},
		"public url no scheme": {[]string{"--set-string", "config.publicUrl=plinth.example.com"}, "publicUrl"},
	} {
		_, out, err := helmTemplate(t, append([]string{"-f", minimal}, c.args...)...)
		if err == nil || !strings.Contains(out, c.want) {
			t.Errorf("%s: want an error containing %q, got err=%v out=%s", name, c.want, err, out)
		}
	}
}

func TestPreviousMasterKeyOnlyWhileRotating(t *testing.T) {
	c := func(args ...string) map[string]corev1.EnvVar {
		return envOf(deployment(t, render(t, append([]string{"-f", minimal}, args...)...)).Spec.Template.Spec.Containers[0])
	}
	if _, ok := c()["PLINTH_MASTER_KEY_PREVIOUS"]; ok {
		t.Error("no previous key unless asked for")
	}
	env := c("--set-string", "masterKey.previous.existingSecret=old-key")["PLINTH_MASTER_KEY_PREVIOUS"]
	if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef.Name != "old-key" || env.ValueFrom.SecretKeyRef.Key != "master-key-previous" {
		t.Errorf("the previous key must come from a Secret, never inline: %+v", env)
	}
}

func TestUnverifiedEmailIsNotTrustedUnlessAskedFor(t *testing.T) {
	r := render(t, "-f", minimal)
	if _, ok := envOf(deployment(t, r).Spec.Template.Spec.Containers[0])["PLINTH_TRUST_UNVERIFIED_EMAIL"]; ok {
		t.Error("unverified e-mail must not be trusted by default")
	}
	r = render(t, "-f", minimal, "--set", "config.oidc.trustUnverifiedEmail=true")
	if envOf(deployment(t, r).Spec.Template.Spec.Containers[0])["PLINTH_TRUST_UNVERIFIED_EMAIL"].Value != "true" {
		t.Error("trustUnverifiedEmail must reach the server")
	}
}

func TestPlainHTTPIdentityProviderOnlyWhenAskedFor(t *testing.T) {
	r := render(t, "-f", minimal, "--set-string", "config.oidc.issuer=http://idp.local:9100", "--set", "config.oidc.allowHttp=true")
	if envOf(deployment(t, r).Spec.Template.Spec.Containers[0])["PLINTH_OIDC_ALLOW_HTTP"].Value != "true" {
		t.Error("allowHttp must reach the server")
	}
}

func TestNamesCanBeOverriddenAndStayWithinLimits(t *testing.T) {
	r := render(t, "-f", minimal, "--set", "fullnameOverride=onboarding")
	if deployment(t, r).Name != "onboarding" || one[corev1.Service](t, r, "Service").Name != "onboarding" {
		t.Error("fullnameOverride")
	}
	long := strings.Repeat("x", 80)
	r = render(t, "-f", minimal, "--set", "fullnameOverride="+long)
	if n := len(deployment(t, r).Name); n > 63 {
		t.Errorf("name length %d exceeds the Kubernetes limit", n)
	}
}

func TestHelmTestPodChecksTheInstallation(t *testing.T) {
	p := one[corev1.Pod](t, render(t, "-f", minimal), "Pod")
	if p.Annotations["helm.sh/hook"] != "test" {
		t.Errorf("annotations: %v", p.Annotations)
	}
	script := strings.Join(p.Spec.Containers[0].Args, " ")
	for _, want := range []string{"/healthz", "/readyz", "/api/v1/config", "/api/v1/me", "https://idp.example.com", "refuses anonymous"} {
		if !strings.Contains(script, want) {
			t.Errorf("the test does not check %q", want)
		}
	}
	sc := p.Spec.Containers[0].SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation {
		t.Errorf("the test pod should be hardened too: %+v", sc)
	}
}

func TestConfigurationProblemsAreExplained(t *testing.T) {
	ok := []string{"-f", minimal}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"nothing set", nil, "database connection is required"},
		{"no audience", []string{"--set-string", "config.oidc.issuer=https://i.example.com", "--set-string", "database.existingSecret=d", "--set-string", "masterKey.existingSecret=m"}, "config.oidc.audience is required"},
		{"audience without issuer", append(append([]string{}, ok...), "--set-string", "config.oidc.issuer=", "--set-string", "config.oidc.audience=plinth"), "config.oidc.issuer is required"},
		{"nobody could sign in", append(append([]string{}, ok...), "--set-string", "config.oidc.issuer=", "--set-string", "config.oidc.audience=", "--set", "admin.create=false"), "nobody could sign in"},
		{"bad admin username", append(append([]string{}, ok...), "--set-string", "admin.username=Admin User"), "username"},
		{"plain http issuer", append(append([]string{}, ok...), "--set-string", "config.oidc.issuer=http://idp.example.com"), "must be https"},
		{"two database sources", append(append([]string{}, ok...), "--set-string", "database.url=postgres://u:p@h/db"), "not both"},
		{"no database", append(append([]string{}, ok...), "--set-string", "database.existingSecret="), "database connection is required"},
		{"two master key sources", append(append([]string{}, ok...), "--set-string", "masterKey.value=abc"), "not both"},
		{"no master key", append(append([]string{}, ok...), "--set-string", "masterKey.existingSecret="), "master key is required"},
		{"ingress without a host", append(append([]string{}, ok...), "--set", "ingress.enabled=true"), "ingress.hosts"},
		{"zero replicas", append(append([]string{}, ok...), "--set", "replicaCount=0"), "/replicaCount"},
		{"too many workers", append(append([]string{}, ok...), "--set", "config.workers=99"), "/config/workers"},
		{"unknown service type", append(append([]string{}, ok...), "--set-string", "service.type=Magic"), "/service/type"},
		{"bad pull policy", append(append([]string{}, ok...), "--set-string", "image.pullPolicy=Sometimes"), "/image/pullPolicy"},
		{"malformed digest", append(append([]string{}, ok...), "--set-string", "image.digest=sha256:123"), "/image/digest"},
		{"database url that is not postgres", append(append([]string{}, ok...), "--set-string", "masterKey.existingSecret=", "--set-string", "masterKey.value=x", "--set-string", "database.existingSecret=", "--set-string", "database.url=mysql://u@h/db"), "/database/url"},
	}
	for _, c := range cases {
		_, stderr, err := helmTemplate(t, c.args...)
		if err == nil {
			t.Errorf("%s: expected the chart to refuse this", c.name)
			continue
		}
		if !strings.Contains(stderr, c.want) {
			t.Errorf("%s: error should mention %q, got:\n%s", c.name, c.want, stderr)
		}
	}
}

// Tools that render the chart and apply it themselves (Argo CD, Flux) do not
// pass --namespace to the cluster, so every namespaced object must carry it.
func TestEveryObjectNamesItsNamespace(t *testing.T) {
	for _, vals := range [][]string{{"-f", minimal}, {"-f", full}} {
		r := render(t, vals...)
		for kind, list := range r {
			for _, o := range list {
				if o.GetNamespace() != "platform" {
					t.Errorf("%s %s has namespace %q, want the release namespace", kind, o.GetName(), o.GetNamespace())
				}
			}
		}
	}
}

func TestDrainDelayIsConfigurableAndTheGracePeriodFollowsIt(t *testing.T) {
	d := deployment(t, render(t, "-f", minimal, "--set", "shutdownDelaySeconds=15"))
	c := d.Spec.Template.Spec.Containers[0]
	if strings.Join(c.Lifecycle.PreStop.Exec.Command, " ") != "sleep 15" || *d.Spec.Template.Spec.TerminationGracePeriodSeconds != 45 {
		t.Errorf("preStop %v, grace %v", c.Lifecycle.PreStop.Exec.Command, *d.Spec.Template.Spec.TerminationGracePeriodSeconds)
	}
	off := deployment(t, render(t, "-f", minimal, "--set", "shutdownDelaySeconds=0"))
	if off.Spec.Template.Spec.Containers[0].Lifecycle != nil {
		t.Error("0 must switch the delay off")
	}
	if _, stderr, err := helmTemplate(t, "-f", minimal, "--set", "shutdownDelaySeconds=-1"); err == nil || !strings.Contains(stderr, "shutdownDelaySeconds") {
		t.Errorf("a negative delay must be refused: %v %s", err, stderr)
	}
}
