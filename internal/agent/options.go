package agent

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options are the settings of one collector instance: the keys of its entry
// in the config file, falling back to the environment variable
// AGENT_<TYPE>_<KEY> (and AGENT_<TYPE>_<KEY>_FILE for secrets), so a
// Dockerfile or compose file can configure the common single-instance case
// without a config file.
type Options struct {
	Type string
	Name string
	m    map[string]any
	env  func(string) (string, bool)
}

// NewOptions builds Options; env nil means the process environment.
func NewOptions(typ, name string, m map[string]any, env func(string) (string, bool)) Options {
	if env == nil {
		env = os.LookupEnv
	}
	if name == "" {
		name = typ
	}
	return Options{Type: typ, Name: name, m: m, env: env}
}

func (o Options) lookup(key string) (string, bool) {
	if v, ok := o.m[key]; ok && v != nil {
		return fmt.Sprint(v), true
	}
	// <key>_file in the config file, or AGENT_<TYPE>_<KEY>_FILE: a secret in a file.
	file, ok := "", false
	if v, has := o.m[key+"_file"]; has && v != nil {
		file, ok = fmt.Sprint(v), true
	}
	base := "AGENT_" + envName(o.Type) + "_" + envName(key)
	if !ok {
		file, ok = o.env(base + "_FILE")
	}
	if ok && file != "" {
		if b, err := os.ReadFile(file); err == nil { //nolint:gosec // the operator names the secret file
			return strings.TrimSpace(string(b)), true
		}
	}
	return o.env(base)
}

func envName(s string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(s))
}

// String returns the option or def.
func (o Options) String(key, def string) string {
	if v, ok := o.lookup(key); ok {
		return v
	}
	return def
}

// Int returns the option or def; a value that is not a number is an error.
func (o Options) Int(key string, def int) (int, error) {
	v, ok := o.lookup(key)
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("%s.%s: %q is not a number", o.Type, key, v)
	}
	return n, nil
}

// Duration returns the option ("5s") or def.
func (o Options) Duration(key string, def time.Duration) (time.Duration, error) {
	v, ok := o.lookup(key)
	if !ok {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s.%s: %q is not a positive duration", o.Type, key, v)
	}
	return d, nil
}

// List returns a comma separated option, or the YAML list, or def.
func (o Options) List(key string, def []string) []string {
	if raw, ok := o.m[key]; ok {
		if l, ok := raw.([]any); ok {
			var out []string
			for _, e := range l {
				out = append(out, fmt.Sprint(e))
			}
			return out
		}
	}
	v, ok := o.lookup(key)
	if !ok {
		return def
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Factory builds a collector from its options.
type Factory func(Options) (Collector, error)

var factories = map[string]Factory{}

// Register adds a collector type; it panics on a duplicate. Call it from the
// collector package's init.
func Register(typ string, f Factory) {
	if _, dup := factories[typ]; dup {
		panic("agent: duplicate collector " + typ)
	}
	factories[typ] = f
}

// Types lists the registered collector types, sorted.
func Types() []string {
	var out []string
	for t := range factories {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
