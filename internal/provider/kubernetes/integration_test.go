//go:build integration

package kubernetes

// Runs the driver against a REAL cluster, authenticating as the least-privilege
// ServiceAccount from deploy/kubernetes/plinth-rbac.yaml.
//
// SAFETY: this creates and deletes namespaces and a ClusterRole. It only runs
// when PLINTH_TEST_CONTEXT names a kubeconfig context containing "plinth-test"
// (see scripts/itest.sh), so it cannot hit a real environment by accident.

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/quota"
)

func adminConfig(t *testing.T) *rest.Config {
	t.Helper()
	ctxName := os.Getenv("PLINTH_TEST_CONTEXT")
	if !strings.Contains(ctxName, "plinth-test") {
		t.Skip("set PLINTH_TEST_CONTEXT to a kubeconfig context containing \"plinth-test\" (see scripts/itest.sh)")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: ctxName},
	).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// bootstrapRBAC applies deploy/kubernetes/plinth-rbac.yaml as admin.
func bootstrapRBAC(t *testing.T, ctx context.Context, admin k8s.Interface) {
	t.Helper()
	raw, err := os.ReadFile("../../../deploy/kubernetes/plinth-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dec := scheme.Codecs.UniversalDeserializer()
	for _, doc := range bytes.Split(raw, []byte("\n---")) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj, _, err := dec.Decode(doc, nil, nil)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := createOrReplace(ctx, admin, obj); err != nil {
			t.Fatalf("bootstrap %T: %v", obj, err)
		}
	}
}

func createOrReplace(ctx context.Context, cs k8s.Interface, obj runtime.Object) error {
	var err error
	switch o := obj.(type) {
	case *corev1.Namespace:
		_, err = cs.CoreV1().Namespaces().Create(ctx, o, metav1.CreateOptions{})
	case *corev1.ServiceAccount:
		_, err = cs.CoreV1().ServiceAccounts(o.Namespace).Create(ctx, o, metav1.CreateOptions{})
	case *rbacv1.ClusterRole:
		if _, err = cs.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
			_ = cs.RbacV1().ClusterRoles().Delete(ctx, o.Name, metav1.DeleteOptions{})
			_, err = cs.RbacV1().ClusterRoles().Create(ctx, o, metav1.CreateOptions{})
		}
	case *rbacv1.ClusterRoleBinding:
		if _, err = cs.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
			_ = cs.RbacV1().ClusterRoleBindings().Delete(ctx, o.Name, metav1.DeleteOptions{})
			_, err = cs.RbacV1().ClusterRoleBindings().Create(ctx, o, metav1.CreateOptions{})
		}
	default:
		return fmt.Errorf("unsupported %T", obj)
	}
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func allowed(t *testing.T, ctx context.Context, cs k8s.Interface, sar authzv1.SubjectAccessReviewSpec) bool {
	t.Helper()
	res, err := cs.AuthorizationV1().SubjectAccessReviews().Create(ctx,
		&authzv1.SubjectAccessReview{Spec: sar}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Status.Allowed
}

func TestKubernetesDriverAgainstRealCluster(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	adminCfg := adminConfig(t)
	admin, err := k8s.NewForConfig(adminCfg)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapRBAC(t, ctx, admin)

	// Authenticate as the least-privilege Plinth ServiceAccount, through FromConfig.
	tok, err := admin.CoreV1().ServiceAccounts("plinth-system").CreateToken(ctx, "plinth",
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{ExpirationSeconds: ptr(int64(600))}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ca := adminCfg.CAData
	if len(ca) == 0 {
		if ca, err = os.ReadFile(adminCfg.CAFile); err != nil {
			t.Fatal(err)
		}
	}
	drv, err := FromConfig(map[string]string{"server": adminCfg.Host, "ca_cert": string(ca), "token": tok.Status.Token})
	if err != nil {
		t.Fatal(err)
	}
	if err := drv.Validate(ctx); err != nil {
		t.Fatalf("Validate as plinth SA: %v", err)
	}

	name := fmt.Sprintf("itest-%06d", rand.Intn(1_000_000))
	t.Cleanup(func() {
		_ = admin.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	spec := provider.NamespaceSpec{
		Name:             name,
		Quota:            quota.Default()["bronze"],
		Labels:           map[string]string{"plinth-test": "true"},
		AccessGroups:     []string{"oidc:itest"},
		IngressNamespace: "kube-system",
	}

	t.Run("create then converge", func(t *testing.T) {
		out, err := drv.EnsureNamespace(ctx, spec)
		if err != nil {
			t.Fatalf("first run: %v", err) // privilege escalation / schema problems surface here
		}
		if out["created"] != "true" {
			t.Fatalf("outputs: %v", out)
		}
		out, err = drv.EnsureNamespace(ctx, spec)
		if err != nil || out["created"] != "false" {
			t.Fatalf("second run: %v %v", out, err)
		}
	})

	t.Run("objects exist with expected values", func(t *testing.T) {
		rq, err := admin.CoreV1().ResourceQuotas(name).Get(ctx, name+"-quota", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cpu := rq.Spec.Hard[corev1.ResourceRequestsCPU]
		if want := resource.MustParse("2500m"); cpu.Cmp(want) != 0 {
			t.Fatalf("requests.cpu = %s", cpu.String())
		}
		rb, err := admin.RbacV1().RoleBindings(name).Get(ctx, name+"-role-binding", metav1.GetOptions{})
		if err != nil || rb.RoleRef.APIGroup != rbacv1.GroupName {
			t.Fatalf("rolebinding: %+v %v", rb, err)
		}
		if _, err := admin.NetworkingV1().NetworkPolicies(name).Get(ctx, name+"-isolation", metav1.GetOptions{}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("tenant permissions", func(t *testing.T) {
		tenant := func(verb, group, res string) bool {
			return allowed(t, ctx, admin, authzv1.SubjectAccessReviewSpec{
				User: "alice", Groups: []string{"oidc:itest"},
				ResourceAttributes: &authzv1.ResourceAttributes{Namespace: name, Verb: verb, Group: group, Resource: res},
			})
		}
		for _, c := range [][3]string{
			{"create", "apps", "deployments"},
			{"create", "", "persistentvolumeclaims"}, // the app template ships a PVC
			{"create", "", "secrets"},
			{"create", "networking.k8s.io", "ingresses"},
		} {
			if !tenant(c[0], c[1], c[2]) {
				t.Errorf("tenant should be able to %s %s", c[0], c[2])
			}
		}
		for _, c := range [][3]string{
			{"update", "networking.k8s.io", "networkpolicies"},
			{"update", "", "resourcequotas"},
			{"create", "rbac.authorization.k8s.io", "rolebindings"},
			{"delete", "", "namespaces"},
		} {
			if tenant(c[0], c[1], c[2]) {
				t.Errorf("tenant must NOT be able to %s %s", c[0], c[2])
			}
		}
		// Not another namespace.
		if allowed(t, ctx, admin, authzv1.SubjectAccessReviewSpec{
			User: "alice", Groups: []string{"oidc:itest"},
			ResourceAttributes: &authzv1.ResourceAttributes{Namespace: "kube-system", Verb: "get", Resource: "pods"},
		}) {
			t.Error("tenant group must not reach kube-system")
		}
	})

	t.Run("limit range rejects oversized containers", func(t *testing.T) {
		_, err := admin.CoreV1().Pods(name).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "toobig"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "c", Image: "registry.k8s.io/pause:3.9",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("256Mi")},
				},
			}}},
		}, metav1.CreateOptions{})
		if err == nil || !strings.Contains(err.Error(), "maximum cpu usage per Container") {
			t.Fatalf("expected LimitRange rejection, got %v", err)
		}
	})

	t.Run("secrets are written but the plinth identity cannot read them", func(t *testing.T) {
		err := drv.EnsureSecret(ctx, provider.SecretSpec{
			Namespace: name, Name: "db-credentials",
			Data: map[string][]byte{"db-password": []byte("integration-test-value")},
		})
		if err != nil {
			t.Fatal(err)
		}
		s, err := admin.CoreV1().Secrets(name).Get(ctx, "db-credentials", metav1.GetOptions{})
		if err != nil || string(s.Data["db-password"]) != "integration-test-value" {
			t.Fatalf("secret not stored: %v", err)
		}
		if allowed(t, ctx, admin, authzv1.SubjectAccessReviewSpec{
			User:               "system:serviceaccount:plinth-system:plinth",
			ResourceAttributes: &authzv1.ResourceAttributes{Namespace: name, Verb: "get", Resource: "secrets"},
		}) {
			t.Error("plinth ServiceAccount should not be able to read secrets")
		}
	})

	t.Run("guards", func(t *testing.T) {
		bad := spec
		bad.Name = "kube-system"
		if _, err := drv.EnsureNamespace(ctx, bad); err == nil {
			t.Error("kube-system must be refused")
		}
		if err := drv.DeleteNamespace(ctx, "kube-system"); err == nil {
			t.Error("deleting an unmanaged namespace must be refused")
		}
	})

	t.Run("delete", func(t *testing.T) {
		if err := drv.DeleteNamespace(ctx, name); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			ns, err := admin.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) || (err == nil && ns.DeletionTimestamp != nil) {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatal("namespace not deleted")
	})
}

func ptr[T any](v T) *T { return &v }
