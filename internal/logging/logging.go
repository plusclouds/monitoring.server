// Package logging builds the process logger: log/slog, JSON by default, with
// a level that can change at runtime (SIGHUP reloads it from the config).
//
// Standard attributes across the code base (ADR-0011): tenant_id, device_id,
// check_id, request_id. Secrets never reach a logger as plain strings: wrap
// them in Secret.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Logger is a slog.Logger whose level can be changed later.
type Logger struct {
	*slog.Logger
	level *slog.LevelVar
}

// New returns a logger writing to w in format "json" or "text".
func New(w io.Writer, format, level string) (*Logger, error) {
	lv := new(slog.LevelVar)
	if err := SetLevel(lv, level); err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
	return &Logger{Logger: slog.New(h), level: lv}, nil
}

// SetLevel changes the level of an existing logger.
func (l *Logger) SetLevel(level string) error { return SetLevel(l.level, level) }

func SetLevel(lv *slog.LevelVar, level string) error {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return fmt.Errorf("unknown log level %q", level)
	}
	lv.Set(l)
	return nil
}

// Secret is a string that logs as "[redacted]" in every handler.
type Secret string

func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

func (Secret) String() string { return "[redacted]" }
