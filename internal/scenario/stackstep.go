package scenario

import (
	"context"
	"fmt"

	"github.com/ddlg3003/d-mock/internal/stack"
	"github.com/ddlg3003/d-mock/internal/tmpl"
)

// stack: {stop|start|restart|kill: NAME} — take a dep (container mode), mock or service down
// or bring it back, to test outages and crash recovery. Whatever a scenario takes down is
// started again after the run (before the rest of the cleanup, which may need it).
func (ru *run) execStack(ctx context.Context, a map[string]any) (map[string]any, error) {
	var action, name string
	for _, act := range stack.ControlActions {
		if v, ok := a[act]; ok {
			if action != "" {
				return nil, fmt.Errorf("stack: one action per step, got %s and %s", action, act)
			}
			action, name = act, tmpl.Stringify(v)
		}
	}
	if action == "" {
		return nil, fmt.Errorf("stack: set one of stop, start, restart, kill (with the dep, mock or service name)")
	}
	if action != "start" {
		ru.ledger.add(undo{Kind: "stack", Mock: name})
	}
	it, err := ru.r.Stack.Control(ctx, action, name)
	out := map[string]any{"action": action, "name": name, "kind": it.Kind, "state": it.State}
	if it.Message != "" {
		out["message"] = it.Message
	}
	return out, err
}

// restoreStack starts a part of the stack again if a scenario left it down.
func (ru *run) restoreStack(ctx context.Context, name string) error {
	up, err := ru.r.Stack.Running(name)
	if err != nil || up {
		return err
	}
	_, err = ru.r.Stack.Control(ctx, "start", name)
	return err
}
