package provider

import (
	"strings"
	"testing"
)

func TestNewUnknownDriver(t *testing.T) {
	if _, err := New(KindGitSource, "nope", nil); err == nil {
		t.Fatal("expected error for unknown driver")
	}
}

func TestRegisteredListsDriversSorted(t *testing.T) {
	Register(KindCluster, "zz-test-driver", func(map[string]string) (Provider, error) { return nil, nil })
	Register(KindCluster, "aa-test-driver", func(map[string]string) (Provider, error) { return nil, nil })
	var seen []string
	for _, r := range Registered() {
		if r.Kind == KindCluster && strings.HasSuffix(r.Driver, "-test-driver") {
			seen = append(seen, r.Driver)
		}
	}
	if len(seen) != 2 || seen[0] != "aa-test-driver" || seen[1] != "zz-test-driver" {
		t.Fatalf("got %v", seen)
	}
}
