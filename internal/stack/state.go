package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is what `up` started, persisted in .dmock/state.json so later commands
// (status, down, mock, run) can find it from a fresh process.
type State struct {
	Project   string                   `json:"project"`
	UpdatedAt time.Time                `json:"updated_at"`
	Deps      map[string]*DepState     `json:"deps"`
	Mocks     map[string]*MockState    `json:"mocks"`
	Services  map[string]*ServiceState `json:"services"`
}

type DepState struct {
	Mode      string         `json:"mode"`
	Container string         `json:"container,omitempty"`
	Vars      map[string]any `json:"vars"`
}

type MockState struct {
	Port      int    `json:"port"`
	Token     string `json:"token"`
	Env       string `json:"env"`
	EnvHash   string `json:"env_hash"`
	PID       int    `json:"pid,omitempty"`
	PIDStart  string `json:"pid_start,omitempty"` // process start time, guards against PID reuse
	Container string `json:"container,omitempty"`
	Log       string `json:"log"`
}

type ServiceState struct {
	PID       int    `json:"pid,omitempty"`
	PIDStart  string `json:"pid_start,omitempty"` // process start time, guards against PID reuse
	Container string `json:"container,omitempty"`
	LogPID    int    `json:"log_pid,omitempty"` // `<cli> logs -f` follower writing the container log to Log
	LogStart  string `json:"log_start,omitempty"`
	Log       string `json:"log"`
	Config    string `json:"config,omitempty"`
}

func statePath(dir string) string { return filepath.Join(dir, ".dmock", "state.json") }

// LoadState returns the saved state, or an empty one when nothing was started yet.
func LoadState(dir string) (*State, error) {
	st := &State{Deps: map[string]*DepState{}, Mocks: map[string]*MockState{}, Services: map[string]*ServiceState{}}
	b, err := os.ReadFile(statePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.Deps == nil {
		st.Deps = map[string]*DepState{}
	}
	if st.Mocks == nil {
		st.Mocks = map[string]*MockState{}
	}
	if st.Services == nil {
		st.Services = map[string]*ServiceState{}
	}
	return st, nil
}

func (s *State) Save(dir string) error {
	s.UpdatedAt = time.Now()
	if err := os.MkdirAll(filepath.Dir(statePath(dir)), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir))
}

// inUse reports whether the state still records something started.
func (s *State) inUse() bool { return len(s.Deps)+len(s.Mocks)+len(s.Services) > 0 }

// State loads the saved state and checks it belongs to this stack. Every stack file in a
// folder shares .dmock/, so `up` with stack.load.yaml while stack.yaml is up would overwrite
// the first stack's state, and its `down` would then stop the wrong processes.
func (r *Runner) State() (*State, error) {
	st, err := LoadState(r.Stack.Dir)
	if err != nil {
		return nil, err
	}
	if st.Project != "" && st.Project != r.Stack.Project && st.inUse() {
		return nil, fmt.Errorf("%s holds the running stack of project %q, not %q: take that one down first (dmock -f <its stack file> down), or keep this stack file in its own folder",
			statePath(r.Stack.Dir), st.Project, r.Stack.Project)
	}
	return st, nil
}
