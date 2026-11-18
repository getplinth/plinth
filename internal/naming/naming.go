// Package naming derives resource names. The defaults reproduce the legacy
// DevPort conventions so existing tenants keep their names; blueprints will
// be able to override the templates later.
package naming

import (
	"regexp"
	"strings"
	"unicode"
)

var invalid = regexp.MustCompile(`[^a-z0-9_-]`)

// Normalize reproduces the legacy normalize_name: lowercase, drop all
// whitespace, drop everything outside [a-z0-9_-].
func Normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	return invalid.ReplaceAllString(s, "")
}

// dns1123Label is what Kubernetes requires for namespace names.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// MaxNamespaceLen is the Kubernetes limit for a namespace name.
const MaxNamespaceLen = 63

// IsDNSLabel reports whether s is a valid DNS-1123 label (<= 63 chars).
// Stricter than Normalize: legacy allowed '_', which Kubernetes rejects.
func IsDNSLabel(s string) bool {
	return len(s) <= MaxNamespaceLen && dns1123Label.MatchString(s)
}

// Namespace is {tenant}-{project}, identical in every environment's cluster.
func Namespace(tenant, project string) string { return tenant + "-" + project }

// ResourceName builds namespaced object names such as "<ns>-sa".
// Standard suffixes are exported below.
func ResourceName(ns, suffix string) string { return ns + "-" + suffix }

const (
	SuffixServiceAccount = "sa"
	SuffixRole           = "role"
	SuffixRoleBinding    = "role-binding"
	SuffixQuota          = "quota"
	SuffixLimitRange     = "limits"
	SuffixNetworkPolicy  = "isolation"
)

// RegistryRepo is {tenant}-{project}.
func RegistryRepo(tenant, project string) string { return tenant + "-" + project }

func ScopeMap(repo string) string      { return repo + "-scope-map" }
func RegistryToken(repo string) string { return repo + "-token" }

// DBName is normalize({tenant}_{project}_{env}); the DB user has the same name.
func DBName(tenant, project, env string) string {
	return Normalize(tenant + "_" + project + "_" + env)
}

// Secret object names created inside the project namespace.
const (
	RegistrySecret = "regcred"
	DBSecret       = "db-credentials"
	// CI variable that holds the base64 docker config.
	CIDockerAuthVar = "DOCKER_AUTH"
)

// CIVarDB returns the per-environment DB variable name, e.g. DB_HOST_DEV.
func CIVarDB(field, env string) string { return "DB_" + strings.ToUpper(field) + "_" + env }

// GitPath is {parent}/{tenant}/{project}.
func GitPath(parent, tenant, project string) string { return parent + "/" + tenant + "/" + project }

// RBACGroup is the identity-provider group bound to the namespace Role.
// Prefix is the cluster's OIDC group prefix (configured per cluster
// integration, may be empty).
func RBACGroup(prefix, parent, tenant string) string { return prefix + parent + "/" + tenant }
