package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const stackYAML = `
project: p
deps:
  db:
    kind: mysql
    mode: remote
    remote: { vars: { dsn: "x" } }
    container: { image: mysql:8, vars: { dsn: "y" } }
mocks:
  fake-api: { spec: m.yaml, port: 3001 }
services:
  api: { cmd: [api], depends_on: [db, worker] }
  worker: { cmd: [worker], depends_on: [fake-api] }
`

func write(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "stack.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAndOrder(t *testing.T) {
	s, err := Load(write(t, stackYAML), nil)
	if err != nil {
		t.Fatal(err)
	}
	order, err := s.ServiceOrder()
	if err != nil || strings.Join(order, ",") != "worker,api" {
		t.Fatalf("order %v err %v", order, err)
	}
	if s.Deps["db"].Vars()["dsn"] != "x" {
		t.Fatal("remote vars not active")
	}
}

func TestModeOverride(t *testing.T) {
	s, err := Load(write(t, stackYAML), map[string]string{"db": "container"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Deps["db"].Vars()["dsn"] != "y" {
		t.Fatal("override not applied")
	}
	if _, err := Load(write(t, stackYAML), map[string]string{"nope": "remote"}); err == nil {
		t.Fatal("unknown dep override should fail")
	}
}

func TestValidation(t *testing.T) {
	bad := []string{
		"deps: {}\n", // no project
		"project: p\ndeps: {d: {mode: container}}\n", // no image
		"project: p\nservices: {a: {cmd: [x], depends_on: [ghost]}}\n",
		"project: p\nservices: {a: {cmd: [x], depends_on: [b]}, b: {cmd: [y], depends_on: [a]}}\n",
		"project: p\nunknown_field: 1\n",
	}
	for i, b := range bad {
		s, err := Load(write(t, b), nil)
		if err == nil {
			_, err = s.ServiceOrder()
		}
		if err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
}
