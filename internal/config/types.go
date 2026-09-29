package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ByteSize is a size in bytes, written in YAML and environment variables as a
// number with an optional IEC unit: 512, 64KiB, 4MiB, 1GiB.
type ByteSize int64

var byteUnits = []struct {
	suffix string
	factor int64
}{
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

// ParseByteSize parses a size such as "64KiB". Decimal units (kB, MB) are
// rejected on purpose so KiB and kB are never confused.
func ParseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	for _, u := range byteUnits {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return ByteSize(n * u.factor), nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q: use a number with KiB, MiB or GiB", s)
	}
	return ByteSize(n), nil
}

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseByteSize(n.Value)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

func (b ByteSize) MarshalYAML() (any, error) { return b.String(), nil }

func (b ByteSize) String() string {
	for _, u := range byteUnits {
		if u.factor > 1 && int64(b) >= u.factor && int64(b)%u.factor == 0 {
			return strconv.FormatInt(int64(b)/u.factor, 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(b), 10)
}

// Duration wraps time.Duration so it prints back in Go syntax ("30s") when the
// effective configuration is shown.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q", n.Value)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d Duration) D() time.Duration { return time.Duration(d) }
