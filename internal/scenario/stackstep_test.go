package scenario

import (
	"context"
	"reflect"
	"testing"

	"github.com/ddlg3003/d-mock/internal/stack"
)

// fakeStack records control calls; two "workers" share one port, so only one may run.
type fakeStack struct {
	up    map[string]bool
	calls []string
}

func (f *fakeStack) Running(name string) (bool, error) { return f.up[name], nil }
func (f *fakeStack) Control(_ context.Context, action, name string) (stack.Item, error) {
	f.calls = append(f.calls, action+" "+name)
	f.up[name] = action == "start" || action == "restart"
	return stack.Item{Name: name, State: action}, nil
}

func runWith(f *fakeStack) *run {
	return &run{r: &Runner{control: f}, id: "dm1", data: map[string]any{}, saved: map[string]any{}, ledger: &ledger{}, logOffs: map[string]int64{}}
}

func TestStackRestoresStateBeforeTheScenario(t *testing.T) {
	// deploy swap: neither worker runs before; db runs before
	f := &fakeStack{up: map[string]bool{"db": true}}
	ru := runWith(f)
	for _, s := range []map[string]any{
		{"start": "worker-old"},
		{"stop": "worker-old"},
		{"start": "worker-new"},
		{"stop": "db"},
		{"start": "db"},
		{"stop": "db"}, // the state recorded is the first one seen (up), not this one
	} {
		if _, err := ru.execStack(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	f.calls = nil
	res := ru.ledger.undo(context.Background(), ru)
	// stop what the scenario started (it was down) before starting what it took down
	if want := []string{"stop worker-new", "start db"}; !reflect.DeepEqual(f.calls, want) {
		t.Errorf("cleanup calls = %v, want %v", f.calls, want)
	}
	if f.up["worker-old"] || f.up["worker-new"] || !f.up["db"] {
		t.Errorf("after cleanup: %v, want only db up", f.up)
	}
	var names []string
	for _, r := range res {
		names = append(names, r.Name)
		if !r.Passed {
			t.Errorf("%s failed: %s", r.Name, r.Error)
		}
	}
	want := []string{
		"auto: stop worker-old (it was not running before the scenario)",
		"auto: stop worker-new (it was not running before the scenario)",
		"auto: make sure db is running again",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("cleanup steps = %v\nwant %v", names, want)
	}
}
