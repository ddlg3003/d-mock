package scenario

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeYAML(t *testing.T, path, data string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMatrixAndRepeatExpand(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.yaml")
	writeYAML(t, p, `name: callback
matrix:
  region: [EU, US]
  case:
    - { callback: APPROVED, want: DONE }
    - { callback: REJECTED, want: CANCELLED }
repeat: 2
steps:
  - sleep: 1ms
`)
	sc, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := sc.Expand()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 8 {
		t.Fatalf("2 regions × 2 cases × repeat 2 = 8 runs, got %d", len(runs))
	}
	if runs[0].Name != "callback [region=EU case={callback:APPROVED want:DONE}] #1" {
		t.Errorf("first name = %q", runs[0].Name)
	}
	last := runs[7]
	if last.MatrixValues["region"] != "US" || last.Iteration != 2 || last.MatrixValues["case"].(map[string]any)["want"] != "CANCELLED" {
		t.Errorf("last run = %+v %d", last.MatrixValues, last.Iteration)
	}
	// no matrix, no repeat: one run with the plain name
	writeYAML(t, p, "name: plain\nsteps:\n  - sleep: 1ms\n")
	sc, _ = Load(p)
	runs, _ = sc.Expand()
	if len(runs) != 1 || runs[0].Name != "plain" {
		t.Errorf("plain = %d %q", len(runs), runs[0].Name)
	}
}

func TestInclude(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "steps", "seed.yaml"), `
- name: seed one
  sleep: 1ms
- include: more.yaml
`)
	writeYAML(t, filepath.Join(dir, "steps", "more.yaml"), "- name: seed two\n  sleep: 1ms\n")
	writeYAML(t, filepath.Join(dir, "scenarios", "a.yaml"), `name: a
steps:
  - include: ../steps/seed.yaml
  - name: act
    sleep: 1ms
`)
	sc, err := Load(filepath.Join(dir, "scenarios", "a.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range sc.Steps {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "seed one,seed two,act" {
		t.Errorf("steps = %v", names)
	}
	writeYAML(t, filepath.Join(dir, "steps", "loop.yaml"), "- include: loop.yaml\n")
	writeYAML(t, filepath.Join(dir, "scenarios", "b.yaml"), "name: b\nsteps:\n  - include: ../steps/loop.yaml\n")
	if _, err := Load(filepath.Join(dir, "scenarios", "b.yaml")); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("include cycle: %v", err)
	}
}

func TestParallelValidation(t *testing.T) {
	bad := map[string]string{
		"one branch":       "parallel: { steps: [ { sleep: 1ms } ] }",
		"times 1":          "parallel: { times: 1, step: { sleep: 1ms } }",
		"both forms":       "parallel: { times: 2, step: { sleep: 1ms }, steps: [ { sleep: 1ms }, { sleep: 1ms } ] }",
		"nested":           "parallel: { steps: [ { parallel: { times: 2, step: { sleep: 1ms } } }, { sleep: 1ms } ] }",
		"same save":        "parallel: { steps: [ { http: { url: x }, save: { a: status } }, { http: { url: y }, save: { a: status } } ] }",
		"save on repeated": "parallel: { times: 2, step: { http: { url: x }, save: { a: status } } }",
	}
	for name, step := range bad {
		p := filepath.Join(t.TempDir(), "s.yaml")
		writeYAML(t, p, "name: x\nsteps:\n  - "+step+"\n")
		if _, err := Load(p); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
	p := filepath.Join(t.TempDir(), "s.yaml")
	writeYAML(t, p, "name: x\nsteps:\n  - sleep: 1ms\n    after: 5ms\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "after") {
		t.Errorf("after outside parallel: %v", err)
	}
}

// TestParallelRuns: branches start together (barrier), `after` delays one, results/count
// report the outcome without depending on who won, and a branch's save is visible after.
func TestParallelRuns(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
		if r.URL.Path == "/slow" {
			w.Write([]byte(`{"who":"slow"}`))
			return
		}
		// first request wins, the others conflict: like a double submit
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(202)
			w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(409)
	}))
	defer srv.Close()
	ru := &run{r: &Runner{}, id: "dm1", data: map[string]any{"vars": map[string]any{"api": srv.URL}}, saved: map[string]any{}, ledger: &ledger{}, logOffs: map[string]int64{}}
	ru.data["saved"] = ru.saved

	step := Step{Parallel: &Parallel{Times: 3, Step: &Step{HTTP: map[string]any{"method": "POST", "url": "${vars.api}/checkout"}}}}
	res := ru.runStep(context.Background(), step, 0)
	if !res.Passed {
		t.Fatalf("parallel failed: %s %v", res.Error, res.Failures)
	}
	got := res.Result
	if c := got["count"].(map[string]any)["status"].(map[string]any); c["202"] != 1 || c["409"] != 2 {
		t.Errorf("count.status = %v, want one 202 and two 409", c)
	}
	if got["passed"] != 3 || len(got["finished"].([]any)) != 3 {
		t.Errorf("passed/finished = %v %v", got["passed"], got["finished"])
	}
	spread := arrivals[len(arrivals)-1].Sub(arrivals[0])
	if spread > 50*time.Millisecond {
		t.Errorf("branches should arrive together, spread %s", spread)
	}
	if fails := Check(got, map[string]any{"count.status.202": 1, "count.status.409": 2, "results.0.passed": true}); len(fails) > 0 {
		t.Errorf("expect on the result: %v", fails)
	}

	arrivals = nil
	step = Step{Parallel: &Parallel{Steps: []Step{
		{Name: "late", After: 150 * time.Millisecond, HTTP: map[string]any{"url": "${vars.api}/slow"}, Save: map[string]string{"who": "json.who"}},
		{Name: "early", HTTP: map[string]any{"url": "${vars.api}/checkout"}},
	}}}
	res = ru.runStep(context.Background(), step, 1)
	if !res.Passed {
		t.Fatalf("parallel 2 failed: %s", res.Error)
	}
	if f := res.Result["finished"].([]any); f[0] != 1 || f[1] != 0 {
		t.Errorf("finished order = %v, want [1 0] (late branch waited 150ms)", f)
	}
	if ru.saved["who"] != "slow" {
		t.Errorf("a branch's save should be visible after the step, saved = %v", ru.saved)
	}

	// a branch that fails its own expect fails the step, and the result says which
	step = Step{Parallel: &Parallel{Times: 2, Step: &Step{Name: "must win", HTTP: map[string]any{"method": "POST", "url": "${vars.api}/checkout"}, Expect: map[string]any{"status": 202}}}}
	res = ru.runStep(context.Background(), step, 2)
	if res.Passed || !strings.Contains(res.Error, "branches failed") || !strings.Contains(res.Error, "must win") {
		t.Errorf("want a branch failure naming the branch, got %q", res.Error)
	}
}
