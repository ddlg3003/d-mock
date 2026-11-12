package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	res := map[string]any{
		"status": 200,
		"json":   map[string]any{"state": "APPROVED", "items": []any{"a", "b"}},
		"count":  3,
		"events": []any{"WorkflowExecutionStarted", "TimerCanceled"},
	}
	pass := map[string]any{
		"status":      200,
		"json.state":  "APPROVED",
		"json.items":  map[string]any{"len": 2, "contains": "b"},
		"count":       map[string]any{"gte": 2, "lt": 4},
		"events":      map[string]any{"contains": "TimerCanceled", "not_contains": "ActivityTaskScheduled"},
		"json.nope":   map[string]any{"exists": false},
		"json.state ": nil, // trailing-space key does not exist; nil eq "" on a missing path fails below
	}
	delete(pass, "json.state ")
	if f := Check(res, pass); len(f) != 0 {
		t.Fatalf("unexpected failures: %v", f)
	}
	fail := map[string]any{
		"status":     201,
		"json.state": map[string]any{"match": "^REJ"},
		"count":      map[string]any{"gt": 5},
		"missing":    "x",
	}
	if f := Check(res, fail); len(f) != 4 {
		t.Fatalf("want 4 failures, got %v", f)
	}
}

func TestSQLGuards(t *testing.T) {
	for _, q := range []string{"DROP TABLE x", "  alter table x add c int", "TRUNCATE x", "create temporary table t (a int)"} {
		if !ddlRe.MatchString(q) {
			t.Errorf("DDL not caught: %q", q)
		}
	}
	if ddlRe.MatchString("SELECT 'drop table' FROM t") {
		t.Error("select flagged as DDL")
	}
	if !writeRe.MatchString("delete from t where id = ?") || writeRe.MatchString("select 1") {
		t.Error("write detection wrong")
	}
	if !mentionsRun("dm1", "delete from t where id=?", []any{"x-dm1-2"}) || mentionsRun("dm1", "delete from t", nil) {
		t.Error("run id detection wrong")
	}
}

func TestEventName(t *testing.T) {
	cases := map[string]string{
		"EVENT_TYPE_TIMER_CANCELED":              "TimerCanceled",
		"EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED": "WorkflowExecutionSignaled",
		"TimerCanceled":                          "TimerCanceled",
	}
	for in, want := range cases {
		if got := EventName(in); got != want {
			t.Errorf("EventName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadRejectsAmbiguousStep(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.yaml")
	os.WriteFile(p, []byte("name: x\nsteps:\n  - sleep: 1s\n    http: {url: x}\n"), 0o644)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "exactly one action") {
		t.Fatalf("want exactly-one-action error, got %v", err)
	}
	os.WriteFile(p, []byte("name: x\nsteps:\n  - sleeep: 1s\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("unknown field should fail")
	}
}
