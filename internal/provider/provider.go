// Package provider defines the contracts every integration implements.
//
// A provider is a driver for one external system of some category: a git
// host, a container registry, a Kubernetes cluster, a database server, a
// secret store. Drivers are named by product ("github", "gitlab", "harbor",
// "postgres"...) but the interfaces below never assume a particular one.
//
// Every method MUST be idempotent: calling it twice with the same spec yields
// the same end state, so the orchestrator can retry or resume a failed
// request safely.
//
// Credentials are created by providers and delivered to consumers by
// separate steps (see Credential). A provider never writes a credential to a
// place the caller did not ask for.
package provider

import (
	"context"
	"log/slog"

	"github.com/getplinth/plinth/internal/quota"
)

// Kind is the category of system a provider integrates with.
type Kind string

const (
	KindGitSource Kind = "gitsource"
	KindRegistry  Kind = "registry"
	KindCluster   Kind = "cluster"
	KindDatabase  Kind = "database"
	KindSecrets   Kind = "secrets"
)

// Provider is implemented by every driver.
type Provider interface {
	Kind() Kind
	// Type is the driver name, e.g. "github", "gitlab", "postgres".
	Type() string
	// Validate checks connectivity and permissions of the configured integration.
	Validate(ctx context.Context) error
}

// Outputs are non-secret values a step produces (URLs, names, ids).
type Outputs map[string]string

// Credential is a secret value set. It lives in memory only, is redacted
// when printed or logged, and must be handed to a delivery step in the same
// step that created it (or a retry must be able to recreate it). Plinth's
// database stores a SecretRef, never a Credential.
type Credential map[string]string

func (Credential) String() string       { return "[redacted]" }
func (Credential) GoString() string     { return "provider.Credential([redacted])" }
func (Credential) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// SecretRef points at a secret value in a SecretStore.
type SecretRef string

// Issued is the result of creating something that has a credential.
type Issued struct {
	Outputs Outputs
	// Credential is set only when the credential was newly created or
	// rotated by this call. nil means "already existed, unchanged".
	Credential Credential
}

// ---- Git source ----------------------------------------------------------

type GroupSpec struct {
	Name   string // tenant / team
	Parent string // org or parent group
}

type RepoSpec struct {
	Group        GroupSpec
	Name         string
	Description  string
	Private      bool
	TemplateRepo string // golden template; empty = empty repo
	// TemplateVars are substituted into the template's tracked files
	// (e.g. PROJECT_NAME -> name). Only done when the repo is newly created.
	TemplateVars map[string]string
	// ExtraBranches are created from the default branch if missing
	// (e.g. "develop", "staging" for a promotion pipeline).
	ExtraBranches []string
}

// VariableSpec is a CI/CD variable or secret on a repository.
type VariableSpec struct {
	Key    string
	Value  string // may be secret; never log
	Masked bool
	File   bool   // deliver as a file rather than an env var, where supported
	Scope  string // deployment environment, "" = all
}

type GitSource interface {
	Provider
	EnsureGroup(ctx context.Context, spec GroupSpec) (Outputs, error)
	// EnsureRepository creates and populates the repo only if it does not exist;
	// an existing repo is never modified. Outputs include "created": "true"|"false".
	EnsureRepository(ctx context.Context, spec RepoSpec) (Outputs, error)
	// SetVariable creates or updates a variable (upsert).
	SetVariable(ctx context.Context, repo RepoSpec, v VariableSpec) error
	DeleteRepository(ctx context.Context, spec RepoSpec) error
}

// ---- Container registry --------------------------------------------------

type RegistrySpec struct {
	Repo   string // repository / project name
	Rotate bool   // issue a new credential even if one exists
}

type Registry interface {
	Provider
	EnsureRepository(ctx context.Context, spec RegistrySpec) (Outputs, error)
	// EnsureCredential issues a credential scoped to the repository only.
	// Credential keys: "server", "username", "password".
	EnsureCredential(ctx context.Context, spec RegistrySpec) (Issued, error)
	DeleteRepository(ctx context.Context, spec RegistrySpec) error
}

// ---- Kubernetes cluster ---------------------------------------------------

type NamespaceSpec struct {
	Name   string
	Quota  quota.Tier
	Labels map[string]string
	// AccessGroups are identity-provider groups granted the namespace Role.
	AccessGroups []string
	// IngressNamespace is allowed to reach pods (ingress controller location).
	IngressNamespace string
}

// SecretSpec is a Secret object inside a namespace.
type SecretSpec struct {
	Namespace string
	Name      string
	Type      string // "" = Opaque, or "kubernetes.io/dockerconfigjson"
	Data      map[string][]byte
}

type Cluster interface {
	Provider
	// EnsureNamespace creates the namespace and its ServiceAccount, Role,
	// RoleBinding, ResourceQuota, LimitRange and NetworkPolicy.
	EnsureNamespace(ctx context.Context, spec NamespaceSpec) (Outputs, error)
	// EnsureSecret creates or updates a Secret (upsert).
	EnsureSecret(ctx context.Context, spec SecretSpec) error
	DeleteNamespace(ctx context.Context, name string) error
}

// ---- Database -------------------------------------------------------------

type DatabaseSpec struct {
	Name   string // database name; the role has the same name
	Rotate bool   // new password even if the role exists
}

type Database interface {
	Provider
	// EnsureDatabase creates the database and a least-privilege role.
	// The password is generated only when the role is created or Rotate is
	// set; Issued.Credential keys: "host", "name", "user", "password".
	EnsureDatabase(ctx context.Context, spec DatabaseSpec) (Issued, error)
	DeleteDatabase(ctx context.Context, spec DatabaseSpec) error
}

// ---- Secret store (optional backend) --------------------------------------

// SecretStore is an optional central store (Vault, a cloud secret manager).
// Without one, credentials are delivered straight to their consumers via
// GitSource.SetVariable and Cluster.EnsureSecret.
type SecretStore interface {
	Provider
	Put(ctx context.Context, path string, data Credential) (SecretRef, error)
	Delete(ctx context.Context, ref SecretRef) error
}
