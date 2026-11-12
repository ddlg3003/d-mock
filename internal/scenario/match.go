package scenario

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"d-mock/internal/tmpl"
)

// Check evaluates expect against a step result. Each key is a dotted path into the result;
// each value is either a literal (compared as text) or a matcher object:
//
//	eq, ne, contains, not_contains, match (regex), gt, gte, lt, lte, exists (bool), len
//
// contains / not_contains on a list test whether any element equals (or contains) the text.
func Check(result map[string]any, expect map[string]any) []string {
	var fails []string
	keys := make([]string, 0, len(expect))
	for k := range expect {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, path := range keys {
		got, ok := tmpl.Lookup(result, path)
		want := expect[path]
		m, isMatcher := want.(map[string]any)
		if !isMatcher {
			m = map[string]any{"eq": want}
		}
		for _, op := range sortedOps(m) {
			if msg := apply(op, m[op], got, ok); msg != "" {
				fails = append(fails, fmt.Sprintf("%s: %s", path, msg))
			}
		}
	}
	return fails
}

func sortedOps(m map[string]any) []string {
	ops := make([]string, 0, len(m))
	for k := range m {
		ops = append(ops, k)
	}
	sort.Strings(ops)
	return ops
}

func apply(op string, want, got any, ok bool) string {
	if op == "exists" {
		if b, _ := want.(bool); b != ok {
			return fmt.Sprintf("exists=%v, want %v", ok, b)
		}
		return ""
	}
	if !ok {
		return fmt.Sprintf("missing (want %s %v)", op, want)
	}
	gs, ws := tmpl.Stringify(got), tmpl.Stringify(want)
	switch op {
	case "eq":
		if gs != ws {
			return fmt.Sprintf("got %q, want %q", short(gs), ws)
		}
	case "ne":
		if gs == ws {
			return fmt.Sprintf("got %q, want anything else", ws)
		}
	case "contains", "not_contains":
		has := containsAny(got, ws)
		if op == "contains" && !has {
			return fmt.Sprintf("%q does not contain %q", short(gs), ws)
		}
		if op == "not_contains" && has {
			return fmt.Sprintf("%q contains %q", short(gs), ws)
		}
	case "match":
		re, err := regexp.Compile(ws)
		if err != nil {
			return "bad regex: " + err.Error()
		}
		if !re.MatchString(gs) {
			return fmt.Sprintf("%q does not match /%s/", short(gs), ws)
		}
	case "gt", "gte", "lt", "lte":
		g, err1 := strconv.ParseFloat(gs, 64)
		w, err2 := strconv.ParseFloat(ws, 64)
		if err1 != nil || err2 != nil {
			return fmt.Sprintf("%s needs numbers, got %q vs %q", op, short(gs), ws)
		}
		pass := map[string]bool{"gt": g > w, "gte": g >= w, "lt": g < w, "lte": g <= w}[op]
		if !pass {
			return fmt.Sprintf("got %v, want %s %v", g, op, w)
		}
	case "len":
		n := -1
		switch t := got.(type) {
		case []any:
			n = len(t)
		case []map[string]any:
			n = len(t)
		case string:
			n = len(t)
		case map[string]any:
			n = len(t)
		}
		if strconv.Itoa(n) != ws {
			return fmt.Sprintf("len %d, want %s", n, ws)
		}
	default:
		return "unknown matcher " + op
	}
	return ""
}

func containsAny(got any, want string) bool {
	switch t := got.(type) {
	case []any:
		for _, e := range t {
			if tmpl.Stringify(e) == want {
				return true
			}
		}
		return false
	case []string:
		for _, e := range t {
			if e == want {
				return true
			}
		}
		return false
	}
	return strings.Contains(tmpl.Stringify(got), want)
}

func short(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
