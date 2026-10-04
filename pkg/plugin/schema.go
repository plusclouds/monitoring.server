package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/invopop/jsonschema"
	jsonschemav "github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaFor generates the JSON Schema (draft 2020-12) of a config struct, so
// the API can validate configs before saving and clients can render forms.
// Use jsonschema struct tags for descriptions, defaults and bounds.
func SchemaFor[T any]() json.RawMessage {
	r := jsonschema.Reflector{
		DoNotReference:            true,
		AllowAdditionalProperties: false,
		ExpandedStruct:            true,
	}
	b, err := json.Marshal(r.Reflect(new(T)))
	if err != nil {
		panic(err) // a config struct that cannot be described is a programming error
	}
	return b
}

// DecodeConfig unmarshals a config onto defaults, rejecting unknown fields.
func DecodeConfig[T any](raw json.RawMessage, defaults T) (T, error) {
	cfg := defaults
	if len(raw) == 0 {
		return cfg, nil
	}
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(&cfg)
	return cfg, err
}

var (
	schemaMu    sync.Mutex
	schemaCache = map[string]*jsonschemav.Schema{}
)

// ValidateConfig checks a config against the plugin's JSON Schema and then
// its own Validate, as the API does before saving a check (F03).
func ValidateConfig(c Check, raw json.RawMessage) error {
	m := c.Manifest()
	sch, err := compiledSchema(m)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	inst, err := jsonschemav.UnmarshalJSON(bytesReader(raw))
	if err != nil {
		return fmt.Errorf("config is not valid JSON: %w", err)
	}
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschemav.ValidationError
		if errors.As(err, &ve) {
			return fmt.Errorf("config does not match the %s schema: %s", m.Type, strings.TrimSpace(ve.Error()))
		}
		return err
	}
	return c.Validate(raw)
}

func compiledSchema(m Manifest) (*jsonschemav.Schema, error) {
	schemaMu.Lock()
	defer schemaMu.Unlock()
	if s, ok := schemaCache[m.Type]; ok {
		return s, nil
	}
	doc, err := jsonschemav.UnmarshalJSON(bytesReader(m.ConfigSchema))
	if err != nil {
		return nil, err
	}
	comp := jsonschemav.NewCompiler()
	if err := comp.AddResource("config.json", doc); err != nil {
		return nil, err
	}
	s, err := comp.Compile("config.json")
	if err != nil {
		return nil, err
	}
	schemaCache[m.Type] = s
	return s, nil
}
