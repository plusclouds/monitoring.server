// Package errs holds the domain errors that the API maps to problem types:
// not found (404), invalid value (422) and conflict (409).
package errs

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound means the object does not exist or is not visible to the caller.
var ErrNotFound = errors.New("not found")

// Invalid is a request value the domain rejects.
type Invalid struct{ Field, Message string }

func (e *Invalid) Error() string { return e.Field + ": " + e.Message }

// Invalidf builds an *Invalid.
func Invalidf(field, format string, args ...any) error {
	return &Invalid{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Conflict is a request that clashes with the current state. Type is the
// problem type suffix ("name-taken", "in-use", "limit-reached").
type Conflict struct{ Type, Message string }

func (e *Conflict) Error() string { return e.Message }

// Conflictf builds a *Conflict.
func Conflictf(typ, format string, args ...any) error {
	return &Conflict{Type: typ, Message: fmt.Sprintf(format, args...)}
}

// FromDB maps constraint violations to domain errors. names maps constraint
// names to the message of a unique violation; other constraints get a
// generic message.
func FromDB(err error, names map[string]string) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case "23505": // unique_violation
		if msg, ok := names[pg.ConstraintName]; ok {
			return &Conflict{Type: "already-exists", Message: msg}
		}
		return &Conflict{Type: "already-exists", Message: "an object with these values already exists"}
	case "23503": // foreign_key_violation
		if msg, ok := names[pg.ConstraintName]; ok {
			return &Conflict{Type: "in-use", Message: msg}
		}
		return &Conflict{Type: "in-use", Message: "a referenced object does not exist or is still in use"}
	case "23514": // check_violation
		return &Invalid{Field: pg.ConstraintName, Message: "value out of range"}
	}
	return err
}
