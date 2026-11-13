package stack

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ddlg3003/d-mock/internal/config"
)

// startProcess runs cmd detached in its own process group, output appended to logPath.
func startProcess(dir string, cmd []string, env []string, logPath string) (int, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	fmt.Fprintf(f, "\n=== d-mock start %s: %s\n", time.Now().Format(time.RFC3339), strings.Join(cmd, " "))
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	c.Stdout, c.Stderr = f, f
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		return 0, err
	}
	pid := c.Process.Pid
	go c.Wait() // reap if it dies while we are still running
	return pid, nil
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// procStart is the process start time as ps prints it ("" when unknown or gone).
func procStart(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := exec.Command("ps", "-o", "lstart=", "-p", fmt.Sprint(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ours reports whether pid is still the process d-mock started: alive and with the start
// time recorded then. After a reboot or a long time the PID can belong to something else,
// and signalling its process group would hit an unrelated program. An empty start (state
// written by an older d-mock) falls back to the bare liveness check.
func ours(pid int, start string) bool {
	if !alive(pid) {
		return false
	}
	return start == "" || procStart(pid) == start
}

// stopProcess sends SIGTERM to the process group, then SIGKILL after grace. It does nothing
// when pid is no longer the process that was started at start.
func stopProcess(pid int, start string, grace time.Duration) {
	if !ours(pid, start) {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// waitReady polls the probe until it passes, the timeout ends, or isDead reports the
// process gone (then it fails fast instead of waiting out the timeout).
func waitReady(ctx context.Context, r *config.Ready, logPath string, isDead func() bool) error {
	if r == nil {
		return nil
	}
	timeout := r.Timeout.Or(60 * time.Second)
	deadline := time.Now().Add(timeout)
	var logRe *regexp.Regexp
	if r.Log != "" {
		var err error
		if logRe, err = regexp.Compile(r.Log); err != nil {
			return fmt.Errorf("ready.log: %w", err)
		}
	}
	client := &http.Client{Timeout: 2 * time.Second}
	dead := func() error {
		if isDead != nil && isDead() {
			return fmt.Errorf("process exited before it was ready; last log lines:\n%s", tail(logPath, 15))
		}
		return nil
	}
	for {
		if err := dead(); err != nil {
			return err
		}
		if probe(client, r, logRe, logPath) {
			// a stale process on the same port also answers the probe: make sure ours is
			// still alive a moment later (a port clash makes the new one exit at once)
			time.Sleep(700 * time.Millisecond)
			return dead()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s (%s); last log lines:\n%s", timeout, describeProbe(r), tail(logPath, 15))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func probe(client *http.Client, r *config.Ready, logRe *regexp.Regexp, logPath string) bool {
	switch {
	case r.TCP != "":
		return tcpReady(r.TCP)
	case r.HTTP != "":
		resp, err := client.Get(r.HTTP)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode < 500
	case logRe != nil:
		return logRe.Match(lastStart(logPath))
	}
	return true
}

func describeProbe(r *config.Ready) string {
	switch {
	case r.TCP != "":
		return "tcp " + r.TCP
	case r.HTTP != "":
		return "http " + r.HTTP
	case r.Log != "":
		return "log /" + r.Log + "/"
	}
	return "no probe"
}

// lastStart returns the log written since the latest d-mock start marker.
func lastStart(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if i := bytes.LastIndex(b, []byte("\n=== d-mock start ")); i >= 0 {
		return b[i:]
	}
	return b
}

// tail returns the last n lines of a file.
func tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	return strings.Join(lines, "\n")
}

// runLogged runs cmd to completion, appending its output to logPath.
func runLogged(dir string, cmd []string, logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(f, "\n=== d-mock pre-build: %s\n", strings.Join(cmd, " "))
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Dir = dir
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	runErr := c.Run()
	f.Write(out.Bytes())
	if runErr != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(cmd, " "), runErr, strings.TrimSpace(out.String()))
	}
	return nil
}

// tcpReady connects and then reads briefly. Container port forwarders (podman's gvproxy,
// docker-proxy) accept the connection before the app listens and then close it, so a bare
// connect is not enough: an immediate EOF means "not listening yet"; a server greeting or a
// read timeout (the server waits for the client) means ready.
func tcpReady(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 1)
	_, err = c.Read(buf)
	if err == nil {
		return true // the server spoke first (MySQL, SMTP, …)
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
