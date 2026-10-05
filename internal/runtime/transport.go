package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/yohn-jp/matagi/internal/jinushi"
	"github.com/yohn-jp/matagi/internal/ssh"
	"github.com/yohn-jp/matagi/internal/tunnel"
)

type remoteRunner interface {
	Run(context.Context, string, []string, time.Duration) (ssh.Result, error)
}
type executor struct {
	client remoteRunner
	host   string
}

func (e executor) Run(ctx context.Context, argv []string, timeout time.Duration) (jinushi.CommandResult, error) {
	r, err := e.client.Run(ctx, e.host, argv, timeout)
	result := jinushi.CommandResult{Stdout: r.Stdout, Stderr: r.Stderr, ExitCode: r.ExitCode}
	if err == nil {
		return result, nil
	}
	var se *ssh.Error
	if errors.As(err, &se) && se.Kind == ssh.FailureRemoteCommand {
		return result, nil
	}
	kind := jinushi.ExecutionTransportFailure
	if errors.As(err, &se) {
		switch se.Kind {
		case ssh.FailureTimeout:
			kind = jinushi.ExecutionTimeout
		case ssh.FailureCanceled:
			kind = jinushi.ExecutionCanceled
		}
	}
	return result, &jinushi.ExecutionError{Kind: kind, Err: err}
}

type commandProcess interface {
	Start() error
	Wait() error
	Kill() error
}
type osCommand struct{ cmd *exec.Cmd }

func (c *osCommand) Start() error { return c.cmd.Start() }
func (c *osCommand) Wait() error  { return c.cmd.Wait() }
func (c *osCommand) Kill() error  { return c.cmd.Process.Kill() }

type Launcher struct {
	executable string
	start      func(string, []string) commandProcess
}

func NewLauncher() (*Launcher, error) {
	path, err := ssh.ResolveExecutable()
	if err != nil {
		return nil, err
	}
	return &Launcher{executable: path, start: func(path string, args []string) commandProcess { return &osCommand{cmd: exec.Command(path, args...)} }}, nil
}
func (l *Launcher) Start(ctx context.Context, s tunnel.ForwardSpec) (tunnel.Process, error) {
	if l == nil || l.executable == "" || l.start == nil {
		return nil, errors.New("SSH launcher unavailable")
	}
	if s.SSHHost == "" || s.SSHHost[0] == '-' || s.LocalAddress != "127.0.0.1" || s.RemoteAddress != "127.0.0.1" || s.LocalPort == 0 || s.RemotePort == 0 {
		return nil, errors.New("invalid forwarding specification")
	}
	args := []string{"-N", "-T", "-o", "ExitOnForwardFailure=yes", "-L", fmt.Sprintf("%s:%d:%s:%d", s.LocalAddress, s.LocalPort, s.RemoteAddress, s.RemotePort), s.SSHHost}
	cmd := l.start(l.executable, args)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &forwardProcess{cmd: cmd, done: make(chan struct{})}
	go func() { p.err = cmd.Wait(); close(p.done) }()
	address := net.JoinHostPort(s.LocalAddress, fmt.Sprint(s.LocalPort))
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return p, nil
		}
		select {
		case <-readyCtx.Done():
			_ = p.Stop()
			<-p.done
			return nil, readyCtx.Err()
		case <-p.done:
			return nil, errors.New("SSH forwarding process exited before becoming ready")
		case <-ticker.C:
		}
	}
}

type forwardProcess struct {
	cmd     commandProcess
	done    chan struct{}
	err     error
	once    sync.Once
	stopErr error
}

func (p *forwardProcess) Wait() error { <-p.done; return p.err }
func (p *forwardProcess) Stop() error {
	p.once.Do(func() {
		select {
		case <-p.done:
		default:
			p.stopErr = p.cmd.Kill()
		}
	})
	return p.stopErr
}

var _ tunnel.Launcher = (*Launcher)(nil)
var _ jinushi.Executor = executor{}
