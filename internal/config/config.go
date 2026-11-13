// Package config loads stack.yaml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Stack is the whole local test stack: dependencies, partner mocks and our own services.
type Stack struct {
	Project  string              `yaml:"project"`
	Deps     map[string]*Dep     `yaml:"deps"`
	Mocks    map[string]*Mock    `yaml:"mocks"`
	Services map[string]*Service `yaml:"services"`
	Mockoon  MockoonEngine       `yaml:"mockoon"`

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

// Mock is one partner API served by Mockoon from a d-mock mock spec.
type Mock struct {
	Spec string `yaml:"spec"`
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
		if m.Spec == "" || m.Port == 0 {
			return fmt.Errorf("mock %s: spec and port are required", n)
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
