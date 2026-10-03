// Package api holds the OpenAPI spec, the source of truth for the REST API
// (ADR-0007).
package api

import _ "embed"

//go:generate go tool oapi-codegen -config oapi-codegen.yaml openapi.yaml

// Spec is openapi.yaml as written, served at /v1/openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte
