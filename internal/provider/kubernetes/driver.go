package kubernetes

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/getplinth/plinth/internal/provider"
)

const fieldManager = "plinth"

func init() {
	provider.Register(provider.KindCluster, "kubernetes", func(cfg map[string]string) (provider.Provider, error) {
		return FromConfig(cfg)
	})
}

// Driver implements provider.Cluster against one cluster.
type Driver struct {
	cs            k8s.Interface
	adoptExisting bool
}

var _ provider.Cluster = (*Driver)(nil)

// New wraps an existing client (used by tests and in-cluster setups).
func New(cs k8s.Interface, adoptExisting bool) *Driver {
	return &Driver{cs: cs, adoptExisting: adoptExisting}
}

// FromConfig builds a driver from an integration's decrypted configuration.
//
//	server           API server URL (required)
//	ca_cert          PEM CA bundle (required; TLS verification cannot be disabled)
//	token            ServiceAccount bearer token (required)
//	adopt_existing   "true" to take over namespaces created before Plinth
//	                 (they lack the managed-by label). Default "false".
func FromConfig(cfg map[string]string) (*Driver, error) {
	for _, k := range []string{"server", "ca_cert", "token"} {
		if cfg[k] == "" {
			return nil, fmt.Errorf("kubernetes: config %q is required", k)
		}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cfg["ca_cert"])) {
		return nil, errors.New("kubernetes: ca_cert is not valid PEM")
	}
	rc := &rest.Config{
		Host:            cfg["server"],
		BearerToken:     cfg["token"],
		TLSClientConfig: rest.TLSClientConfig{CAData: []byte(cfg["ca_cert"])},
		UserAgent:       "plinth",
	}
	cs, err := k8s.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}
	adopt, _ := strconv.ParseBool(cfg["adopt_existing"])
	return New(cs, adopt), nil
}

func (*Driver) Kind() provider.Kind { return provider.KindCluster }
func (*Driver) Type() string        { return "kubernetes" }

// Validate checks the API server is reachable with the configured credentials.
func (d *Driver) Validate(ctx context.Context) error {
	if _, err := d.cs.Discovery().ServerVersion(); err != nil {
		return fmt.Errorf("kubernetes: cannot reach API server: %w", err)
	}
	if _, err := d.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("kubernetes: credentials cannot list namespaces: %w", err)
	}
	return nil
}

// EnsureNamespace creates or updates the namespace and everything in it.
// Re-running converges the objects to the desired state (server-side apply),
// so it is safe to retry. Outputs: namespace, created ("true"/"false").
func (d *Driver) EnsureNamespace(ctx context.Context, spec provider.NamespaceSpec) (provider.Outputs, error) {
	objs, err := Build(spec)
	if err != nil {
		return nil, err
	}

	created := "false"
	existing, err := d.cs.CoreV1().Namespaces().Get(ctx, spec.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		created = "true"
	case err != nil:
		return nil, fmt.Errorf("kubernetes: get namespace %q: %w", spec.Name, err)
	case !isManaged(existing) && !d.adoptExisting:
		return nil, fmt.Errorf("kubernetes: namespace %q exists but is not managed by Plinth (set adopt_existing to take it over)", spec.Name)
	}

	for _, o := range objs {
		if err := d.apply(ctx, o.Kind); err != nil {
			return nil, fmt.Errorf("kubernetes: apply %s in %q: %w", o.Name, spec.Name, err)
		}
	}
	return provider.Outputs{"namespace": spec.Name, "created": created}, nil
}

// EnsureSecret creates or updates a Secret in a Plinth-managed namespace.
func (d *Driver) EnsureSecret(ctx context.Context, spec provider.SecretSpec) error {
	ns, err := d.cs.CoreV1().Namespaces().Get(ctx, spec.Namespace, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("kubernetes: namespace %q: %w", spec.Namespace, err)
	}
	if !isManaged(ns) {
		return fmt.Errorf("kubernetes: refusing to write secrets into %q: not managed by Plinth", spec.Namespace)
	}
	t := corev1.SecretType(spec.Type)
	if t == "" {
		t = corev1.SecretTypeOpaque
	}
	// Errors from here never include spec.Data.
	return d.apply(ctx, &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name: spec.Name, Namespace: spec.Namespace, Labels: labels(nil),
		},
		Type: t,
		Data: spec.Data,
	})
}

// DeleteNamespace removes a namespace, but only one Plinth created or adopted.
func (d *Driver) DeleteNamespace(ctx context.Context, name string) error {
	ns, err := d.cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kubernetes: get namespace %q: %w", name, err)
	}
	if !isManaged(ns) {
		return fmt.Errorf("kubernetes: refusing to delete %q: not managed by Plinth", name)
	}
	if err := d.cs.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kubernetes: delete namespace %q: %w", name, err)
	}
	return nil
}

func isManaged(ns *corev1.Namespace) bool {
	return ns.Labels[ManagedByLabel] == ManagedByValue
}

// apply performs a server-side apply of one object, taking ownership of the
// fields Plinth sets and overriding conflicting manual edits to them.
func (d *Driver) apply(ctx context.Context, obj runtime.Object) error {
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	force := true
	opts := metav1.PatchOptions{FieldManager: fieldManager, Force: &force}
	p := types.ApplyPatchType

	switch o := obj.(type) {
	case *corev1.Namespace:
		_, err = d.cs.CoreV1().Namespaces().Patch(ctx, o.Name, p, data, opts)
	case *corev1.ServiceAccount:
		_, err = d.cs.CoreV1().ServiceAccounts(o.Namespace).Patch(ctx, o.Name, p, data, opts)
	case *corev1.ResourceQuota:
		_, err = d.cs.CoreV1().ResourceQuotas(o.Namespace).Patch(ctx, o.Name, p, data, opts)
	case *corev1.LimitRange:
		_, err = d.cs.CoreV1().LimitRanges(o.Namespace).Patch(ctx, o.Name, p, data, opts)
	case *corev1.Secret:
		_, err = d.cs.CoreV1().Secrets(o.Namespace).Patch(ctx, o.Name, p, data, opts)
	case *rbacv1.Role:
		_, err = d.cs.RbacV1().Roles(o.Namespace).Patch(ctx, o.Name, p, data, opts)
	case *rbacv1.RoleBinding:
		_, err = d.cs.RbacV1().RoleBindings(o.Namespace).Patch(ctx, o.Name, p, data, opts)
	case *networkingv1.NetworkPolicy:
		_, err = d.cs.NetworkingV1().NetworkPolicies(o.Namespace).Patch(ctx, o.Name, p, data, opts)
	default:
		return fmt.Errorf("unsupported object type %T", obj)
	}
	return err
}
