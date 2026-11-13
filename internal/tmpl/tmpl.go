// Package tmpl expands ${...} references in scenario and stack files.
//
// A reference is a dotted path into a nested map: ${run}, ${vars.order_id},
// ${saved.order.status}, ${env.HOME}, ${deps.redis.addr}. A reference that
// cannot be resolved is an error: d-mock never substitutes a default value.
package tmpl

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var refRe = regexp.MustCompile(`\$\{([A-Za-z0-9_.\-]+)\}`)

const escMark = "\x00DMOCK_ESC\x00"

// Expand replaces every ${path} in s with its value from data. Write $${ for a literal ${.
func Expand(s string, data map[string]any) (string, error) {
	var firstErr error
	s = strings.ReplaceAll(s, "$${", escMark)
	out := refRe.ReplaceAllStringFunc(s, func(m string) string {
		path := m[2 : len(m)-1]
		v, ok := Lookup(data, path)
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("unresolved reference ${%s}", path)
			}
			return m
		}
		return Stringify(v)
	})
	return strings.ReplaceAll(out, escMark, "${"), firstErr
}

// ExpandAny walks v (maps, slices, strings) and expands every string in it.
// A string that is exactly one reference keeps the referenced value's type.
func ExpandAny(v any, data map[string]any) (any, error) {
	switch t := v.(type) {
	case string:
		if m := refRe.FindStringSubmatch(t); m != nil && m[0] == t {
			val, ok := Lookup(data, m[1])
			if !ok {
				return nil, fmt.Errorf("unresolved reference ${%s}", m[1])
			}
			return val, nil
		}
		return Expand(t, data)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			x, err := ExpandAny(e, data)
			if err != nil {
				return nil, err
			}
			out[k] = x
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			x, err := ExpandAny(e, data)
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	default:
		return v, nil
	}
}

// Lookup resolves a dotted path in nested maps (and slices by index).
func Lookup(data map[string]any, path string) (any, bool) {
	var cur any = data
	for _, part := range strings.Split(path, ".") {
		switch t := cur.(type) {
		case map[string]any:
			v, ok := t[part]
			if !ok {
				return nil, false
			}
			cur = v
		case map[string]string:
			v, ok := t[part]
			if !ok {
				return nil, false
			}
			cur = v
		case []any:
			var i int
			if _, err := fmt.Sscanf(part, "%d", &i); err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// Stringify renders scalars plainly and everything else as JSON.
func Stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case json.Number:
		return t.String()
	case float64: // never scientific notation: 250317000000433, not 2.50317000000433e+14
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case map[string]any, []any:
		b, _ := json.Marshal(t)
		return string(b)
	default:
		return fmt.Sprint(t)
	}
}
