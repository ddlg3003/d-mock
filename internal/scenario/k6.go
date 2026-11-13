package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ddlg3003/d-mock/internal/config"
	"github.com/ddlg3003/d-mock/internal/tmpl"
)

// k6: {script, vus, duration, iterations, env: {}, args: [], timeout}
// Runs `k6 run` and exposes its end-of-test summary for expect:
//
//	metrics.<name>.<stat>   e.g. metrics.http_req_duration.p95, metrics.http_req_failed.rate,
//	                        metrics.http_reqs.count, metrics.iterations.rate
//	checks.<name>.passes / .fails
//	exit                    99 when a k6 threshold failed
//
// Load against a remote (shared) dep is refused unless run with --allow-remote-load.
func (ru *run) execK6(ctx context.Context, a map[string]any) (map[string]any, error) {
	if !ru.r.Opt.AllowRemoteLoad {
		var remote []string
		for _, n := range config.SortedKeys(ru.r.Stack.Stack.Deps) {
			if ru.r.Stack.Stack.Deps[n].Mode == "remote" {
				remote = append(remote, n)
			}
		}
		if len(remote) > 0 {
			return nil, fmt.Errorf("k6: refusing load while deps %v are remote (shared servers); switch them with --mode or pass --allow-remote-load", remote)
		}
	}
	bin, err := exec.LookPath("k6")
	if err != nil {
		return nil, errors.New("k6 not found: install it (brew install k6)")
	}
	script := str(a, "script")
	if script == "" {
		return nil, errors.New("k6: script is required")
	}
	if !filepath.IsAbs(script) {
		script = ru.r.Stack.Stack.Path(script)
	}
	summary, err := os.CreateTemp("", "dmock-k6-*.json")
	if err != nil {
		return nil, err
	}
	summary.Close()
	defer os.Remove(summary.Name())

	args := []string{"run", "--quiet", "--no-color", "--summary-export", summary.Name()}
	if v := str(a, "vus"); v != "" {
		args = append(args, "--vus", v)
	}
	if v := str(a, "duration"); v != "" {
		args = append(args, "--duration", v)
	}
	if v := str(a, "iterations"); v != "" {
		args = append(args, "--iterations", v)
	}
	if e, ok := a["env"].(map[string]any); ok {
		for _, k := range config.SortedKeys(e) {
			args = append(args, "-e", k+"="+tmpl.Stringify(e[k]))
		}
	}
	args = append(args, strList(a["args"])...)
	args = append(args, script)

	timeout := 30 * time.Minute
	if t := str(a, "timeout"); t != "" {
		if timeout, err = time.ParseDuration(t); err != nil {
			return nil, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	runErr := cmd.Run()
	exit := 0
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		exit = ee.ExitCode()
	} else if runErr != nil {
		return nil, runErr
	}
	b, err := os.ReadFile(summary.Name())
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("k6 exited %d without a summary: %s", exit, lastLines(se.String()+so.String(), 15))
	}
	res, err := parseK6Summary(b)
	if err != nil {
		return nil, err
	}
	res["exit"] = exit
	if exit != 0 {
		res["output"] = lastLines(se.String()+so.String(), 15)
	}
	// how the container services came out of the load: an OOM kill is the usual reason for a
	// sudden wall of failed requests, so put it next to the metrics
	services := map[string]any{}
	for _, n := range config.SortedKeys(ru.r.Stack.Stack.Services) {
		if ru.r.Stack.Stack.Services[n].Container == nil {
			continue
		}
		it, _, err := ru.r.Stack.ServiceStatus(n)
		if err != nil {
			continue
		}
		services[n] = map[string]any{"state": it.State, "oom_killed": it.OOMKilled}
		if it.OOMKilled {
			res["warning"] = fmt.Sprintf("service %s was OOM killed during the load", n)
		}
	}
	res["services"] = services
	return res, nil
}

var pctKey = regexp.MustCompile(`^p\((\d+(?:\.\d+)?)\)$`)

// parseK6Summary turns the --summary-export JSON into expect-friendly paths:
// "p(95)" also becomes "p95", "p(99.9)" becomes "p99_9".
func parseK6Summary(b []byte) (map[string]any, error) {
	var raw struct {
		Metrics   map[string]map[string]any `json:"metrics"`
		RootGroup struct {
			Checks map[string]map[string]any `json:"checks"`
		} `json:"root_group"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("k6 summary: %w", err)
	}
	metrics := map[string]any{}
	for name, stats := range raw.Metrics {
		m := map[string]any{}
		for k, v := range stats {
			if k == "thresholds" {
				continue
			}
			m[k] = v
			if g := pctKey.FindStringSubmatch(k); g != nil {
				m["p"+strings.ReplaceAll(g[1], ".", "_")] = v
			}
		}
		// rate metrics (e.g. http_req_failed) export the rate as "value"
		if v, ok := m["value"]; ok {
			if _, has := m["rate"]; !has {
				m["rate"] = v
			}
		}
		metrics[name] = m
	}
	checks := map[string]any{}
	for name, c := range raw.RootGroup.Checks {
		checks[name] = map[string]any{"passes": c["passes"], "fails": c["fails"]}
	}
	return map[string]any{"metrics": metrics, "checks": checks}, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
