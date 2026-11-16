package scenario

import (
	"context"
	"fmt"

	"github.com/ddlg3003/d-mock/internal/stack"
	"github.com/ddlg3003/d-mock/internal/tmpl"
)

// stack: {stop|start|restart|kill: NAME} — take a dep (container mode), mock or service down
// or bring it up, to test outages, crash recovery and deploy swaps. The first time a scenario
// touches a part, d-mock records whether it was running; after the run it puts it back that
// way: started again if it was up, stopped if the scenario started it. This happens before the
// rest of the cleanup, which may need it.
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
	ctl := ru.r.stackCtl()
	if !ru.ledger.touched(name) {
		up, err := ctl.Running(name)
		if err != nil {
			return nil, err
		}
		state := "down"
		if up {
			state = "up"
		}
		ru.ledger.add(undo{Kind: "stack", Mock: name, Value: state})
	}
	it, err := ctl.Control(ctx, action, name)
	out := map[string]any{"action": action, "name": name, "kind": it.Kind, "state": it.State}
	if it.Message != "" {
		out["message"] = it.Message
	}
	return out, err
}

// restoreStack puts a part of the stack back to how it was before the scenario touched it.
func (ru *run) restoreStack(ctx context.Context, u undo) error {
	ctl := ru.r.stackCtl()
	up, err := ctl.Running(u.Mock)
	if err != nil {
		return err
	}
	switch {
	case u.Value == "up" && !up:
		_, err = ctl.Control(ctx, "start", u.Mock)
	case u.Value == "down" && up:
		_, err = ctl.Control(ctx, "stop", u.Mock)
	}
	return err
}

func stackUndoName(u undo) string {
	if u.Value == "down" {
		return "auto: stop " + u.Mock + " (it was not running before the scenario)"
	}
	return "auto: make sure " + u.Mock + " is running again"
}
