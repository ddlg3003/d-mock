package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ddlg3003/d-mock/internal/config"
	"github.com/ddlg3003/d-mock/internal/stack"
)

// check is one prerequisite in `dmock doctor`.
type check struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Status  string `json:"status"` // ok | missing | not-running
	Version string `json:"version,omitempty"`
	Needed  string `json:"needed"` // required | optional | needed-by-stack
	Install string `json:"install,omitempty"`
	Message string `json:"message,omitempty"`
}

func runQuiet(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		return "", fmt.Errorf("timed out")
	}
	return strings.TrimSpace(string(out)), err
}

var semverRe = regexp.MustCompile(`v?\d+\.\d+\.\d+`)

// firstLine returns the first version number in a tool's version output (or its first line).
func firstLine(s string) string {
	if v := semverRe.FindString(s); v != "" {
		return strings.TrimPrefix(v, "v")
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// doctor checks every external tool dmock can use. With a stack.yaml it marks what that stack
// needs; scenario files/dirs given as args mark k6/temporal when their steps use them.
func (c *cli) doctor() int {
	var st *config.Stack
	if _, err := os.Stat(c.file); err == nil {
		if s, err := config.Load(c.file, c.modes); err == nil {
			st = s
		}
	}
	usesStep := func(step string) bool {
		for _, f := range expandScenarioArgs(c.args[1:]) {
			if b, err := os.ReadFile(f); err == nil && strings.Contains(string(b), step+":") {
				return true
			}
		}
		return false
	}
	need := func(required bool) string {
		if required {
			return "needed-by-stack"
		}
		return "optional"
	}

	var checks []check
	add := func(ch check) { checks = append(checks, ch) }

	// container engine
	needContainers := false
	runner := "cli"
	needTemporal := usesStep("temporal")
	if st != nil {
		runner = st.Mockoon.Runner
		for _, d := range st.Deps {
			if d.Mode == "container" {
				needContainers = true
			}
			if d.Kind == "temporal" {
				needTemporal = true
			}
		}
		for _, s := range st.Services {
			if s.Container != nil {
				needContainers = true
			}
		}
		if runner == "docker" && len(st.Mocks) > 0 {
			needContainers = true
		}
	}
	cli := stack.ContainerCLI()
	ch := check{Name: cli, Purpose: "container deps, container services, mockoon.runner: docker", Needed: need(needContainers),
		Install: "Docker Desktop (https://docs.docker.com/get-docker/) or Podman (brew install podman && podman machine init && podman machine start)"}
	if _, err := exec.LookPath(cli); err != nil {
		ch.Status = "missing"
	} else if v, err := runQuiet(cli, "version", "--format", "{{.Server.Version}}"); err != nil {
		ch.Status, ch.Message = "not-running", "the engine is installed but not reachable: start Docker Desktop or `podman machine start`"
	} else {
		ch.Status, ch.Version = "ok", v
	}
	add(ch)

	// mockoon
	needMockoon := st == nil || (len(st.Mocks) > 0 && runner == "cli")
	if st == nil {
		needMockoon = true
	}
	ch = check{Name: "mockoon-cli", Purpose: "serves the mocked HTTP APIs", Needed: "required",
		Install: "npm i -g @mockoon/cli@9.10.0   (needs Node.js 18+), or set mockoon.runner: docker"}
	if !needMockoon {
		ch.Needed = "optional"
	}
	if _, err := exec.LookPath("mockoon-cli"); err != nil {
		ch.Status = "missing"
	} else {
		v, _ := runQuiet("mockoon-cli", "--version")
		ch.Status, ch.Version = "ok", firstLine(v)
		if !strings.HasPrefix(firstLine(v), "9.") {
			ch.Message = "d-mock is tested with Mockoon 9.x"
		}
	}
	add(ch)

	// temporal
	ch = check{Name: "temporal", Purpose: "temporal steps (start/signal/describe/history/terminate)", Needed: need(needTemporal),
		Install: "brew install temporal   (or https://docs.temporal.io/cli)"}
	if _, err := exec.LookPath("temporal"); err != nil {
		ch.Status = "missing"
	} else {
		v, _ := runQuiet("temporal", "--version")
		ch.Status, ch.Version = "ok", firstLine(v)
	}
	add(ch)

	// k6
	ch = check{Name: "k6", Purpose: "k6 load-test steps", Needed: need(usesStep("k6")),
		Install: "brew install k6   (or https://grafana.com/docs/k6/latest/set-up/install-k6/)"}
	if _, err := exec.LookPath("k6"); err != nil {
		ch.Status = "missing"
	} else {
		v, _ := runQuiet("k6", "version")
		ch.Status, ch.Version = "ok", firstLine(v)
	}
	add(ch)

	// curl is used by many exec steps but is not required by dmock itself; skip.

	problems := 0
	for _, c := range checks {
		if c.Status != "ok" && c.Needed != "optional" {
			problems++
		}
	}
	if c.json {
		c.out(map[string]any{"ok": problems == 0, "stack": stackName(st, c.file), "checks": checks})
	} else {
		if st != nil {
			fmt.Printf("stack: %s (%s)\n\n", st.Project, c.file)
		} else {
			fmt.Println("no stack.yaml here: showing everything dmock can use")
			fmt.Println()
		}
		for _, ch := range checks {
			mark := "✓"
			switch {
			case ch.Status != "ok" && ch.Needed == "optional":
				mark = "-"
			case ch.Status != "ok":
				mark = "✗"
			}
			ver := ch.Version
			if ch.Status != "ok" {
				ver = ch.Status
			}
			fmt.Printf("%s %-12s %-9s %-18s %s\n", mark, ch.Name, ver, "("+ch.Needed+")", ch.Purpose)
			if ch.Status != "ok" {
				fmt.Printf("    install: %s\n", ch.Install)
			}
			if ch.Message != "" {
				fmt.Printf("    note: %s\n", ch.Message)
			}
		}
		fmt.Println()
		if problems == 0 {
			fmt.Println("all needed tools are present")
		} else {
			fmt.Printf("%d needed tool(s) missing\n", problems)
		}
	}
	if problems > 0 {
		return 1
	}
	return 0
}

func stackName(st *config.Stack, file string) string {
	if st == nil {
		return ""
	}
	abs, _ := filepath.Abs(file)
	return abs
}
