package kubernetes

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/quota"
)

func spec() provider.NamespaceSpec {
	return provider.NamespaceSpec{
		Name:             "team-a-shop",
		Quota:            quota.Default()["bronze"],
		Labels:           map[string]string{"team": "team-a", ManagedByLabel: "someone-else"},
		AccessGroups:     []string{"oidc:tenants/team-a"},
		IngressNamespace: "ingress",
	}
}

func find[T any](t *testing.T, objs []Object) T {
	t.Helper()
	for _, o := range objs {
		if v, ok := o.Kind.(T); ok {
			return v
		}
	}
	var zero T
	t.Fatalf("no object of type %T", zero)
	return zero
}

func TestBuildProducesLegacyObjectNames(t *testing.T) {
	objs, err := Build(spec())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 7 {
		t.Fatalf("got %d objects", len(objs))
	}
	if find[*corev1.ServiceAccount](t, objs).Name != "team-a-shop-sa" ||
		find[*rbacv1.Role](t, objs).Name != "team-a-shop-role" ||
		find[*rbacv1.RoleBinding](t, objs).Name != "team-a-shop-role-binding" ||
		find[*corev1.ResourceQuota](t, objs).Name != "team-a-shop-quota" ||
		find[*corev1.LimitRange](t, objs).Name != "team-a-shop-limits" ||
		find[*networkingv1.NetworkPolicy](t, objs).Name != "team-a-shop-isolation" {
		t.Fatal("object names differ from the legacy convention")
	}
}

func TestManagedByLabelCannotBeOverridden(t *testing.T) {
	objs, _ := Build(spec())
	ns := find[*corev1.Namespace](t, objs)
	if ns.Labels[ManagedByLabel] != ManagedByValue || ns.Labels["team"] != "team-a" {
		t.Fatalf("labels: %v", ns.Labels)
	}
}

func TestQuotaFromTier(t *testing.T) {
	objs, _ := Build(spec())
	rq := find[*corev1.ResourceQuota](t, objs)
	if rq.Spec.Hard.Pods().Value() != 5 {
		t.Fatalf("pods: %v", rq.Spec.Hard.Pods())
	}
	want := map[corev1.ResourceName]string{
		corev1.ResourceRequestsCPU:    "2500m",
		corev1.ResourceRequestsMemory: "2560Mi",
		corev1.ResourceLimitsCPU:      "3500m",
		corev1.ResourceLimitsMemory:   "3584Mi",
	}
	for name, v := range want {
		got := rq.Spec.Hard[name]
		w := resource.MustParse(v)
		if got.Cmp(w) != 0 {
			t.Errorf("%s = %s, want %s", name, got.String(), v)
		}
	}
	lr := find[*corev1.LimitRange](t, objs)
	max := lr.Spec.Limits[0].Max[corev1.ResourceCPU]
	if one := resource.MustParse("1"); max.Cmp(one) != 0 {
		t.Fatalf("limit max cpu %s", max.String())
	}
}

func TestRoleBindingFixesLegacyGaps(t *testing.T) {
	objs, _ := Build(spec())
	rb := find[*rbacv1.RoleBinding](t, objs)
	if rb.RoleRef.APIGroup != rbacv1.GroupName {
		t.Fatal("roleRef.apiGroup missing")
	}
	var sa, grp bool
	for _, s := range rb.Subjects {
		switch s.Kind {
		case rbacv1.ServiceAccountKind:
			sa = s.Namespace == "team-a-shop"
		case rbacv1.GroupKind:
			grp = s.Name == "oidc:tenants/team-a"
		}
	}
	if !sa || !grp {
		t.Fatalf("subjects: %+v", rb.Subjects)
	}
}

func TestRoleCoversTemplateButNotIsolation(t *testing.T) {
	objs, _ := Build(spec())
	role := find[*rbacv1.Role](t, objs)
	has := func(res string) bool {
		for _, r := range role.Rules {
			for _, x := range r.Resources {
				if x == res {
					return true
				}
			}
		}
		return false
	}
	if !has("persistentvolumeclaims") {
		t.Error("app template ships a PVC; role must allow it")
	}
	for _, forbidden := range []string{"networkpolicies", "resourcequotas", "limitranges", "roles", "rolebindings"} {
		if has(forbidden) {
			t.Errorf("tenants must not manage %s", forbidden)
		}
	}
	for _, r := range role.Rules {
		for _, g := range r.APIGroups {
			if g == "extensions" {
				t.Error("removed API group 'extensions' present")
			}
		}
	}
}

func TestNetworkPolicyAllowsOnlySameNamespaceAndIngress(t *testing.T) {
	objs, _ := Build(spec())
	np := find[*networkingv1.NetworkPolicy](t, objs)
	if len(np.Spec.Ingress) != 2 || len(np.Spec.PolicyTypes) != 1 {
		t.Fatalf("%+v", np.Spec)
	}
	if np.Spec.Ingress[1].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "ingress" {
		t.Fatal("ingress namespace not allowed")
	}
	s := spec()
	s.IngressNamespace = ""
	objs, _ = Build(s)
	if n := len(find[*networkingv1.NetworkPolicy](t, objs).Spec.Ingress); n != 1 {
		t.Fatalf("without ingress namespace expected 1 rule, got %d", n)
	}
}

func TestReservedAndInvalidNamespacesRejected(t *testing.T) {
	for _, name := range []string{"default", "kube-system", "kube-public", "ingress", "Bad_Name", ""} {
		s := spec()
		s.Name = name
		if _, err := Build(s); err == nil {
			t.Errorf("namespace %q should be rejected", name)
		} else if strings.Contains(err.Error(), "<nil>") {
			t.Errorf("bad error for %q: %v", name, err)
		}
	}
}

func TestBadQuotaValueRejected(t *testing.T) {
	s := spec()
	s.Quota.Requests.CPU = "lots"
	if _, err := Build(s); err == nil {
		t.Fatal("expected error for unparsable quantity")
	}
}
