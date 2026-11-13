package scenario

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/ddlg3003/d-mock/internal/config"
	"gopkg.in/yaml.v3"
)

// LoadHooks decodes the suite hooks in dmock.yaml (before_all, before_each, after_each,
// after_all), each a list of ordinary steps; include paths resolve from the suite folder.
func LoadHooks(st *config.Stack) (map[string][]Step, error) {
	out := map[string][]Step{}
	for _, name := range config.HookNames {
		node, ok := st.Hooks[name]
		if !ok {
			continue
		}
		b, err := yaml.Marshal(&node)
		if err != nil {
			return nil, err
		}
		var steps []Step
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&steps); err != nil {
			return nil, fmt.Errorf("hooks.%s: %w (a hook is a list of steps)", name, err)
		}
		if steps, err = expandIncludes(steps, st.Dir, 0); err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", name, err)
		}
		if err := ValidateSteps(steps, false); err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", name, err)
		}
		out[name] = steps
	}
	return out, nil
}

func hookSteps(hook string, steps []Step) []Step {
	out := make([]Step, len(steps))
	for i, s := range steps {
		out[i] = s
		out[i].Name = hook + ": " + stepName(s, i)
	}
	return out
}

// RunSuite runs scenario runs with the suite hooks around them:
//
//	before_all   once, first. Its saved values are ${saved.x} in every scenario. If it
//	             fails, the scenarios are skipped (and reported failed).
//	before_each  before each scenario's steps; a failure skips that scenario's steps.
//	after_each   after each scenario's cleanup steps, always.
//	after_all    once, last, always; then what before_all created is cleaned up.
//
// before_all and after_all appear as their own entries in the reports ("hook: before_all").
// onStart and onDone report progress (may be nil).
func (r *Runner) RunSuite(ctx context.Context, scs []*Scenario, onStart func(name, file string), onDone func(*Report)) ([]*Report, error) {
	hooks, err := LoadHooks(r.Stack.Stack)
	if err != nil {
		return nil, err
	}
	r.hooks = hooks
	var reports []*Report
	done := func(rep *Report) {
		reports = append(reports, rep)
		if onDone != nil {
			onDone(rep)
		}
	}
	started := func(name, file string) {
		if onStart != nil {
			onStart(name, file)
		}
	}
	suiteFile := config.SuiteFileIn(r.Stack.Stack.Dir)

	var suite *run
	suiteRep := func(name string) *Report {
		return &Report{Scenario: "hook: " + name, File: suiteFile, RunID: suite.id, Passed: true, DryRun: r.Opt.DryRun}
	}
	beforeOK := true
	if len(hooks["before_all"]) > 0 || len(hooks["after_all"]) > 0 {
		if suite, err = r.newRun(&Scenario{}, NewRunID(), time.Now()); err != nil {
			return nil, err
		}
	}
	if steps := hooks["before_all"]; len(steps) > 0 {
		start := time.Now()
		started("hook: before_all", suiteFile)
		rep := suiteRep("before_all")
		rep.Passed = suite.runSteps(ctx, steps, "", &rep.Steps)
		rep.Duration = time.Since(start).Round(time.Millisecond).String()
		beforeOK = rep.Passed
		r.suiteSaved = map[string]any{}
		for k, v := range suite.saved {
			r.suiteSaved[k] = v
		}
		done(rep)
	}
	for i, sc := range scs {
		if !beforeOK || ctx.Err() != nil {
			why := "skipped: the before_all hook failed"
			if ctx.Err() != nil {
				why = "skipped: interrupted"
			}
			done(&Report{Scenario: sc.Name, File: sc.Path, Skipped: true, Steps: []StepResult{{Name: why, Skipped: true}}, Duration: "0s"})
			continue
		}
		started(sc.Name, sc.Path)
		saved := r.Opt.RunID
		if saved != "" && len(scs) > 1 { // one --run-id for many runs: keep each run's data apart
			r.Opt.RunID = fmt.Sprintf("%s-%d", saved, i+1)
		}
		rep, err := r.Run(ctx, sc)
		r.Opt.RunID = saved
		if err != nil {
			return reports, fmt.Errorf("%s: %w", sc.Path, err)
		}
		done(rep)
	}
	if suite != nil {
		start := time.Now()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if len(hooks["after_all"]) > 0 || len(suite.ledger.items) > 0 {
			rep := suiteRep("after_all")
			started("hook: after_all", suiteFile)
			rep.Passed = suite.runAll(cctx, hooks["after_all"], "", &rep.Steps)
			suite.undoLedger(cctx, &rep.Cleanup) // what before_all created
			rep.Duration = time.Since(start).Round(time.Millisecond).String()
			done(rep)
		}
	}
	return reports, nil
}
