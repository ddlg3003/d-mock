package stack

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/ddlg3003/d-mock/internal/config"
)

// ControlActions are what a scenario's `stack` step can do to one dep, mock or service.
var ControlActions = []string{"stop", "start", "restart", "kill"}

// Control stops, starts, restarts or kills one part of the stack, to test outages and crash
// recovery:
//
//   - stop: graceful (SIGTERM / container stop); kill: immediate (SIGKILL / container kill).
//     Containers are kept, so a dep keeps its data across stop/start.
//   - start: brings it back. A stopped container restarts as it was (no after_ready: the
//     data is still there); anything else starts the way `up` does. A mock always starts
//     fresh: its state, faults and recorded calls are gone.
//   - restart: stop, then start.
//
// Remote deps can't be controlled: they are shared servers.
func (r *Runner) Control(ctx context.Context, action, name string) (Item, error) {
	kind := r.kindOf(name)
	it := Item{Kind: kind, Name: name}
	switch kind {
	case "":
		return it, fmt.Errorf("no dep, mock or service named %q", name)
	case "dep":
		if r.Stack.Deps[name].Mode != "container" {
			return it, fmt.Errorf("dep %s is remote: d-mock never stops a shared server (run it with --mode %s=container to test its outage)", name, name)
		}
	}
	switch action {
	case "stop", "kill":
		if err := r.halt(name, kind, action == "kill"); err != nil {
			return it, err
		}
		it.State = map[string]string{"stop": "stopped", "kill": "killed"}[action]
		return it, nil
	case "start":
		return r.resume(ctx, name, kind)
	case "restart":
		if err := r.halt(name, kind, false); err != nil {
			return it, err
		}
		it, err := r.resume(ctx, name, kind)
		if err == nil {
			it.State = "restarted"
		}
		return it, err
	}
	return it, fmt.Errorf("unknown stack action %q (have: stop, start, restart, kill)", action)
}

func (r *Runner) kindOf(name string) string {
	switch {
	case r.Stack.Deps[name] != nil:
		return "dep"
	case r.Stack.Mocks[name] != nil:
		return "mock"
	case r.Stack.Services[name] != nil:
		return "service"
	}
	return ""
}

// Running reports whether one dep, mock or service is up right now.
func (r *Runner) Running(name string) (bool, error) {
	st, err := r.State()
	if err != nil {
		return false, err
	}
	switch r.kindOf(name) {
	case "dep":
		if r.Stack.Deps[name].Mode != "container" {
			return true, nil
		}
		return containerRunning(containerName(r.Stack.Project, name)), nil
	case "mock":
		ms := st.Mocks[name]
		return ms != nil && (ours(ms.PID, ms.PIDStart) || (ms.Container != "" && containerRunning(ms.Container))), nil
	case "service":
		return st.Services[name].running(), nil
	}
	return false, fmt.Errorf("no dep, mock or service named %q", name)
}

func (r *Runner) halt(name, kind string, hard bool) error {
	st, err := r.State()
	if err != nil {
		return err
	}
	stopContainer := func(cname string) error {
		if hard {
			_, err := docker("kill", cname)
			return err
		}
		_, err := docker("stop", "-t", "10", cname)
		return err
	}
	stopProc := func(pid int, start string, grace time.Duration) {
		if hard {
			if ours(pid, start) {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				for i := 0; i < 50 && alive(pid); i++ {
					time.Sleep(100 * time.Millisecond)
				}
			}
			return
		}
		stopProcess(pid, start, grace)
	}
	switch kind {
	case "dep":
		cname := containerName(r.Stack.Project, name)
		if !containerRunning(cname) {
			return fmt.Errorf("dep %s is not running", name)
		}
		return stopContainer(cname)
	case "mock":
		ms := st.Mocks[name]
		if ms == nil {
			return fmt.Errorf("mock %s is not running", name)
		}
		if ms.Container != "" {
			return stopContainer(ms.Container)
		}
		stopProc(ms.PID, ms.PIDStart, 5*time.Second)
		return nil
	default:
		ss := st.Services[name]
		if !ss.running() {
			return fmt.Errorf("service %s is not running", name)
		}
		if ss.Container != "" {
			return stopContainer(ss.Container)
		}
		stopProc(ss.PID, ss.PIDStart, 10*time.Second)
		return nil
	}
}

func (r *Runner) resume(ctx context.Context, name, kind string) (Item, error) {
	st, err := r.State()
	if err != nil {
		return Item{Kind: kind, Name: name}, err
	}
	// a stopped container comes back as it was: same data, no rebuild, no after_ready
	cname := ""
	switch kind {
	case "dep":
		cname = containerName(r.Stack.Project, name)
	case "service":
		if ss := st.Services[name]; ss != nil {
			cname = ss.Container
		}
	}
	if cname != "" && inspectContainer(cname).Exists && !containerRunning(cname) {
		return r.restartContainer(ctx, st, name, kind, cname)
	}
	items, err := r.Up(ctx, UpOptions{Only: []string{name}})
	for _, it := range items {
		if it.Name == name {
			return it, err
		}
	}
	return Item{Kind: kind, Name: name, State: "started"}, err
}

func (r *Runner) restartContainer(ctx context.Context, st *State, name, kind, cname string) (Item, error) {
	it := Item{Kind: kind, Name: name, Container: cname, State: "started"}
	if _, err := docker("start", cname); err != nil {
		return it, err
	}
	var ready *config.Ready
	logPath := ""
	var after []config.Command
	if kind == "dep" {
		ready = r.Stack.Deps[name].Ready
	} else {
		svc := r.Stack.Services[name]
		ready, after = svc.Ready, svc.AfterReady
		ss := st.Services[name]
		logPath = ss.Log
		// the old `logs -f` follower ended with the container: follow again (from now on)
		if pid, err := startProcess(r.Dir, []string{ContainerCLI(), "logs", "-f", "--since", "1s", cname}, nil, ss.Log); err == nil {
			ss.LogPID, ss.LogStart = pid, procStart(pid)
		}
		if err := st.Save(r.Stack.Dir); err != nil {
			return it, err
		}
	}
	if err := waitReady(ctx, ready, logPath, func() bool { return !containerRunning(cname) }); err != nil {
		it.State, it.Message = "error", err.Error()
		return it, fmt.Errorf("%s %s: %w", kind, name, err)
	}
	if len(after) > 0 {
		data, err := r.Data(st)
		if err != nil {
			return it, err
		}
		if err := r.afterReady(ctx, kind, name, after, data); err != nil {
			return it, err
		}
	}
	return it, nil
}
