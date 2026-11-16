package scenario

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/ddlg3003/d-mock/internal/config"
	"github.com/ddlg3003/d-mock/internal/stack"
	"github.com/ddlg3003/d-mock/internal/tmpl"
)

// StepResult is one executed step in the report.
type StepResult struct {
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Passed   bool           `json:"passed"`
	Attempts int            `json:"attempts"`
	Duration string         `json:"duration"`
	Error    string         `json:"error,omitempty"`
	Failures []string       `json:"failures,omitempty"`
	Result   map[string]any `json:"result,omitempty"`
	Skipped  bool           `json:"skipped,omitempty"`
}

type Report struct {
	Scenario string       `json:"scenario"`
	File     string       `json:"file"`
	RunID    string       `json:"run_id"`
	Passed   bool         `json:"passed"`
	Skipped  bool         `json:"skipped,omitempty"` // never ran: before_all failed or the run was interrupted
	DryRun   bool         `json:"dry_run,omitempty"`
	Duration string       `json:"duration"`
	Steps    []StepResult `json:"steps"`
	Cleanup  []StepResult `json:"cleanup,omitempty"`
}

type Options struct {
	DryRun    bool
	KeepState bool   // skip automatic cleanup (explicit cleanup steps still run)
	RunID     string // reuse a run id instead of generating one
	Verbose   bool   // keep full step results in the report
	// AllowRemoteLoad lets k6 steps run while some deps are remote (shared servers).
	AllowRemoteLoad bool
	Progress        func(StepResult)
}

// Runner executes scenarios against a started stack.
type Runner struct {
	Stack *stack.Runner
	Opt   Options

	suiteSaved map[string]any    // what before_all saved, seen by every scenario as ${saved.x}
	hooks      map[string][]Step // suite hooks, set by RunSuite
	control    stackControl      // stack steps; nil = Stack (tests swap in a fake)
}

// stackControl is what stack steps and their cleanup need from the stack.
type stackControl interface {
	Control(ctx context.Context, action, name string) (stack.Item, error)
	Running(name string) (bool, error)
}

func (r *Runner) stackCtl() stackControl {
	if r.control != nil {
		return r.control
	}
	return r.Stack
}

// run is the state of one scenario execution.
type run struct {
	r       *Runner
	id      string
	data    map[string]any
	saved   map[string]any
	ledger  *ledger
	logOffs map[string]int64
	start   time.Time // when the scenario started: kafka consume reads from here
}

func NewRunID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "dm" + hex.EncodeToString(b)
}

// Run executes one scenario run: before_each, its steps, its cleanup steps, after_each, then
// the automatic cleanup.
func (r *Runner) Run(ctx context.Context, sc *Scenario) (*Report, error) {
	start := time.Now()
	id := r.Opt.RunID
	if id == "" {
		id = NewRunID()
	}
	ru, err := r.newRun(sc, id, start)
	if err != nil {
		return nil, err
	}
	rep := &Report{Scenario: sc.Name, File: sc.Path, RunID: id, Passed: true, DryRun: r.Opt.DryRun}
	steps := append(hookSteps("before_each", r.hooks["before_each"]), sc.Steps...)
	rep.Passed = ru.runSteps(ctx, steps, "", &rep.Steps)

	// cleanup always runs, even after Ctrl-C: it gets its own context so a cancelled run
	// still deletes its rows and terminates its workflows. Explicit steps first, then the
	// after_each hook, then the ledger (unless --keep).
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	ru.runAll(cctx, sc.Cleanup, "cleanup: ", &rep.Cleanup)
	if !ru.runAll(cctx, hookSteps("after_each", r.hooks["after_each"]), "", &rep.Cleanup) {
		rep.Passed = false // a failed after_each hook fails the scenario it ran after
	}
	ru.undoLedger(cctx, &rep.Cleanup)
	rep.Duration = time.Since(start).Round(time.Millisecond).String()
	return rep, nil
}

// newRun prepares the data a run's steps see: ${run}, ${saved}, ${matrix}, ${iteration},
// ${vars}, and the log offsets for log steps.
func (r *Runner) newRun(sc *Scenario, id string, start time.Time) (*run, error) {
	st, err := r.Stack.State()
	if err != nil {
		return nil, err
	}
	data, err := r.Stack.Data(st)
	if err != nil {
		return nil, err
	}
	ru := &run{r: r, id: id, data: data, saved: map[string]any{}, ledger: &ledger{}, logOffs: map[string]int64{}, start: start}
	data["run"] = id
	for k, v := range r.suiteSaved { // values saved by the before_all hook
		ru.saved[k] = v
	}
	data["saved"] = ru.saved
	if sc.MatrixValues != nil {
		data["matrix"] = sc.MatrixValues
	}
	data["iteration"] = max(sc.Iteration, 1)
	vars := map[string]any{}
	data["vars"] = vars
	// vars may reference earlier vars and ${run}: expand in file order is not kept by YAML maps,
	// so expand repeatedly until stable (max depth 5)
	for k, v := range sc.Vars {
		vars[k] = v
	}
	for i := 0; i < 5; i++ {
		for k, v := range sc.Vars {
			if x, err := tmpl.ExpandAny(v, data); err == nil {
				vars[k] = x
			}
		}
	}
	for k, v := range sc.Vars {
		x, err := tmpl.ExpandAny(v, data)
		if err != nil {
			return nil, fmt.Errorf("vars.%s: %w", k, err)
		}
		vars[k] = x
	}
	// log assertions only see lines written after the scenario started
	names := config.SortedKeys(r.Stack.Stack.Services)
	names = append(names, config.SortedKeys(r.Stack.Stack.Mocks)...)
	for _, name := range names {
		if p, ok := r.Stack.LogFile(name); ok {
			if fi, err := os.Stat(p); err == nil {
				ru.logOffs[name] = fi.Size()
			}
		}
	}
	return ru, nil
}

// runSteps runs steps in order and stops at the first failure (the rest are reported as
// skipped). It reports whether all passed.
func (ru *run) runSteps(ctx context.Context, steps []Step, prefix string, into *[]StepResult) bool {
	ok := true
	for i, step := range steps {
		if !ok {
			typ, _, _ := step.Type()
			*into = append(*into, StepResult{Name: prefix + stepName(step, i), Type: typ, Skipped: true})
			continue
		}
		res := ru.runStep(ctx, step, i)
		res.Name = prefix + res.Name
		*into = append(*into, res)
		if ru.r.Opt.Progress != nil {
			ru.r.Opt.Progress(res)
		}
		ok = res.Passed
	}
	return ok
}

// runAll runs every step even after a failure (cleanup and after_* hooks).
func (ru *run) runAll(ctx context.Context, steps []Step, prefix string, into *[]StepResult) bool {
	ok := true
	for i, step := range steps {
		res := ru.runStep(ctx, step, i)
		res.Name = prefix + res.Name
		*into = append(*into, res)
		if ru.r.Opt.Progress != nil {
			ru.r.Opt.Progress(res)
		}
		ok = ok && res.Passed
	}
	return ok
}

func (ru *run) undoLedger(ctx context.Context, into *[]StepResult) {
	if ru.r.Opt.KeepState || ru.r.Opt.DryRun {
		return
	}
	for _, res := range ru.ledger.undo(ctx, ru) {
		*into = append(*into, res)
		if ru.r.Opt.Progress != nil {
			ru.r.Opt.Progress(res)
		}
	}
}

func stepName(s Step, i int) string {
	if s.Name != "" {
		return s.Name
	}
	typ, _, _ := s.Type()
	return fmt.Sprintf("#%d %s", i+1, typ)
}

func (ru *run) runStep(ctx context.Context, step Step, i int) (res StepResult) {
	start := time.Now()
	typ, rawArgs, err := step.Type()
	name := stepName(step, i)
	if x, xerr := tmpl.Expand(name, ru.data); xerr == nil { // names may use ${matrix.x} etc.
		name = x
	}
	res = StepResult{Name: name, Type: typ}
	defer func() { res.Duration = time.Since(start).Round(time.Millisecond).String() }()
	if err != nil {
		res.Error = err.Error()
		return res
	}
	timeout, every := time.Duration(0), time.Second
	if step.Retry != nil {
		timeout = step.Retry.Timeout
		if step.Retry.Every > 0 {
			every = step.Retry.Every
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		res.Attempts++
		var args any
		var out map[string]any
		if typ == "parallel" {
			out, err = ru.execParallel(ctx, step)
			if ru.r.Opt.DryRun { // the branches showed their args; nothing ran to check
				res.Passed, res.Result = true, out
				return res
			}
		} else if args, err = expandArgs(typ, rawArgs, ru.data); err == nil {
			if ru.r.Opt.DryRun {
				res.Passed = true
				res.Result = map[string]any{"args": args}
				// nothing ran, so nothing was saved: later steps see a placeholder that names
				// the value, instead of failing on an unresolved ${saved.x}
				for name := range step.Save {
					ru.saved[name] = "<saved." + name + ">"
				}
				return res
			}
			out, err = ru.exec(ctx, typ, args.(map[string]any))
		}
		res.Error, res.Failures = "", nil
		if err != nil {
			res.Error = err.Error()
		} else {
			exp, xerr := tmpl.ExpandAny(toMap(step.Expect), ru.data)
			if xerr != nil {
				res.Error = "expect: " + xerr.Error()
			} else {
				res.Failures = Check(out, exp.(map[string]any))
			}
		}
		res.Result = out
		if res.Error == "" && len(res.Failures) == 0 {
			res.Passed = true
			break
		}
		if timeout == 0 || time.Now().Add(every).After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			res.Error = ctx.Err().Error()
			return res
		case <-time.After(every):
		}
	}
	if res.Passed {
		for name, path := range step.Save {
			v, ok := tmpl.Lookup(res.Result, path)
			if !ok {
				res.Passed = false
				res.Error = fmt.Sprintf("save %s: result has no %s", name, path)
				break
			}
			ru.saved[name] = v
		}
	}
	ru.data["last"] = res.Result
	if !ru.r.Opt.Verbose && res.Passed {
		res.Result = summarize(res.Result)
	}
	return res
}

func toMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// summarize trims large result fields from passed steps to keep reports short.
func summarize(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := map[string]any{}
	for k, v := range m {
		if s, ok := v.(string); ok && len(s) > 200 {
			v = s[:200] + "…"
		}
		if l, ok := v.([]any); ok && len(l) > 20 {
			v = fmt.Sprintf("[%d items]", len(l))
		}
		out[k] = v
	}
	return out
}

func (ru *run) exec(ctx context.Context, typ string, args map[string]any) (map[string]any, error) {
	switch typ {
	case "http":
		return execHTTP(ctx, args, ru.suiteDir())
	case "exec":
		return execCmd(ctx, args, ru.suiteDir())
	case "sql":
		return ru.execSQL(ctx, args)
	case "kafka":
		return ru.execKafka(ctx, args)
	case "temporal":
		return ru.execTemporal(ctx, args)
	case "mock":
		return ru.execMock(args)
	case "log":
		return ru.execLog(args)
	case "service":
		return ru.execService(args)
	case "stack":
		return ru.execStack(ctx, args)
	case "k6":
		return ru.execK6(ctx, args)
	case "sleep":
		d, err := time.ParseDuration(tmpl.Stringify(args["duration"]))
		if err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d):
		}
		return map[string]any{"slept": d.String()}, nil
	}
	return nil, fmt.Errorf("unknown step type %s", typ)
}

// expandArgs fills in a step's references. A command a shell will read (exec's cmd as a
// line, or an argv list starting `sh -c`) gets each reference quoted as one literal word,
// so a value from the system under test can never run as shell syntax.
func expandArgs(typ string, raw any, data map[string]any) (any, error) {
	a, ok := raw.(map[string]any)
	if typ != "exec" || !ok {
		return tmpl.ExpandAny(raw, data)
	}
	rest := make(map[string]any, len(a))
	for k, v := range a {
		if k != "cmd" {
			rest[k] = v
		}
	}
	x, err := tmpl.ExpandAny(rest, data)
	if err != nil {
		return nil, err
	}
	out := x.(map[string]any)
	switch c := a["cmd"].(type) {
	case string:
		line, err := tmpl.ExpandShell(c, data)
		if err != nil {
			return nil, err
		}
		out["cmd"] = line
	case []any:
		argv, err := tmpl.ExpandArgv(strList(c), data)
		if err != nil {
			return nil, err
		}
		l := make([]any, len(argv))
		for i, s := range argv {
			l[i] = s
		}
		out["cmd"] = l
	default:
		out["cmd"] = c
	}
	return out, nil
}

// suiteDir is where commands (exec, encode, decode) run: the suite folder.
func (ru *run) suiteDir() string {
	if ru.r == nil || ru.r.Stack == nil || ru.r.Stack.Stack == nil {
		return "."
	}
	return ru.r.Stack.Stack.Dir
}
