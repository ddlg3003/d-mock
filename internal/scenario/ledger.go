package scenario

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
)

// undo is something a scenario created that cleanup removes again.
type undo struct {
	Kind       string // sql | temporal | mock-state | mock-fault | stack (Mock = the name)
	Dep        string
	Table      string
	Key        string
	Value      string
	WorkflowID string
	Mock       string
	Fields     []string
}

type ledger struct {
	mu    sync.Mutex
	items []undo
}

func (l *ledger) add(u undo) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, x := range l.items { // dedupe repeated mock sets on the same key
		if x.Kind == u.Kind && x.Mock == u.Mock && x.Key == u.Key && x.Table == u.Table && x.Value == u.Value && x.WorkflowID == u.WorkflowID {
			if u.Kind == "mock-state" {
				l.items[i].Fields = append(l.items[i].Fields, u.Fields...)
			}
			return
		}
	}
	l.items = append(l.items, u)
}

// undo runs in reverse order; every item is attempted even if an earlier one fails. Parts of
// the stack a scenario took down come back first: deleting rows needs the database up.
func (l *ledger) undo(ctx context.Context, ru *run) []StepResult {
	var out []StepResult
	order := make([]undo, 0, len(l.items))
	for _, u := range l.items {
		if u.Kind == "stack" {
			order = append(order, u)
		}
	}
	for i := len(l.items) - 1; i >= 0; i-- {
		if l.items[i].Kind != "stack" {
			order = append(order, l.items[i])
		}
	}
	for _, u := range order {
		res := StepResult{Type: "cleanup", Attempts: 1, Passed: true}
		var err error
		switch u.Kind {
		case "sql":
			res.Name = fmt.Sprintf("auto: delete %s.%s=%s", u.Table, u.Key, u.Value)
			err = ru.deleteRow(ctx, u)
		case "temporal":
			res.Name = "auto: terminate workflow " + u.WorkflowID
			err = ru.terminateIfRunning(ctx, u)
		case "mock-state":
			res.Name = fmt.Sprintf("auto: clear mock %s state for %s", u.Mock, u.Key)
			err = ru.clearMockState(u)
		case "stack":
			res.Name = "auto: make sure " + u.Mock + " is running again"
			err = ru.restoreStack(ctx, u.Mock)
		case "mock-fault":
			res.Name = fmt.Sprintf("auto: clear mock %s fault for %s", u.Mock, u.Key)
			if admin, _, aerr := ru.r.Stack.MockAdmin(u.Mock); aerr != nil {
				err = aerr
			} else {
				err = admin.SetFault(u.Key, "", 0)
			}
		}
		if err != nil {
			res.Passed = false
			res.Error = err.Error()
		}
		out = append(out, res)
	}
	return out
}

func (ru *run) deleteRow(ctx context.Context, u undo) error {
	if !strings.Contains(u.Value, ru.id) { // never delete a row this run did not create
		return fmt.Errorf("refusing to delete %s=%s: no run id", u.Key, u.Value)
	}
	vars, _, err := ru.dep(u.Dep, "mysql")
	if err != nil {
		return err
	}
	db, err := sql.Open("mysql", fmt.Sprint(vars["dsn"]))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, fmt.Sprintf("DELETE FROM `%s` WHERE `%s` = ?", u.Table, u.Key), u.Value)
	return err
}

func (ru *run) terminateIfRunning(ctx context.Context, u undo) error {
	res, err := ru.execTemporal(ctx, map[string]any{"dep": u.Dep, "action": "describe", "workflow_id": u.WorkflowID})
	if err != nil {
		return err
	}
	if res["status"] != "RUNNING" {
		return nil
	}
	_, err = ru.execTemporal(ctx, map[string]any{"dep": u.Dep, "action": "terminate", "workflow_id": u.WorkflowID, "reason": "d-mock cleanup " + ru.id})
	return err
}

func (ru *run) clearMockState(u undo) error {
	admin, _, err := ru.r.Stack.MockAdmin(u.Mock)
	if err != nil {
		return err
	}
	fields := map[string]string{}
	for _, f := range u.Fields {
		fields[f] = "" // empty = back to the route's default
	}
	return admin.SetState(u.Key, fields)
}
