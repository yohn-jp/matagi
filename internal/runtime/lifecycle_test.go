package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/ssh"
	"github.com/yohn-jp/matagi/internal/tunnel"
)

func TestAmbiguousSubmissionAndRotation(t *testing.T) {
	r, f := fixture(t)
	owner := r.services[key("env", "svc")].CorrelationOwner()
	var submissions []string
	attempt := 0
	f.fn = func(args []string) (ssh.Result, error) {
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		case "run":
			for _, arg := range args {
				if strings.HasPrefix(arg, "--submission-id=") {
					submissions = append(submissions, arg)
				}
			}
			if !strings.Contains(strings.Join(args, " "), "--cwd=/work") || !strings.Contains(strings.Join(args, " "), "--correlation=owner="+owner) {
				t.Fatal(args)
			}
			attempt++
			if attempt < 3 {
				return ssh.Result{ExitCode: -1}, &ssh.Error{Kind: ssh.FailureTransport, Err: errors.New("lost response")}
			}
			return ssh.Result{Stdout: []byte(`{"version":1,"run":{"runId":"run","state":"running","spec":{"correlation":{"owner":"` + owner + `"}}}}`)}, nil
		}
		return ssh.Result{}, errors.New("unexpected command")
	}
	ctx := context.Background()
	var started Service
	for i := 0; i < 3; i++ {
		result, err := r.Start(ctx, "env", "svc")
		if i < 2 && err == nil || i == 2 && err != nil {
			t.Fatal(i, err)
		}
		if i == 2 {
			started = result
		}
	}
	if started.Process != "running" || started.State != "unknown" {
		t.Fatalf("Start() result = %#v; want direct Jinushi process state with readiness still unknown", started)
	}
	snapshot := r.Snapshot()
	if observed := snapshot.Environments[0].Services[0]; observed.Process != "running" || observed.State != "unknown" {
		t.Fatalf("immediate runtime snapshot = %#v; want direct Jinushi process state without invented readiness", observed)
	}
	if environment := snapshot.Environments[0]; environment.Connectivity != "connected" || environment.Jinushi != "ready" {
		t.Fatalf("immediate environment snapshot = %#v; successful Jinushi action did not refresh its authority state", environment)
	}
	if submissions[0] != submissions[1] || submissions[1] != submissions[2] || r.pending[key("env", "svc")].submissionID != "" {
		t.Fatal(submissions)
	}
	_, err := r.Start(ctx, "env", "svc")
	if err != nil || submissions[3] == submissions[2] {
		t.Fatal(submissions, err)
	}
	_ = r.Close(ctx)
}

func TestRestartAmbiguousSubmissionDoesNotCancelAttemptedRun(t *testing.T) {
	tests := []struct {
		name             string
		initialRunID     string
		wantCancelledIDs []string
	}{
		{name: "original Run", initialRunID: "run-old", wantCancelledIDs: []string{"run-old"}},
		{name: "no original Run"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, client, _ := lifecycleRuntime(t, registry.DesiredRunning)
			owner := r.services[key("env", "svc")].CorrelationOwner()
			runJSON := func(id, state string, generation int, receipt string) string {
				receiptJSON := ""
				if receipt != "" {
					receiptJSON = fmt.Sprintf(`,"receipt":{"outcome":%q}`, receipt)
				}
				return fmt.Sprintf(`{"runId":%q,"state":%q,"generation":%d,"createdAt":"2026-10-07T00:00:00Z","spec":{"correlation":{"owner":%q}}%s}`, id, state, generation, owner, receiptJSON)
			}
			runs := map[string]string{}
			if test.initialRunID != "" {
				runs[test.initialRunID] = runJSON(test.initialRunID, "running", 1, "")
			}
			currentRunID := test.initialRunID
			createdBySubmission := map[string]string{}
			var submissionIDs []string
			var cancelledRunIDs []string
			physicalRuns := 0
			runCalls := 0
			client.fn = func(args []string) (ssh.Result, error) {
				if len(args) == 1 && args[0] == "true" {
					return ssh.Result{ExitCode: 0}, nil
				}
				switch args[1] {
				case "status":
					return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
				case "list":
					if currentRunID == "" {
						return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
					}
					return ssh.Result{Stdout: []byte(fmt.Sprintf(`{"version":1,"runs":[%s],"nextCursor":""}`, runs[currentRunID]))}, nil
				case "inspect":
					id := args[len(args)-1]
					return ssh.Result{Stdout: []byte(fmt.Sprintf(`{"version":1,"run":%s}`, runs[id]))}, nil
				case "cancel":
					cancelledRunIDs = append(cancelledRunIDs, args[len(args)-1])
					return ssh.Result{Stdout: []byte(`{"version":1}`)}, nil
				case "await":
					id := args[len(args)-1]
					runs[id] = runJSON(id, "terminal", 2, "cancelled")
					if currentRunID == id {
						currentRunID = ""
					}
					return ssh.Result{Stdout: []byte(fmt.Sprintf(`{"version":1,"run":%s}`, runs[id]))}, nil
				case "run":
					var submissionID string
					for _, arg := range args {
						if strings.HasPrefix(arg, "--submission-id=") {
							submissionID = strings.TrimPrefix(arg, "--submission-id=")
						}
					}
					submissionIDs = append(submissionIDs, submissionID)
					id, exists := createdBySubmission[submissionID]
					if !exists {
						id = "run-new"
						createdBySubmission[submissionID] = id
						physicalRuns++
					}
					runs[id] = runJSON(id, "accepted", 1, "")
					currentRunID = id
					runCalls++
					if runCalls == 1 {
						return ssh.Result{ExitCode: -1}, &ssh.Error{Kind: ssh.FailureTransport, Err: errors.New("lost response after remote submission")}
					}
					return ssh.Result{Stdout: []byte(fmt.Sprintf(`{"version":1,"run":%s}`, runs[id]))}, nil
				default:
					return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %v", args)
				}
			}

			if _, err := r.Restart(context.Background(), "env", "svc"); err == nil {
				t.Fatal("first Restart() succeeded despite losing the submission response")
			}
			submissionID := r.pending[key("env", "svc")].submissionID
			if len(submissionIDs) != 1 || submissionID == "" || submissionIDs[0] != submissionID {
				t.Fatalf("pending submission ID = %q, submitted IDs = %v; want the first attempt retained", submissionID, submissionIDs)
			}
			service, err := r.Restart(context.Background(), "env", "svc")
			if err != nil {
				t.Fatalf("retry Restart(): %v", err)
			}
			if service.Process != "starting" || service.DesiredState != registry.DesiredRunning {
				t.Fatalf("retry Restart() = %#v; want the attempted Run and running intent", service)
			}
			if len(cancelledRunIDs) != len(test.wantCancelledIDs) {
				t.Fatalf("cancelled Run IDs = %v; want %v", cancelledRunIDs, test.wantCancelledIDs)
			}
			for i := range test.wantCancelledIDs {
				if cancelledRunIDs[i] != test.wantCancelledIDs[i] {
					t.Fatalf("cancelled Run IDs = %v; want %v", cancelledRunIDs, test.wantCancelledIDs)
				}
			}
			if physicalRuns != 1 {
				t.Fatalf("physical Runs created = %d; want one for the stable submission identity", physicalRuns)
			}
			if len(submissionIDs) != 2 || submissionIDs[0] != submissionID || submissionIDs[1] != submissionID {
				t.Fatalf("submission IDs = %v; want one stable identity across both attempts", submissionIDs)
			}
		})
	}
}

func lifecycleRuntime(t *testing.T, desired registry.DesiredState) (*Runtime, *fakeSSH, *config.Store) {
	t.Helper()
	snapshot, err := registry.NewSnapshot(
		[]registry.Environment{{ID: "env", SSHHost: "host", Jinushi: registry.JinushiDefinition{StateDir: "/state"}}},
		[]registry.Service{{
			ID: "svc", EnvironmentID: "env", DesiredState: desired,
			Execution: registry.ExecutionIntent{Argv: []string{"binary"}, CWD: "/work", Lifetime: registry.LifetimeDetached},
			Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "health", Path: "/ready"},
			Endpoints: []registry.Endpoint{
				{ID: "health", Label: "Health", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1234},
				{ID: "ui", Label: "UI", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1235},
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeSSH{fn: func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 0}, nil
		}
		if len(args) < 2 {
			return ssh.Result{}, fmt.Errorf("unexpected SSH command %v", args)
		}
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		case "run":
			return ssh.Result{Stdout: lifecycleRunResponse(t, args, "run-new", "accepted")}, nil
		default:
			return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %v", args)
		}
	}}
	manager, err := tunnel.NewManager(noLauncher{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compose(snapshot, client, manager)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}
	r.store = store
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	return r, client, store
}

func lifecycleRunResponse(t *testing.T, args []string, id, state string) []byte {
	t.Helper()
	owner := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "--correlation=owner=") {
			owner = strings.TrimPrefix(arg, "--correlation=owner=")
		}
	}
	if owner == "" {
		t.Fatalf("Jinushi run command omitted correlation owner: %v", args)
	}
	return []byte(fmt.Sprintf(`{"version":1,"run":{"runId":%q,"state":%q,"generation":1,"createdAt":"2026-10-07T00:00:00Z","spec":{"correlation":{"owner":%q}}}}`, id, state, owner))
}

func TestLifecycleActionsPersistDesiredIntentAndReload(t *testing.T) {
	tests := []struct {
		name        string
		action      string
		initial     registry.DesiredState
		wantDesired registry.DesiredState
		wantProcess string
		runState    string
	}{
		{name: "start sets running", action: "start", initial: registry.DesiredStopped, wantDesired: registry.DesiredRunning, wantProcess: "starting"},
		{name: "start preserves intent after immediate exit", action: "start", initial: registry.DesiredStopped, wantDesired: registry.DesiredRunning, wantProcess: "stopped", runState: "terminal"},
		{name: "restart preserves running intent", action: "restart", initial: registry.DesiredRunning, wantDesired: registry.DesiredRunning, wantProcess: "starting"},
		{name: "stop sets stopped", action: "stop", initial: registry.DesiredRunning, wantDesired: registry.DesiredStopped, wantProcess: "stopped"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, client, store := lifecycleRuntime(t, test.initial)
			if test.action == "stop" || test.action == "restart" || test.runState != "" {
				owner := r.services[key("env", "svc")].CorrelationOwner()
				liveRun := fmt.Sprintf(`{"runId":"run-live","state":"running","generation":2,"createdAt":"2026-10-07T00:00:00Z","spec":{"correlation":{"owner":%q}}}`, owner)
				terminalRun := fmt.Sprintf(`{"runId":"run-live","state":"terminal","generation":3,"createdAt":"2026-10-07T00:00:00Z","spec":{"correlation":{"owner":%q}},"receipt":{"outcome":"cancelled"}}`, owner)
				client.fn = func(args []string) (ssh.Result, error) {
					switch args[1] {
					case "status":
						return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
					case "list":
						if test.action == "start" {
							return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
						}
						return ssh.Result{Stdout: []byte(`{"version":1,"runs":[` + liveRun + `],"nextCursor":""}`)}, nil
					case "inspect":
						return ssh.Result{Stdout: []byte(`{"version":1,"run":` + liveRun + `}`)}, nil
					case "cancel":
						return ssh.Result{Stdout: []byte(`{"version":1}`)}, nil
					case "await":
						return ssh.Result{Stdout: []byte(`{"version":1,"run":` + terminalRun + `}`)}, nil
					case "run":
						if test.action == "restart" {
							return ssh.Result{Stdout: lifecycleRunResponse(t, args, "run-restarted", "accepted")}, nil
						}
						if test.action == "start" {
							state := test.runState
							if state == "" {
								state = "accepted"
							}
							return ssh.Result{Stdout: lifecycleRunResponse(t, args, "run-new", state)}, nil
						}
						return ssh.Result{}, errors.New("Stop unexpectedly started a Run")
					default:
						return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %v", args)
					}
				}
			}

			var service Service
			var err error
			switch test.action {
			case "start":
				service, err = r.Start(context.Background(), "env", "svc")
			case "restart":
				service, err = r.Restart(context.Background(), "env", "svc")
			case "stop":
				service, err = r.Stop(context.Background(), "env", "svc")
			}
			if err != nil {
				t.Fatalf("%s(): %v", test.action, err)
			}
			if service.DesiredState != test.wantDesired || string(service.Process) != test.wantProcess {
				t.Fatalf("%s() = %#v; want desired=%q process=%q", test.action, service, test.wantDesired, test.wantProcess)
			}
			current := r.Snapshot().Environments[0].Services[0]
			if current.DesiredState != test.wantDesired || string(current.Process) != test.wantProcess {
				t.Fatalf("runtime projection = %#v; want desired=%q process=%q", current, test.wantDesired, test.wantProcess)
			}
			saved, err := store.Load()
			if err != nil || saved.Services()[0].DesiredState != test.wantDesired {
				t.Fatalf("persisted desired state = %#v, err=%v; want %q", saved, err, test.wantDesired)
			}

			manager, err := tunnel.NewManager(noLauncher{})
			if err != nil {
				t.Fatal(err)
			}
			reloaded, err := Compose(saved, client, manager)
			if err != nil {
				t.Fatal(err)
			}
			defer reloaded.Close(context.Background())
			if got := reloaded.Snapshot().Environments[0].Services[0].DesiredState; got != test.wantDesired {
				t.Fatalf("reloaded desired state = %q, want %q", got, test.wantDesired)
			}
		})
	}
}

func TestFailedAmbiguousAndUncertainActionsDoNotCommitDesiredState(t *testing.T) {
	tests := []struct {
		name        string
		action      string
		initial     registry.DesiredState
		failCommand string
		runState    string
	}{
		{name: "start rejected", action: "start", initial: registry.DesiredStopped, failCommand: "run"},
		{name: "restart transport ambiguous", action: "restart", initial: registry.DesiredStopped, failCommand: "run"},
		{name: "stop transport ambiguous", action: "stop", initial: registry.DesiredRunning, failCommand: "list"},
		{name: "start uncertain Run response", action: "start", initial: registry.DesiredStopped, failCommand: "run", runState: "uncertain"},
		{name: "start unknown Run response", action: "start", initial: registry.DesiredStopped, failCommand: "run", runState: "future-state"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, client, store := lifecycleRuntime(t, test.initial)
			client.fn = func(args []string) (ssh.Result, error) {
				if len(args) == 1 && args[0] == "true" {
					return ssh.Result{ExitCode: 0}, nil
				}
				if len(args) < 2 {
					return ssh.Result{}, fmt.Errorf("unexpected SSH command %v", args)
				}
				if args[1] == test.failCommand {
					if test.name == "start rejected" {
						return ssh.Result{Stdout: []byte(`{"version":1,"error":{"code":"invalid-run","message":"rejected"}}`)}, nil
					}
					if test.runState != "" {
						return ssh.Result{Stdout: lifecycleRunResponse(t, args, "run-uncertain", test.runState)}, nil
					}
					return ssh.Result{ExitCode: -1}, &ssh.Error{Kind: ssh.FailureTransport, Err: errors.New("lost response")}
				}
				if len(args) == 1 && args[0] == "true" {
					return ssh.Result{ExitCode: 0}, nil
				}
				switch args[1] {
				case "status":
					return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
				case "list":
					return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
				case "run":
					return ssh.Result{Stdout: lifecycleRunResponse(t, args, "run-new", "accepted")}, nil
				default:
					return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %v", args)
				}
			}

			var err error
			switch test.action {
			case "start":
				_, err = r.Start(context.Background(), "env", "svc")
			case "restart":
				_, err = r.Restart(context.Background(), "env", "svc")
			case "stop":
				_, err = r.Stop(context.Background(), "env", "svc")
			}
			if err == nil {
				t.Fatalf("%s() succeeded despite failed or uncertain Jinushi evidence", test.action)
			}
			if got := r.Snapshot().Environments[0].Services[0].DesiredState; got != test.initial {
				t.Fatalf("runtime desired state = %q, want unchanged %q", got, test.initial)
			}
			saved, loadErr := store.Load()
			if loadErr != nil || saved.Services()[0].DesiredState != test.initial {
				t.Fatalf("persisted desired state = %#v, err=%v; want unchanged %q", saved, loadErr, test.initial)
			}
		})
	}
}

func TestDesiredStatePersistenceFailureKeepsPublishedRegistry(t *testing.T) {
	r, _, store := lifecycleRuntime(t, registry.DesiredStopped)
	root := filepath.Dir(store.Path())
	backup := root + "-saved"
	if err := os.Rename(root, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("blocks registry directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Start(context.Background(), "env", "svc"); err == nil {
		t.Fatal("Start() reported success when desired state could not be persisted")
	} else {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Code != "lifecycle-conflict" || !strings.Contains(failure.Evidence, "could not be saved") {
			t.Fatalf("Start() persistence failure = %v, want actionable lifecycle conflict", err)
		}
	}
	if got := r.Snapshot().Environments[0].Services[0].DesiredState; got != registry.DesiredStopped {
		t.Fatalf("runtime desired state = %q after failed save, want previous stopped state", got)
	}
	previous, err := config.NewStore(backup)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := previous.Load()
	if err != nil || saved.Services()[0].DesiredState != registry.DesiredStopped {
		t.Fatalf("previous registry desired state = %#v, err=%v; want stopped", saved, err)
	}
}

func TestPollUsesUpdatedDesiredStateForHealthTunnelPolicy(t *testing.T) {
	r, client, launcher := runtimeWithTrackingTunnels(t, 2000)
	defer r.Close(context.Background())
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(r.registry); err != nil {
		t.Fatal(err)
	}
	r.store = store
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("initial Poll(): %v", err)
	}
	if len(launcher.processes) != 1 {
		t.Fatalf("initial health tunnel launches = %d, want 1", len(launcher.processes))
	}
	client.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 0}, nil
		}
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		default:
			return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %v", args)
		}
	}
	if _, err := r.Stop(context.Background(), "env", "svc"); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if got := r.Snapshot().Environments[0].Services[0].DesiredState; got != registry.DesiredStopped {
		t.Fatalf("Stop() desired state = %q, want stopped", got)
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() after Stop: %v", err)
	}
	if len(launcher.processes) != 1 {
		t.Fatalf("Poll() relaunched a stopped service health tunnel: launch count=%d", len(launcher.processes))
	}
	identity := tunnel.Identity{Environment: "env", Service: "svc", Endpoint: "health"}
	if snapshot, err := r.tunnels.Get(identity); err != nil || snapshot.State != tunnel.StateStopped {
		t.Fatalf("health tunnel after stopped-state Poll = %#v, err=%v; want stopped", snapshot, err)
	}
}

func TestStartDesiredStateCausesPollToRecreateHealthTunnel(t *testing.T) {
	r, client, launcher := runtimeWithTrackingTunnels(t, 2000)
	defer r.Close(context.Background())
	services := r.registry.Services()
	services[0].DesiredState = registry.DesiredStopped
	stopped, err := registry.NewSnapshot(r.registry.Environments(), services)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.configure(stopped); err != nil {
		t.Fatal(err)
	}
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(stopped); err != nil {
		t.Fatal(err)
	}
	r.store = store
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() while stopped: %v", err)
	}
	if len(launcher.processes) != 0 {
		t.Fatalf("stopped service opened %d health tunnels during Poll", len(launcher.processes))
	}
	client.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 0}, nil
		}
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		case "run":
			return ssh.Result{Stdout: lifecycleRunResponse(t, args, "run-started", "accepted")}, nil
		default:
			return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %v", args)
		}
	}
	if _, err := r.Start(context.Background(), "env", "svc"); err != nil {
		t.Fatalf("Start(): %v", err)
	}
	identity := tunnel.Identity{Environment: "env", Service: "svc", Endpoint: "health"}
	if len(launcher.processes) != 1 {
		t.Fatalf("Start() health tunnel launches = %d, want 1", len(launcher.processes))
	}
	if _, err := r.tunnels.Stop(context.Background(), identity); err != nil {
		t.Fatalf("stop test-owned health tunnel: %v", err)
	}
	if err := r.Poll(context.Background()); err != nil {
		t.Fatalf("Poll() after Start: %v", err)
	}
	if got := r.Snapshot().Environments[0].Services[0].DesiredState; got != registry.DesiredRunning {
		t.Fatalf("desired state after Start and Poll = %q, want running", got)
	}
	if len(launcher.processes) != 2 {
		t.Fatalf("Poll() did not recreate health tunnel for running intent: launch count=%d, want 2", len(launcher.processes))
	}
}

func TestSnapshotRemainsResponsiveDuringLifecycleRemoteIO(t *testing.T) {
	r, client, _ := lifecycleRuntime(t, registry.DesiredStopped)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	base := client.fn
	client.fn = func(args []string) (ssh.Result, error) {
		if args[1] == "run" {
			close(entered)
			<-release
		}
		return base(args)
	}

	started := make(chan error, 1)
	startFinished := false
	go func() {
		_, err := r.Start(context.Background(), "env", "svc")
		started <- err
	}()
	<-entered
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !startFinished {
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Error("blocked Start did not finish during cleanup")
			}
		}
	})
	observed := make(chan State, 1)
	go func() { observed <- r.Snapshot() }()
	select {
	case snapshot := <-observed:
		if got := snapshot.Environments[0].Services[0].DesiredState; got != registry.DesiredStopped {
			t.Fatalf("Snapshot() desired state during unresolved Start = %q, want stopped", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot() waited for remote Start I/O")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-started; err != nil {
		startFinished = true
		t.Fatalf("Start(): %v", err)
	}
	startFinished = true
	if got := r.Snapshot().Environments[0].Services[0].DesiredState; got != registry.DesiredRunning {
		t.Fatalf("Snapshot() desired state = %q after Start, want running", got)
	}
}

func TestSlowPollDoesNotBlockSnapshotOrUnrelatedServiceControl(t *testing.T) {
	r, client, _ := lifecycleRuntime(t, registry.DesiredStopped)
	fast := r.registry.Services()[0]
	fast.ID = "fast"
	fast.Execution.Argv = []string{"fast-service"}
	fast.Execution.CWD = "/work/fast"
	if err := r.AddService(fast); err != nil {
		t.Fatalf("AddService(fast): %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	base := client.fn
	client.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			close(entered)
			<-release // Simulate a late transport return after Runtime cancels Poll.
			return ssh.Result{ExitCode: 0}, nil
		}
		return base(args)
	}
	pollDone := make(chan error, 1)
	go func() { pollDone <- r.Poll(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Poll did not reach the blocked connectivity probe")
	}
	var startDone chan error
	pollFinished := false
	startFinished := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !pollFinished {
			select {
			case <-pollDone:
			case <-time.After(3 * time.Second):
				t.Error("blocked Poll did not finish during cleanup")
			}
		}
		if startDone != nil && !startFinished {
			select {
			case <-startDone:
			case <-time.After(3 * time.Second):
				t.Error("unrelated Start did not finish during cleanup")
			}
		}
	})

	snapshotDone := make(chan State, 1)
	go func() { snapshotDone <- r.Snapshot() }()
	select {
	case snapshot := <-snapshotDone:
		if len(snapshot.Environments[0].Services) != 2 {
			t.Fatalf("Snapshot() services = %d, want 2", len(snapshot.Environments[0].Services))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot() waited for slow Poll I/O")
	}

	startDone = make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), "env", "fast")
		startDone <- err
	}()
	select {
	case err := <-startDone:
		startFinished = true
		if err != nil {
			t.Fatalf("Start(fast) during slow Poll: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated service Start waited for slow Poll I/O")
	}

	releaseOnce.Do(func() { close(release) })
	if err := <-pollDone; err != nil {
		pollFinished = true
		t.Fatalf("superseded Poll() error = %v, want a discarded observation", err)
	}
	pollFinished = true
	for _, service := range r.Snapshot().Environments[0].Services {
		if service.ID == "fast" && service.DesiredState != registry.DesiredRunning {
			t.Fatalf("fast service desired state = %q after Start", service.DesiredState)
		}
	}
}

func waitForActiveOperations(t *testing.T, r *Runtime, want int) {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		active := r.activeOps
		r.mu.Unlock()
		if active >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("active operations = %d, want at least %d", active, want)
		case <-ticker.C:
		}
	}
}

type blockingRuntimeLauncher struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *blockingRuntimeLauncher) Start(context.Context, tunnel.ForwardSpec) (tunnel.Process, error) {
	l.once.Do(func() { close(l.entered) })
	<-l.release
	return newTrackingProcess(), nil
}

func TestPollStartedDuringLifecycleActionCannotRestoreOldEndpointFailure(t *testing.T) {
	snapshot, err := registry.NewSnapshot(
		[]registry.Environment{{ID: "env", SSHHost: "host", Jinushi: registry.JinushiDefinition{StateDir: "/state"}}},
		[]registry.Service{{
			ID: "svc", EnvironmentID: "env", DesiredState: registry.DesiredStopped,
			Execution: registry.ExecutionIntent{Argv: []string{"service"}, CWD: "/work", Lifetime: registry.LifetimeDetached},
			Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "health", Path: "/ready"},
			Endpoints: []registry.Endpoint{
				{ID: "health", Label: "Health", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1234},
				{ID: "ui", Label: "UI", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1235},
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeSSH{fn: func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 0}, nil
		}
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		case "run":
			return ssh.Result{Stdout: lifecycleRunResponse(t, args, "run-new", "accepted")}, nil
		default:
			return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %v", args)
		}
	}}
	launcher := &blockingRuntimeLauncher{entered: make(chan struct{}), release: make(chan struct{})}
	manager, err := tunnel.NewManager(launcher)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compose(snapshot, client, manager)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(launcher.release) })
		_ = r.Close(context.Background())
	})
	identity := tunnel.Identity{Environment: "env", Service: "svc", Endpoint: "health"}
	r.mu.Lock()
	r.endpointFailures[identity] = "Dynamic endpoint evidence is stale or conflicting."
	r.mu.Unlock()
	startDone := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), "env", "svc")
		startDone <- err
	}()
	select {
	case <-launcher.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not reach health tunnel setup after committing running intent")
	}
	pollDone := make(chan error, 1)
	go func() { pollDone <- r.Poll(context.Background()) }()
	waitForActiveOperations(t, r, 2)
	deadline := time.NewTimer(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	for {
		r.mu.Lock()
		started := r.pollCancel != nil
		r.mu.Unlock()
		if started {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("late Poll did not capture its candidate state")
		case <-ticker.C:
		}
	}
	ticker.Stop()
	deadline.Stop()
	releaseOnce.Do(func() { close(launcher.release) })
	if err := <-startDone; err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := <-pollDone; err != nil {
		t.Fatalf("late Poll(): %v; internally superseded observations should be discarded", err)
	}
	r.mu.Lock()
	failure := r.endpointFailures[identity]
	r.mu.Unlock()
	if failure != "" {
		t.Fatalf("late Poll restored endpoint failure %q after successful tunnel setup", failure)
	}
}

func TestConcurrentRestartRetryKeepsOneSubmissionAndRun(t *testing.T) {
	r, client, _ := lifecycleRuntime(t, registry.DesiredRunning)
	owner := r.services[key("env", "svc")].CorrelationOwner()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = r.Close(context.Background())
	})
	runs := map[string]string{}
	createdBySubmission := map[string]string{}
	currentRun := ""
	var submissions []string
	physicalRuns := 0
	runCalls := 0
	client.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			return ssh.Result{ExitCode: 0}, nil
		}
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		case "list":
			if currentRun == "" {
				return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
			}
			return ssh.Result{Stdout: []byte(fmt.Sprintf(`{"version":1,"runs":[%s],"nextCursor":""}`, runs[currentRun]))}, nil
		case "run":
			var submissionID string
			for _, arg := range args {
				if strings.HasPrefix(arg, "--submission-id=") {
					submissionID = strings.TrimPrefix(arg, "--submission-id=")
				}
			}
			submissions = append(submissions, submissionID)
			id, exists := createdBySubmission[submissionID]
			if !exists {
				id = "run-new"
				createdBySubmission[submissionID] = id
				physicalRuns++
			}
			runs[id] = fmt.Sprintf(`{"runId":%q,"state":"accepted","generation":1,"createdAt":"2026-10-07T00:00:00Z","spec":{"correlation":{"owner":%q}}}`, id, owner)
			currentRun = id
			runCalls++
			if runCalls == 1 {
				close(entered)
				<-release
				return ssh.Result{ExitCode: -1}, &ssh.Error{Kind: ssh.FailureTransport, Err: errors.New("lost response after remote submission")}
			}
			return ssh.Result{Stdout: lifecycleRunResponse(t, args, id, "accepted")}, nil
		default:
			return ssh.Result{}, fmt.Errorf("unexpected Jinushi command %v", args)
		}
	}
	type result struct {
		service Service
		err     error
	}
	firstDone := make(chan result, 1)
	go func() {
		service, err := r.Restart(context.Background(), "env", "svc")
		firstDone <- result{service, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first Restart did not reach its ambiguous submission")
	}
	secondDone := make(chan result, 1)
	go func() {
		service, err := r.Restart(context.Background(), "env", "svc")
		secondDone <- result{service, err}
	}()
	waitForActiveOperations(t, r, 2)
	releaseOnce.Do(func() { close(release) })
	select {
	case first := <-firstDone:
		if first.err == nil {
			t.Fatal("first Restart succeeded despite the lost response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first Restart did not return after the response loss")
	}
	select {
	case second := <-secondDone:
		if second.err != nil || second.service.Process != "starting" {
			t.Fatalf("serialized retry Restart() = %#v, %v; want accepted attempted Run", second.service, second.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Restart did not reconcile the retained attempt")
	}
	if physicalRuns != 1 || len(submissions) != 2 || submissions[0] == "" || submissions[0] != submissions[1] {
		t.Fatalf("physical Runs=%d, submission IDs=%v; want one Run under one retained submission", physicalRuns, submissions)
	}
}

func TestEnsureWaiterCanBeCanceledBehindLifecycleAction(t *testing.T) {
	r, client, _ := lifecycleRuntime(t, registry.DesiredStopped)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	base := client.fn
	client.fn = func(args []string) (ssh.Result, error) {
		if len(args) > 1 && args[1] == "run" {
			close(entered)
			<-release
		}
		return base(args)
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), "env", "svc")
		startDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not reach blocked Jinushi I/O")
	}
	ctx, cancel := context.WithCancel(context.Background())
	startFinished := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		if !startFinished {
			select {
			case <-startDone:
			case <-time.After(3 * time.Second):
				t.Error("blocked Start did not finish during cleanup")
			}
		}
		_ = r.Close(context.Background())
	})
	ensureDone := make(chan error, 1)
	go func() {
		_, err := r.Ensure(ctx, "env", "svc", "ui")
		ensureDone <- err
	}()
	waitForActiveOperations(t, r, 2)
	cancel()
	select {
	case err := <-ensureDone:
		var failure *Failure
		if !errors.As(err, &failure) || failure.Code != "operation-canceled" {
			t.Fatalf("canceled Ensure() = %v; want operation-canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ensure did not leave the service gate after cancellation")
	}
	identity := tunnel.Identity{Environment: "env", Service: "svc", Endpoint: "ui"}
	if _, err := r.tunnels.Get(identity); err == nil {
		t.Fatal("canceled Ensure started endpoint work after its service gate was canceled")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-startDone; err != nil {
		startFinished = true
		t.Fatalf("Start(): %v", err)
	}
	startFinished = true
}

func TestEnsureJinushiDoesNotPublishAfterRegistryReplacement(t *testing.T) {
	environments := []registry.Environment{{ID: "env", SSHHost: "host", Jinushi: registry.JinushiDefinition{StateDir: "/state"}}}
	empty, err := registry.NewSnapshot(environments, nil)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	client := &fakeSSH{fn: func(args []string) (ssh.Result, error) {
		if len(args) > 1 && args[1] == "status" {
			close(entered)
			<-release
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		}
		return ssh.Result{}, fmt.Errorf("unexpected SSH command %v", args)
	}}
	manager, err := tunnel.NewManager(noLauncher{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compose(empty, client, manager)
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(empty); err != nil {
		t.Fatal(err)
	}
	r.store = store
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		_ = r.Close(context.Background())
	})
	ensureDone := make(chan error, 1)
	go func() { ensureDone <- r.EnsureJinushi(context.Background(), "env") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureJinushi did not reach delayed remote status")
	}
	updated, err := registry.NewSnapshot(environments, []registry.Service{{
		ID: "svc", EnvironmentID: "env", DesiredState: registry.DesiredStopped,
		Execution: registry.ExecutionIntent{Argv: []string{"service"}, CWD: "/work", Lifetime: registry.LifetimeDetached},
		Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "health", Path: "/ready"},
		Endpoints: []registry.Endpoint{{ID: "health", Label: "Health", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1234}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Register(updated); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-ensureDone; err != nil {
		t.Fatalf("EnsureJinushi(): %v", err)
	}
	if got := r.Snapshot().Environments[0].Jinushi; got != "unknown" {
		t.Fatalf("late EnsureJinushi result published %q after Register; want unknown", got)
	}
}

func TestCloseBoundsWaitForBlockedLifecycleOperation(t *testing.T) {
	r, client, _ := lifecycleRuntime(t, registry.DesiredStopped)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	base := client.fn
	client.fn = func(args []string) (ssh.Result, error) {
		if len(args) > 1 && args[1] == "run" {
			close(entered)
			<-release // Deliberately ignores Runtime cancellation until released.
		}
		return base(args)
	}
	startDone := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), "env", "svc")
		startDone <- err
	}()
	startFinished := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		if !startFinished {
			select {
			case <-startDone:
			case <-time.After(3 * time.Second):
				t.Error("blocked Start did not finish during cleanup")
			}
		}
		_ = r.Close(context.Background())
	})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not reach blocked Jinushi I/O")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() = %v; want caller deadline while the remote operation is blocked", err)
	}
	snapshotDone := make(chan State, 1)
	go func() { snapshotDone <- r.Snapshot() }()
	select {
	case snapshot := <-snapshotDone:
		if got := snapshot.Environments[0].Services[0].DesiredState; got != registry.DesiredStopped {
			t.Fatalf("closed Snapshot desired state = %q; want no post-Close publication", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Snapshot waited on the blocked lifecycle operation after Close")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-startDone; err == nil {
		startFinished = true
		t.Fatal("Start succeeded and published after Runtime Close")
	}
	startFinished = true
}

type lateStartingRuntimeLauncher struct {
	entered  chan struct{}
	release  chan struct{}
	process  *trackingProcess
	startOne sync.Once
}

func (l *lateStartingRuntimeLauncher) Start(context.Context, tunnel.ForwardSpec) (tunnel.Process, error) {
	l.startOne.Do(func() { close(l.entered) })
	<-l.release
	return l.process, nil
}

func TestCloseReapsTunnelWhoseLauncherSucceedsAfterCloseDeadline(t *testing.T) {
	snapshot, err := registry.NewSnapshot(
		[]registry.Environment{{ID: "env", SSHHost: "host", Jinushi: registry.JinushiDefinition{StateDir: "/state"}}},
		[]registry.Service{{
			ID: "svc", EnvironmentID: "env", DesiredState: registry.DesiredStopped,
			Execution: registry.ExecutionIntent{Argv: []string{"service"}, CWD: "/work", Lifetime: registry.LifetimeDetached},
			Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "health", Path: "/ready"},
			Endpoints: []registry.Endpoint{
				{ID: "health", Label: "Health", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1234},
				{ID: "ui", Label: "UI", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: 1235},
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeSSH{fn: func([]string) (ssh.Result, error) { return ssh.Result{}, errors.New("unexpected SSH call") }}
	launcher := &lateStartingRuntimeLauncher{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		process: newTrackingProcess(),
	}
	manager, err := tunnel.NewManager(launcher)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compose(snapshot, client, manager)
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	ensureDone := make(chan error, 1)
	go func() {
		_, err := r.Ensure(context.Background(), "env", "svc", "ui")
		ensureDone <- err
	}()
	ensureFinished := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(launcher.release) })
		if !ensureFinished {
			select {
			case <-ensureDone:
			case <-time.After(3 * time.Second):
				t.Error("Ensure did not finish during cleanup")
			}
		}
		_ = r.Close(context.Background())
	})
	select {
	case <-launcher.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Ensure did not reach the blocked tunnel launcher")
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close() = %v; want caller deadline while tunnel launch is blocked", err)
	}
	releaseOnce.Do(func() { close(launcher.release) })
	select {
	case err := <-ensureDone:
		ensureFinished = true
		if err == nil {
			t.Fatal("Ensure succeeded after Runtime Close canceled its tunnel launch")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ensure did not finish after the late launcher returned")
	}
	select {
	case <-launcher.process.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("late tunnel process was not stopped and reaped")
	}
	identity := tunnel.Identity{Environment: "env", Service: "svc", Endpoint: "ui"}
	got, err := r.tunnels.Get(identity)
	if err != nil || got.State != tunnel.StateStopped {
		t.Fatalf("late tunnel state = %#v, %v; want stopped with no owned ready tunnel", got, err)
	}
}

func TestRunContinuesAfterInternallySupersededPoll(t *testing.T) {
	r, client, _ := lifecycleRuntime(t, registry.DesiredStopped)
	fast := r.registry.Services()[0]
	fast.ID = "fast"
	fast.Execution.Argv = []string{"fast-service"}
	fast.Execution.CWD = "/work/fast"
	if err := r.AddService(fast); err != nil {
		t.Fatalf("AddService(fast): %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	base := client.fn
	client.fn = func(args []string) (ssh.Result, error) {
		if len(args) == 1 && args[0] == "true" {
			close(entered)
			<-release
			return ssh.Result{ExitCode: 0}, nil
		}
		return base(args)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(runCtx) }()
	runFinished := false
	t.Cleanup(func() {
		cancelRun()
		releaseOnce.Do(func() { close(release) })
		if !runFinished {
			select {
			case <-runDone:
			case <-time.After(3 * time.Second):
				t.Error("Run did not stop during cleanup")
			}
		}
		_ = r.Close(context.Background())
	})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not begin its slow Poll")
	}
	if _, err := r.Start(context.Background(), "env", "fast"); err != nil {
		t.Fatalf("unrelated Start during slow Run Poll: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
	pollRelease, err := acquireGate(waitCtx, r.pollGate)
	cancelWait()
	if err != nil {
		t.Fatalf("wait for superseded Poll completion: %v", err)
	}
	pollRelease()
	select {
	case err := <-runDone:
		t.Fatalf("Run exited after an internally superseded Poll: %v", err)
	default:
	}
	cancelRun()
	if err := <-runDone; err != nil {
		runFinished = true
		t.Fatalf("Run after caller cancellation: %v", err)
	}
	runFinished = true
}
