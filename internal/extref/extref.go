// Package extref is the external ID every linkable resource carries
// (ADR-0012): which system the object belongs to, its type there, and its
// ID there. The engine stores it but never interprets it.
package extref

import (
	"fmt"

	"github.com/plusclouds/monitoring.server/internal/errs"
)

// Ref is an external ID. Type is optional.
type Ref struct {
	Source string  `json:"source"`
	Type   *string `json:"type,omitempty"`
	ID     string  `json:"id"`
}

// Columns returns the three column values, all nil for no reference.
func (r *Ref) Columns() (source, typ, id *string) {
	if r == nil {
		return nil, nil, nil
	}
	s, i := r.Source, r.ID
	return &s, r.Type, &i
}

// FromColumns builds a reference from scanned columns; nil when unset.
func FromColumns(source, typ, id *string) *Ref {
	if source == nil || id == nil {
		return nil
	}
	return &Ref{Source: *source, Type: typ, ID: *id}
}

// Validate checks lengths.
func (r *Ref) Validate() error {
	switch {
	case r == nil:
		return nil
	case r.Source == "" || len(r.Source) > 50:
		return errs.Invalidf("external.source", "must be 1 to 50 characters")
	case r.ID == "" || len(r.ID) > 200:
		return errs.Invalidf("external.id", "must be 1 to 200 characters")
	case r.Type != nil && len(*r.Type) > 200:
		return errs.Invalidf("external.type", "must be at most 200 characters")
	}
	return nil
}

// Key identifies a lookup by external ID. A nil Type matches any type; a
// lookup that then finds more than one object is ambiguous.
type Key struct {
	Source string
	Type   *string
	ID     string
}

// Where is the SQL condition for a lookup by Key, using placeholders $n,
// $n+1 and $n+2 for source, type and ID.
func Where(n int) string {
	return fmt.Sprintf("external_source = $%d AND ($%d::text IS NULL OR external_type = $%d) AND external_id = $%d",
		n, n+1, n+1, n+2)
}

// Args returns the placeholder values for Where.
func (k Key) Args() []any { return []any{k.Source, k.Type, k.ID} }

// ErrAmbiguous is returned when a lookup without a type matches several objects.
var ErrAmbiguous = errs.Conflictf("ambiguous-external-id",
	"several objects have this external ID with different types; pass the type")

// Ref returns the reference the key describes.
func (k Key) Ref() *Ref { return &Ref{Source: k.Source, Type: k.Type, ID: k.ID} }
