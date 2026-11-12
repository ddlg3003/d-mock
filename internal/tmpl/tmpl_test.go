package tmpl

import "testing"

func TestExpand(t *testing.T) {
	data := map[string]any{
		"run":  "dm1a2b",
		"vars": map[string]any{"id": "x-1", "n": 3},
		"list": []any{"a", "b"},
	}
	got, err := Expand("order-${run}-${vars.id}-${list.1}", data)
	if err != nil || got != "order-dm1a2b-x-1-b" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := Expand("${missing}", data); err == nil {
		t.Fatal("expected error for unresolved reference")
	}
}

func TestEscape(t *testing.T) {
	got, err := Expand("a=$${HOME} b=${x}", map[string]any{"x": 1})
	if err != nil || got != "a=${HOME} b=1" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestExpandAnyKeepsType(t *testing.T) {
	data := map[string]any{"vars": map[string]any{"n": 3}}
	v, err := ExpandAny(map[string]any{"n": "${vars.n}", "s": "n=${vars.n}"}, data)
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if m["n"] != 3 || m["s"] != "n=3" {
		t.Fatalf("got %#v", m)
	}
}
