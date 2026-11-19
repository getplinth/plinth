// Package quota holds namespace resource tiers (bronze/silver/gold...).
// The defaults are the tiers from the legacy DevPort quota.json; admins will
// be able to override them per blueprint.
package quota

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"
)

// DefaultTier is used when a project does not name one (legacy default).
const DefaultTier = "bronze"

type Resources struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type LimitRange struct {
	Min            Resources `json:"min"`
	Max            Resources `json:"max"`
	Default        Resources `json:"default"`
	DefaultRequest Resources `json:"defaultRequest"`
}

// Tier maps to one ResourceQuota plus one LimitRange.
type Tier struct {
	Pods       int        `json:"pods"`
	Requests   Resources  `json:"requests"`
	Limits     Resources  `json:"limits"`
	LimitRange LimitRange `json:"limitRange"`
}

type Tiers map[string]Tier

//go:embed tiers.json
var defaultJSON []byte

// Default returns the built-in tiers.
func Default() Tiers {
	t, err := Parse(defaultJSON)
	if err != nil {
		panic("quota: embedded tiers.json is invalid: " + err.Error())
	}
	return t
}

// Parse reads the legacy {"tiers": {...}} format.
func Parse(b []byte) (Tiers, error) {
	var f struct {
		Tiers Tiers `json:"tiers"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("quota: %w", err)
	}
	if len(f.Tiers) == 0 {
		return nil, fmt.Errorf("quota: no tiers defined")
	}
	return f.Tiers, nil
}

// Names returns tier names, sorted.
func (t Tiers) Names() []string {
	n := make([]string, 0, len(t))
	for k := range t {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// MaxPods bounds what one tier may grant.
const MaxPods = 1000

// ValidName reports whether s can name a tier.
func ValidName(s string) bool { return nameRe.MatchString(s) }

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// Validate checks a tier before it is stored. It returns every problem at once,
// in sentences an administrator can act on. A tier that passes can be turned
// into a Kubernetes ResourceQuota and LimitRange that the cluster accepts.
func (t Tier) Validate() []string {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	if t.Pods < 1 || t.Pods > MaxPods {
		add("pods must be between 1 and %d", MaxPods)
	}
	q := func(label, v string) (resource.Quantity, bool) {
		parsed, err := resource.ParseQuantity(v)
		switch {
		case v == "" || err != nil:
			add("%s %q is not a quantity such as 500m, 2 or 512Mi", label, v)
			return parsed, false
		case parsed.Sign() <= 0:
			add("%s must be more than zero", label)
			return parsed, false
		}
		return parsed, true
	}
	// Each of CPU and memory is checked on its own, so one bad value does not hide other problems.
	type value struct {
		q  resource.Quantity
		ok bool
	}
	type res struct{ cpu, mem value }
	get := func(name string, r Resources) res {
		c, okc := q(name+" CPU", r.CPU)
		m, okm := q(name+" memory", r.Memory)
		return res{value{c, okc}, value{m, okm}}
	}
	req := get("requests", t.Requests)
	lim := get("limits", t.Limits)
	lmin := get("minimum", t.LimitRange.Min)
	lmax := get("maximum", t.LimitRange.Max)
	def := get("default limit", t.LimitRange.Default)
	dreq := get("default request", t.LimitRange.DefaultRequest)

	le := func(small, big value, what string) {
		if small.ok && big.ok && small.q.Cmp(big.q) > 0 {
			add("%s", what)
		}
	}
	both := func(small, big res, cpuMsg, memMsg string) {
		le(small.cpu, big.cpu, cpuMsg)
		le(small.mem, big.mem, memMsg)
	}
	both(req, lim, "requests CPU must not exceed limits CPU", "requests memory must not exceed limits memory")
	both(lmin, dreq, "minimum CPU must not exceed the default request", "minimum memory must not exceed the default request")
	both(dreq, def, "default request CPU must not exceed the default limit", "default request memory must not exceed the default limit")
	both(def, lmax, "default limit CPU must not exceed the maximum", "default limit memory must not exceed the maximum")
	both(lmin, lmax, "minimum CPU must not exceed the maximum", "minimum memory must not exceed the maximum")
	// One container can never use more than the whole namespace may.
	both(lmax, lim, "maximum CPU per container must not exceed the namespace limits CPU", "maximum memory per container must not exceed the namespace limits memory")
	return problems
}
