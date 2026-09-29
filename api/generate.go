// Package api holds the OpenAPI spec, the source of truth for the REST API
// (ADR-0007).
package api

//go:generate go tool oapi-codegen -config oapi-codegen.yaml openapi.yaml
