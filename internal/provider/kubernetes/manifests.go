// Package kubernetes is the Cluster driver for any Kubernetes distribution.
//
// manifests.go builds the per-project objects as plain typed values with no
// cluster access, so every rule (RBAC, quota, isolation) is unit-testable.
package kubernetes

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/getplinth/plinth/internal/naming"
	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/quota"
)

const (
	// ManagedByLabel marks objects Plinth owns. Deletion is refused for
	// namespaces without it.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "plinth"
)

// Object is a manifest ready to apply.
type Object struct {
	Kind runtime.Object
	Name string
}

// reservedNamespaces can never be created or modified through a request.
var reservedNamespaces = map[string]bool{"default": true}

// CheckNamespace rejects names that are invalid or must never be managed.
func CheckNamespace(spec provider.NamespaceSpec) error {
	n := spec.Name
	if !naming.IsDNSLabel(n) {
		return fmt.Errorf("namespace %q is not a valid DNS label", n)
	}
	if reservedNamespaces[n] || strings.HasPrefix(n, "kube-") {
		return fmt.Errorf("namespace %q is reserved", n)
	}
	if spec.IngressNamespace != "" && n == spec.IngressNamespace {
		return fmt.Errorf("namespace %q is the ingress namespace", n)
	}
	return nil
}

func labels(extra map[string]string) map[string]string {
	l := map[string]string{}
	for k, v := range extra {
		l[k] = v
	}
	l[ManagedByLabel] = ManagedByValue // not overridable
	return l
}

// Build returns every object for a project namespace, in apply order.
func Build(spec provider.NamespaceSpec) ([]Object, error) {
	if err := CheckNamespace(spec); err != nil {
		return nil, err
	}
	ns := spec.Name
	lbl := labels(spec.Labels)
	meta := func(suffix string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: naming.ResourceName(ns, suffix), Namespace: ns, Labels: lbl}
	}

	rq, err := resourceQuota(meta(naming.SuffixQuota), spec.Quota)
	if err != nil {
		return nil, err
	}
	lr, err := limitRange(meta(naming.SuffixLimitRange), spec.Quota.LimitRange)
	if err != nil {
		return nil, err
	}

	return []Object{
		{&corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: lbl},
		}, ns},
		{&corev1.ServiceAccount{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
			ObjectMeta: meta(naming.SuffixServiceAccount),
		}, "serviceaccount"},
		{role(meta(naming.SuffixRole)), "role"},
		{roleBinding(meta(naming.SuffixRoleBinding), ns, spec.AccessGroups), "rolebinding"},
		{rq, "resourcequota"},
		{lr, "limitrange"},
		{networkPolicy(meta(naming.SuffixNetworkPolicy), spec.IngressNamespace), "networkpolicy"},
	}, nil
}

// role is what project members can do inside their namespace. Differences
// from the legacy Role: adds persistentvolumeclaims, statefulsets and pod
// logs (the app template ships a PVC), drops the removed "extensions" group,
// and deliberately omits networkpolicies, resourcequotas and rolebindings so
// tenants cannot weaken their own isolation or limits.
func role(m metav1.ObjectMeta) *rbacv1.Role {
	crud := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	read := []string{"get", "list", "watch"}
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: m,
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"services", "configmaps", "secrets", "pods", "persistentvolumeclaims", "serviceaccounts"}, Verbs: crud},
			{APIGroups: []string{""}, Resources: []string{"pods/log", "events"}, Verbs: read},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments", "statefulsets"}, Verbs: crud},
			{APIGroups: []string{"apps"}, Resources: []string{"replicasets"}, Verbs: read},
			{APIGroups: []string{"batch"}, Resources: []string{"jobs", "cronjobs"}, Verbs: crud},
			{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses"}, Verbs: crud},
		},
	}
}

func roleBinding(m metav1.ObjectMeta, ns string, groups []string) *rbacv1.RoleBinding {
	subjects := []rbacv1.Subject{{
		Kind:      rbacv1.ServiceAccountKind,
		Name:      naming.ResourceName(ns, naming.SuffixServiceAccount),
		Namespace: ns, // required by the API; omitted in the legacy manifest
	}}
	for _, g := range groups {
		subjects = append(subjects, rbacv1.Subject{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: g})
	}
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: m,
		Subjects:   subjects,
		// apiGroup was missing in the legacy manifest.
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: naming.ResourceName(ns, naming.SuffixRole)},
	}
}

func resourceQuota(m metav1.ObjectMeta, t quota.Tier) (*corev1.ResourceQuota, error) {
	q := func(s string) (resource.Quantity, error) { return resource.ParseQuantity(s) }
	hard := corev1.ResourceList{}
	for name, val := range map[corev1.ResourceName]string{
		corev1.ResourceRequestsCPU:    t.Requests.CPU,
		corev1.ResourceRequestsMemory: t.Requests.Memory,
		corev1.ResourceLimitsCPU:      t.Limits.CPU,
		corev1.ResourceLimitsMemory:   t.Limits.Memory,
	} {
		v, err := q(val)
		if err != nil {
			return nil, fmt.Errorf("quota %s=%q: %w", name, val, err)
		}
		hard[name] = v
	}
	hard[corev1.ResourcePods] = *resource.NewQuantity(int64(t.Pods), resource.DecimalSI)
	return &corev1.ResourceQuota{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
		ObjectMeta: m,
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
	}, nil
}

func limitRange(m metav1.ObjectMeta, l quota.LimitRange) (*corev1.LimitRange, error) {
	list := func(what string, r quota.Resources) (corev1.ResourceList, error) {
		cpu, err := resource.ParseQuantity(r.CPU)
		if err != nil {
			return nil, fmt.Errorf("limitRange %s cpu=%q: %w", what, r.CPU, err)
		}
		mem, err := resource.ParseQuantity(r.Memory)
		if err != nil {
			return nil, fmt.Errorf("limitRange %s memory=%q: %w", what, r.Memory, err)
		}
		return corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: mem}, nil
	}
	var item corev1.LimitRangeItem
	item.Type = corev1.LimitTypeContainer
	var err error
	if item.Min, err = list("min", l.Min); err != nil {
		return nil, err
	}
	if item.Max, err = list("max", l.Max); err != nil {
		return nil, err
	}
	if item.Default, err = list("default", l.Default); err != nil {
		return nil, err
	}
	if item.DefaultRequest, err = list("defaultRequest", l.DefaultRequest); err != nil {
		return nil, err
	}
	return &corev1.LimitRange{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "LimitRange"},
		ObjectMeta: m,
		Spec:       corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{item}},
	}, nil
}

// networkPolicy isolates the namespace for ingress: only same-namespace pods
// and the ingress controller namespace may connect. Egress is unrestricted
// (as in the legacy setup).
func networkPolicy(m metav1.ObjectMeta, ingressNS string) *networkingv1.NetworkPolicy {
	from := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	if ingressNS != "" {
		from = append(from, networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"kubernetes.io/metadata.name": ingressNS},
		}})
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: m,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			// One rule per peer, matching the legacy two "from" entries.
			Ingress: ingressRules(from),
		},
	}
}

func ingressRules(peers []networkingv1.NetworkPolicyPeer) []networkingv1.NetworkPolicyIngressRule {
	rules := make([]networkingv1.NetworkPolicyIngressRule, 0, len(peers))
	for _, p := range peers {
		rules = append(rules, networkingv1.NetworkPolicyIngressRule{From: []networkingv1.NetworkPolicyPeer{p}})
	}
	return rules
}
