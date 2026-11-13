package tmpl

import (
	"fmt"
	"strings"
)

const (
	shBare = iota
	shSingle
	shDouble
)

// ExpandShell expands references in a shell command line so that each one is exactly one
// word of literal text, whatever it contains: a value (a DSN with `&`, an answer from the
// system under test with `; cmd` or `$(cmd)`) can never become shell syntax. The quoting
// follows where the reference sits (bare, inside "double" or inside 'single' quotes), so
// lines that already quote a reference keep working.
func ExpandShell(s string, data map[string]any) (string, error) {
	var b strings.Builder
	state := shBare
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '$' && strings.HasPrefix(s[i:], "$${") {
			b.WriteString("${")
			i += 2
			continue
		}
		if c == '$' {
			if m := refRe.FindStringSubmatchIndex(s[i:]); m != nil && m[0] == 0 {
				path := s[i+m[2] : i+m[3]]
				v, ok := Lookup(data, path)
				if !ok {
					return s, fmt.Errorf("unresolved reference ${%s}", path)
				}
				b.WriteString(shellQuote(Stringify(v), state))
				i += m[1] - 1
				continue
			}
		}
		b.WriteByte(c)
		switch {
		case state == shBare && c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case state == shBare && c == '\'':
			state = shSingle
		case state == shBare && c == '"':
			state = shDouble
		case state == shSingle && c == '\'':
			state = shBare
		case state == shDouble && c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case state == shDouble && c == '"':
			state = shBare
		}
	}
	return b.String(), nil
}

func shellQuote(v string, state int) string {
	switch state {
	case shSingle: // inside '…': close, add an escaped quote, reopen
		return strings.ReplaceAll(v, "'", `'\''`)
	case shDouble: // inside "…": only \ " $ ` are special
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(v)
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// ExpandArgv expands an argv list. Every argument is passed as is (no shell sees it),
// except the script of a shell call (`sh -c SCRIPT`, also bash/zsh/dash): there references
// are quoted with ExpandShell.
func ExpandArgv(argv []string, data map[string]any) ([]string, error) {
	out := make([]string, len(argv))
	shell := len(argv) > 0 && isShell(argv[0])
	for i, a := range argv {
		var err error
		if shell && i > 0 && argv[i-1] == "-c" {
			out[i], err = ExpandShell(a, data)
		} else {
			out[i], err = Expand(a, data)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func isShell(bin string) bool {
	if i := strings.LastIndexByte(bin, '/'); i >= 0 {
		bin = bin[i+1:]
	}
	switch bin {
	case "sh", "bash", "zsh", "dash", "ash", "ksh":
		return true
	}
	return false
}
