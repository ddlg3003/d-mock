package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddlg3003/d-mock/internal/config"
	"github.com/ddlg3003/d-mock/internal/scenario"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	it := filepath.Join(root, "it")
	write(t, filepath.Join(it, "dmock.yaml"), "project: it\n")
	for _, f := range []string{"scenarios/02_b.yaml", "scenarios/01_a.yaml", "scenarios/pay/03_c.yml", "scenarios/.hidden/x.yaml", "scenarios/notes.txt"} {
		write(t, filepath.Join(it, f), "name: x\n")
	}
	write(t, filepath.Join(it, "scenarios", "nested", "dmock.yaml"), "project: n\n") // a nested suite inside scenarios/
	write(t, filepath.Join(it, "scenarios", "nested", "scenarios", "n.yaml"), "name: n\n")
	write(t, filepath.Join(root, "other", "dmock.yaml"), "project: o\n")
	write(t, filepath.Join(root, "other", "scenarios", "o.yaml"), "name: o\n")

	s, err := config.Load(filepath.Join(it, "dmock.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &cli{}
	rel := func(fs []string) string {
		var out []string
		for _, f := range fs {
			r, _ := filepath.Rel(it, f)
			out = append(out, r)
		}
		return strings.Join(out, " ")
	}
	got, err := c.discover(s, nil)
	if want := "scenarios/01_a.yaml scenarios/02_b.yaml scenarios/pay/03_c.yml"; err != nil || rel(got) != want {
		t.Errorf("whole suite = %q, %v; want %q", rel(got), err, want)
	}
	got, _ = c.discover(s, []string{filepath.Join(it, "scenarios", "pay")})
	if rel(got) != "scenarios/pay/03_c.yml" {
		t.Errorf("sub folder = %q", rel(got))
	}
	if _, err := c.discover(s, []string{filepath.Join(root, "other", "scenarios")}); err == nil {
		t.Error("a path from another suite must be refused")
	}
	c.file = "explicit.yaml" // with -f, paths are taken as given
	if _, err := c.discover(s, []string{filepath.Join(root, "other", "scenarios", "o.yaml")}); err != nil {
		t.Errorf("-f: %v", err)
	}
}

func TestWriteJUnit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.xml")
	reps := []*scenario.Report{
		{Scenario: "ok one", File: "s/01.yaml", RunID: "dm1", Passed: true, Duration: "1.5s"},
		{Scenario: "bad one", File: "s/02.yaml", RunID: "dm2", Duration: "2s", Steps: []scenario.StepResult{
			{Name: "check", Type: "http", Failures: []string{`json.status: got "A", want "B"`}},
			{Name: "after", Type: "http", Skipped: true},
		}},
		{Scenario: "never ran", File: "s/03.yaml", Duration: "0s", Skipped: true, Steps: []scenario.StepResult{{Name: "skipped: the before_all hook failed", Skipped: true}}},
	}
	if err := writeJUnit(p, "suite", reps); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	x := string(b)
	for _, want := range []string{`tests="3" failures="1" skipped="1" time="3.500"`, `<skipped message="skipped: the before_all hook failed">`, `<testcase name="ok one" classname="s/01.yaml" time="1.500">`, `message="failed at: check"`, `want &#34;B&#34;`} {
		if !strings.Contains(x, want) {
			t.Errorf("junit missing %s:\n%s", want, x)
		}
	}
}
