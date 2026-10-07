package jinushi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const listPageSize = 64
const maxRunOutputBytes = 64 * 1024

type response struct {
	Version      int             `json:"version"`
	Run          *Run            `json:"run,omitempty"`
	Runs         []Run           `json:"runs,omitempty"`
	NextCursor   *string         `json:"nextCursor"`
	Status       json.RawMessage `json:"status,omitempty"`
	Data         string          `json:"data,omitempty"`
	Gap          bool            `json:"gap,omitempty"`
	RetainedFrom uint64          `json:"retainedFrom,omitempty"`
	Error        *commandFailure `json:"error,omitempty"`
}

type commandFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Client performs service-oriented operations using Jinushi's public CLI.
type Client struct {
	executor               Executor
	stateDir               string
	timeout                time.Duration
	supervisorStartCommand []string
}

// New constructs a lifecycle client. A positive timeout is required because
// every remote command must have a bounded lifetime.
func New(executor Executor, options Options) (*Client, error) {
	if executor == nil {
		return nil, &Failure{Kind: KindInvalid, Message: "executor is required"}
	}
	if options.CommandTimeout <= 0 {
		return nil, &Failure{Kind: KindInvalid, Message: "command timeout must be positive"}
	}
	for _, arg := range options.SupervisorStartCommand {
		if arg == "" {
			return nil, &Failure{Kind: KindInvalid, Message: "supervisor start command contains an empty argument"}
		}
	}
	return &Client{
		executor:               executor,
		stateDir:               options.StateDir,
		timeout:                options.CommandTimeout,
		supervisorStartCommand: append([]string(nil), options.SupervisorStartCommand...),
	}, nil
}

// NewSubmissionID creates a fresh caller-owned id for one physical launch.
// The caller must retain and reuse this ID when retrying an ambiguous Start,
// and create a different ID for a later launch or restart.
func NewSubmissionID() (string, error) {
	return newID("matagi-run-")
}

// EnsureReady proves supervisor usability with machine-readable status. It
// runs the configured start command only when status explicitly reports that
// the supervisor is unavailable, then proves readiness again.
func (c *Client) EnsureReady(ctx context.Context) error {
	if err := c.validContext(ctx); err != nil {
		return err
	}
	if _, err := c.probe(ctx); err == nil {
		return nil
	} else if !isKind(err, KindSupervisorUnavailable) {
		return err
	} else if len(c.supervisorStartCommand) == 0 {
		return &Failure{Kind: KindBootstrapNotConfigured, Code: failureCode(err), Message: "Jinushi is unavailable and no supervisor start command is configured", Cause: err}
	}

	result, err := c.executor.Run(ctx, append([]string(nil), c.supervisorStartCommand...), c.timeout)
	if err != nil {
		return executionFailure(err, "start Jinushi supervisor")
	}
	if result.ExitCode != 0 {
		return &Failure{Kind: KindBootstrapFailed, Code: "remote-exit", Message: fmt.Sprintf("supervisor start command exited with status %d", result.ExitCode), ExitCode: result.ExitCode}
	}
	if _, err := c.probe(ctx); err != nil {
		if isKind(err, KindSupervisorUnavailable) {
			return &Failure{Kind: KindSupervisorUnavailable, Code: failureCode(err), Message: "Jinushi supervisor remained unavailable after bootstrap", Cause: err}
		}
		return err
	}
	return nil
}

// Start returns the current Run when a live Run is already established for
// the service. Otherwise it creates a detached Run. SubmissionID is required
// so an ambiguous submission can be safely retried with the same identity.
func (c *Client) Start(ctx context.Context, request StartRequest) (Run, error) {
	if err := validateStartRequest(request); err != nil {
		return Run{}, err
	}
	if err := c.EnsureReady(ctx); err != nil {
		return Run{}, err
	}
	current, err := c.currentRun(ctx, request.Service.ID)
	if err != nil {
		return Run{}, err
	}
	if current != nil && current.State != StateTerminal {
		if !knownLiveState(current.State) {
			return *current, &Failure{Kind: KindUncertain, RunID: current.ID, State: current.State, Message: "Jinushi Run state does not prove that a new launch is safe"}
		}
		return *current, nil
	}
	return c.startRun(ctx, request)
}

// Status returns a fresh Jinushi inspect snapshot for the latest Run
// associated with the service. A nil Run means Jinushi retains no associated
// Run and does not assert that the service is stopped.
func (c *Client) Status(ctx context.Context, serviceID string) (ServiceStatus, error) {
	if err := validateServiceID(serviceID); err != nil {
		return ServiceStatus{}, err
	}
	if err := c.EnsureReady(ctx); err != nil {
		return ServiceStatus{ServiceID: serviceID}, err
	}
	current, err := c.currentRun(ctx, serviceID)
	if err != nil {
		return ServiceStatus{ServiceID: serviceID}, err
	}
	if current == nil {
		return ServiceStatus{ServiceID: serviceID}, nil
	}
	inspected, err := c.inspect(ctx, *current, serviceID)
	if err != nil {
		return ServiceStatus{ServiceID: serviceID, Run: current}, err
	}
	return ServiceStatus{ServiceID: serviceID, Run: &inspected}, nil
}

// ReadRunOutput returns the retained stdout for one opaque Jinushi Run ID.
// Dynamic endpoint resolution uses it only as per-Run correlation evidence;
// incomplete or over-limit output is rejected instead of guessed around.
func (c *Client) ReadRunOutput(ctx context.Context, runID string) ([]byte, error) {
	if err := c.validContext(ctx); err != nil {
		return nil, err
	}
	if !validRunID(runID) {
		return nil, &Failure{Kind: KindInvalid, RunID: runID, Message: "Jinushi Run ID is invalid"}
	}
	page, err := c.readOutputPage(ctx, runID, 0, maxRunOutputBytes)
	if err != nil {
		return nil, err
	}
	if page.Gap || page.RetainedFrom != 0 {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout has an output retention gap"}
	}
	if len(page.Data) > base64.StdEncoding.EncodedLen(maxRunOutputBytes) {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout exceeds the endpoint evidence limit"}
	}
	data, err := base64.StdEncoding.DecodeString(page.Data)
	if err != nil {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout was not valid base64", Cause: err}
	}
	if len(data) > maxRunOutputBytes {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout exceeds the endpoint evidence limit"}
	}
	if len(data) < maxRunOutputBytes {
		return data, nil
	}

	extra, err := c.readOutputPage(ctx, runID, maxRunOutputBytes, 1)
	if err != nil {
		return nil, err
	}
	if extra.Gap || extra.RetainedFrom > maxRunOutputBytes {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout changed while it was being read"}
	}
	if len(extra.Data) > base64.StdEncoding.EncodedLen(1) {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout changed while it was being read"}
	}
	more, err := base64.StdEncoding.DecodeString(extra.Data)
	if err != nil {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout was not valid base64", Cause: err}
	}
	if len(more) > 1 {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout changed while it was being read"}
	}
	if len(more) != 0 {
		return nil, &Failure{Kind: KindProtocol, RunID: runID, Message: "Jinushi Run stdout exceeds the endpoint evidence limit"}
	}
	return data, nil
}

func (c *Client) readOutputPage(ctx context.Context, runID string, offset, limit int) (response, error) {
	argv := c.command("output",
		"--json",
		"--stream=stdout",
		"--offset="+strconv.Itoa(offset),
		"--limit="+strconv.Itoa(limit),
		runID,
	)
	return c.call(ctx, argv, false)
}

func validRunID(runID string) bool {
	if runID == "" || len(runID) > 128 {
		return false
	}
	for _, char := range runID {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

// Stop cancels the current Run using its freshly inspected generation and
// returns only after Jinushi proves a terminal outcome.
func (c *Client) Stop(ctx context.Context, serviceID string) (ServiceStatus, error) {
	if err := validateServiceID(serviceID); err != nil {
		return ServiceStatus{}, err
	}
	if err := c.EnsureReady(ctx); err != nil {
		return ServiceStatus{ServiceID: serviceID}, err
	}
	current, err := c.currentRun(ctx, serviceID)
	if err != nil {
		return ServiceStatus{ServiceID: serviceID}, err
	}
	if current == nil {
		return ServiceStatus{ServiceID: serviceID}, nil
	}
	inspected, err := c.inspect(ctx, *current, serviceID)
	if err != nil {
		return ServiceStatus{ServiceID: serviceID, Run: current}, err
	}
	if inspected.State == StateTerminal {
		return ServiceStatus{ServiceID: serviceID, Run: &inspected}, nil
	}
	if inspected.State == StateUncertain || !knownLiveState(inspected.State) {
		return ServiceStatus{ServiceID: serviceID, Run: &inspected}, &Failure{Kind: KindUncertain, RunID: inspected.ID, State: inspected.State, Message: "Jinushi cannot prove that this Run is safely controllable"}
	}
	stopped, err := c.stopRun(ctx, inspected)
	return ServiceStatus{ServiceID: serviceID, Run: &stopped}, err
}

// Restart proves the previous Run terminal before submitting a new Run. The
// request must contain a fresh submission ID; retries of an ambiguous restart
// must reuse that same submission ID.
func (c *Client) Restart(ctx context.Context, request StartRequest) (Run, error) {
	if err := validateStartRequest(request); err != nil {
		return Run{}, err
	}
	if err := c.EnsureReady(ctx); err != nil {
		return Run{}, err
	}
	current, err := c.currentRun(ctx, request.Service.ID)
	if err != nil {
		return Run{}, err
	}
	if current != nil {
		inspected, err := c.inspect(ctx, *current, request.Service.ID)
		if err != nil {
			return Run{}, err
		}
		if inspected.State != StateTerminal {
			if inspected.State == StateUncertain || !knownLiveState(inspected.State) {
				return inspected, &Failure{Kind: KindUncertain, RunID: inspected.ID, State: inspected.State, Message: "Jinushi cannot prove that this Run is safely controllable"}
			}
			stopped, err := c.stopRun(ctx, inspected)
			if err != nil {
				return stopped, err
			}
			if stopped.State != StateTerminal || stopped.Receipt == nil {
				return stopped, &Failure{Kind: KindNotStopped, RunID: stopped.ID, State: stopped.State, Message: "Jinushi did not prove the previous Run terminal"}
			}
		}
	}
	return c.startRun(ctx, request)
}

func (c *Client) startRun(ctx context.Context, request StartRequest) (Run, error) {
	argv := c.command("run")
	argv = append(argv,
		"--submission-id="+request.SubmissionID,
		"--lifetime=detached",
		"--cwd="+request.Service.Cwd,
		"--correlation="+ownerCorrelationKey+"="+request.Service.ID,
	)
	keys := make([]string, 0, len(request.Service.Environment))
	for key := range request.Service.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		argv = append(argv, "--env="+key+"="+request.Service.Environment[key])
	}
	unset := append([]string(nil), request.Service.UnsetEnv...)
	sort.Strings(unset)
	for _, key := range unset {
		argv = append(argv, "--unset-env="+key)
	}
	argv = append(argv, "--")
	argv = append(argv, request.Service.Argv...)

	response, err := c.call(ctx, argv, false)
	if err != nil {
		return Run{}, err
	}
	if response.Run == nil || response.Run.ID == "" || response.Run.State == "" {
		return Run{}, &Failure{Kind: KindProtocol, Message: "Jinushi run response omitted its Run identity or state"}
	}
	if response.Run.Spec.Correlation[ownerCorrelationKey] != request.Service.ID {
		return Run{}, &Failure{Kind: KindProtocol, RunID: response.Run.ID, State: response.Run.State, Message: "Jinushi run response did not preserve the Matagi service correlation owner"}
	}
	return *response.Run, nil
}

func (c *Client) stopRun(ctx context.Context, run Run) (Run, error) {
	if run.Generation == 0 {
		return run, &Failure{Kind: KindProtocol, RunID: run.ID, State: run.State, Message: "Jinushi Run omitted its current generation"}
	}
	requestID, err := newID("matagi-cancel-")
	if err != nil {
		return run, &Failure{Kind: KindInvalid, RunID: run.ID, State: run.State, Message: "could not create a Jinushi cancellation request ID", Cause: err}
	}
	argv := c.command("cancel")
	argv = append(argv,
		"--request-id="+requestID,
		"--expected-generation="+strconv.FormatUint(run.Generation, 10),
		run.ID,
	)
	if _, err := c.call(ctx, argv, false); err != nil {
		return run, err
	}

	argv = c.command("await")
	argv = append(argv, run.ID)
	response, err := c.call(ctx, argv, true)
	if err != nil {
		return run, err
	}
	if response.Run == nil || response.Run.ID != run.ID {
		return run, &Failure{Kind: KindProtocol, RunID: run.ID, Message: "Jinushi await response did not identify the canceled Run"}
	}
	if response.Run.State == StateUncertain {
		return *response.Run, &Failure{Kind: KindUncertain, RunID: run.ID, State: response.Run.State, Message: "Jinushi could not prove the Run terminal"}
	}
	if response.Run.State != StateTerminal || response.Run.Receipt == nil {
		return *response.Run, &Failure{Kind: KindNotStopped, RunID: run.ID, State: response.Run.State, Message: "Jinushi await did not return a terminal receipt"}
	}
	return *response.Run, nil
}

func (c *Client) probe(ctx context.Context) (response, error) {
	result, err := c.call(ctx, c.command("status"), false)
	if err != nil {
		return result, err
	}
	if len(result.Status) == 0 || bytes.Equal(bytes.TrimSpace(result.Status), []byte("null")) {
		return response{}, &Failure{Kind: KindProtocol, Message: "Jinushi status response omitted the machine-readable supervisor status"}
	}
	return result, nil
}

func (c *Client) inspect(ctx context.Context, known Run, serviceID string) (Run, error) {
	argv := c.command("inspect")
	argv = append(argv, known.ID)
	response, err := c.call(ctx, argv, false)
	if err != nil {
		return Run{}, err
	}
	if response.Run == nil || response.Run.ID != known.ID || response.Run.State == "" {
		return Run{}, &Failure{Kind: KindProtocol, RunID: known.ID, Message: "Jinushi inspect response omitted or changed the requested Run identity"}
	}
	if response.Run.Spec.Correlation[ownerCorrelationKey] != serviceID {
		return Run{}, &Failure{Kind: KindProtocol, RunID: known.ID, State: response.Run.State, Message: "Jinushi inspect response did not preserve the Matagi service correlation owner"}
	}
	return *response.Run, nil
}

func (c *Client) currentRun(ctx context.Context, serviceID string) (*Run, error) {
	var matches []Run
	cursor := ""
	seenCursors := map[string]struct{}{}
	seenRuns := map[string]struct{}{}
	for {
		argv := c.command("list")
		argv = append(argv, "--limit="+strconv.Itoa(listPageSize))
		if cursor != "" {
			argv = append(argv, "--cursor="+cursor)
		}
		response, err := c.call(ctx, argv, false)
		if err != nil {
			return nil, err
		}
		if response.NextCursor == nil {
			return nil, &Failure{Kind: KindProtocol, Message: "Jinushi list response omitted nextCursor"}
		}
		for _, run := range response.Runs {
			if run.ID == "" || run.State == "" || run.CreatedAt.IsZero() {
				return nil, &Failure{Kind: KindProtocol, Message: "Jinushi list response contained an incomplete Run"}
			}
			if _, exists := seenRuns[run.ID]; exists {
				return nil, &Failure{Kind: KindProtocol, RunID: run.ID, Message: "Jinushi list repeated a Run identity"}
			}
			seenRuns[run.ID] = struct{}{}
			if run.Spec.Correlation[ownerCorrelationKey] == serviceID {
				matches = append(matches, run)
			}
		}
		next := *response.NextCursor
		if next == "" {
			break
		}
		if next == cursor {
			return nil, &Failure{Kind: KindProtocol, Message: "Jinushi list cursor did not advance"}
		}
		if _, exists := seenCursors[next]; exists {
			return nil, &Failure{Kind: KindProtocol, Message: "Jinushi list repeated a pagination cursor"}
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}

	var liveRuns []Run
	var terminalRuns []Run
	for i := range matches {
		run := &matches[i]
		if run.State != StateTerminal {
			liveRuns = append(liveRuns, *run)
		}
	}
	if len(liveRuns) > 1 {
		return nil, &Failure{Kind: KindAmbiguous, Message: "multiple nonterminal Jinushi Runs carry the same Matagi service identity"}
	}
	if len(liveRuns) == 1 {
		return &liveRuns[0], nil
	}
	for i := range matches {
		if matches[i].State == StateTerminal {
			terminalRuns = append(terminalRuns, matches[i])
		}
	}
	if len(terminalRuns) == 0 {
		return nil, nil
	}
	latest := terminalRuns[0]
	tiedLatest := false
	for _, run := range terminalRuns[1:] {
		if run.CreatedAt.After(latest.CreatedAt) {
			latest = run
			tiedLatest = false
		} else if run.CreatedAt.Equal(latest.CreatedAt) && run.ID != latest.ID {
			tiedLatest = true
		}
	}
	if tiedLatest {
		return nil, &Failure{Kind: KindAmbiguous, Message: "multiple latest terminal Jinushi Runs have the same creation time for one Matagi service"}
	}
	return &latest, nil
}

func (c *Client) call(ctx context.Context, argv []string, allowAwaitOutcome bool) (response, error) {
	if err := c.validContext(ctx); err != nil {
		return response{}, err
	}
	result, err := c.executor.Run(ctx, append([]string(nil), argv...), c.timeout)
	if err != nil {
		return response{}, executionFailure(err, strings.Join(argv[:min(2, len(argv))], " "))
	}
	if result.ExitCode < 0 {
		return response{}, &Failure{Kind: KindProtocol, Message: "remote command returned no exit status"}
	}
	var decoded response
	if err := json.Unmarshal(result.Stdout, &decoded); err != nil {
		if result.ExitCode != 0 {
			return response{}, &Failure{Kind: KindCommand, Code: "remote-exit", ExitCode: result.ExitCode, Message: fmt.Sprintf("remote command exited with status %d without a Jinushi JSON response", result.ExitCode)}
		}
		return response{}, &Failure{Kind: KindProtocol, Message: "Jinushi returned invalid machine JSON", Cause: err}
	}
	if decoded.Version != 1 {
		return response{}, &Failure{Kind: KindProtocol, Message: fmt.Sprintf("unsupported Jinushi response version %d", decoded.Version)}
	}
	if decoded.Error != nil {
		kind := KindCommand
		if decoded.Error.Code == string(KindSupervisorUnavailable) {
			kind = KindSupervisorUnavailable
		}
		return response{}, &Failure{Kind: kind, Code: decoded.Error.Code, Message: decoded.Error.Message}
	}
	if result.ExitCode != 0 {
		if allowAwaitOutcome && decoded.Run != nil && decoded.Run.State == StateUncertain {
			return decoded, nil
		}
		if allowAwaitOutcome && decoded.Run != nil && decoded.Run.State == StateTerminal && decoded.Run.Receipt != nil {
			return decoded, nil
		}
		return response{}, &Failure{Kind: KindCommand, Code: "remote-exit", ExitCode: result.ExitCode, Message: fmt.Sprintf("remote Jinushi command exited with status %d", result.ExitCode)}
	}
	return decoded, nil
}

func (c *Client) command(name string, args ...string) []string {
	argv := make([]string, 0, len(args)+4)
	argv = append(argv, "jinushi", name)
	if c.stateDir != "" {
		argv = append(argv, "--state-dir="+c.stateDir)
	}
	argv = append(argv, args...)
	return argv
}

func (c *Client) validContext(ctx context.Context) error {
	if c == nil || c.executor == nil {
		return &Failure{Kind: KindInvalid, Message: "Jinushi client is not initialized"}
	}
	if ctx == nil {
		return &Failure{Kind: KindInvalid, Message: "context is required"}
	}
	return nil
}

func executionFailure(err error, operation string) *Failure {
	var executionErr *ExecutionError
	if errors.As(err, &executionErr) {
		kind := KindTransport
		switch executionErr.Kind {
		case ExecutionTimeout:
			kind = KindTimeout
		case ExecutionCanceled:
			kind = KindCanceled
		}
		return &Failure{Kind: kind, Message: operation + ": " + err.Error(), Cause: err}
	}
	return &Failure{Kind: KindTransport, Message: operation + ": " + err.Error(), Cause: err}
}

func validateStartRequest(request StartRequest) error {
	if err := validateService(request.Service); err != nil {
		return err
	}
	if request.SubmissionID == "" || len(request.SubmissionID) > 128 {
		return &Failure{Kind: KindInvalid, Message: "submission ID must contain between 1 and 128 bytes"}
	}
	return nil
}

func validateService(service Service) error {
	if err := validateServiceID(service.ID); err != nil {
		return err
	}
	if len(service.Argv) == 0 || service.Argv[0] == "" {
		return &Failure{Kind: KindInvalid, Message: "service argv must include an executable"}
	}
	if service.Cwd == "" {
		return &Failure{Kind: KindInvalid, Message: "service working directory is required"}
	}
	for key := range service.Environment {
		if key == "" || strings.Contains(key, "=") {
			return &Failure{Kind: KindInvalid, Message: "environment keys must be non-empty and cannot contain '='"}
		}
	}
	for _, key := range service.UnsetEnv {
		if key == "" || strings.Contains(key, "=") {
			return &Failure{Kind: KindInvalid, Message: "unset environment keys must be non-empty and cannot contain '='"}
		}
	}
	return nil
}

func validateServiceID(id string) error {
	if id == "" {
		return &Failure{Kind: KindInvalid, Message: "service ID is required"}
	}
	if strings.ContainsRune(id, '\x00') {
		return &Failure{Kind: KindInvalid, Message: "service ID cannot contain NUL"}
	}
	return nil
}

func knownLiveState(state State) bool {
	switch state {
	case StateAccepted, StateStarting, StateRunning, StateTerminating, StateReconciling:
		return true
	default:
		return false
	}
}

func isKind(err error, kind Kind) bool {
	var failure *Failure
	return errors.As(err, &failure) && failure.Kind == kind
}

func failureCode(err error) string {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func newID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}
