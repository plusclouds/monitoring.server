package push

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A selector picks one value out of a JSON document: $ is the document,
// .name a field, ["name"] a field with any characters, [n] an array
// element. For example $.sensors.temp, $.readings[0].value or
// $["temp-1"]. A subset of JSONPath without wildcards or filters, so one
// selector always names at most one value.
type selector []step

type step struct {
	field string
	index int // when field is ""
}

const maxSelector = 256

func parseSelector(s string) (selector, error) {
	if len(s) > maxSelector {
		return nil, fmt.Errorf("selector longer than %d characters", maxSelector)
	}
	rest, ok := strings.CutPrefix(s, "$")
	if !ok {
		return nil, errors.New(`a selector starts with $, e.g. "$.sensors.temp"`)
	}
	var sel selector
	for rest != "" {
		switch rest[0] {
		case '.':
			rest = rest[1:]
			n := 0
			for n < len(rest) && rest[n] != '.' && rest[n] != '[' {
				n++
			}
			if n == 0 {
				return nil, fmt.Errorf("selector %q: empty field name", s)
			}
			sel = append(sel, step{field: rest[:n]})
			rest = rest[n:]
		case '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("selector %q: missing ]", s)
			}
			inner := rest[1:end]
			rest = rest[end+1:]
			if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0] {
				if len(inner) == 2 {
					return nil, fmt.Errorf("selector %q: empty field name", s)
				}
				sel = append(sel, step{field: inner[1 : len(inner)-1]})
				continue
			}
			i, err := strconv.Atoi(inner)
			if err != nil || i < 0 {
				return nil, fmt.Errorf("selector %q: [%s] is neither an array index nor a quoted name", s, inner)
			}
			sel = append(sel, step{index: i})
		default:
			return nil, fmt.Errorf("selector %q: expected . or [ after %q", s, s[:len(s)-len(rest)])
		}
	}
	return sel, nil
}

// get returns the selected value of a decoded document, or false.
func (sel selector) get(doc any) (any, bool) {
	v := doc
	for _, st := range sel {
		if st.field != "" {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, false
			}
			if v, ok = m[st.field]; !ok {
				return nil, false
			}
			continue
		}
		a, ok := v.([]any)
		if !ok || st.index >= len(a) {
			return nil, false
		}
		v = a[st.index]
	}
	return v, true
}
