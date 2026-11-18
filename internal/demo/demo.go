// Package demo provides simulated drivers so Plinth can be explored without
// real Git, registry, cluster or database systems. They do nothing except
// wait a little (so progress is visible) and, for tenants whose name contains
// "fail", fail once per project so the failure and retry flow can be tried.
//
// It is linked only into cmd/plinth-demo, never into the real server.
package demo

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/getplinth/plinth/internal/provider"
)

// Delay is how long each simulated call takes.
var Delay = 600 * time.Millisecond

var (
	mu     sync.Mutex
	failed = map[string]bool{}
)

func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(Delay):
		return nil
	}
}

// failOnce fails the first call for any name containing "fail".
func failOnce(key string) error {
	if !strings.Contains(key, "fail") {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	if failed[key] {
		return nil
	}
	failed[key] = true
	return fmt.Errorf("simulated outage while contacting the cluster (this fails once; retry the request)")
}

type base struct{ kind provider.Kind }

func (b base) Kind() provider.Kind            { return b.kind }
func (b base) Type() string                   { return "demo" }
func (b base) Validate(context.Context) error { return pause(context.Background()) }

type git struct{ base }

func (git) EnsureGroup(ctx context.Context, s provider.GroupSpec) (provider.Outputs, error) {
	return provider.Outputs{"name": s.Name}, pause(ctx)
}
func (git) EnsureRepository(ctx context.Context, s provider.RepoSpec) (provider.Outputs, error) {
	return provider.Outputs{"full_name": s.Group.Name + "/" + s.Name, "html_url": "https://git.demo.invalid/" + s.Group.Name + "/" + s.Name}, pause(ctx)
}
func (git) SetVariable(ctx context.Context, _ provider.RepoSpec, _ provider.VariableSpec) error {
	return pause(ctx)
}
func (git) DeleteRepository(context.Context, provider.RepoSpec) error { return nil }

type registry struct{ base }

func (registry) EnsureRepository(ctx context.Context, s provider.RegistrySpec) (provider.Outputs, error) {
	return provider.Outputs{"image_prefix": "registry.demo.invalid/" + s.Repo}, pause(ctx)
}
func (registry) EnsureCredential(ctx context.Context, s provider.RegistrySpec) (provider.Issued, error) {
	return provider.Issued{Outputs: provider.Outputs{"issued_to": s.Repo}, Credential: provider.Credential{
		"server": "registry.demo.invalid", "username": "robot", "password": "demo-only"}}, pause(ctx)
}
func (registry) DeleteRepository(context.Context, provider.RegistrySpec) error { return nil }

type cluster struct {
	base
	name string
}

func (c cluster) EnsureNamespace(ctx context.Context, s provider.NamespaceSpec) (provider.Outputs, error) {
	if err := pause(ctx); err != nil {
		return nil, err
	}
	if c.name == "prod" {
		if err := failOnce(s.Name); err != nil {
			return nil, err
		}
	}
	return provider.Outputs{"namespace": s.Name, "created": "true"}, nil
}
func (cluster) EnsureSecret(ctx context.Context, _ provider.SecretSpec) error { return pause(ctx) }
func (cluster) DeleteNamespace(context.Context, string) error                 { return nil }

type database struct{ base }

// Type matches the real PostgreSQL driver so requests for "postgresql" work.
func (database) Type() string { return "postgres" }
func (database) EnsureDatabase(ctx context.Context, s provider.DatabaseSpec) (provider.Issued, error) {
	return provider.Issued{Outputs: provider.Outputs{"name": s.Name}, Credential: provider.Credential{
		"host": "db.demo.invalid", "port": "5432", "name": s.Name, "user": s.Name, "password": "demo-only"}}, pause(ctx)
}
func (database) DeleteDatabase(context.Context, provider.DatabaseSpec) error { return nil }

// Register makes the simulated drivers available under the driver name "demo".
func Register() {
	reg := func(kind provider.Kind, mk func(map[string]string) provider.Provider) {
		provider.Register(kind, "demo", func(cfg map[string]string) (provider.Provider, error) { return mk(cfg), nil })
	}
	reg(provider.KindGitSource, func(map[string]string) provider.Provider { return git{base{provider.KindGitSource}} })
	reg(provider.KindRegistry, func(map[string]string) provider.Provider { return registry{base{provider.KindRegistry}} })
	reg(provider.KindCluster, func(cfg map[string]string) provider.Provider {
		return cluster{base{provider.KindCluster}, cfg["name"]}
	})
	// Registered under the real driver's name so requests and the UI treat it
	// as PostgreSQL. (cmd/plinth-demo never links the real driver.)
	provider.Register(provider.KindDatabase, "postgres", func(map[string]string) (provider.Provider, error) {
		return database{base{provider.KindDatabase}}, nil
	})
}
