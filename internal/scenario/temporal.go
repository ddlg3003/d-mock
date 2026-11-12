package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/ddlg3003/d-mock/internal/tmpl"
)

// temporal steps shell out to the `temporal` CLI, so no SDK or proto version is pinned here.
//
//	temporal: {dep, action: start,     type, task_queue, workflow_id, input: [args]}
//	temporal: {dep, action: signal,    workflow_id, signal, input: [args]}
//	temporal: {dep, action: describe,  workflow_id}   -> {status, json}
//	temporal: {dep, action: history,   workflow_id}   -> {events: [EventName...], json}
//	temporal: {dep, action: terminate, workflow_id, reason}
//
// dep vars: address, namespace, cli_args (extra flags, e.g. TLS).
// Workflows started by a scenario are terminated during cleanup if still running.
func (ru *run) execTemporal(ctx context.Context, a map[string]any) (map[string]any, error) {
	vars, _, err := ru.dep(str(a, "dep"), "temporal")
	if err != nil {
		return nil, err
	}
	conn := temporalConn(vars)
	wid := str(a, "workflow_id")
	switch str(a, "action") {
	case "start":
		args := []string{"workflow", "start", "--type", str(a, "type"), "--task-queue", str(a, "task_queue"), "--workflow-id", wid}
		in, err := inputs(a["input"])
		if err != nil {
			return nil, err
		}
		args = append(args, in...)
		out, err := temporalCLI(ctx, conn, args...)
		if err != nil {
			return nil, err
		}
		ru.ledger.add(undo{Kind: "temporal", Dep: str(a, "dep"), WorkflowID: wid})
		return withJSON(map[string]any{"workflow_id": wid}, out), nil
	case "signal":
		args := []string{"workflow", "signal", "--workflow-id", wid, "--name", str(a, "signal")}
		in, err := inputs(a["input"])
		if err != nil {
			return nil, err
		}
		args = append(args, in...)
		out, err := temporalCLI(ctx, conn, args...)
		if err != nil {
			return nil, err
		}
		return withJSON(map[string]any{"workflow_id": wid}, out), nil
	case "describe":
		out, err := temporalCLI(ctx, conn, "workflow", "describe", "--workflow-id", wid)
		if err != nil {
			return nil, err
		}
		res := withJSON(map[string]any{"workflow_id": wid}, out)
		if s, ok := tmpl.Lookup(res, "json.workflowExecutionInfo.status"); ok {
			res["status"] = strings.TrimPrefix(tmpl.Stringify(s), "WORKFLOW_EXECUTION_STATUS_")
		}
		return res, nil
	case "history":
		out, err := temporalCLI(ctx, conn, "workflow", "show", "--workflow-id", wid)
		if err != nil {
			return nil, err
		}
		res := withJSON(map[string]any{"workflow_id": wid}, out)
		events := []any{}
		if list, ok := tmpl.Lookup(res, "json.events"); ok {
			for _, e := range list.([]any) {
				if m, ok := e.(map[string]any); ok {
					events = append(events, EventName(tmpl.Stringify(m["eventType"])))
				}
			}
		}
		res["events"] = events
		return res, nil
	case "terminate":
		reason := str(a, "reason")
		if reason == "" {
			reason = "d-mock"
		}
		_, err := temporalCLI(ctx, conn, "workflow", "terminate", "--workflow-id", wid, "--reason", reason)
		return map[string]any{"workflow_id": wid}, err
	}
	return nil, fmt.Errorf("temporal: unknown action %q (start, signal, describe, history, terminate)", str(a, "action"))
}

func temporalConn(vars map[string]any) []string {
	var c []string
	if v := tmpl.Stringify(vars["address"]); v != "" {
		c = append(c, "--address", v)
	}
	if v := tmpl.Stringify(vars["namespace"]); v != "" {
		c = append(c, "--namespace", v)
	}
	return append(c, strList(vars["cli_args"])...)
}

// inputs encodes each workflow/signal argument as one --input JSON value.
func inputs(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		list = []any{v}
	}
	var args []string
	for _, e := range list {
		b, err := json.Marshal(e)
		if err != nil {
			return nil, err
		}
		args = append(args, "--input", string(b))
	}
	return args, nil
}

func temporalCLI(ctx context.Context, conn []string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	full := append(append(args, conn...), "--output", "json")
	cmd := exec.CommandContext(cctx, "temporal", full...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		var nf *exec.Error
		if errors.As(err, &nf) {
			return "", fmt.Errorf("temporal CLI not found: install it (brew install temporal)")
		}
		return "", fmt.Errorf("temporal %s: %v: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(se.String()))
	}
	return so.String(), nil
}

func withJSON(res map[string]any, out string) map[string]any {
	if j, ok := parseJSON(out); ok {
		res["json"] = j
	} else if s := strings.TrimSpace(out); s != "" {
		res["output"] = s
	}
	return res
}

// EventName turns EVENT_TYPE_TIMER_CANCELED or TimerCanceled into TimerCanceled.
func EventName(t string) string {
	t = strings.TrimPrefix(t, "EVENT_TYPE_")
	if !strings.Contains(t, "_") && strings.ToUpper(t) != t {
		return t
	}
	var b strings.Builder
	for _, part := range strings.Split(strings.ToLower(t), "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}
