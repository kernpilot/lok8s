package driver

import (
	"regexp"
	"sort"
	"sync"
)

// Factory builds a driver instance over its dispatch-provided dependencies.
type Factory func(deps *Deps) (Driver, error)

// NameRe is the driver-name allowlist (bash: libs/drivers — the name lands
// in a path the bash sourced, so it is constrained to a safe charset; the Go
// registry keeps the same rule for parity and for the `lo drivers` port).
var NameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var (
	regMu    sync.RWMutex
	registry = map[string]Factory{}
)

// Register adds a driver factory under name. Panics on a duplicate or an
// invalid name — both are programmer errors (registration happens in
// init()).
func Register(name string, f Factory) {
	if !NameRe.MatchString(name) {
		panic("driver: invalid driver name: " + name)
	}
	if f == nil {
		panic("driver: nil factory for " + name)
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("driver: duplicate driver: " + name)
	}
	registry[name] = f
}

// Get returns the registered factory for name.
func Get(name string) (Factory, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}

// Names returns the registered driver names, sorted (the `lo drivers --list`
// source once that command ports).
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
