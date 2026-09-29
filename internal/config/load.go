package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvPrefix starts every environment override: MONITOR_DATABASE__APP__DSN
// sets database.app.dsn.
const EnvPrefix = "MONITOR_"

// LookupEnv matches os.LookupEnv; tests pass their own.
type LookupEnv func(string) (string, bool)

// Load reads a core config file (path may be empty for defaults only),
// applies environment overrides and fills the node ID. It does not validate:
// call Validate with the roles the process will run.
func Load(path string, env LookupEnv) (Config, error) {
	c := Default()
	if err := decodeFile(path, &c); err != nil {
		return c, err
	}
	if err := applyEnv(&c, env); err != nil {
		return c, err
	}
	if c.Node.ID == "" {
		h, err := os.Hostname()
		if err != nil {
			return c, fmt.Errorf("node.id is empty and the hostname is unavailable: %w", err)
		}
		c.Node.ID = h
	}
	return c, nil
}

// LoadProbe reads a probe config file and applies environment overrides.
func LoadProbe(path string, env LookupEnv) (ProbeConfig, error) {
	c := DefaultProbe()
	if err := decodeFile(path, &c); err != nil {
		return c, err
	}
	return c, applyEnv(&c, env)
}

func decodeFile(path string, out any) error {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // the operator chooses the config file
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

var (
	durationType = reflect.TypeFor[Duration]()
	byteSizeType = reflect.TypeFor[ByteSize]()
	timeType     = reflect.TypeFor[time.Time]()
)

// applyEnv walks the struct by its yaml tags and sets every field that has a
// matching environment variable. Lists of plain values are comma-separated;
// lists of objects (keys, tokens, heartbeat targets) are file-only.
func applyEnv(target any, env LookupEnv) error {
	if env == nil {
		env = os.LookupEnv
	}
	var errs []error
	walk(reflect.ValueOf(target).Elem(), nil, func(path []string, v reflect.Value, _ reflect.StructField) {
		name := EnvPrefix + strings.ToUpper(strings.Join(path, "__"))
		raw, ok := env(name)
		if !ok {
			return
		}
		if err := setFromString(v, raw); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	})
	return errors.Join(errs...)
}

// walk calls fn for every leaf field, with its yaml key path.
func walk(v reflect.Value, path []string, fn func([]string, reflect.Value, reflect.StructField)) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if key == "" || key == "-" {
			continue
		}
		fv := v.Field(i)
		p := append(append([]string(nil), path...), key)
		if f.Type.Kind() == reflect.Struct && f.Type != timeType {
			walk(fv, p, fn)
			continue
		}
		fn(p, fv, f)
	}
}

func setFromString(v reflect.Value, raw string) error {
	switch v.Type() {
	case durationType:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	case byteSizeType:
		b, err := ParseByteSize(raw)
		if err != nil {
			return err
		}
		v.SetInt(int64(b))
		return nil
	case timeType:
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(t))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Float64:
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.Slice:
		elem := v.Type().Elem()
		if elem.Kind() == reflect.Struct {
			return errors.New("lists of objects can only be set in the config file")
		}
		var parts []string
		if strings.TrimSpace(raw) != "" {
			parts = strings.Split(raw, ",")
		}
		s := reflect.MakeSlice(v.Type(), len(parts), len(parts))
		for i, p := range parts {
			if err := setFromString(s.Index(i), strings.TrimSpace(p)); err != nil {
				return err
			}
		}
		v.Set(s)
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}

// ReadSecret returns a secret given inline or through a file. Setting both is
// an error; so is a file that cannot be read. Trailing newlines are removed.
func ReadSecret(name, inline, file string) (string, error) {
	switch {
	case inline != "" && file != "":
		return "", fmt.Errorf("%s: set either the value or the _file key, not both", name)
	case file != "":
		b, err := os.ReadFile(file) //nolint:gosec // secret files are named in the operator's config
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	default:
		return inline, nil
	}
}

// Redacted returns a copy of the config with every inline secret replaced,
// for `monitor config print`.
func Redacted[T Config | ProbeConfig](c T) T {
	out := c
	walk(reflect.ValueOf(&out).Elem(), nil, func(_ []string, v reflect.Value, f reflect.StructField) {
		if f.Tag.Get("secret") == "true" && v.String() != "" {
			v.SetString("[redacted]")
		}
	})
	// Slices of structs are shared with c; copy the ones that hold secrets.
	if cfg, ok := any(&out).(*Config); ok {
		tokens := make([]PresharedEnrollment, len(cfg.ProbeService.PresharedEnrollmentTokens))
		for i, t := range cfg.ProbeService.PresharedEnrollmentTokens {
			if t.Token != "" {
				t.Token = "[redacted]"
			}
			tokens[i] = t
		}
		cfg.ProbeService.PresharedEnrollmentTokens = tokens
	}
	return out
}
