package scenario

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

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
	Progress  func(StepResult)
}

// Runner executes scenarios against a started stack.
type Runner struct {
	Stack *stack.Runner
	Opt   Options
}

// run is the state of one scenario execution.
type run struct {
	r       *Runner
	id      string
	data    map[string]any
	saved   map[string]any
	ledger  *ledger
	logOffs map[string]int64
}

func NewRunID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "dm" + hex.EncodeToString(b)
}

func (r *Runner) Run(ctx context.Context, sc *Scenario) (*Report, error) {
	start := time.Now()
	id := r.Opt.RunID
	if id == "" {
		id = NewRunID()
	}
	st, err := stack.LoadState(r.Stack.Stack.Dir)
	if err != nil {
		return nil, err
	}
	data, err := r.Stack.Data(st)
	if err != nil {
		return nil, err
	}
	ru := &run{r: r, id: id, data: data, saved: map[string]any{}, ledger: &ledger{}, logOffs: map[string]int64{}}
	data["run"] = id
	data["saved"] = ru.saved
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
	for name := range r.Stack.Stack.Services {
		if p, ok := r.Stack.LogFile(name); ok {
			if fi, err := os.Stat(p); err == nil {
				ru.logOffs[name] = fi.Size()
			}
		}
	}

	rep := &Report{Scenario: sc.Name, File: sc.Path, RunID: id, Passed: true, DryRun: r.Opt.DryRun}
	failed := false
	for i, step := range sc.Steps {
		if failed {
			typ, _, _ := step.Type()
			rep.Steps = append(rep.Steps, StepResult{Name: stepName(step, i), Type: typ, Skipped: true})
			continue
		}
		res := ru.runStep(ctx, step, i)
		rep.Steps = append(rep.Steps, res)
		if r.Opt.Progress != nil {
			r.Opt.Progress(res)
		}
		if !res.Passed {
			failed = true
			rep.Passed = false
		}
	}

	// cleanup always runs: explicit steps first, then the ledger (unless --keep)
	for i, step := range sc.Cleanup {
		res := ru.runStep(ctx, step, i)
		res.Name = "cleanup: " + res.Name
		rep.Cleanup = append(rep.Cleanup, res)
		if r.Opt.Progress != nil {
			r.Opt.Progress(res)
		}
	}
	if !r.Opt.KeepState && !r.Opt.DryRun {
		for _, res := range ru.ledger.undo(ctx, ru) {
			rep.Cleanup = append(rep.Cleanup, res)
			if r.Opt.Progress != nil {
				r.Opt.Progress(res)
			}
		}
	}
	rep.Duration = time.Since(start).Round(time.Millisecond).String()
	return rep, nil
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
	res = StepResult{Name: stepName(step, i), Type: typ}
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
		args, err := tmpl.ExpandAny(rawArgs, ru.data)
		var out map[string]any
		if err == nil {
			if ru.r.Opt.DryRun {
				res.Passed = true
				res.Result = map[string]any{"args": args}
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
		return execHTTP(ctx, args)
	case "exec":
		return execCmd(ctx, args, ru.r.Stack.Stack.Dir)
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
