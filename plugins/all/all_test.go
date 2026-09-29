package all_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	_ "github.com/plusclouds/monitoring.server/plugins/all"
)

// F03: every plugin's config schema is a valid JSON Schema, and its default
// config (empty object) validates against it.
func TestManifestsHaveValidSchemas(t *testing.T) {
	all := plugin.All()
	if len(all) < 2 {
		t.Fatalf("expected the MVP plugins to be registered, got %d", len(all))
	}
	for _, p := range all {
		m := p.Manifest()
		t.Run(m.Type, func(t *testing.T) {
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(m.ConfigSchema))
			if err != nil {
				t.Fatal(err)
			}
			c := jsonschema.NewCompiler()
			if err := c.AddResource("schema.json", doc); err != nil {
				t.Fatal(err)
			}
			sch, err := c.Compile("schema.json")
			if err != nil {
				t.Fatalf("invalid JSON Schema: %v", err)
			}
			if err := sch.Validate(map[string]any{}); err != nil {
				t.Errorf("empty config rejected: %v", err)
			}
			if err := sch.Validate(map[string]any{"no_such_field": 1}); err == nil {
				t.Error("schema should reject unknown fields")
			}
			var probe map[string]any
			if err := json.Unmarshal(m.ConfigSchema, &probe); err != nil || probe["type"] != "object" {
				t.Errorf("schema should describe an object: %s", m.ConfigSchema)
			}
		})
	}
}
