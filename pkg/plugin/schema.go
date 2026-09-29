package plugin

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
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
