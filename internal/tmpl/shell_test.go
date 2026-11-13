package tmpl

import (
	"os/exec"
	"strings"
	"testing"
)

// TestExpandShell runs each expanded line in a real sh: a value must arrive as one literal
// word, never as shell syntax, whether the reference is bare or already quoted.
func TestExpandShell(t *testing.T) {
	evil := `ord-1; echo INJECTED $(echo SUB) "q" 'x' \ $HOME ` + "`id`"
	data := map[string]any{"v": map[string]any{"evil": evil, "dsn": "u:p@tcp(h)/db?a=1&b=2", "n": 3}}
	for _, line := range []string{
		`printf '%s' ${v.evil}`,
		`printf '%s' "${v.evil}"`,
		`printf '%s' 'pre-${v.evil}-post'`,
		`printf '%s' "pre-${v.evil}-post"`,
	} {
		got, err := ExpandShell(line, data)
		if err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("sh", "-c", got).CombinedOutput()
		if err != nil {
			t.Fatalf("%s → %s: %v %s", line, got, err, out)
		}
		want := evil
		if strings.Contains(line, "pre-") {
			want = "pre-" + evil + "-post"
		}
		if string(out) != want {
			t.Errorf("%s\n  ran  %s\n  got  %q\n  want %q", line, got, out, want)
		}
	}
	got, _ := ExpandShell(`printf '%s|%s' ${v.dsn} ${v.n}`, data)
	if out, _ := exec.Command("sh", "-c", got).Output(); string(out) != "u:p@tcp(h)/db?a=1&b=2|3" {
		t.Errorf("dsn: %q", out)
	}
	if got, _ := ExpandShell(`echo $${HOME} \${x}`, data); got != `echo ${HOME} \${x}` {
		t.Errorf("escapes: %q", got)
	}
	if _, err := ExpandShell(`echo ${v.nope}`, data); err == nil {
		t.Error("unresolved reference should fail")
	}
}
