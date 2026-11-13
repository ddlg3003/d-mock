// Package scenario runs scenario.yaml files: a list of typed steps (http, exec, sql, kafka,
// temporal, mock, log, service, stack, k6, sleep, parallel), each with optional
// expect/save/retry, then cleanup. A scenario can expand into several runs (matrix, repeat),
// and steps can come from shared files (include).
package scenario

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Scenario struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Vars        map[string]any `yaml:"vars"`
	// Matrix runs the scenario once per combination of its values (like a table-driven
	// test): keys in file order, each a list; ${matrix.<key>} is the current value.
	Matrix yaml.Node `yaml:"matrix"`
	// Repeat runs the scenario (each matrix case) N times, each with a fresh run id;
	// ${iteration} is 1..N. Useful to shake out races.
	Repeat  int    `yaml:"repeat"`
	Steps   []Step `yaml:"steps"`
	Cleanup []Step `yaml:"cleanup"`
	Path    string `yaml:"-"`

	// set by Expand for one run
	MatrixValues map[string]any `yaml:"-"`
	Iteration    int            `yaml:"-"`
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
	Stack    map[string]any `yaml:"stack"`
	K6       map[string]any `yaml:"k6"`
	Sleep    string         `yaml:"sleep"`
	Parallel *Parallel      `yaml:"parallel"`
	// Include is replaced, when the file loads, by the steps of another file (a YAML list of
	// steps, path relative to the including file).
	Include string `yaml:"include"`

	// After delays a parallel branch after the common start (to force an interleaving).
	After time.Duration `yaml:"after"`

	// Expect maps a path in the step result to a matcher (see match.go).
	Expect map[string]any `yaml:"expect"`
	// Save copies result paths into ${saved.<name>}.
	Save map[string]string `yaml:"save"`
	// Retry re-runs the step until expect passes or the timeout ends.
	Retry *Retry `yaml:"retry"`
}

// Parallel runs its branches at the same moment (a start barrier) and waits for all of them.
// Either list different steps, or repeat one step `times` times.
type Parallel struct {
	Steps []Step `yaml:"steps"`
	Times int    `yaml:"times"`
	Step  *Step  `yaml:"step"`
}

// Branches returns the steps to run side by side.
func (p *Parallel) Branches() []Step {
	if p.Step == nil {
		return p.Steps
	}
	out := make([]Step, p.Times)
	for i := range out {
		out[i] = *p.Step
		if out[i].Name != "" {
			out[i].Name = fmt.Sprintf("%s #%d", p.Step.Name, i+1)
		}
	}
	return out
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
	set("stack", s.Stack)
	set("k6", s.K6)
	if s.Parallel != nil {
		set("parallel", map[string]any{})
	}
	if s.Include != "" {
		found = append(found, "include")
	}
	if s.Sleep != "" {
		found = append(found, "sleep")
		args = map[string]any{"duration": s.Sleep}
	}
	if len(found) != 1 {
		return "", nil, fmt.Errorf("step %q must have exactly one action (http, exec, sql, kafka, temporal, mock, log, service, stack, k6, sleep, parallel, include), has %v", s.Name, found)
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
	dir := filepath.Dir(path)
	if sc.Steps, err = expandIncludes(sc.Steps, dir, 0); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if sc.Cleanup, err = expandIncludes(sc.Cleanup, dir, 0); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := ValidateSteps(sc.Steps, false); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := ValidateSteps(sc.Cleanup, false); err != nil {
		return nil, fmt.Errorf("%s cleanup: %w", path, err)
	}
	if sc.Repeat < 0 {
		return nil, fmt.Errorf("%s: repeat must be 1 or more", path)
	}
	if _, err := sc.matrixAxes(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sc, nil
}

// ValidateSteps checks every step (and parallel branch) has exactly one action, that `after`
// is only used on parallel branches, and that branches don't save to the same name.
func ValidateSteps(steps []Step, inParallel bool) error {
	for i, st := range steps {
		typ, _, err := st.Type()
		if err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		if st.After != 0 && !inParallel {
			return fmt.Errorf("step %d (%s): `after` only applies to a parallel branch", i+1, stepLabel(st))
		}
		if typ != "parallel" {
			continue
		}
		p := st.Parallel
		if (len(p.Steps) > 0) == (p.Step != nil) {
			return fmt.Errorf("step %d (%s): parallel needs either steps or step + times", i+1, stepLabel(st))
		}
		if p.Step != nil && p.Times < 2 {
			return fmt.Errorf("step %d (%s): parallel times must be 2 or more", i+1, stepLabel(st))
		}
		branches := p.Branches()
		if len(branches) < 2 {
			return fmt.Errorf("step %d (%s): parallel needs 2 or more branches", i+1, stepLabel(st))
		}
		saved := map[string]bool{}
		for j, b := range branches {
			if b.Parallel != nil {
				return fmt.Errorf("step %d (%s) branch %d: parallel can't be nested", i+1, stepLabel(st), j+1)
			}
			for name := range b.Save {
				if saved[name] && p.Step == nil {
					return fmt.Errorf("step %d (%s): two branches save %q", i+1, stepLabel(st), name)
				}
				saved[name] = true
			}
			if p.Step != nil && len(b.Save) > 0 {
				return fmt.Errorf("step %d (%s): a repeated branch can't save (every copy would write the same name)", i+1, stepLabel(st))
			}
		}
		if err := ValidateSteps(branches, true); err != nil {
			return fmt.Errorf("step %d (%s): %w", i+1, stepLabel(st), err)
		}
	}
	return nil
}

func stepLabel(s Step) string {
	if s.Name != "" {
		return s.Name
	}
	typ, _, _ := s.Type()
	return typ
}

// expandIncludes replaces every `include: file` with that file's steps, recursively.
func expandIncludes(steps []Step, dir string, depth int) ([]Step, error) {
	if depth > 10 {
		return nil, fmt.Errorf("include nested more than 10 deep (a cycle?)")
	}
	var out []Step
	for _, st := range steps {
		if st.Parallel != nil {
			p := *st.Parallel
			var err error
			if p.Steps, err = expandIncludes(p.Steps, dir, depth); err != nil {
				return nil, err
			}
			st.Parallel = &p
		}
		if st.Include == "" {
			out = append(out, st)
			continue
		}
		if typ, _, err := st.Type(); err != nil || typ != "include" || st.Expect != nil || st.Save != nil || st.Retry != nil {
			return nil, fmt.Errorf("include %s: an include step has only `include` (and an optional name)", st.Include)
		}
		path := st.Include
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("include: %w", err)
		}
		var inc []Step
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&inc); err != nil {
			return nil, fmt.Errorf("include %s: %w (an included file is a YAML list of steps)", st.Include, err)
		}
		if inc, err = expandIncludes(inc, filepath.Dir(path), depth+1); err != nil {
			return nil, fmt.Errorf("include %s: %w", st.Include, err)
		}
		out = append(out, inc...)
	}
	return out, nil
}

type axis struct {
	key    string
	values []any
}

func (sc *Scenario) matrixAxes() ([]axis, error) {
	n := sc.Matrix
	if n.Kind == 0 {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("matrix: a map of key → list of values")
	}
	var axes []axis
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i].Value, n.Content[i+1]
		if v.Kind != yaml.SequenceNode || len(v.Content) == 0 {
			return nil, fmt.Errorf("matrix.%s: a non-empty list of values", k)
		}
		var vals []any
		if err := v.Decode(&vals); err != nil {
			return nil, fmt.Errorf("matrix.%s: %w", k, err)
		}
		axes = append(axes, axis{k, vals})
	}
	return axes, nil
}

// Expand turns one scenario into its runs: one per matrix combination (first key varies
// slowest), each repeated `repeat` times. Names get the case and the iteration, e.g.
// "callback moves the loan [partner=CAKE case={callback:APPROVED want:DISBURSED}] #2".
func (sc *Scenario) Expand() ([]*Scenario, error) {
	axes, err := sc.matrixAxes()
	if err != nil {
		return nil, err
	}
	cases := []map[string]any{nil}
	for _, a := range axes {
		var next []map[string]any
		for _, c := range cases {
			for _, v := range a.values {
				m := map[string]any{}
				for k, x := range c {
					m[k] = x
				}
				m[a.key] = v
				next = append(next, m)
			}
		}
		cases = next
	}
	repeat := sc.Repeat
	if repeat == 0 {
		repeat = 1
	}
	var out []*Scenario
	for _, c := range cases {
		label := ""
		if c != nil {
			var parts []string
			for _, a := range axes {
				parts = append(parts, a.key+"="+caseValue(c[a.key]))
			}
			label = " [" + strings.Join(parts, " ") + "]"
		}
		for it := 1; it <= repeat; it++ {
			x := *sc
			x.MatrixValues, x.Iteration = c, it
			x.Name = sc.Name + label
			if repeat > 1 {
				x.Name += fmt.Sprintf(" #%d", it)
			}
			out = append(out, &x)
		}
	}
	return out, nil
}

func caseValue(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, k+":"+caseValue(t[k]))
		}
		return "{" + strings.Join(parts, " ") + "}"
	case []any:
		var parts []string
		for _, e := range t {
			parts = append(parts, caseValue(e))
		}
		return "[" + strings.Join(parts, " ") + "]"
	}
	return fmt.Sprint(v)
}
