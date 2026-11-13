package scenario

import (
	"encoding/json"
	"fmt"
)

// service: {name}: the service's state; for container services also OOM kill, exit code
// and a one-shot resource sample.
// result: {state, oom_killed, exit_code, cpu_percent, mem_bytes, mem_limit_bytes, mem_percent}
func (ru *run) execService(a map[string]any) (map[string]any, error) {
	it, stats, err := ru.r.Stack.ServiceStatus(str(a, "name"))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"state": it.State, "oom_killed": it.OOMKilled}
	if it.ExitCode != nil {
		out["exit_code"] = *it.ExitCode
	}
	if it.PID > 0 {
		out["pid"] = it.PID
	}
	if stats != nil {
		b, _ := json.Marshal(stats)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for k, v := range m {
			out[k] = v
		}
	}
	if it.Container == "" && it.State == "" {
		return nil, fmt.Errorf("service %s: unknown state", str(a, "name"))
	}
	return out, nil
}
