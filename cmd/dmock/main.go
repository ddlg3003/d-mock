// Command dmock starts a local integration-test stack (deps, Mockoon mocks, our services)
// and runs YAML scenarios against it. Every command accepts --json for machine output.
//
// Exit codes: 0 ok, 1 failed (scenario failed or a component errored), 2 usage/config error.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	dmock "github.com/ddlg3003/d-mock"
	"github.com/ddlg3003/d-mock/internal/config"
	"github.com/ddlg3003/d-mock/internal/mock"
	"github.com/ddlg3003/d-mock/internal/scenario"
	"github.com/ddlg3003/d-mock/internal/stack"
)

const usage = `dmock — local integration-test harness (stack + Mockoon mocks + YAML scenarios)

Usage: dmock [global flags] <command> [args]

New here? Run: dmock help guide

Global flags (anywhere on the line):
  -f, --file PATH        stack file (default ./stack.yaml, or $DMOCK_STACK)
      --json             machine-readable output
      --mode DEP=MODE    switch a dep to remote|container for this command (repeatable)

Setup:
  doctor [scenario...]                           check the tools dmock needs (and how to install them)

Stack:
  up [--only a,b] [--restart a,b] [--dry-run]   start what is not running (idempotent)
  down [name...]                                 stop services, mocks, containers (remote deps untouched)
  status                                         what is running
  logs NAME [--grep RE] [--tail N]               a mock's or service's log (deps: container logs)
  validate [scenario...]                         check stack.yaml, mock specs and scenarios

Mocks:
  mock set NAME KEY field=value...               set per-key state (KEY "-" = routes without key)
  mock fault NAME KEY FAULT [--times N]          inject a fault (default: until cleared)
  mock clear NAME KEY                            clear a key's fault
  mock calls NAME [--route ID] [--key K]         recorded requests
  mock reset NAME                                clear all state, faults and recorded calls
  mock compile NAME                              print the generated Mockoon environment

Help:
  help [TOPIC]                                   usage, or a topic (guide); "dmock guide" = "dmock help guide"

Scenarios:
  run FILE|DIR... [--dry-run] [--keep] [--run-id ID] [--verbose] [--allow-remote-load]
      --keep               skip automatic cleanup (seeded rows, started workflows, mock state)
      --allow-remote-load  let k6 steps run while some deps are remote (shared servers)
`

type cli struct {
	file  string
	json  bool
	modes map[string]string
	flags map[string]string // command flags
	bools map[string]bool
	args  []string
}

var boolFlags = map[string]bool{"json": true, "dry-run": true, "keep": true, "verbose": true, "help": true, "h": true, "allow-remote-load": true}

// valueFlags are the flags that take a value; anything else is an error, so a typo such as
// --time 3 fails instead of being ignored.
var valueFlags = map[string]bool{"f": true, "file": true, "mode": true, "only": true, "restart": true, "grep": true, "tail": true, "times": true, "route": true, "key": true, "run-id": true}

func parse(argv []string) (*cli, error) {
	c := &cli{modes: map[string]string{}, flags: map[string]string{}, bools: map[string]bool{}}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			c.args = append(c.args, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		if boolFlags[name] {
			c.bools[name] = true
			continue
		}
		if !valueFlags[name] {
			return c, fmt.Errorf("unknown flag %s (see dmock help)", a)
		}
		if !hasVal {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("flag --%s needs a value", name)
			}
			i++
			val = argv[i]
		}
		switch name {
		case "f", "file":
			c.file = val
		case "mode":
			k, v, ok := strings.Cut(val, "=")
			if !ok {
				return nil, fmt.Errorf("--mode wants DEP=MODE, got %q", val)
			}
			c.modes[k] = v
		default:
			c.flags[name] = val
		}
	}
	c.json = c.bools["json"]
	if c.file == "" {
		c.file = os.Getenv("DMOCK_STACK")
	}
	if c.file == "" {
		c.file = "stack.yaml"
	}
	return c, nil
}

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	c, err := parse(argv)
	if err != nil {
		return c.fail(2, err)
	}
	if len(c.args) == 0 || c.bools["help"] || c.bools["h"] {
		fmt.Print(usage)
		if len(c.args) == 0 {
			return 2
		}
		return 0
	}
	if c.args[0] == "doctor" { // works with or without a stack.yaml
		return c.doctor()
	}
	if c.args[0] == "help" || c.args[0] == "guide" { // needs no stack.yaml
		topic := "guide"
		if c.args[0] == "help" {
			if len(c.args) < 2 {
				fmt.Print(usage)
				fmt.Println("\nHelp topics: dmock help guide")
				return 0
			}
			topic = c.args[1]
		}
		text, ok := dmock.HelpTopics[topic]
		if !ok {
			return c.fail(2, fmt.Errorf("unknown help topic %q (topics: guide)", topic))
		}
		fmt.Print(text)
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	st, err := config.Load(c.file, c.modes)
	if err != nil {
		return c.fail(2, err)
	}
	r := stack.New(st)

	switch c.args[0] {
	case "up":
		items, err := r.Up(ctx, stack.UpOptions{Only: list(c.flags["only"]), Restart: list(c.flags["restart"]), DryRun: c.bools["dry-run"]})
		c.items(items)
		if err != nil {
			return c.fail(1, err)
		}
		return 0
	case "down":
		items, err := r.Down(c.args[1:])
		c.items(items)
		if err != nil {
			return c.fail(1, err)
		}
		return 0
	case "status":
		items, err := r.Status()
		if err != nil {
			return c.fail(1, err)
		}
		c.items(items)
		return 0
	case "logs":
		return c.logs(r)
	case "validate":
		return c.validate(r)
	case "mock":
		return c.mock(r)
	case "run":
		return c.runScenarios(ctx, r)
	}
	return c.fail(2, fmt.Errorf("unknown command %q (see dmock --help)", c.args[0]))
}

func list(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func (c *cli) out(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (c *cli) fail(code int, err error) int {
	if c != nil && c.json {
		c.out(map[string]any{"ok": false, "error": err.Error(), "exit_code": code})
	} else {
		fmt.Fprintln(os.Stderr, "dmock:", err)
	}
	return code
}

func (c *cli) items(items []stack.Item) {
	if c.json {
		c.out(map[string]any{"ok": true, "items": items})
		return
	}
	for _, it := range items {
		extra := []string{}
		if it.Mode != "" {
			extra = append(extra, "mode="+it.Mode)
		}
		if it.PID > 0 {
			extra = append(extra, "pid="+strconv.Itoa(it.PID))
		}
		if it.URL != "" {
			extra = append(extra, it.URL)
		}
		if it.Container != "" {
			extra = append(extra, "container="+it.Container)
		}
		if it.OOMKilled {
			extra = append(extra, "OOM-KILLED")
		}
		if it.ExitCode != nil {
			extra = append(extra, fmt.Sprintf("exit=%d", *it.ExitCode))
		}
		fmt.Printf("%-8s %-20s %-16s %s\n", it.Kind, it.Name, it.State, strings.Join(extra, " "))
		if it.Message != "" {
			fmt.Printf("         %s\n", strings.ReplaceAll(it.Message, "\n", "\n         "))
		}
	}
}

func (c *cli) logs(r *stack.Runner) int {
	if len(c.args) < 2 {
		return c.fail(2, fmt.Errorf("usage: dmock logs NAME [--grep RE] [--tail N]"))
	}
	name := c.args[1]
	var lines []string
	if path, ok := r.LogFile(name); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return c.fail(1, err)
		}
		lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	} else if _, ok := r.Stack.Deps[name]; ok {
		out, err := stack.ContainerLogs(r.Stack.Project, name, 500)
		if err != nil {
			return c.fail(1, err)
		}
		lines = strings.Split(strings.TrimRight(out, "\n"), "\n")
	} else {
		return c.fail(2, fmt.Errorf("no dep, mock or service named %q", name))
	}
	if g := c.flags["grep"]; g != "" {
		re, err := regexp.Compile(g)
		if err != nil {
			return c.fail(2, err)
		}
		var kept []string
		for _, l := range lines {
			if re.MatchString(l) {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	n := 200
	if t := c.flags["tail"]; t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			n = v
		}
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if c.json {
		c.out(map[string]any{"ok": true, "name": name, "lines": lines})
		return 0
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return 0
}

func (c *cli) validate(r *stack.Runner) int {
	var problems []string
	for _, n := range config.SortedKeys(r.Stack.Mocks) {
		m := r.Stack.Mocks[n]
		spec, err := mock.LoadSpec(r.Stack.Path(m.Spec))
		if err == nil {
			_, err = mock.Compile(n, m.Port, spec, filepath.Dir(r.Stack.Path(m.Spec)))
		}
		if err != nil {
			problems = append(problems, err.Error())
		}
	}
	for _, f := range expandScenarioArgs(c.args[1:]) {
		if _, err := scenario.Load(f); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if c.json {
		c.out(map[string]any{"ok": len(problems) == 0, "problems": problems})
	} else if len(problems) == 0 {
		fmt.Println("ok")
	} else {
		for _, p := range problems {
			fmt.Println("✗", p)
		}
	}
	if len(problems) > 0 {
		return 2
	}
	return 0
}

func (c *cli) mock(r *stack.Runner) int {
	if len(c.args) < 3 {
		return c.fail(2, fmt.Errorf("usage: dmock mock set|fault|clear|calls|reset|compile NAME ..."))
	}
	sub, name := c.args[1], c.args[2]
	if sub == "compile" {
		m, ok := r.Stack.Mocks[name]
		if !ok {
			return c.fail(2, fmt.Errorf("no mock named %q", name))
		}
		spec, err := mock.LoadSpec(r.Stack.Path(m.Spec))
		if err != nil {
			return c.fail(2, err)
		}
		env, err := mock.Compile(name, m.Port, spec, filepath.Dir(r.Stack.Path(m.Spec)))
		if err != nil {
			return c.fail(2, err)
		}
		os.Stdout.Write(append(env, '\n'))
		return 0
	}
	admin, spec, err := r.MockAdmin(name)
	if err != nil {
		return c.fail(1, err)
	}
	key := func(i int) (string, error) {
		if len(c.args) <= i {
			return "", fmt.Errorf("missing KEY (use - for routes without a key)")
		}
		if c.args[i] == "-" {
			return mock.NoKey, nil
		}
		return c.args[i], nil
	}
	var result any
	switch sub {
	case "set":
		k, err := key(3)
		if err != nil {
			return c.fail(2, err)
		}
		fields := map[string]string{}
		for _, kv := range c.args[4:] {
			f, v, ok := strings.Cut(kv, "=")
			if !ok {
				return c.fail(2, fmt.Errorf("expected field=value, got %q", kv))
			}
			fields[f] = v
		}
		if len(fields) == 0 {
			return c.fail(2, fmt.Errorf("mock set needs at least one field=value"))
		}
		if err := spec.CheckState(fields); err != nil {
			return c.fail(2, fmt.Errorf("mock %s: %w", name, err))
		}
		err = admin.SetState(k, fields)
		result = map[string]any{"mock": name, "key": k, "state": fields}
	case "fault":
		k, err := key(3)
		if err != nil {
			return c.fail(2, err)
		}
		if len(c.args) < 5 {
			return c.fail(2, fmt.Errorf("usage: dmock mock fault NAME KEY FAULT [--times N]"))
		}
		times := 0
		if t := c.flags["times"]; t != "" {
			if times, err = strconv.Atoi(t); err != nil {
				return c.fail(2, err)
			}
		}
		if err := spec.CheckFault(c.args[4]); err != nil {
			return c.fail(2, fmt.Errorf("mock %s: %w", name, err))
		}
		err = admin.SetFault(k, c.args[4], times)
		result = map[string]any{"mock": name, "key": k, "fault": c.args[4], "times": times}
	case "clear":
		k, err := key(3)
		if err != nil {
			return c.fail(2, err)
		}
		err = admin.SetFault(k, "", 0)
		result = map[string]any{"mock": name, "key": k, "cleared": true}
	case "reset":
		err = admin.Reset()
		result = map[string]any{"mock": name, "reset": true}
	case "calls":
		calls, cerr := admin.Calls(spec)
		err = cerr
		calls = mock.Filter(calls, c.flags["route"], c.flags["key"])
		if !c.json && err == nil {
			for _, call := range calls {
				fmt.Printf("%d %-6s %-40s %-16s %d %s\n", call.TimestampMs, call.Method, call.Path, call.RouteID, call.Status, oneLine(call.Body))
			}
			return 0
		}
		result = map[string]any{"mock": name, "count": len(calls), "calls": calls}
	default:
		return c.fail(2, fmt.Errorf("unknown mock command %q", sub))
	}
	if err != nil {
		return c.fail(1, err)
	}
	if c.json {
		c.out(map[string]any{"ok": true, "result": result})
	} else {
		b, _ := json.Marshal(result)
		fmt.Println(string(b))
	}
	return 0
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}

func expandScenarioArgs(args []string) []string {
	var files []string
	for _, a := range args {
		fi, err := os.Stat(a)
		if err == nil && fi.IsDir() {
			matches, _ := filepath.Glob(filepath.Join(a, "*.yaml"))
			more, _ := filepath.Glob(filepath.Join(a, "*.yml"))
			matches = append(matches, more...)
			sort.Strings(matches)
			files = append(files, matches...)
			continue
		}
		files = append(files, a)
	}
	return files
}

func (c *cli) runScenarios(ctx context.Context, r *stack.Runner) int {
	files := expandScenarioArgs(c.args[1:])
	if len(files) == 0 {
		return c.fail(2, fmt.Errorf("usage: dmock run FILE|DIR... [--dry-run] [--keep] [--run-id ID]"))
	}
	var scs []*scenario.Scenario
	for _, f := range files {
		sc, err := scenario.Load(f)
		if err != nil {
			return c.fail(2, err)
		}
		scs = append(scs, sc)
	}
	opt := scenario.Options{DryRun: c.bools["dry-run"], KeepState: c.bools["keep"], RunID: c.flags["run-id"], Verbose: c.bools["verbose"], AllowRemoteLoad: c.bools["allow-remote-load"]}
	if !c.json {
		opt.Progress = func(s scenario.StepResult) {
			mark := "✓"
			if !s.Passed {
				mark = "✗"
			}
			att := ""
			if s.Attempts > 1 {
				att = fmt.Sprintf(" (%d attempts)", s.Attempts)
			}
			fmt.Printf("  %s %s [%s] %s%s\n", mark, s.Name, s.Type, s.Duration, att)
			if s.Error != "" {
				fmt.Printf("      error: %s\n", s.Error)
			}
			for _, f := range s.Failures {
				fmt.Printf("      expect: %s\n", f)
			}
			if w, ok := s.Result["warning"].(string); ok && w != "" {
				fmt.Printf("      warning: %s\n", w)
			}
		}
	}
	runner := &scenario.Runner{Stack: r, Opt: opt}
	var reports []*scenario.Report
	failed := 0
	for _, sc := range scs {
		if !c.json {
			fmt.Printf("▶ %s (%s)\n", sc.Name, sc.Path)
		}
		rep, err := runner.Run(ctx, sc)
		if err != nil {
			return c.fail(2, fmt.Errorf("%s: %w", sc.Path, err))
		}
		reports = append(reports, rep)
		if !rep.Passed {
			failed++
		}
		if !c.json {
			verdict := "PASS"
			if !rep.Passed {
				verdict = "FAIL"
			}
			fmt.Printf("%s %s run=%s %s\n\n", verdict, sc.Name, rep.RunID, rep.Duration)
		}
	}
	if c.json {
		c.out(map[string]any{"ok": failed == 0, "passed": len(reports) - failed, "failed": failed, "reports": reports})
	} else {
		fmt.Printf("%d passed, %d failed\n", len(reports)-failed, failed)
	}
	if failed > 0 {
		return 1
	}
	return 0
}
