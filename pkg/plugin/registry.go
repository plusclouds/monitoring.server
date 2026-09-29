package plugin

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

var (
	mu       sync.RWMutex
	registry = map[string]Check{}
)

// Register adds a check plugin. Call it from the plugin package's init. It
// panics on an invalid manifest or a duplicate type, so mistakes fail at
// start-up, never at run time.
func Register(c Check) {
	m := c.Manifest()
	if err := validManifest(m); err != nil {
		panic(fmt.Sprintf("plugin %q: %v", m.Type, err))
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[m.Type]; dup {
		panic(fmt.Sprintf("plugin %q registered twice", m.Type))
	}
	registry[m.Type] = c
}

// Lookup returns the check plugin of a type.
func Lookup(typ string) (Check, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[typ]
	return c, ok
}

// All returns every registered check, sorted by type.
func All() []Check {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Check, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Check) int { return strings.Compare(a.Manifest().Type, b.Manifest().Type) })
	return out
}

func validManifest(m Manifest) error {
	switch {
	case m.Type == "":
		return fmt.Errorf("empty type")
	case m.Kind != KindCheck && m.Kind != KindCollector && m.Kind != KindIngester:
		return fmt.Errorf("unknown kind %q", m.Kind)
	case len(m.ConfigSchema) == 0:
		return fmt.Errorf("missing config schema")
	case m.DefaultInterval <= 0 || m.MinInterval <= 0 || m.DefaultInterval < m.MinInterval:
		return fmt.Errorf("default interval must be at least the minimum interval")
	case !slices.Contains([]string{BillingBasic, BillingStandard, BillingPush, BillingAdvanced, BillingFree}, m.BillingClass):
		return fmt.Errorf("unknown billing class %q", m.BillingClass)
	}
	seen := map[string]bool{}
	for _, d := range m.Metrics {
		if d.Name == "" || seen[d.Name] {
			return fmt.Errorf("metric names must be unique and non-empty")
		}
		seen[d.Name] = true
		if d.Kind != "gauge" && d.Kind != "rate" {
			return fmt.Errorf("metric %s: kind must be gauge or rate", d.Name)
		}
		if !slices.Contains([]string{RetentionHighFrequency, RetentionStandard, RetentionCapacity}, d.RetentionClass) {
			return fmt.Errorf("metric %s: unknown retention class %q", d.Name, d.RetentionClass)
		}
	}
	return nil
}
