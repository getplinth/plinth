package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/quota"
	"github.com/getplinth/plinth/internal/store"
)

type liar struct{}

func (liar) Kind() provider.Kind            { return provider.KindGitSource }
func (liar) Type() string                   { return "not-the-registered-name" }
func (liar) Validate(context.Context) error { return nil }
func (liar) EnsureGroup(context.Context, provider.GroupSpec) (provider.Outputs, error) {
	return nil, nil
}
func (liar) EnsureRepository(context.Context, provider.RepoSpec) (provider.Outputs, error) {
	return nil, nil
}
func (liar) SetVariable(context.Context, provider.RepoSpec, provider.VariableSpec) error { return nil }
func (liar) DeleteRepository(context.Context, provider.RepoSpec) error                   { return nil }

func TestBuildRejectsADriverThatDoesNotReportItsRegisteredName(t *testing.T) {
	provider.Register(provider.KindGitSource, "liar-test", func(map[string]string) (provider.Provider, error) { return liar{}, nil })
	rb := store.ResolvedBlueprint{
		Blueprint: store.Blueprint{Name: "x", GitSource: "g", Environments: []store.BlueprintEnv{{Name: "dev", Cluster: "c"}}},
		Integrations: map[string]store.IntegrationSpec{
			"g": {Name: "g", Kind: "gitsource", Driver: "liar-test"},
		},
	}
	_, err := Build(rb, quota.Default(), "t")
	if err == nil || !strings.Contains(err.Error(), "reports itself as") {
		t.Fatalf("expected a driver name mismatch error, got %v", err)
	}
}

func TestBuildReportsMissingIntegrations(t *testing.T) {
	rb := store.ResolvedBlueprint{Blueprint: store.Blueprint{Name: "x", GitSource: "gone"}, Integrations: map[string]store.IntegrationSpec{}}
	if _, err := Build(rb, quota.Default(), "t"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("got %v", err)
	}
}

func TestTemplateVarsSubstituteProjectAndTenant(t *testing.T) {
	provider.Register(provider.KindGitSource, "ok-test", func(map[string]string) (provider.Provider, error) { return okGit{}, nil })
	provider.Register(provider.KindCluster, "ok-test", func(map[string]string) (provider.Provider, error) { return nil, nil })
	rb := store.ResolvedBlueprint{
		Blueprint:    store.Blueprint{Name: "x", GitSource: "g", TemplateVars: map[string]string{"NAME": "{tenant}-{project}", "FIXED": "v"}},
		Integrations: map[string]store.IntegrationSpec{"g": {Name: "g", Kind: "gitsource", Driver: "ok-test"}},
	}
	bp, err := Build(rb, quota.Default(), "team-a")
	if err != nil {
		t.Fatal(err)
	}
	got := bp.TemplateVars("shop")
	if got["NAME"] != "team-a-shop" || got["FIXED"] != "v" {
		t.Fatalf("vars: %v", got)
	}
}

type okGit struct{ liar }

func (okGit) Type() string { return "ok-test" }
