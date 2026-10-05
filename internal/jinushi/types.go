// Package jinushi translates Matagi service lifecycle requests into Jinushi
// physical Run operations.
package jinushi

import (
	"context"
	"time"
)

const ownerCorrelationKey = "owner"

// Executor runs one remote command. A completed remote command returns its
// exit status in CommandResult; err means no trustworthy remote result was
// received. Implementations must preserve argv boundaries and bound execution
// by timeout.
type Executor interface {
	Run(ctx context.Context, argv []string, timeout time.Duration) (CommandResult, error)
}

// CommandResult is the completed remote process result.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// ExecutionErrorKind describes why an Executor could not return a remote
// command result.
type ExecutionErrorKind string

const (
	ExecutionTransportFailure ExecutionErrorKind = "transport"
	ExecutionTimeout          ExecutionErrorKind = "timeout"
	ExecutionCanceled         ExecutionErrorKind = "canceled"
)

// ExecutionError lets an Executor preserve transport, timeout, and
// cancellation failures without coupling this package to an SSH client.
type ExecutionError struct {
	Kind ExecutionErrorKind
	Err  error
}

func (e *ExecutionError) Error() string {
	if e == nil || e.Err == nil {
		return string(e.Kind)
	}
	return e.Err.Error()
}

func (e *ExecutionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Kind classifies adapter, Jinushi, and execution failures.
type Kind string

const (
	KindInvalid                Kind = "invalid-request"
	KindTransport              Kind = "transport"
	KindTimeout                Kind = "timeout"
	KindCanceled               Kind = "canceled"
	KindCommand                Kind = "jinushi-command"
	KindProtocol               Kind = "jinushi-protocol"
	KindSupervisorUnavailable  Kind = "supervisor-unavailable"
	KindBootstrapNotConfigured Kind = "bootstrap-not-configured"
	KindBootstrapFailed        Kind = "bootstrap-failed"
	KindUncertain              Kind = "run-uncertain"
	KindAmbiguous              Kind = "ambiguous-run-ownership"
	KindNotStopped             Kind = "run-not-stopped"
)

// Failure preserves Jinushi's machine error code when a command is rejected.
type Failure struct {
	Kind     Kind
	Code     string
	Message  string
	RunID    string
	State    State
	ExitCode int
	Cause    error
}

func (e *Failure) Error() string {
	if e == nil {
		return "jinushi operation failed"
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	if e.Code != "" {
		return string(e.Kind) + ": " + e.Code
	}
	return string(e.Kind)
}

func (e *Failure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Options configures the bounded Jinushi command path. SupervisorStartCommand
// must be a configured command that starts the supervisor and returns; the
// adapter does not daemonize the foreground `jinushi supervisor` command.
type Options struct {
	StateDir               string
	CommandTimeout         time.Duration
	SupervisorStartCommand []string
}

// Service is the lifecycle input translated to one detached Jinushi Run.
type Service struct {
	ID          string
	Argv        []string
	Cwd         string
	Environment map[string]string
	UnsetEnv    []string
}

// StartRequest carries a caller-generated submission ID. Reuse the same
// request and ID to retry an ambiguous submission; use a fresh ID for a new
// physical execution, including restart.
type StartRequest struct {
	Service      Service
	SubmissionID string
}

// State is deliberately string-backed so a newer Jinushi state remains
// visible to callers rather than being flattened to a known state.
type State string

const (
	StateAccepted    State = "accepted"
	StateStarting    State = "starting"
	StateRunning     State = "running"
	StateTerminating State = "terminating"
	StateReconciling State = "reconciling"
	StateTerminal    State = "terminal"
	StateUncertain   State = "uncertain"
)

// Run is the public Jinushi execution evidence used by the adapter.
type Run struct {
	ID         string    `json:"runId"`
	State      State     `json:"state"`
	Generation uint64    `json:"generation"`
	CreatedAt  time.Time `json:"createdAt"`
	Spec       RunSpec   `json:"spec"`
	Receipt    *Receipt  `json:"receipt,omitempty"`
}

// RunSpec contains the public fields used to correlate a Run with a Matagi
// service.
type RunSpec struct {
	Correlation map[string]string `json:"correlation"`
}

// Receipt records the physical terminal outcome without assigning semantic
// service success to an exit code.
type Receipt struct {
	Outcome  string `json:"outcome"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

// ServiceStatus has no Run when Jinushi retains no Run for the service. That
// does not claim the service is stopped or that it never ran.
type ServiceStatus struct {
	ServiceID string `json:"serviceId"`
	Run       *Run   `json:"run,omitempty"`
}
