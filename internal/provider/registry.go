package provider

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Factory builds a provider from its stored, decrypted configuration.
type Factory func(config map[string]string) (Provider, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

func key(k Kind, t string) string { return string(k) + "/" + t }

// Register makes a driver available. Drivers call this from init().
func Register(k Kind, driverType string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[key(k, driverType)]; dup {
		panic("provider: duplicate registration " + key(k, driverType))
	}
	factories[key(k, driverType)] = f
}

// New instantiates a registered driver.
func New(k Kind, driverType string, config map[string]string) (Provider, error) {
	mu.RLock()
	f, ok := factories[key(k, driverType)]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider: unknown driver %s", key(k, driverType))
	}
	return f(config)
}

// Registration identifies an available driver.
type Registration struct {
	Kind   Kind   `json:"kind"`
	Driver string `json:"driver"`
}

// Registered lists the drivers compiled into this binary, sorted.
func Registered() []Registration {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Registration, 0, len(factories))
	for k := range factories {
		kind, driver, _ := strings.Cut(k, "/")
		out = append(out, Registration{Kind: Kind(kind), Driver: driver})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Driver < out[j].Driver
	})
	return out
}
