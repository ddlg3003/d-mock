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

// Spec is one mocked third-party API.
type Spec struct {
	// Codec, when set, puts d-mock in front of Mockoon to decode every request body and
	// encode every response body per request (encrypted or signed payloads). A route's own
	// codec replaces it.
	Codec  *Codec  `yaml:"codec"`
	Routes []Route `yaml:"routes"`
}

// Codec is a pair of commands (argv lists, run from the spec's folder, payload on stdin,
// result on stdout). decode turns an incoming request body into plain text before Mockoon
// sees it (so key, state and recorded calls work on it); encode turns each response body,
// after templating, into what is sent back. Either may be left out.
type Codec struct {
	Decode []string `yaml:"decode"`
	Encode []string `yaml:"encode"`
}

func (c *Codec) empty() bool { return c == nil || (len(c.Decode) == 0 && len(c.Encode) == 0) }

// RouteCodec is the codec that applies to a route: its own, else the spec's.
func (s *Spec) RouteCodec(r *Route) *Codec {
	if r.Codec != nil {
		return r.Codec
	}
	return s.Codec
}

// HasCodec reports whether any route needs the codec proxy.
func (s *Spec) HasCodec() bool {
	for i := range s.Routes {
		if !s.RouteCodec(&s.Routes[i]).empty() {
			return true
		}
	}
	return false
}

// Route is one endpoint. Key is a Mockoon Handlebars expression that picks the state key
// out of the request, e.g. "urlParam 'id'", "bodyRaw 'order_id'", "queryParam 'ref'".
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
	// Codec replaces the spec's codec for this route (`codec: {}` = none).
	Codec *Codec `yaml:"codec"`
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
		if c := s.RouteCodec(&s.Routes[i]); c != nil && len(c.Encode) > 0 {
			static := len(r.Encode) > 0
			for _, v := range r.Variants {
				static = static || len(v.Encode) > 0
			}
			if static {
				return nil, fmt.Errorf("%s: route %s has both a codec encode (per request) and a static encode (once at start): keep one", path, r.ID)
			}
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
