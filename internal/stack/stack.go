// Package stack starts, inspects and stops a stack.yaml: deps (remote or Docker containers),
// Mockoon mocks and our own services as supervised subprocesses.
package stack

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ddlg3003/d-mock/internal/config"
	"github.com/ddlg3003/d-mock/internal/mock"
	"github.com/ddlg3003/d-mock/internal/tmpl"
)

// Item is one line of up/down/status output.
type Item struct {
	Kind    string `json:"kind"` // dep | mock | service
	Name    string `json:"name"`
	State   string `json:"state"` // running | stopped | remote | started | already-running | restarted | error
	Mode    string `json:"mode,omitempty"`
	PID     int    `json:"pid,omitempty"`
	Port    int    `json:"port,omitempty"`
	URL     string `json:"url,omitempty"`
	Log     string `json:"log,omitempty"`
	Message string `json:"message,omitempty"`
	// container services only
	Container string `json:"container,omitempty"`
	OOMKilled bool   `json:"oom_killed,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

type Runner struct {
	Stack *config.Stack
	Dir   string // .dmock directory
}

func New(s *config.Stack) *Runner {
	return &Runner{Stack: s, Dir: filepath.Join(s.Dir, ".dmock")}
}

func (r *Runner) logPath(name string) string { return filepath.Join(r.Dir, "logs", name+".log") }

// Data is what ${...} references in service configs and env resolve against.
func (r *Runner) Data(st *State) (map[string]any, error) {
	env := map[string]any{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	// host: how a container service reaches this machine (mocks, container deps' published ports)
	data := map[string]any{"env": env, "host": HostAlias(), "stack": map[string]any{"dir": r.Stack.Dir, "project": r.Stack.Project}}
	deps := map[string]any{}
	for name, d := range r.Stack.Deps {
		v, err := tmpl.ExpandAny(toAny(d.Vars()), map[string]any{"env": env})
		if err != nil {
			return nil, fmt.Errorf("dep %s vars: %w", name, err)
		}
		m, _ := v.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		m["mode"] = d.Mode
		deps[name] = m
	}
	data["deps"] = deps
	mocks := map[string]any{}
	for name, m := range r.Stack.Mocks {
		mocks[name] = map[string]any{"port": m.Port, "url": fmt.Sprintf("http://localhost:%d", m.Port)}
	}
	data["mocks"] = mocks
	services := map[string]any{}
	for name, svc := range r.Stack.Services {
		e := map[string]any{"log": r.logPath(name)}
		if svc.Config != nil {
			e["config"] = filepath.Join(r.Dir, "rendered", name+filepath.Ext(svc.Config.Template))
		}
		if st != nil && st.Services[name] != nil {
			e["pid"] = st.Services[name].PID
		}
		services[name] = e
	}
	data["services"] = services
	return data, nil
}

func toAny(m map[string]any) any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// UpOptions narrows `up` to some components and forces restarts.
type UpOptions struct {
	Only    []string // names to start (with what they depend on); empty = all
	Restart []string // names to restart even if running
	DryRun  bool
}

// Up starts everything not running yet. It is idempotent: running parts are left alone
// unless named in Restart, and a mock restarts by itself when its compiled spec changed.
func (r *Runner) Up(ctx context.Context, opt UpOptions) ([]Item, error) {
	if err := os.MkdirAll(filepath.Join(r.Dir, "logs"), 0o755); err != nil {
		return nil, err
	}
	st, err := r.State()
	if err != nil {
		return nil, err
	}
	st.Project = r.Stack.Project
	want := r.selection(opt.Only)
	restart := map[string]bool{}
	for _, n := range opt.Restart {
		restart[n] = true
		want[n] = true
	}
	var items []Item
	save := func() error { return st.Save(r.Stack.Dir) }

	for _, name := range config.SortedKeys(r.Stack.Deps) {
		if !want[name] {
			continue
		}
		it, err := r.upDep(ctx, st, name, restart[name], opt.DryRun)
		items = append(items, it)
		if err != nil {
			save()
			return items, err
		}
	}
	for _, name := range config.SortedKeys(r.Stack.Mocks) {
		if !want[name] {
			continue
		}
		it, err := r.upMock(ctx, st, name, restart[name], opt.DryRun)
		items = append(items, it)
		if err != nil {
			save()
			return items, err
		}
	}
	order, err := r.Stack.ServiceOrder()
	if err != nil {
		return items, err
	}
	for _, name := range order {
		if !want[name] {
			continue
		}
		it, err := r.upService(ctx, st, name, restart[name], opt.DryRun)
		items = append(items, it)
		if err != nil {
			save()
			return items, err
		}
	}
	if opt.DryRun {
		return items, nil
	}
	return items, save()
}

// selection returns the named components plus everything they depend on.
func (r *Runner) selection(only []string) map[string]bool {
	want := map[string]bool{}
	if len(only) == 0 {
		for n := range r.Stack.Deps {
			want[n] = true
		}
		for n := range r.Stack.Mocks {
			want[n] = true
		}
		for n := range r.Stack.Services {
			want[n] = true
		}
		return want
	}
	var add func(n string)
	add = func(n string) {
		if want[n] {
			return
		}
		want[n] = true
		if svc := r.Stack.Services[n]; svc != nil {
			for _, d := range svc.DependsOn {
				add(d)
			}
		}
	}
	for _, n := range only {
		add(n)
	}
	return want
}

func (r *Runner) upDep(ctx context.Context, st *State, name string, restart, dry bool) (Item, error) {
	d := r.Stack.Deps[name]
	it := Item{Kind: "dep", Name: name, Mode: d.Mode}
	data, err := r.Data(st)
	if err != nil {
		return it, err
	}
	vars := data["deps"].(map[string]any)[name].(map[string]any)
	if d.Mode == "remote" {
		it.State = "remote"
		if !dry {
			st.Deps[name] = &DepState{Mode: d.Mode, Vars: vars}
		}
		return it, nil
	}
	cname := containerName(r.Stack.Project, name)
	if containerRunning(cname) && !restart {
		it.State = "already-running"
		st.Deps[name] = &DepState{Mode: d.Mode, Container: cname, Vars: vars}
		return it, nil
	}
	if dry {
		it.State = "would-start"
		it.Message = "docker run " + d.Container.Image
		return it, nil
	}
	c := d.Container
	if _, err := runContainer(r.Stack.Project, name, c.Image, c.Ports, c.Env, nil, c.Args); err != nil {
		it.State = "error"
		it.Message = err.Error()
		return it, err
	}
	st.Deps[name] = &DepState{Mode: d.Mode, Container: cname, Vars: vars}
	if err := waitReady(ctx, d.Ready, "", func() bool { return !containerRunning(cname) }); err != nil {
		logs, _ := docker("logs", "--tail", "15", cname)
		it.State = "error"
		it.Message = err.Error() + "\n" + logs
		return it, fmt.Errorf("dep %s: %s", name, it.Message)
	}
	it.State = "started"
	return it, nil
}

func (r *Runner) upMock(ctx context.Context, st *State, name string, restart, dry bool) (Item, error) {
	m := r.Stack.Mocks[name]
	it := Item{Kind: "mock", Name: name, Port: m.Port, URL: fmt.Sprintf("http://localhost:%d", m.Port), Log: r.logPath(name)}
	specPath := r.Stack.Path(m.Spec)
	spec, err := mock.LoadSpec(specPath)
	if err != nil {
		return it, err
	}
	env, err := mock.Compile(name, m.Port, spec, filepath.Dir(specPath))
	if err != nil {
		return it, err
	}
	sum := sha256.Sum256(env)
	hash := hex.EncodeToString(sum[:8])
	envPath := filepath.Join(r.Dir, "mocks", name+".json")
	prev := st.Mocks[name]
	running := prev != nil && (ours(prev.PID, prev.PIDStart) || (prev.Container != "" && containerRunning(prev.Container)))
	if running && !restart && prev.EnvHash == hash {
		it.State = "already-running"
		it.PID = prev.PID
		return it, nil
	}
	if dry {
		it.State = "would-start"
		return it, nil
	}
	if running {
		r.stopMock(prev)
		it.State = "restarted"
	} else {
		it.State = "started"
	}
	if err := os.MkdirAll(filepath.Dir(envPath), 0o755); err != nil {
		return it, err
	}
	if err := os.WriteFile(envPath, env, 0o644); err != nil {
		return it, err
	}
	ms := &MockState{Port: m.Port, Token: randomToken(), Env: envPath, EnvHash: hash, Log: r.logPath(name)}
	switch r.Stack.Mockoon.Runner {
	case "cli":
		bin, err := exec.LookPath("mockoon-cli")
		if err != nil {
			return it, fmt.Errorf("mock %s: mockoon-cli not found (npm i -g @mockoon/cli@%s, or set mockoon.runner: docker)", name, r.Stack.Mockoon.Version)
		}
		pid, err := startProcess(r.Dir, []string{bin, "start", "-d", envPath, "--admin-api-token", ms.Token, "-X", "--max-transaction-logs", "1000"}, nil, ms.Log)
		if err != nil {
			return it, err
		}
		ms.PID, ms.PIDStart = pid, procStart(pid)
	case "docker":
		cname, err := runContainer(r.Stack.Project, name, "mockoon/cli:"+r.Stack.Mockoon.Version,
			[]string{fmt.Sprintf("%d:%d", m.Port, m.Port)}, nil,
			[]string{filepath.Dir(envPath) + ":/data:ro"},
			[]string{"-d", "/data/" + name + ".json", "--admin-api-token", ms.Token, "--max-transaction-logs", "1000"})
		if err != nil {
			return it, err
		}
		ms.Container = cname
	}
	st.Mocks[name] = ms
	it.PID = ms.PID
	isDead := func() bool {
		if ms.PID > 0 {
			return !alive(ms.PID)
		}
		return !containerRunning(ms.Container)
	}
	// ready = our Mockoon answers its admin API with our token (a stray server on the port
	// would answer 404 or 401, so it can never look ready)
	if err := waitMockReady(ctx, mock.NewAdmin(name, m.Port, ms.Token), ms.Log, isDead); err != nil {
		it.State = "error"
		it.Message = err.Error()
		return it, fmt.Errorf("mock %s: %w", name, err)
	}
	return it, nil
}

func (r *Runner) upService(ctx context.Context, st *State, name string, restart, dry bool) (Item, error) {
	svc := r.Stack.Services[name]
	it := Item{Kind: "service", Name: name, Log: r.logPath(name)}
	prev := st.Services[name]
	if prev.running() && !restart {
		it.State = "already-running"
		it.PID, it.Container = prev.PID, prev.Container
		return it, nil
	}
	data, err := r.Data(st)
	if err != nil {
		return it, err
	}
	ss := &ServiceState{Log: r.logPath(name)}
	if svc.Config != nil {
		src, err := os.ReadFile(r.Stack.Path(svc.Config.Template))
		if err != nil {
			return it, err
		}
		out, err := tmpl.Expand(string(src), data)
		if err != nil {
			return it, fmt.Errorf("service %s config %s: %w", name, svc.Config.Template, err)
		}
		ss.Config = tmpl.Stringify(data["services"].(map[string]any)[name].(map[string]any)["config"])
		if !dry {
			if err := os.MkdirAll(filepath.Dir(ss.Config), 0o755); err != nil {
				return it, err
			}
			if err := os.WriteFile(ss.Config, []byte(out), 0o600); err != nil {
				return it, err
			}
		}
	}
	var env []string
	for _, k := range config.SortedKeys(svc.Env) {
		v, err := tmpl.Expand(svc.Env[k], data)
		if err != nil {
			return it, fmt.Errorf("service %s env %s: %w", name, k, err)
		}
		env = append(env, k+"="+v)
	}
	if svc.Container != nil {
		return r.upServiceContainer(ctx, st, name, svc, prev, ss, env, data, it, dry)
	}
	cmd := make([]string, len(svc.Cmd))
	for i, a := range svc.Cmd {
		if cmd[i], err = tmpl.Expand(a, data); err != nil {
			return it, fmt.Errorf("service %s cmd: %w", name, err)
		}
	}
	if dry {
		it.State = "would-start"
		it.Message = strings.Join(cmd, " ")
		return it, nil
	}
	if prev.running() {
		r.stopService(prev)
		it.State = "restarted"
	} else {
		it.State = "started"
	}
	dir := r.Stack.Dir
	if svc.Dir != "" {
		dir = r.Stack.Path(svc.Dir)
	}
	pid, err := startProcess(dir, cmd, env, ss.Log)
	if err != nil {
		it.State = "error"
		it.Message = err.Error()
		return it, fmt.Errorf("service %s: %w", name, err)
	}
	ss.PID, ss.PIDStart = pid, procStart(pid)
	st.Services[name] = ss
	it.PID = pid
	if err := waitReady(ctx, svc.Ready, ss.Log, func() bool { return !alive(pid) }); err != nil {
		it.State = "error"
		it.Message = err.Error()
		return it, fmt.Errorf("service %s: %w", name, err)
	}
	return it, nil
}

func (r *Runner) upServiceContainer(ctx context.Context, st *State, name string, svc *config.Service, prev, ss *ServiceState, env []string, data map[string]any, it Item, dry bool) (Item, error) {
	var err error
	c := svc.Container
	opts := containerOpts{CPUs: c.CPUs, Memory: c.Memory, Ports: c.Ports, Env: map[string]string{}}
	for _, kv := range env { // service-level env applies to the container too
		k, v, _ := strings.Cut(kv, "=")
		opts.Env[k] = v
	}
	for _, k := range config.SortedKeys(c.Env) {
		v, err := tmpl.Expand(c.Env[k], data)
		if err != nil {
			return it, fmt.Errorf("service %s container.env %s: %w", name, k, err)
		}
		opts.Env[k] = v
	}
	for _, m := range c.Mounts {
		v, err := tmpl.Expand(m, data)
		if err != nil {
			return it, fmt.Errorf("service %s mount: %w", name, err)
		}
		// relative host paths resolve against stack.yaml
		if host, rest, ok := strings.Cut(v, ":"); ok && !filepath.IsAbs(host) {
			v = r.Stack.Path(host) + ":" + rest
		}
		opts.Mounts = append(opts.Mounts, v)
	}
	for _, a := range c.Args {
		v, err := tmpl.Expand(a, data)
		if err != nil {
			return it, fmt.Errorf("service %s container.args: %w", name, err)
		}
		opts.Args = append(opts.Args, v)
	}
	image := c.Image
	if c.Build != nil {
		image = containerName(r.Stack.Project, name) + ":dmock"
	}
	if dry {
		it.State = "would-start"
		it.Message = fmt.Sprintf("%s run %s cpus=%s memory=%s", ContainerCLI(), image, c.CPUs, c.Memory)
		return it, nil
	}
	if prev.running() {
		r.stopService(prev)
		it.State = "restarted"
	} else {
		it.State = "started"
	}
	if c.Build != nil {
		ctxDir := r.Stack.Path(c.Build.Context)
		if len(c.Build.Pre) > 0 {
			pre := make([]string, len(c.Build.Pre))
			for i, a := range c.Build.Pre {
				if pre[i], err = tmpl.Expand(a, data); err != nil {
					return it, fmt.Errorf("service %s build.pre: %w", name, err)
				}
			}
			if err := runLogged(ctxDir, pre, ss.Log); err != nil {
				it.State, it.Message = "error", err.Error()
				return it, fmt.Errorf("service %s build.pre: %w", name, err)
			}
		}
		df := ""
		if c.Build.Dockerfile != "" {
			df = filepath.Join(ctxDir, c.Build.Dockerfile)
		}
		if err := buildImage(image, ctxDir, df, c.Build.Args, ss.Log); err != nil {
			it.State, it.Message = "error", err.Error()
			return it, fmt.Errorf("service %s: %w", name, err)
		}
	}
	cname, err2 := runServiceContainer(r.Stack.Project, name, image, opts)
	if err2 != nil {
		it.State, it.Message = "error", err2.Error()
		return it, fmt.Errorf("service %s: %w", name, err2)
	}
	ss.Container = cname
	// follow the container log into the service log file, so `logs` and `log` steps work the same
	if pid, err := startProcess(r.Dir, []string{ContainerCLI(), "logs", "-f", cname}, nil, ss.Log); err == nil {
		ss.LogPID, ss.LogStart = pid, procStart(pid)
	}
	st.Services[name] = ss
	it.Container = cname
	if err := waitReady(ctx, svc.Ready, ss.Log, func() bool { return !inspectContainer(cname).Running }); err != nil {
		info := inspectContainer(cname)
		it.State, it.Message = "error", err.Error()
		if info.OOMKilled {
			it.Message = "OOM killed (memory limit " + c.Memory + ")\n" + it.Message
		}
		return it, fmt.Errorf("service %s: %s", name, it.Message)
	}
	return it, nil
}

func (s *ServiceState) running() bool {
	if s == nil {
		return false
	}
	if s.Container != "" {
		return inspectContainer(s.Container).Running
	}
	return ours(s.PID, s.PIDStart)
}

func (r *Runner) stopService(s *ServiceState) {
	if s.Container != "" {
		_ = removeContainer(s.Container)
		if s.LogPID > 0 {
			stopProcess(s.LogPID, s.LogStart, 2*time.Second)
		}
		return
	}
	stopProcess(s.PID, s.PIDStart, 10*time.Second)
}

// ServiceStatus reports a service's state; for containers also OOM kill, exit code and a
// resource sample (nil when not running).
func (r *Runner) ServiceStatus(name string) (Item, *ContainerStats, error) {
	if _, ok := r.Stack.Services[name]; !ok {
		return Item{}, nil, fmt.Errorf("no service named %q", name)
	}
	st, err := r.State()
	if err != nil {
		return Item{}, nil, err
	}
	it := r.serviceItem(name, st.Services[name])
	if it.Container == "" || it.State != "running" {
		return it, nil, nil
	}
	stats, err := statsContainer(it.Container)
	if err != nil {
		return it, nil, err
	}
	return it, &stats, nil
}

func (r *Runner) serviceItem(name string, ss *ServiceState) Item {
	it := Item{Kind: "service", Name: name, State: "stopped", Log: r.logPath(name)}
	if ss == nil {
		return it
	}
	if ss.Container != "" {
		info := inspectContainer(ss.Container)
		it.Container = ss.Container
		if !info.Exists {
			return it
		}
		if info.Running {
			it.State = "running"
			return it
		}
		it.State = "exited"
		it.OOMKilled = info.OOMKilled
		code := info.ExitCode
		it.ExitCode = &code
		return it
	}
	if ours(ss.PID, ss.PIDStart) {
		it.State, it.PID = "running", ss.PID
	}
	return it
}

func (r *Runner) stopMock(m *MockState) {
	if m.PID > 0 {
		stopProcess(m.PID, m.PIDStart, 5*time.Second)
	}
	if m.Container != "" {
		_ = removeContainer(m.Container)
	}
}

// Down stops services (reverse order), mocks and containers this stack started.
// Remote deps are never touched. only limits it to some names.
func (r *Runner) Down(only []string) ([]Item, error) {
	st, err := r.State()
	if err != nil {
		return nil, err
	}
	pick := func(n string) bool {
		if len(only) == 0 {
			return true
		}
		for _, o := range only {
			if o == n {
				return true
			}
		}
		return false
	}
	var items []Item
	order, _ := r.Stack.ServiceOrder()
	known := map[string]bool{}
	for _, n := range order {
		known[n] = true
	}
	// services from state that are no longer in stack.yaml still get stopped
	for _, n := range config.SortedKeys(st.Services) {
		if !known[n] {
			order = append([]string{n}, order...)
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		n := order[i]
		ss := st.Services[n]
		if ss == nil || !pick(n) {
			continue
		}
		state := "stopped"
		if ss.running() || ss.Container != "" {
			r.stopService(ss) // containers are removed even when already exited
		} else {
			state = "not-running"
		}
		items = append(items, Item{Kind: "service", Name: n, State: state, PID: ss.PID, Container: ss.Container})
		delete(st.Services, n)
	}
	for _, n := range config.SortedKeys(st.Mocks) {
		if !pick(n) {
			continue
		}
		r.stopMock(st.Mocks[n])
		items = append(items, Item{Kind: "mock", Name: n, State: "stopped"})
		delete(st.Mocks, n)
	}
	for _, n := range config.SortedKeys(st.Deps) {
		if !pick(n) {
			continue
		}
		d := st.Deps[n]
		if d.Container != "" {
			if err := removeContainer(d.Container); err != nil {
				items = append(items, Item{Kind: "dep", Name: n, State: "error", Message: err.Error()})
				continue
			}
			items = append(items, Item{Kind: "dep", Name: n, State: "stopped", Mode: d.Mode})
		} else {
			items = append(items, Item{Kind: "dep", Name: n, State: "remote", Mode: d.Mode, Message: "left untouched"})
		}
		delete(st.Deps, n)
	}
	return items, st.Save(r.Stack.Dir)
}

// Status reports every component of stack.yaml and whether it is running.
func (r *Runner) Status() ([]Item, error) {
	st, err := r.State()
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, n := range config.SortedKeys(r.Stack.Deps) {
		d := r.Stack.Deps[n]
		it := Item{Kind: "dep", Name: n, Mode: d.Mode}
		switch {
		case d.Mode == "remote":
			it.State = "remote"
		case containerRunning(containerName(r.Stack.Project, n)):
			it.State = "running"
		default:
			it.State = "stopped"
		}
		items = append(items, it)
	}
	for _, n := range config.SortedKeys(r.Stack.Mocks) {
		m := r.Stack.Mocks[n]
		it := Item{Kind: "mock", Name: n, Port: m.Port, URL: fmt.Sprintf("http://localhost:%d", m.Port), State: "stopped", Log: r.logPath(n)}
		if ms := st.Mocks[n]; ms != nil && (ours(ms.PID, ms.PIDStart) || (ms.Container != "" && containerRunning(ms.Container))) {
			it.State, it.PID = "running", ms.PID
		}
		items = append(items, it)
	}
	for _, n := range config.SortedKeys(r.Stack.Services) {
		items = append(items, r.serviceItem(n, st.Services[n]))
	}
	return items, nil
}

// LogFile returns the log path for a mock or service (deps: docker logs).
func (r *Runner) LogFile(name string) (string, bool) {
	if _, ok := r.Stack.Mocks[name]; ok {
		return r.logPath(name), true
	}
	if _, ok := r.Stack.Services[name]; ok {
		return r.logPath(name), true
	}
	return "", false
}

// MockAdmin returns the admin client and spec for a running mock.
func (r *Runner) MockAdmin(name string) (*mock.Admin, *mock.Spec, error) {
	m, ok := r.Stack.Mocks[name]
	if !ok {
		return nil, nil, fmt.Errorf("no mock named %q (have: %s)", name, strings.Join(config.SortedKeys(r.Stack.Mocks), ", "))
	}
	st, err := r.State()
	if err != nil {
		return nil, nil, err
	}
	ms := st.Mocks[name]
	if ms == nil {
		return nil, nil, fmt.Errorf("mock %s is not running: run `dmock up` first", name)
	}
	spec, err := mock.LoadSpec(r.Stack.Path(m.Spec))
	if err != nil {
		return nil, nil, err
	}
	return mock.NewAdmin(name, ms.Port, ms.Token), spec, nil
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func waitMockReady(ctx context.Context, a *mock.Admin, logPath string, isDead func() bool) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if isDead() {
			return fmt.Errorf("mockoon exited before it was ready; last log lines:\n%s", tail(logPath, 15))
		}
		if a.Ping() == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mockoon admin API not ready after 60s; last log lines:\n%s", tail(logPath, 15))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}
