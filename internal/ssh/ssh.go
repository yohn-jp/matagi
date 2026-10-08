// Package ssh runs bounded remote commands through the system OpenSSH client.
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// FailureKind classifies a failure to resolve or use the SSH transport, or a
// non-zero status returned by the remote command.
type FailureKind string

const (
	FailureExecutableLookup FailureKind = "executable_lookup"
	FailureInvalidRequest   FailureKind = "invalid_request"
	FailureProcess          FailureKind = "process"
	FailureTransport        FailureKind = "transport"
	FailureTimeout          FailureKind = "timeout"
	FailureCanceled         FailureKind = "canceled"
	FailureRemoteCommand    FailureKind = "remote_command"
)

// Error describes an SSH execution failure. A remote command's exit code is
// available in both Error and Result; transport failures do not represent a
// remote command exit status.
type Error struct {
	Kind     FailureKind
	ExitCode int
	Err      error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("ssh %s failure: %v", e.Kind, e.Err)
	}
	if e.Kind == FailureRemoteCommand {
		return fmt.Sprintf("remote command exited with status %d", e.ExitCode)
	}
	return fmt.Sprintf("ssh %s failure", e.Kind)
}

func (e *Error) Unwrap() error { return e.Err }

// Result contains output and the remote command's exit status. ExitCode is -1
// when OpenSSH did not return a remote command status.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Client invokes the system OpenSSH executable resolved from PATH.
type Client struct {
	executable string
	run        processRunner
}

type processResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	err      error
}

type processRunner func(context.Context, string, []string) processResult

// ResolveExecutable returns the system ssh executable found through PATH.
func ResolveExecutable() (string, error) {
	return resolveExecutable(exec.LookPath)
}

func resolveExecutable(lookPath func(string) (string, error)) (string, error) {
	path, err := lookPath("ssh")
	if err != nil {
		return "", &Error{Kind: FailureExecutableLookup, Err: err, ExitCode: -1}
	}
	if path == "" {
		return "", &Error{Kind: FailureExecutableLookup, Err: errors.New("ssh was not found on PATH"), ExitCode: -1}
	}
	return path, nil
}

// New resolves the system OpenSSH executable from PATH.
func New() (*Client, error) {
	path, err := ResolveExecutable()
	if err != nil {
		return nil, err
	}
	return newClient(path, runProcess), nil
}

func newClient(executable string, run processRunner) *Client {
	return &Client{executable: executable, run: run}
}

// Run executes command against the registered OpenSSH host target. The remote
// arguments are shell-quoted into one command string because OpenSSH sends the
// command through the remote user's shell. A positive timeout is required so
// the SSH process always has a bounded lifetime.
func (c *Client) Run(ctx context.Context, target string, command []string, timeout time.Duration) (Result, error) {
	result := Result{ExitCode: -1}
	if ctx == nil {
		return result, &Error{Kind: FailureInvalidRequest, Err: errors.New("context is nil"), ExitCode: -1}
	}
	if c == nil || c.executable == "" || c.run == nil {
		return result, &Error{Kind: FailureExecutableLookup, Err: errors.New("client has no SSH executable"), ExitCode: -1}
	}
	if target == "" || target[0] == '-' {
		return result, &Error{Kind: FailureInvalidRequest, Err: errors.New("SSH target must be non-empty and cannot start with a hyphen"), ExitCode: -1}
	}
	if len(command) == 0 || command[0] == "" {
		return result, &Error{Kind: FailureInvalidRequest, Err: errors.New("remote command must be non-empty"), ExitCode: -1}
	}
	if timeout <= 0 {
		return result, &Error{Kind: FailureInvalidRequest, Err: errors.New("timeout must be positive"), ExitCode: -1}
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{target, serializeRemoteCommand(command)}

	process := c.run(runCtx, c.executable, args)
	result.Stdout = append([]byte(nil), process.stdout...)
	result.Stderr = append([]byte(nil), process.stderr...)
	result.ExitCode = process.exitCode
	if process.err == nil {
		return result, nil
	}
	if runCtx.Err() != nil {
		kind := FailureCanceled
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			kind = FailureTimeout
		}
		return result, &Error{Kind: kind, Err: runCtx.Err(), ExitCode: -1}
	}
	if process.exitCode == 255 {
		return result, &Error{Kind: FailureTransport, Err: process.err, ExitCode: 255}
	}
	if process.exitCode >= 0 {
		return result, &Error{Kind: FailureRemoteCommand, Err: process.err, ExitCode: process.exitCode}
	}
	return result, &Error{Kind: FailureProcess, Err: process.err, ExitCode: -1}
}

func serializeRemoteCommand(command []string) string {
	serialized := make([]string, len(command))
	for index, argument := range command {
		serialized[index] = "'" + strings.ReplaceAll(argument, "'", "'\\''") + "'"
	}
	return strings.Join(serialized, " ")
}

func runProcess(ctx context.Context, executable string, args []string) processResult {
	cmd := exec.CommandContext(ctx, executable, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := -1
	if err == nil {
		exitCode = 0
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
	}
	return processResult{
		stdout:   append([]byte(nil), stdout.Bytes()...),
		stderr:   append([]byte(nil), stderr.Bytes()...),
		exitCode: exitCode,
		err:      err,
	}
}
