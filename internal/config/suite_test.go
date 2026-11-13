package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFindSuite(t *testing.T) {
	root := t.TempDir()
	suite := filepath.Join(root, "it", "dmock.yaml")
	writeFile(t, suite, "project: x\n")
	writeFile(t, filepath.Join(root, "it", "scenarios", "a", "01.yaml"), "name: a\n")
	nested := filepath.Join(root, "it", "load", "dmock.yaml")
	writeFile(t, nested, "project: y\n")
	cases := map[string]string{
		filepath.Join(root, "it"): suite,
		suite:                     suite,
		filepath.Join(root, "it", "scenarios", "a", "01.yaml"): suite, // walks up
		filepath.Join(root, "it", "scenarios", "a"):            suite,
		filepath.Join(root, "it", "load"):                      nested, // nearest wins
	}
	for in, want := range cases {
		got, err := FindSuite(in)
		if err != nil || got != want {
			t.Errorf("FindSuite(%s) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := FindSuite(t.TempDir()); err == nil || !strings.Contains(err.Error(), "dmock init") {
		t.Errorf("no suite: want a hint to dmock init, got %v", err)
	}
	// the old name still makes a suite
	old := filepath.Join(root, "legacy", "stack.yaml")
	writeFile(t, old, "project: z\n")
	if got, _ := FindSuite(filepath.Join(root, "legacy")); got != old {
		t.Errorf("stack.yaml not found: %s", got)
	}
}

func TestConventions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "dmock.yaml"), `project: x
mocks:
  fake-api: { port: 14000 }
  other: { spec: elsewhere/o.yaml, port: 14001 }
services:
  app: { cmd: [true] }
  bare: { cmd: [true] }
  explicit: { cmd: [true], config: { template: my.yaml } }
`)
	writeFile(t, filepath.Join(dir, "configs", "app.tmpl.yaml"), "a: 1\n")
	s, err := Load(filepath.Join(dir, "dmock.yaml"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Mocks["fake-api"].Spec != filepath.Join("mocks", "fake-api.yaml") || s.Mocks["other"].Spec != "elsewhere/o.yaml" {
		t.Errorf("mock specs: %+v %+v", s.Mocks["fake-api"], s.Mocks["other"])
	}
	if s.Services["app"].Config == nil || s.Services["app"].Config.Template != filepath.Join("configs", "app.tmpl.yaml") {
		t.Errorf("app config: %+v", s.Services["app"].Config)
	}
	if s.Services["bare"].Config != nil {
		t.Error("a service without a configs/ template must get no config")
	}
	if s.Services["explicit"].Config.Template != "my.yaml" {
		t.Error("an explicit template must win")
	}
	writeFile(t, filepath.Join(dir, "configs", "app.tmpl.json"), "{}")
	if _, err := Load(filepath.Join(dir, "dmock.yaml"), nil); err == nil {
		t.Error("two templates for one service must be an error")
	}
}

func TestDuplicatePorts(t *testing.T) {
	cases := map[string]string{
		"two mocks": `project: x
mocks:
  a: { spec: a.yaml, port: 14000 }
  b: { spec: b.yaml, port: 14000 }
`,
		"mock vs container dep": `project: x
deps:
  db: { kind: mysql, mode: container, container: { image: m, ports: ["14000:3306"] } }
mocks:
  a: { spec: a.yaml, port: 14000 }
`,
		"dep vs container service (ip:host:container)": `project: x
deps:
  db: { kind: mysql, mode: container, container: { image: m, ports: ["13306:3306"] } }
services:
  app: { container: { image: i, ports: ["127.0.0.1:13306:8080"] } }
`,
	}
	for name, y := range cases {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "dmock.yaml"), y)
		if _, err := Load(filepath.Join(dir, "dmock.yaml"), nil); err == nil || !strings.Contains(err.Error(), "host port") {
			t.Errorf("%s: want a host port error, got %v", name, err)
		}
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "dmock.yaml"), `project: x
deps:
  db: { kind: mysql, mode: remote, container: { image: m, ports: ["14000:3306"] }, remote: { vars: {} } }
mocks:
  a: { spec: a.yaml, port: 14000 }
`)
	if _, err := Load(filepath.Join(dir, "dmock.yaml"), nil); err != nil {
		t.Errorf("a remote dep publishes nothing, so no clash: %v", err)
	}
}

func TestWarnsOnMockoonAsService(t *testing.T) {
	s := &Stack{Services: map[string]*Service{
		"fake-api": {Cmd: []string{"mockoon-cli", "start", "-d", "env.json"}},
		"app":      {Cmd: []string{"go", "run", "."}},
	}}
	w := s.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "service fake-api runs Mockoon") {
		t.Errorf("warnings = %v", w)
	}
}
