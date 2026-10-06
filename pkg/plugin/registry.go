package plugin

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

var (
	mu       sync.RWMutex
	registry = map[string]Plugin{}
)

// Register adds a check or collector. Call it from the plugin package's
// init. It panics on an invalid manifest, a kind that does not match the
// plugin's interface, or a duplicate type, so mistakes fail at start-up,
// never at run time.
func Register(c Plugin) {
	m := c.Manifest()
	if err := validManifest(m); err != nil {
		panic(fmt.Sprintf("plugin %q: %v", m.Type, err))
	}
	_, isCheck := c.(Check)
	_, isCollector := c.(Collector)
	if (m.Kind == KindCheck && !isCheck) || (m.Kind == KindCollector && !isCollector) || m.Kind == KindIngester {
		panic(fmt.Sprintf("plugin %q: kind %s does not match its interface", m.Type, m.Kind))
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[m.Type]; dup {
		panic(fmt.Sprintf("plugin %q registered twice", m.Type))
	}
	registry[m.Type] = c
}

// Lookup returns the plugin of a type.
func Lookup(typ string) (Plugin, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[typ]
	return c, ok
}

// All returns every registered plugin, sorted by type.
func All() []Plugin {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Plugin, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Plugin) int { return strings.Compare(a.Manifest().Type, b.Manifest().Type) })
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
	if m.WhoopsyMetric != "" && !slices.ContainsFunc(m.Metrics, func(d MetricDef) bool { return d.Name == m.WhoopsyMetric }) {
		return fmt.Errorf("whoopsy metric %q is not in the layout", m.WhoopsyMetric)
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
