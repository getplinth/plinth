package quota

import (
	"reflect"
	"strings"
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

func TestBuiltInTiersAreValid(t *testing.T) {
	for name, tier := range Default() {
		if p := tier.Validate(); len(p) != 0 {
			t.Errorf("%s: %v", name, p)
		}
	}
}

func TestValidateExplainsEveryProblem(t *testing.T) {
	good := Default()["bronze"]
	mutate := func(f func(*Tier)) []string { c := good; f(&c); return c.Validate() }
	for name, c := range map[string]struct {
		change func(*Tier)
		want   string
	}{
		"no pods":               {func(t *Tier) { t.Pods = 0 }, "pods must be between"},
		"too many pods":         {func(t *Tier) { t.Pods = MaxPods + 1 }, "pods must be between"},
		"not a quantity":        {func(t *Tier) { t.Requests.CPU = "lots" }, `requests CPU "lots" is not a quantity`},
		"empty":                 {func(t *Tier) { t.Limits.Memory = "" }, "limits memory"},
		"zero":                  {func(t *Tier) { t.LimitRange.Min.CPU = "0" }, "must be more than zero"},
		"negative":              {func(t *Tier) { t.Requests.Memory = "-1Gi" }, "must be more than zero"},
		"requests above limits": {func(t *Tier) { t.Requests.CPU = "99" }, "requests CPU must not exceed limits CPU"},
		"memory units differ":   {func(t *Tier) { t.Requests.Memory = "4Gi"; t.Limits.Memory = "3500Mi" }, "requests memory must not exceed limits memory"},
		"min above default":     {func(t *Tier) { t.LimitRange.Min.Memory = "2Gi" }, "minimum memory must not exceed"},
		"request above limit":   {func(t *Tier) { t.LimitRange.DefaultRequest.CPU = "900m" }, "default request CPU must not exceed the default limit"},
		"default above max":     {func(t *Tier) { t.LimitRange.Default.CPU = "2" }, "default limit CPU must not exceed the maximum"},
		"max above namespace":   {func(t *Tier) { t.LimitRange.Max.CPU = "4" }, "maximum CPU per container must not exceed"},
	} {
		got := mutate(c.change)
		found := false
		for _, p := range got {
			found = found || strings.Contains(p, c.want)
		}
		if !found {
			t.Errorf("%s: want a problem containing %q, got %v", name, c.want, got)
		}
	}
	if got := (Tier{}).Validate(); len(got) < 6 {
		t.Errorf("an empty tier should report everything that is missing at once: %v", got)
	}
}

func TestValidName(t *testing.T) {
	for s, ok := range map[string]bool{"bronze": true, "team-a": true, "9lives": true, "": false, "Gold": false, "-x": false, "a b": false, "a_b": false, strings.Repeat("a", 32): false} {
		if ValidName(s) != ok {
			t.Errorf("ValidName(%q) != %v", s, ok)
		}
	}
}
