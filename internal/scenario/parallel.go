package scenario

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ddlg3003/d-mock/internal/tmpl"
)

// execParallel runs a parallel step's branches at the same moment: every branch waits on one
// start barrier, then sleeps its own `after`, then runs like a normal step (with its own
// expect, retry and save). The result lets a test check the outcome without knowing which
// branch won:
//
//	results.<i>.<path>   branch i's result (declared order), plus passed / error / failures
//	count.<field>.<val>  how many branches returned that value for a top-level field,
//	                     e.g. count.status.202: 1 and count.status.409: 1
//	passed, failed       how many branches passed their own checks
//	finished             branch indexes in the order they completed
//
// A branch that fails its own checks fails the step.
func (ru *run) execParallel(ctx context.Context, step Step) (map[string]any, error) {
	branches := step.Parallel.Branches()
	results := make([]StepResult, len(branches))
	forks := make([]*run, len(branches))
	var (
		mu       sync.Mutex
		finished []any
		wg       sync.WaitGroup
	)
	barrier := make(chan struct{})
	for i, b := range branches {
		forks[i] = ru.fork()
		wg.Add(1)
		go func(i int, b Step) {
			defer wg.Done()
			<-barrier
			if b.After > 0 {
				select {
				case <-ctx.Done():
				case <-time.After(b.After):
				}
			}
			results[i] = forks[i].runStep(ctx, b, i)
			mu.Lock()
			finished = append(finished, i)
			mu.Unlock()
		}(i, b)
	}
	close(barrier)
	wg.Wait()

	out := map[string]any{"finished": finished}
	count := map[string]any{}
	var list []any
	passed := 0
	var failures []string
	for i, res := range results {
		entry := map[string]any{}
		for k, v := range res.Result {
			entry[k] = v
		}
		entry["name"], entry["passed"] = res.Name, res.Passed
		if res.Error != "" {
			entry["error"] = res.Error
		}
		if len(res.Failures) > 0 {
			entry["failures"] = res.Failures
		}
		list = append(list, entry)
		if res.Passed {
			passed++
		} else {
			why := res.Error
			if why == "" {
				why = strings.Join(res.Failures, "; ")
			}
			failures = append(failures, fmt.Sprintf("branch %d (%s): %s", i, res.Name, why))
		}
		for k, v := range entry {
			switch v.(type) {
			case map[string]any, []any, []string:
				continue // only scalar fields are counted
			}
			m, _ := count[k].(map[string]any)
			if m == nil {
				m = map[string]any{}
				count[k] = m
			}
			key := tmpl.Stringify(v)
			n, _ := m[key].(int)
			m[key] = n + 1
		}
		// what a branch saved becomes visible to the following steps
		for name := range branches[i].Save {
			if v, ok := forks[i].saved[name]; ok {
				ru.saved[name] = v
			}
		}
	}
	out["results"], out["count"], out["passed"], out["failed"] = list, count, passed, len(branches)-passed
	if len(failures) > 0 {
		return out, fmt.Errorf("%d of %d branches failed: %s", len(failures), len(branches), strings.Join(failures, " | "))
	}
	return out, nil
}

// fork gives a parallel branch its own view of the run: the same run id, vars and cleanup
// ledger (which is safe for concurrent use), but its own saved values and `last`, so
// branches don't race on them.
func (ru *run) fork() *run {
	data := make(map[string]any, len(ru.data))
	for k, v := range ru.data {
		data[k] = v
	}
	saved := make(map[string]any, len(ru.saved))
	for k, v := range ru.saved {
		saved[k] = v
	}
	data["saved"] = saved
	return &run{r: ru.r, id: ru.id, data: data, saved: saved, ledger: ru.ledger, logOffs: ru.logOffs, start: ru.start}
}
