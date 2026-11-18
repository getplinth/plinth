package kubernetes

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getplinth/plinth/internal/provider"
)

func TestEnsureNamespaceCreatesThenConverges(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset()
	d := New(cs, false)

	out, err := d.EnsureNamespace(ctx, spec())
	if err != nil {
		t.Fatal(err)
	}
	if out["created"] != "true" || out["namespace"] != "team-a-shop" {
		t.Fatalf("outputs: %v", out)
	}
	if _, err := cs.CoreV1().ResourceQuotas("team-a-shop").Get(ctx, "team-a-shop-quota", metav1.GetOptions{}); err != nil {
		t.Fatalf("quota not applied: %v", err)
	}
	if _, err := cs.RbacV1().RoleBindings("team-a-shop").Get(ctx, "team-a-shop-role-binding", metav1.GetOptions{}); err != nil {
		t.Fatalf("rolebinding not applied: %v", err)
	}
	if _, err := cs.NetworkingV1().NetworkPolicies("team-a-shop").Get(ctx, "team-a-shop-isolation", metav1.GetOptions{}); err != nil {
		t.Fatalf("networkpolicy not applied: %v", err)
	}

	// Second run: idempotent, reports created=false.
	out, err = d.EnsureNamespace(ctx, spec())
	if err != nil {
		t.Fatal(err)
	}
	if out["created"] != "false" {
		t.Fatalf("second run outputs: %v", out)
	}
}

func TestUnmanagedNamespaceNotTakenOverByDefault(t *testing.T) {
	ctx := context.Background()
	pre := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a-shop"}}
	cs := fake.NewClientset(pre)

	if _, err := New(cs, false).EnsureNamespace(ctx, spec()); err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if _, err := New(cs, true).EnsureNamespace(ctx, spec()); err != nil {
		t.Fatalf("adopt_existing should allow takeover: %v", err)
	}
	ns, _ := cs.CoreV1().Namespaces().Get(ctx, "team-a-shop", metav1.GetOptions{})
	if !isManaged(ns) {
		t.Fatal("adopted namespace should now carry the managed-by label")
	}
}

func TestEnsureSecretOnlyInManagedNamespaces(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "other"}})
	d := New(cs, false)
	if _, err := d.EnsureNamespace(ctx, spec()); err != nil {
		t.Fatal(err)
	}

	err := d.EnsureSecret(ctx, provider.SecretSpec{
		Namespace: "team-a-shop", Name: "db-credentials",
		Data: map[string][]byte{"db-password": []byte("hunter2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := cs.CoreV1().Secrets("team-a-shop").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil || string(s.Data["db-password"]) != "hunter2" || s.Type != corev1.SecretTypeOpaque {
		t.Fatalf("secret: %+v err=%v", s, err)
	}

	err = d.EnsureSecret(ctx, provider.SecretSpec{Namespace: "other", Name: "x", Data: map[string][]byte{"k": []byte("hunter2")}})
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("expected refusal without leaking data, got %v", err)
	}
}

func TestDeleteOnlyManagedNamespaces(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "legacy"}})
	d := New(cs, false)

	if err := d.DeleteNamespace(ctx, "legacy"); err == nil {
		t.Fatal("must refuse to delete an unmanaged namespace")
	}
	if _, err := d.EnsureNamespace(ctx, spec()); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteNamespace(ctx, "team-a-shop"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteNamespace(ctx, "team-a-shop"); err != nil {
		t.Fatalf("deleting a missing namespace should succeed: %v", err)
	}
}

func TestFromConfigRequiresFieldsAndValidCA(t *testing.T) {
	if _, err := FromConfig(map[string]string{"server": "https://x"}); err == nil {
		t.Fatal("missing fields should fail")
	}
	_, err := FromConfig(map[string]string{"server": "https://x", "ca_cert": "nope", "token": "t"})
	if err == nil || strings.Contains(err.Error(), "token") {
		t.Fatalf("expected PEM error, got %v", err)
	}
}

func TestRegisteredAsClusterDriver(t *testing.T) {
	if _, err := provider.New(provider.KindCluster, "kubernetes", map[string]string{}); err == nil ||
		!strings.Contains(err.Error(), "required") {
		t.Fatalf("driver should be registered and validate config, got %v", err)
	}
}
