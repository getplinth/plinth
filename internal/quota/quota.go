// Package quota holds namespace resource tiers (bronze/silver/gold...).
// The defaults are the tiers from the legacy DevPort quota.json; admins will
// be able to override them per blueprint.
package quota

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
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
