package mock

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testSpec = `
routes:
  - id: status
    method: GET
    path: orders/:id
    key: "urlParam 'id'"
    state: {status: PENDING}
    headers: {Content-Type: application/json}
    body: '{"id":"${key}","status":"${state.status}"}'
    variants:
      - when: {status: GONE}
        status: 404
        body: '{"error":"not found"}'
    faults:
      http500: {status: 500, body: '{"error":"boom"}'}
  - id: static
    method: GET
    path: blob
    body_file: blob.txt
    encode: ["tr", "a-z", "A-Z"]
`

func writeSpec(t *testing.T) (*Spec, string) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(testSpec), 0o644)
	os.WriteFile(filepath.Join(dir, "blob.txt"), []byte("abc={{x}}"), 0o644)
	s, err := LoadSpec(filepath.Join(dir, "spec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestCompileShape(t *testing.T) {
	s, dir := writeSpec(t)
	b, err := Compile("partner", 3999, s, dir)
	if err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	routes := env["routes"].([]any)
	status := routes[0].(map[string]any)["responses"].([]any)
	if len(status) != 3 { // fault, variant, default
		t.Fatalf("want 3 responses, got %d", len(status))
	}
	def := status[2].(map[string]any)
	if !strings.Contains(def["body"].(string), "{{{getGlobalVar (concat 'partner|status|' (urlParam 'id'))}}}") {
		t.Fatalf("default body not raw state lookup: %s", def["body"])
	}
	blob := routes[1].(map[string]any)["responses"].([]any)[0].(map[string]any)
	if blob["body"] != "ABC={{X}}" || blob["disableTemplating"] != true {
		t.Fatalf("encoded body: %v templating off: %v", blob["body"], blob["disableTemplating"])
	}
}

func TestUndeclaredStateField(t *testing.T) {
	s := &Spec{Routes: []Route{{ID: "r", Method: "GET", Path: "x", Body: "${state.nope}"}}}
	if _, err := Compile("m", 1, s, "."); err == nil {
		t.Fatal("expected error for state field without default")
	}
}

// TestMockoonLive runs the compiled environment in a real Mockoon CLI (skipped when absent).
func TestMockoonLive(t *testing.T) {
	bin, err := exec.LookPath("mockoon-cli")
	if err != nil {
		t.Skip("mockoon-cli not installed")
	}
	s, dir := writeSpec(t)
	port := freePort(t)
	b, _ := Compile("partner", port, s, dir)
	envPath := filepath.Join(dir, "env.json")
	os.WriteFile(envPath, b, 0o644)
	cmd := exec.Command(bin, "start", "-d", envPath, "--admin-api-token", "tok", "-X")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := fmt.Sprintf("http://localhost:%d", port)
	waitHTTP(t, base+"/blob")

	a := NewAdmin("partner", port, "tok")
	get := func(p string) (int, string) {
		r, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if code, body := get("/orders/o1"); code != 200 || body != `{"id":"o1","status":"PENDING"}` {
		t.Fatalf("default: %d %s", code, body)
	}
	if err := a.SetState("o1", map[string]string{"status": "OK&<x>"}); err != nil {
		t.Fatal(err)
	}
	if _, body := get("/orders/o1"); body != `{"id":"o1","status":"OK&<x>"}` {
		t.Fatalf("state not raw: %s", body)
	}
	a.SetFault("o1", "http500", 2)
	for i := 0; i < 2; i++ {
		if code, _ := get("/orders/o1"); code != 500 {
			t.Fatalf("fault hit %d: %d", i, code)
		}
	}
	if code, _ := get("/orders/o1"); code != 200 {
		t.Fatalf("fault should be used up, got %d", code)
	}
	a.SetState("o2", map[string]string{"status": "GONE"})
	if code, _ := get("/orders/o2"); code != 404 {
		t.Fatalf("variant: %d", code)
	}
	if _, body := get("/blob"); body != "ABC={{X}}" {
		t.Fatalf("static body: %s", body)
	}
	calls, err := a.Calls(s)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(Filter(calls, "status", "o1")); n != 5 {
		t.Fatalf("want 5 calls for o1, got %d", n)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, body := get("/orders/o1"); !strings.Contains(body, "PENDING") {
		t.Fatalf("reset: %s", body)
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitHTTP(t *testing.T, u string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := http.Get(u); err == nil {
			r.Body.Close()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s never came up", u)
}
