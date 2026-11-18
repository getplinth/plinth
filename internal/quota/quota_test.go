package quota

import (
	"reflect"
	"testing"
)

func TestDefaultMatchesLegacyTiers(t *testing.T) {
	d := Default()
	if !reflect.DeepEqual(d.Names(), []string{"bronze", "gold", "silver"}) {
		t.Fatalf("names: %v", d.Names())
	}
	b := d["bronze"]
	if b.Pods != 5 || b.Requests.CPU != "2.5" || b.Limits.Memory != "3.5Gi" || b.LimitRange.Max.CPU != "1" {
		t.Fatalf("bronze: %+v", b)
	}
	if d["gold"].LimitRange.DefaultRequest.Memory != "2Gi" {
		t.Fatalf("gold: %+v", d["gold"])
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse([]byte(`{"tiers":{}}`)); err == nil {
		t.Fatal("expected error")
	}
}
