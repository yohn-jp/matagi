package jinushi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeExecutor struct {
	run   func(context.Context, []string, time.Duration) (CommandResult, error)
	calls [][]string
}

func (f *fakeExecutor) Run(ctx context.Context, argv []string, timeout time.Duration) (CommandResult, error) {
	f.calls = append(f.calls, append([]string(nil), argv...))
	if f.run == nil {
		return CommandResult{}, errors.New("unexpected remote command")
	}
	return f.run(ctx, argv, timeout)
}

func TestEnsureReadyBootstrapsOnlyWhenStatusReportsUnavailable(t *testing.T) {
	statusCalls := 0
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		if argv[0] == "start-supervisor" {
			return CommandResult{ExitCode: 0}, nil
		}
		switch argv[1] {
		case "status":
			statusCalls++
			if statusCalls == 1 {
				return jsonResult(t, 1, map[string]any{
					"version": 1, "nextCursor": "", "error": map[string]string{"code": "supervisor-unavailable", "message": "not running"},
				}), nil
			}
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second, SupervisorStartCommand: []string{"start-supervisor", "jinushi"}})

	if err := client.EnsureReady(context.Background()); err != nil {
		t.Fatalf("EnsureReady() error = %v", err)
	}
	if len(executor.calls) != 3 {
		t.Fatalf("commands = %v, want status, bootstrap, status", executor.calls)
	}
	if !reflect.DeepEqual(executor.calls[1], []string{"start-supervisor", "jinushi"}) {
		t.Fatalf("bootstrap command = %v", executor.calls[1])
	}
}

func TestEnsureReadyDoesNotBootstrapWhenReady(t *testing.T) {
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		if argv[1] != "status" {
			t.Fatalf("unexpected command %v", argv)
		}
		return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second, SupervisorStartCommand: []string{"start-supervisor"}})

	if err := client.EnsureReady(context.Background()); err != nil {
		t.Fatalf("EnsureReady() error = %v", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("commands = %v, want status only", executor.calls)
	}
}

func TestEnsureReadyKeepsUnavailableDistinctWhenBootstrapIsNotConfigured(t *testing.T) {
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		if argv[1] != "status" {
			t.Fatalf("unexpected command %v", argv)
		}
		return jsonResult(t, 1, map[string]any{
			"version": 1, "nextCursor": "", "error": map[string]string{"code": "supervisor-unavailable", "message": "not running"},
		}), nil
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})

	err := client.EnsureReady(context.Background())
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != KindBootstrapNotConfigured || failure.Code != "supervisor-unavailable" {
		t.Fatalf("EnsureReady() error = %#v, want explicit unavailable/bootstrap configuration failure", err)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("commands = %v, want status only", executor.calls)
	}
}

func TestReadRunOutputUsesBoundedRunSpecificStdout(t *testing.T) {
	want := []byte("dashboard: http://127.0.0.1:43123/\n")
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		if argv[1] != "output" || !containsArg(argv, "--json") || !containsArg(argv, "--stream=stdout") || !containsArg(argv, "--offset=0") || !containsArg(argv, "--limit=65536") || argv[len(argv)-1] != "run-managed" {
			t.Fatalf("output command = %v", argv)
		}
		return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "data": base64.StdEncoding.EncodeToString(want)}), nil
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second, StateDir: "/state"})

	got, err := client.ReadRunOutput(context.Background(), "run-managed")
	if err != nil || string(got) != string(want) {
		t.Fatalf("ReadRunOutput() = %q, %v; want %q", got, err, want)
	}
	if len(executor.calls) != 1 || !containsArg(executor.calls[0], "--state-dir=/state") {
		t.Fatalf("output calls = %v", executor.calls)
	}
}

func TestReadRunOutputFailsClosedOnGapsAndOutputOverflow(t *testing.T) {
	t.Run("retention gap", func(t *testing.T) {
		executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "data": "", "gap": true, "retainedFrom": 12}), nil
		}}
		client := newTestClient(t, executor, Options{CommandTimeout: time.Second})
		if _, err := client.ReadRunOutput(context.Background(), "run-managed"); err == nil || !strings.Contains(err.Error(), "retention gap") {
			t.Fatalf("ReadRunOutput() error = %v, want retention gap", err)
		}
	})

	t.Run("over limit", func(t *testing.T) {
		calls := 0
		executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
			calls++
			if calls == 1 {
				return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "data": base64.StdEncoding.EncodeToString(make([]byte, maxRunOutputBytes))}), nil
			}
			if !containsArg(argv, "--offset=65536") || !containsArg(argv, "--limit=1") {
				t.Fatalf("overflow probe command = %v", argv)
			}
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "data": base64.StdEncoding.EncodeToString([]byte("x"))}), nil
		}}
		client := newTestClient(t, executor, Options{CommandTimeout: time.Second})
		if _, err := client.ReadRunOutput(context.Background(), "run-managed"); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("ReadRunOutput() error = %v, want output limit failure", err)
		}
		if calls != 2 {
			t.Fatalf("output calls = %d, want bounded overflow probe", calls)
		}
	})
	t.Run("oversized encoded response", func(t *testing.T) {
		executor := &fakeExecutor{run: func(_ context.Context, _ []string, _ time.Duration) (CommandResult, error) {
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "data": base64.StdEncoding.EncodeToString(make([]byte, 2*maxRunOutputBytes))}), nil
		}}
		client := newTestClient(t, executor, Options{CommandTimeout: time.Second})
		if _, err := client.ReadRunOutput(context.Background(), "run-managed"); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("ReadRunOutput() error = %v, want encoded output limit failure", err)
		}
	})
}

func TestStartUsesDetachedRunAndStableCorrelation(t *testing.T) {
	var runCommand []string
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		switch argv[1] {
		case "status":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		case "list":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{}}), nil
		case "run":
			runCommand = append([]string(nil), argv...)
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": testRun("run-new", StateAccepted, 1, "service-a")}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second, StateDir: "/srv/jinushi"})
	request := StartRequest{
		Service:      Service{ID: "service-a", Argv: []string{"./serve", "--label", "two words"}, Cwd: "/srv/app", Environment: map[string]string{"Z": "last", "A": "first"}, UnsetEnv: []string{"OLD"}},
		SubmissionID: "submission-1",
	}

	run, err := client.Start(context.Background(), request)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if run.ID != "run-new" || run.State != StateAccepted {
		t.Fatalf("Start() = %#v", run)
	}
	want := []string{
		"jinushi", "run", "--state-dir=/srv/jinushi", "--submission-id=submission-1", "--lifetime=detached",
		"--cwd=/srv/app", "--correlation=owner=service-a", "--env=A=first", "--env=Z=last", "--unset-env=OLD",
		"--", "./serve", "--label", "two words",
	}
	if !reflect.DeepEqual(runCommand, want) {
		t.Fatalf("run command = %#v, want %#v", runCommand, want)
	}
}

func TestStartDiscoversCorrelatedRunAcrossListPages(t *testing.T) {
	other := testRun("run-other", StateRunning, 3, "different-service")
	other.CreatedAt = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	current := testRun("run-current", StateRunning, 4, "service-a")
	current.CreatedAt = other.CreatedAt.Add(time.Minute)
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		switch argv[1] {
		case "status":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		case "list":
			if containsArg(argv, "--cursor=run-other") {
				return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{current}}), nil
			}
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "run-other", "runs": []Run{other}}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})

	run, err := client.Start(context.Background(), StartRequest{Service: Service{ID: "service-a", Argv: []string{"serve"}, Cwd: "/srv/app"}, SubmissionID: "fresh-id"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if run.ID != "run-current" || run.State != StateRunning {
		t.Fatalf("Start() did not recover the correlated Run: %#v", run)
	}
	for _, argv := range executor.calls {
		if argv[1] == "run" {
			t.Fatal("Start() duplicated a correlated live Run")
		}
	}
}

func TestStartDoesNotAdoptUncorrelatedManualRun(t *testing.T) {
	manual := testRun("manual-run", StateRunning, 3, "")
	manual.Spec.Correlation = map[string]string{"operator": "manual"}
	created := testRun("managed-run", StateAccepted, 1, "service-a")
	runCalled := false
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		switch argv[1] {
		case "status":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		case "list":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{manual}}), nil
		case "run":
			runCalled = true
			if !containsArg(argv, "--correlation=owner=service-a") {
				t.Fatalf("new Run omitted Matagi owner correlation: %v", argv)
			}
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": created}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})

	run, err := client.Start(context.Background(), StartRequest{Service: Service{ID: "service-a", Argv: []string{"serve"}, Cwd: "/srv/app"}, SubmissionID: "fresh-id"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !runCalled || run.ID != "managed-run" {
		t.Fatalf("Start() = %#v, run command called = %t; manual Run was adopted", run, runCalled)
	}
}

func TestAmbiguousStartRetryReusesSubmissionID(t *testing.T) {
	runAttempts := 0
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		switch argv[1] {
		case "status":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		case "list":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{}}), nil
		case "run":
			runAttempts++
			if runAttempts == 1 {
				return CommandResult{}, &ExecutionError{Kind: ExecutionTimeout, Err: errors.New("reply timed out")}
			}
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": testRun("run-retried", StateAccepted, 1, "service-a")}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})
	request := StartRequest{Service: Service{ID: "service-a", Argv: []string{"serve"}, Cwd: "/srv/app"}, SubmissionID: "submission-same"}

	if _, err := client.Start(context.Background(), request); !isKind(err, KindTimeout) {
		t.Fatalf("first Start() error = %v, want timeout", err)
	}
	if _, err := client.Start(context.Background(), request); err != nil {
		t.Fatalf("retry Start() error = %v", err)
	}
	var submissions []string
	for _, argv := range executor.calls {
		if len(argv) > 1 && argv[1] == "run" {
			for _, arg := range argv {
				if strings.HasPrefix(arg, "--submission-id=") {
					submissions = append(submissions, strings.TrimPrefix(arg, "--submission-id="))
				}
			}
		}
	}
	if !reflect.DeepEqual(submissions, []string{"submission-same", "submission-same"}) {
		t.Fatalf("run submission IDs = %v", submissions)
	}
}

func TestStatusPreservesUnknownRunState(t *testing.T) {
	created := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	listed := testRun("run-1", State("future-state"), 5, "service-a")
	listed.CreatedAt = created
	inspected := listed
	inspected.Generation = 6
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		switch argv[1] {
		case "status":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		case "list":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{listed}}), nil
		case "inspect":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": inspected}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})

	status, err := client.Status(context.Background(), "service-a")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.Run == nil || status.Run.State != State("future-state") || status.Run.Generation != 6 {
		t.Fatalf("Status() = %#v, unknown state was not preserved", status)
	}
}

func TestStopUsesFreshGenerationAndRetainsTerminalOutcome(t *testing.T) {
	listed := testRun("run-1", StateRunning, 2, "service-a")
	listed.CreatedAt = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	inspected := listed
	inspected.Generation = 9
	terminal := testRun("run-1", StateTerminal, 10, "service-a")
	terminal.Receipt = &Receipt{Outcome: "exited", ExitCode: intPointer(7)}
	var cancelCommand []string
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		switch argv[1] {
		case "status":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		case "list":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{listed}}), nil
		case "inspect":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": inspected}), nil
		case "cancel":
			cancelCommand = append([]string(nil), argv...)
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": inspected}), nil
		case "await":
			return jsonResult(t, 7, map[string]any{"version": 1, "nextCursor": "", "run": terminal}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})

	status, err := client.Stop(context.Background(), "service-a")
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if status.Run == nil || status.Run.State != StateTerminal || status.Run.Receipt == nil || status.Run.Receipt.ExitCode == nil || *status.Run.Receipt.ExitCode != 7 {
		t.Fatalf("Stop() did not preserve terminal outcome: %#v", status)
	}
	if !containsArg(cancelCommand, "--expected-generation=9") {
		t.Fatalf("cancel did not use inspected generation: %v", cancelCommand)
	}
	if !containsPrefix(cancelCommand, "--request-id=matagi-cancel-") {
		t.Fatalf("cancel omitted request identity: %v", cancelCommand)
	}
	if cancelCommand[len(cancelCommand)-1] != "run-1" {
		t.Fatalf("cancel did not target the opaque Run ID: %v", cancelCommand)
	}
	for _, arg := range cancelCommand {
		if strings.Contains(arg, "pid") || strings.Contains(arg, "PID") {
			t.Fatalf("cancel unexpectedly targeted a PID: %v", cancelCommand)
		}
	}
}

func TestRestartWaitsForTerminalAndUsesNewSubmissionID(t *testing.T) {
	old := testRun("run-old", StateRunning, 3, "service-a")
	old.CreatedAt = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	inspected := old
	inspected.Generation = 4
	terminal := testRun("run-old", StateTerminal, 5, "service-a")
	terminal.Receipt = &Receipt{Outcome: "cancelled"}
	newRun := testRun("run-new", StateAccepted, 1, "service-a")
	var runCommand []string
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		switch argv[1] {
		case "status":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		case "list":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{old}}), nil
		case "inspect":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": inspected}), nil
		case "cancel":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": inspected}), nil
		case "await":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": terminal}), nil
		case "run":
			runCommand = append([]string(nil), argv...)
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": newRun}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})
	request := StartRequest{Service: Service{ID: "service-a", Argv: []string{"serve"}, Cwd: "/srv/app"}, SubmissionID: "submission-new"}

	result, err := client.Restart(context.Background(), request)
	if err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if result.ID != "run-new" {
		t.Fatalf("Restart() = %#v", result)
	}
	if !containsArg(runCommand, "--submission-id=submission-new") {
		t.Fatalf("restart did not use its fresh submission ID: %v", runCommand)
	}
	awaitIndex := -1
	runIndex := -1
	for i, argv := range executor.calls {
		if argv[1] == "await" {
			awaitIndex = i
		}
		if argv[1] == "run" {
			runIndex = i
		}
	}
	if awaitIndex < 0 || runIndex <= awaitIndex {
		t.Fatalf("restart submitted before terminal proof: %v", executor.calls)
	}
}

func TestStartDoesNotDuplicateUncertainRun(t *testing.T) {
	uncertain := testRun("run-uncertain", StateUncertain, 8, "service-a")
	uncertain.CreatedAt = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		switch argv[1] {
		case "status":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		case "list":
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{uncertain}}), nil
		default:
			t.Fatalf("unexpected command %v", argv)
			return CommandResult{}, nil
		}
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})

	run, err := client.Start(context.Background(), StartRequest{Service: Service{ID: "service-a", Argv: []string{"serve"}, Cwd: "/srv/app"}, SubmissionID: "new-id"})
	if !isKind(err, KindUncertain) || run.State != StateUncertain {
		t.Fatalf("Start() = %#v, %v; want preserved uncertainty", run, err)
	}
	for _, argv := range executor.calls {
		if argv[1] == "run" {
			t.Fatal("Start() launched a duplicate Run while ownership was uncertain")
		}
	}
}

func TestCommandAndTransportFailuresRemainDistinct(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		executor := &fakeExecutor{run: func(_ context.Context, _ []string, _ time.Duration) (CommandResult, error) {
			return CommandResult{}, &ExecutionError{Kind: ExecutionTransportFailure, Err: errors.New("connection lost")}
		}}
		client := newTestClient(t, executor, Options{CommandTimeout: time.Second})
		_, err := client.Status(context.Background(), "service-a")
		if !isKind(err, KindTransport) {
			t.Fatalf("Status() error = %v, want transport", err)
		}
	})

	t.Run("Jinushi failure code", func(t *testing.T) {
		listed := testRun("run-1", StateRunning, 2, "service-a")
		listed.CreatedAt = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
		inspected := listed
		inspected.Generation = 3
		executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
			switch argv[1] {
			case "status":
				return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
			case "list":
				return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "runs": []Run{listed}}), nil
			case "inspect":
				return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "run": inspected}), nil
			case "cancel":
				return jsonResult(t, 1, map[string]any{"version": 1, "nextCursor": "", "error": map[string]string{"code": "stale-run-generation", "message": "generation changed"}}), nil
			default:
				t.Fatalf("unexpected command %v", argv)
				return CommandResult{}, nil
			}
		}}
		client := newTestClient(t, executor, Options{CommandTimeout: time.Second})
		_, err := client.Stop(context.Background(), "service-a")
		var failure *Failure
		if !errors.As(err, &failure) || failure.Kind != KindCommand || failure.Code != "stale-run-generation" {
			t.Fatalf("Stop() error = %#v, want preserved Jinushi command code", err)
		}
	})
}

func TestMalformedMachineResponseIsProtocolFailure(t *testing.T) {
	commandCalls := 0
	executor := &fakeExecutor{run: func(_ context.Context, argv []string, _ time.Duration) (CommandResult, error) {
		commandCalls++
		if argv[1] == "status" {
			return jsonResult(t, 0, map[string]any{"version": 1, "nextCursor": "", "status": map[string]any{}}), nil
		}
		return CommandResult{Stdout: []byte("not-json"), ExitCode: 0}, nil
	}}
	client := newTestClient(t, executor, Options{CommandTimeout: time.Second})

	_, err := client.Status(context.Background(), "service-a")
	if !isKind(err, KindProtocol) {
		t.Fatalf("Status() error = %v, want protocol failure", err)
	}
	if commandCalls != 2 {
		t.Fatalf("command calls = %d, want status and list", commandCalls)
	}
}

func TestNewSubmissionIDIsBoundedAndUnique(t *testing.T) {
	first, err := NewSubmissionID()
	if err != nil {
		t.Fatalf("NewSubmissionID() error = %v", err)
	}
	second, err := NewSubmissionID()
	if err != nil {
		t.Fatalf("NewSubmissionID() error = %v", err)
	}
	if first == second || len(first) > 128 || first == "" {
		t.Fatalf("submission IDs = %q and %q", first, second)
	}
}

func newTestClient(t *testing.T, executor Executor, options Options) *Client {
	t.Helper()
	client, err := New(executor, options)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func jsonResult(t *testing.T, exitCode int, value any) CommandResult {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return CommandResult{Stdout: data, ExitCode: exitCode}
}

func testRun(id string, state State, generation uint64, serviceID string) Run {
	return Run{
		ID:         id,
		State:      state,
		Generation: generation,
		CreatedAt:  time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		Spec:       RunSpec{Correlation: map[string]string{ownerCorrelationKey: serviceID}},
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func containsPrefix(args []string, prefix string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

func intPointer(value int) *int {
	return &value
}
