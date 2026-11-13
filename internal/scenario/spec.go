// Package scenario runs scenario.yaml files: a list of typed steps (http, exec, sql, kafka,
// temporal, mock, log, sleep), each with optional expect/save/retry, then cleanup.
package scenario

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Scenario struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Vars        map[string]any `yaml:"vars"`
	Steps       []Step         `yaml:"steps"`
	Cleanup     []Step         `yaml:"cleanup"`
	Path        string         `yaml:"-"`
}

// Step is one action. Exactly one of the action fields is set.
type Step struct {
	Name string `yaml:"name"`

	HTTP     map[string]any `yaml:"http"`
	Exec     map[string]any `yaml:"exec"`
	SQL      map[string]any `yaml:"sql"`
	Kafka    map[string]any `yaml:"kafka"`
	Temporal map[string]any `yaml:"temporal"`
	Mock     map[string]any `yaml:"mock"`
	Log      map[string]any `yaml:"log"`
	Service  map[string]any `yaml:"service"`
	K6       map[string]any `yaml:"k6"`
	Sleep    string         `yaml:"sleep"`

	// Expect maps a path in the step result to a matcher (see match.go).
	Expect map[string]any `yaml:"expect"`
	// Save copies result paths into ${saved.<name>}.
	Save map[string]string `yaml:"save"`
	// Retry re-runs the step until expect passes or the timeout ends.
	Retry *Retry `yaml:"retry"`
}

type Retry struct {
	Timeout time.Duration `yaml:"timeout"`
	Every   time.Duration `yaml:"every"`
}

// Type returns the action name and its arguments.
func (s Step) Type() (string, map[string]any, error) {
	var found []string
	var args map[string]any
	set := func(name string, m map[string]any) {
		if m != nil {
			found = append(found, name)
			args = m
		}
	}
	set("http", s.HTTP)
	set("exec", s.Exec)
	set("sql", s.SQL)
	set("kafka", s.Kafka)
	set("temporal", s.Temporal)
	set("mock", s.Mock)
	set("log", s.Log)
	set("service", s.Service)
	set("k6", s.K6)
	if s.Sleep != "" {
		found = append(found, "sleep")
		args = map[string]any{"duration": s.Sleep}
	}
	if len(found) != 1 {
		return "", nil, fmt.Errorf("step %q must have exactly one action (http, exec, sql, kafka, temporal, mock, log, service, k6, sleep), has %v", s.Name, found)
	}
	return found[0], args, nil
}

func Load(path string) (*Scenario, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sc := &Scenario{Path: path}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(sc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if sc.Name == "" {
		sc.Name = path
	}
	for i, st := range append(append([]Step{}, sc.Steps...), sc.Cleanup...) {
		if _, _, err := st.Type(); err != nil {
			return nil, fmt.Errorf("%s step %d: %w", path, i+1, err)
		}
	}
	return sc, nil
}
