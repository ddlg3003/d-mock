// Package mock turns a short d-mock mock spec into a Mockoon environment and drives it at
// runtime through Mockoon's admin API (per-key state, faults, recorded calls).
package mock

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Spec is one mocked partner API.
type Spec struct {
	Routes []Route `yaml:"routes"`
}

// Route is one endpoint. Key is a Mockoon Handlebars expression that picks the state key
// out of the request, e.g. "urlParam 'id'", "bodyRaw 'loan_id'", "queryParam 'ref'".
// Without a key, all requests to the route share one state ("_").
type Route struct {
	ID      string            `yaml:"id"`
	Method  string            `yaml:"method"`
	Path    string            `yaml:"path"`
	Key     string            `yaml:"key"`
	State   map[string]string `yaml:"state"` // default value of every state field the route reads
	Status  int               `yaml:"status"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	// BodyFile is read relative to the spec; Encode, if set, is run once at compile time with
	// the file on stdin and its stdout becomes the body (e.g. PGP-encrypt a fixture).
	BodyFile string        `yaml:"body_file"`
	Encode   []string      `yaml:"encode"`
	Latency  time.Duration `yaml:"latency"`
	// Variants are alternative responses picked when every `when` state field matches.
	Variants []Variant        `yaml:"variants"`
	Faults   map[string]Fault `yaml:"faults"`
}

type Variant struct {
	When     map[string]string `yaml:"when"`
	Status   int               `yaml:"status"`
	Headers  map[string]string `yaml:"headers"`
	Body     string            `yaml:"body"`
	BodyFile string            `yaml:"body_file"`
	Encode   []string          `yaml:"encode"`
	Latency  time.Duration     `yaml:"latency"`
}

// Fault is an injectable failure, switched on per key with `dmock mock fault`.
// A fault with no body reuses the route's normal body (useful for pure latency faults).
type Fault struct {
	Status  int           `yaml:"status"`
	Body    string        `yaml:"body"`
	Latency time.Duration `yaml:"latency"`
}

// LoadSpec reads and validates a mock spec file.
func LoadSpec(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Spec{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	for i, r := range s.Routes {
		if r.ID == "" || r.Method == "" || r.Path == "" {
			return nil, fmt.Errorf("%s: route %d needs id, method and path", path, i)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("%s: duplicate route id %q", path, r.ID)
		}
		seen[r.ID] = true
		if r.Body != "" && r.BodyFile != "" {
			return nil, fmt.Errorf("%s: route %s sets both body and body_file", path, r.ID)
		}
	}
	return s, nil
}

// Find returns the route with the given id.
func (s *Spec) Find(id string) (*Route, bool) {
	for i := range s.Routes {
		if s.Routes[i].ID == id {
			return &s.Routes[i], true
		}
	}
	return nil, false
}

// CheckState fails on a field no route reads: setting it would silently change nothing.
func (s *Spec) CheckState(fields map[string]string) error {
	known := map[string]bool{}
	for _, r := range s.Routes {
		for f := range r.State {
			known[f] = true
		}
	}
	for f := range fields {
		if !known[f] {
			return fmt.Errorf("no route reads state field %q (declared: %s)", f, strings.Join(sortedSet(known), ", "))
		}
	}
	return nil
}

// CheckFault fails on a fault no route declares ("" clears and is always allowed).
func (s *Spec) CheckFault(name string) error {
	if name == "" {
		return nil
	}
	known := map[string]bool{}
	for _, r := range s.Routes {
		for f := range r.Faults {
			known[f] = true
		}
	}
	if !known[name] {
		return fmt.Errorf("no route declares fault %q (declared: %s)", name, strings.Join(sortedSet(known), ", "))
	}
	return nil
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
