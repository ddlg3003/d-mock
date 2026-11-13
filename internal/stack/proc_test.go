package stack

import (
	"net"
	"os"
	"testing"

	"github.com/ddlg3003/d-mock/internal/config"
)

// TestTCPReady: a forwarder that accepts then closes (podman before the app listens) is
// not ready; a server that waits for the client, or greets first, is.
func TestTCPReady(t *testing.T) {
	serve := func(handle func(net.Conn)) string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go handle(c)
			}
		}()
		return l.Addr().String()
	}
	closer := serve(func(c net.Conn) { c.Close() })
	waiter := serve(func(c net.Conn) { buf := make([]byte, 1); c.Read(buf); c.Close() })
	greeter := serve(func(c net.Conn) { c.Write([]byte("220 hi\r\n")); c.Close() })
	if tcpReady(closer) {
		t.Error("accept-then-close counted as ready")
	}
	if !tcpReady(waiter) {
		t.Error("silent server not ready")
	}
	if !tcpReady(greeter) {
		t.Error("greeting server not ready")
	}
}

func TestOursChecksStartTime(t *testing.T) {
	pid := os.Getpid()
	start := procStart(pid)
	if start == "" {
		t.Fatal("no start time for own pid")
	}
	if !ours(pid, start) || !ours(pid, "") {
		t.Error("own process not recognised")
	}
	if ours(pid, "Thu Jan  1 00:00:00 1970") {
		t.Error("reused pid (different start time) treated as ours")
	}
}

func TestStateBelongsToProject(t *testing.T) {
	dir := t.TempDir()
	a := New(&config.Stack{Project: "a", Dir: dir})
	st := &State{Project: "a", Deps: map[string]*DepState{}, Mocks: map[string]*MockState{"m": {Port: 1}}, Services: map[string]*ServiceState{}}
	if err := st.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := a.State(); err != nil {
		t.Fatalf("own state rejected: %v", err)
	}
	b := New(&config.Stack{Project: "b", Dir: dir})
	if _, err := b.State(); err == nil {
		t.Fatal("another project's running state accepted")
	}
	st.Mocks = map[string]*MockState{}
	st.Save(dir)
	if _, err := b.State(); err != nil {
		t.Fatalf("empty state of another project should be free to take over: %v", err)
	}
}
