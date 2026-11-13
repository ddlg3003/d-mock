// Package config loads a suite file (dmock.yaml, or stack.yaml).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Stack is the whole local test stack: dependencies, mocks of third-party APIs and our own services.
type Stack struct {
	Project  string              `yaml:"project"`
	Deps     map[string]*Dep     `yaml:"deps"`
	Mocks    map[string]*Mock    `yaml:"mocks"`
	Services map[string]*Service `yaml:"services"`
	Mockoon  MockoonEngine       `yaml:"mockoon"`
	// Hooks are scenario steps run around scenarios (before_all, before_each, after_each,
	// after_all). They are decoded by the scenario package, which owns the step format.
	Hooks map[string]yaml.Node `yaml:"hooks"`

	// Dir is the directory of stack.yaml; every relative path resolves against it.
	Dir string `yaml:"-"`
}

// Dep is infrastructure the services need. Mode picks which block is used for this run:
// remote = an existing server (e.g. a shared dev DB), container = started locally in Docker.
type Dep struct {
	Mode      string     `yaml:"mode"`
	Kind      string     `yaml:"kind"` // mysql | kafka | redis | temporal | other: tells scenario steps how to talk to it
	Container *Container `yaml:"container"`
	Remote    *Remote    `yaml:"remote"`
	Ready     *Ready     `yaml:"ready"`
	// AfterReady runs once a container dep is (re)created and ready, before anything that
	// depends on it starts: migrations, fixture loads. Never for remote deps.
	AfterReady []Command `yaml:"after_ready"`
}

type Container struct {
	Image string            `yaml:"image"`
	Ports []string          `yaml:"ports"`
	Env   map[string]string `yaml:"env"`
	Args  []string          `yaml:"args"`
	Vars  map[string]any    `yaml:"vars"`
}

type Remote struct {
	Vars map[string]any `yaml:"vars"`
}

// Vars returns the variables of the active mode, used by config templates and scenario steps.
func (d *Dep) Vars() map[string]any {
	switch d.Mode {
	case "container":
		if d.Container != nil {
			return d.Container.Vars
		}
	case "remote":
		if d.Remote != nil {
			return d.Remote.Vars
		}
	}
	return nil
}

// Mock is one third-party API served by Mockoon from a d-mock mock spec.
type Mock struct {
	Spec string `yaml:"spec"` // default: mocks/<name>.yaml
	Port int    `yaml:"port"`
}

// Service is one of our own codebases, run as a supervised subprocess.
// Service is one of our own codebases: a local subprocess (cmd) or a container (container).
type Service struct {
	Dir       string            `yaml:"dir"`
	Cmd       []string          `yaml:"cmd"`
	Container *ServiceContainer `yaml:"container"`
	Env       map[string]string `yaml:"env"`
	Config    *ConfigFile       `yaml:"config"`
	Ready     *Ready            `yaml:"ready"`
	DependsOn []string          `yaml:"depends_on"`
	// AfterReady runs each time the service has (re)started and is ready.
	AfterReady []Command `yaml:"after_ready"`
}

// ServiceContainer runs a service in a container with k8s-like resource limits.
type ServiceContainer struct {
	Image  string            `yaml:"image"`
	Build  *Build            `yaml:"build"`
	CPUs   string            `yaml:"cpus"`   // e.g. "0.5"  ≈ k8s resources.limits.cpu
	Memory string            `yaml:"memory"` // e.g. "512m" ≈ k8s resources.limits.memory
	Ports  []string          `yaml:"ports"`
	Env    map[string]string `yaml:"env"`
	Mounts []string          `yaml:"mounts"` // host:container[:ro]; ${...} allowed
	Args   []string          `yaml:"args"`
}

// Build builds the service image before it starts (on every `up` of that service).
type Build struct {
	// Pre runs in the context directory before the image build, e.g. a local `go build`
	// so the Dockerfile only copies the binary.
	Pre        []string          `yaml:"pre"`
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Args       map[string]string `yaml:"args"`
}

// ConfigFile is a template rendered with ${...} references before the service starts.
type ConfigFile struct {
	Template string `yaml:"template"`
}

// Ready is how d-mock knows a dep or service is up. Exactly one probe should be set.
type Ready struct {
	TCP     string   `yaml:"tcp"`
	HTTP    string   `yaml:"http"`
	Log     string   `yaml:"log"`
	Timeout Duration `yaml:"timeout"`
}

type MockoonEngine struct {
	Runner  string `yaml:"runner"`  // cli | docker
	Version string `yaml:"version"` // docker image tag / expected CLI version
}

// Duration parses "30s"-style YAML values.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

// Load reads stack.yaml; overrides is name→mode to switch a dep for this run (--mode redis=remote).
func Load(path string, overrides map[string]string) (*Stack, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &Stack{}
	dec := yaml.NewDecoder(bytesReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s.Dir = filepath.Dir(abs)
	if err := s.applyConventions(); err != nil {
		return nil, err
	}
	for name, mode := range overrides {
		d, ok := s.Deps[name]
		if !ok {
			return nil, fmt.Errorf("--mode %s=%s: no such dep", name, mode)
		}
		d.Mode = mode
	}
	if s.Mockoon.Runner == "" {
		s.Mockoon.Runner = "cli"
	}
	if s.Mockoon.Version == "" {
		s.Mockoon.Version = "9.10.0"
	}
	return s, s.Validate()
}

func (s *Stack) Validate() error {
	if s.Project == "" {
		return fmt.Errorf("stack: project is required")
	}
	names := map[string]string{}
	add := func(kind, n string) error {
		if prev, ok := names[n]; ok {
			return fmt.Errorf("stack: name %q used by both a %s and a %s", n, prev, kind)
		}
		names[n] = kind
		return nil
	}
	for n, d := range s.Deps {
		if err := add("dep", n); err != nil {
			return err
		}
		switch d.Mode {
		case "container":
			if d.Container == nil || d.Container.Image == "" {
				return fmt.Errorf("dep %s: mode container needs container.image", n)
			}
		case "remote":
			if d.Remote == nil {
				return fmt.Errorf("dep %s: mode remote needs a remote block", n)
			}
		default:
			return fmt.Errorf("dep %s: mode must be container or remote, got %q", n, d.Mode)
		}
	}
	for n, m := range s.Mocks {
		if err := add("mock", n); err != nil {
			return err
		}
		if m == nil || m.Port == 0 {
			return fmt.Errorf("mock %s: port is required", n)
		}
	}
	for n, svc := range s.Services {
		if err := add("service", n); err != nil {
			return err
		}
		switch {
		case len(svc.Cmd) == 0 && svc.Container == nil:
			return fmt.Errorf("service %s: set cmd or container", n)
		case len(svc.Cmd) > 0 && svc.Container != nil:
			return fmt.Errorf("service %s: set cmd or container, not both", n)
		case svc.Container != nil && (svc.Container.Image == "") == (svc.Container.Build == nil):
			return fmt.Errorf("service %s: container needs exactly one of image or build", n)
		}
		for _, d := range svc.DependsOn {
			if _, ok := names[d]; !ok && s.Services[d] == nil {
				return fmt.Errorf("service %s: depends_on %q is not defined", n, d)
			}
		}
	}
	for name := range s.Hooks {
		known := false
		for _, h := range HookNames {
			known = known || h == name
		}
		if !known {
			return fmt.Errorf("hooks.%s: unknown hook (have: %s)", name, strings.Join(HookNames, ", "))
		}
	}
	if err := s.checkPorts(); err != nil {
		return err
	}
	if s.Mockoon.Runner != "cli" && s.Mockoon.Runner != "docker" {
		return fmt.Errorf("mockoon.runner must be cli or docker")
	}
	return nil
}

// Path resolves p against the stack directory.
func (s *Stack) Path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(s.Dir, p)
}

// ServiceOrder returns services so that each comes after the services it depends on.
func (s *Stack) ServiceOrder() ([]string, error) {
	var order []string
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(n string) error
	visit = func(n string) error {
		switch state[n] {
		case 1:
			return fmt.Errorf("service dependency cycle at %s", n)
		case 2:
			return nil
		}
		state[n] = 1
		for _, d := range s.Services[n].DependsOn {
			if s.Services[d] != nil {
				if err := visit(d); err != nil {
					return err
				}
			}
		}
		state[n] = 2
		order = append(order, n)
		return nil
	}
	for _, n := range SortedKeys(s.Services) {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func SortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkPorts fails when two things in the suite would listen on the same host port: mocks,
// and the published ports of container deps and container services ("host:container" or
// "ip:host:container"). Otherwise the second one dies at `up` with "port already in use".
func (s *Stack) checkPorts() error {
	owner := map[string]string{}
	claim := func(port, who string) error {
		if prev, ok := owner[port]; ok {
			return fmt.Errorf("host port %s is used by both %s and %s", port, prev, who)
		}
		owner[port] = who
		return nil
	}
	published := func(specs []string, who string) error {
		for _, p := range specs {
			parts := strings.Split(p, ":")
			if len(parts) < 2 {
				continue // container-only port: nothing published on the host
			}
			if err := claim(parts[len(parts)-2], who); err != nil {
				return err
			}
		}
		return nil
	}
	for _, n := range SortedKeys(s.Mocks) {
		if err := claim(fmt.Sprint(s.Mocks[n].Port), "mock "+n); err != nil {
			return err
		}
	}
	for _, n := range SortedKeys(s.Deps) {
		if d := s.Deps[n]; d.Mode == "container" && d.Container != nil {
			if err := published(d.Container.Ports, "dep "+n); err != nil {
				return err
			}
		}
	}
	for _, n := range SortedKeys(s.Services) {
		if c := s.Services[n].Container; c != nil {
			if err := published(c.Ports, "service "+n); err != nil {
				return err
			}
		}
	}
	return nil
}

// Command is a hook command: a list is an argv, a plain string runs through `sh -c`.
type Command []string

func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*c = Command{"sh", "-c", n.Value}
		return nil
	case yaml.SequenceNode:
		var argv []string
		if err := n.Decode(&argv); err != nil {
			return err
		}
		if len(argv) == 0 {
			return fmt.Errorf("line %d: empty command", n.Line)
		}
		*c = argv
		return nil
	}
	return fmt.Errorf("line %d: a command is a string or a list", n.Line)
}

// Warnings are suite choices that work but lose d-mock features (lint prints them; they
// don't fail it).
func (s *Stack) Warnings() []string {
	var out []string
	for _, n := range SortedKeys(s.Services) {
		svc := s.Services[n]
		runsMockoon := strings.Contains(strings.Join(svc.Cmd, " "), "mockoon")
		if svc.Container != nil && strings.Contains(svc.Container.Image, "mockoon") {
			runsMockoon = true
		}
		if runsMockoon {
			out = append(out, fmt.Sprintf("service %s runs Mockoon itself: as a service it has no `mock set/fault/calls`, no per-key state and no cleanup. "+
				"Declare it under mocks: with a spec in mocks/%s.yaml instead (encoded or encrypted payloads: body_file + encode, or a codec)", n, n))
		}
	}
	return out
}

// HookNames are the suite hooks, in the order they run.
var HookNames = []string{"before_all", "before_each", "after_each", "after_all"}
