// Package credential stores device credentials (F02). Secret fields are
// encrypted with the keyring (ADR-0006) and never leave the engine through
// the API; only the runner and device tests decrypt them.
package credential

import (
	"encoding/json"
	"slices"
	"sort"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Field is one field of a credential type.
type Field struct {
	Name     string
	Secret   bool
	Required bool
	Enum     []string
	Default  string
	Help     string
}

// Type is a credential type: its fields and which plugins accept it.
type Type struct {
	Name        string
	Description string
	Fields      []Field
}

var types = map[string]Type{}

func define(t Type) { types[t.Name] = t }

func init() {
	user := Field{Name: "username", Required: true}
	password := Field{Name: "password", Secret: true, Required: true}
	define(Type{Name: "snmp_v2c", Description: "SNMP v2c community. Use only where SNMPv3 is not available.",
		Fields: []Field{{Name: "community", Secret: true, Required: true}}})
	define(Type{Name: "snmp_v3", Description: "SNMPv3 user-based security. authPriv is the default.",
		Fields: []Field{
			user,
			{Name: "security_level", Enum: []string{"noAuthNoPriv", "authNoPriv", "authPriv"}, Default: "authPriv"},
			{Name: "auth_protocol", Enum: []string{"SHA", "SHA-224", "SHA-256", "SHA-384", "SHA-512", "MD5"}, Default: "SHA-256"},
			{Name: "auth_password", Secret: true},
			{Name: "priv_protocol", Enum: []string{"AES", "AES-192", "AES-256", "DES"}, Default: "AES"},
			{Name: "priv_password", Secret: true},
			{Name: "context_name"},
		}})
	define(Type{Name: "redfish", Description: "Redfish (BMC) account; use a read-only role.", Fields: []Field{user, password}})
	define(Type{Name: "ipmi", Description: "IPMI over LAN v2.0 account.", Fields: []Field{
		user, password, {Name: "privilege", Enum: []string{"user", "operator"}, Default: "user"},
	}})
	define(Type{Name: "xapi", Description: "XenServer/XCP-ng account with the read-only RBAC role.", Fields: []Field{user, password}})
	define(Type{Name: "http_basic", Description: "HTTP basic authentication.", Fields: []Field{user, password}})
	define(Type{Name: "http_bearer", Description: "HTTP bearer token.", Fields: []Field{{Name: "token", Secret: true, Required: true}}})
	define(Type{Name: "rtsp", Description: "RTSP camera account.", Fields: []Field{user, password}})
	define(Type{Name: "mqtt", Description: "MQTT client credentials for an external broker.", Fields: []Field{user, password}})
}

// Lookup returns a credential type.
func Lookup(name string) (Type, bool) {
	t, ok := types[name]
	return t, ok
}

// All returns every type, sorted by name.
func All() []Type {
	out := make([]Type, 0, len(types))
	for _, t := range types {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Schema is the JSON Schema of the type's fields. Secret fields are
// writeOnly: clients send them, the API never returns them.
func (t Type) Schema() json.RawMessage {
	props := map[string]any{}
	var required []string
	for _, f := range t.Fields {
		p := map[string]any{"type": "string", "minLength": 1, "maxLength": 1024}
		if f.Secret {
			p["writeOnly"] = true
		}
		if len(f.Enum) > 0 {
			p["enum"] = f.Enum
		}
		if f.Default != "" {
			p["default"] = f.Default
		}
		if f.Help != "" {
			p["description"] = f.Help
		}
		props[f.Name] = p
		if f.Required {
			required = append(required, f.Name)
		}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	b, _ := json.Marshal(s)
	return b
}

// Values are a credential's fields split by kind.
type Values struct {
	Fields  map[string]string // stored in clear
	Secrets map[string]string // encrypted
}

// split checks values against the type and separates secret fields.
// Defaults are filled in for missing non-secret fields.
func (t Type) split(in map[string]*string) (Values, error) {
	v := Values{Fields: map[string]string{}, Secrets: map[string]string{}}
	for name := range in {
		if !slices.ContainsFunc(t.Fields, func(f Field) bool { return f.Name == name }) {
			return v, errs.Invalidf("fields."+name, "not a field of %s", t.Name)
		}
	}
	for _, f := range t.Fields {
		p, ok := in[f.Name]
		if !ok || p == nil || *p == "" {
			if f.Default != "" {
				v.Fields[f.Name] = f.Default
			}
			continue
		}
		if len(*p) > 1024 {
			return v, errs.Invalidf("fields."+f.Name, "longer than 1024 characters")
		}
		if len(f.Enum) > 0 && !slices.Contains(f.Enum, *p) {
			return v, errs.Invalidf("fields."+f.Name, "must be one of %v", f.Enum)
		}
		if f.Secret {
			v.Secrets[f.Name] = *p
		} else {
			v.Fields[f.Name] = *p
		}
	}
	return v, nil
}

// check verifies required fields and the SNMPv3 security level rules.
func (t Type) check(v Values) error {
	has := func(name string) bool { return v.Fields[name] != "" || v.Secrets[name] != "" }
	for _, f := range t.Fields {
		if f.Required && !has(f.Name) {
			return errs.Invalidf("fields."+f.Name, "is required")
		}
	}
	if t.Name == "snmp_v3" {
		level := v.Fields["security_level"]
		if level != "noAuthNoPriv" && !has("auth_password") {
			return errs.Invalidf("fields.auth_password", "is required for %s", level)
		}
		if level == "authPriv" && !has("priv_password") {
			return errs.Invalidf("fields.priv_password", "is required for authPriv")
		}
	}
	return nil
}
