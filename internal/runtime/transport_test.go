package runtime

import (
	"context"
	"net"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/tunnel"
)

type fakeProcess struct {
	done    chan struct{}
	stopped bool
}

func (p *fakeProcess) Start() error { return nil }
func (p *fakeProcess) Wait() error  { <-p.done; return nil }
func (p *fakeProcess) Kill() error  { p.stopped = true; close(p.done); return nil }
func TestLauncherArgvAndCleanup(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	defer listener.Close()
	p := &fakeProcess{done: make(chan struct{})}
	var argv []string
	l := &Launcher{executable: "ssh", start: func(_ string, args []string) commandProcess { argv = args; return p }}
	forward, err := l.Start(context.Background(), tunnel.ForwardSpec{SSHHost: "configured-host", LocalAddress: "127.0.0.1", LocalPort: port, RemoteAddress: "127.0.0.1", RemotePort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"-N", "-T", "-o", "ExitOnForwardFailure=yes", "-L", net.JoinHostPort("127.0.0.1", "0"), "configured-host"}
	expected[5] = "127.0.0.1:" + strconv.Itoa(int(port)) + ":127.0.0.1:8080"
	if !reflect.DeepEqual(argv, expected) {
		t.Fatal(argv)
	}
	if err := forward.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := forward.Wait(); err != nil || !p.stopped {
		t.Fatal(err, p.stopped)
	}
}
func TestLauncherCanceledReaps(t *testing.T) {
	p := &fakeProcess{done: make(chan struct{})}
	l := &Launcher{executable: "ssh", start: func(_ string, _ []string) commandProcess { return p }}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := l.Start(ctx, tunnel.ForwardSpec{SSHHost: "host", LocalAddress: "127.0.0.1", LocalPort: 1, RemoteAddress: "127.0.0.1", RemotePort: 2})
	if err == nil || !p.stopped {
		t.Fatal(err, p.stopped)
	}
}
