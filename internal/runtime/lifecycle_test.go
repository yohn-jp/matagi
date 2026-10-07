package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if submissions[0] != submissions[1] || submissions[1] != submissions[2] || r.pending[key("env", "svc")] != "" {
		t.Fatal(submissions)
	}
	_, err := r.Start(ctx, "env", "svc")
	if err != nil || submissions[3] == submissions[2] {
		t.Fatal(submissions, err)
	}
	_ = r.Close(ctx)
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

func TestDesiredStateMutationSerializesSnapshotWithLifecycleAction(t *testing.T) {
	r, client, _ := lifecycleRuntime(t, registry.DesiredStopped)
	entered := make(chan struct{})
	release := make(chan struct{})
	base := client.fn
	client.fn = func(args []string) (ssh.Result, error) {
		if args[1] == "run" {
			close(entered)
			<-release
		}
		return base(args)
	}

	started := make(chan error, 1)
	go func() {
		_, err := r.Start(context.Background(), "env", "svc")
		started <- err
	}()
	<-entered
	observed := make(chan registry.DesiredState, 1)
	snapshotStarted := make(chan struct{})
	go func() {
		close(snapshotStarted)
		observed <- r.Snapshot().Environments[0].Services[0].DesiredState
	}()
	<-snapshotStarted
	early := false
	select {
	case <-observed:
		early = true
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-started; err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if early {
		t.Fatal("Snapshot() returned while Start was unresolved")
	}
	if got := <-observed; got != registry.DesiredRunning {
		t.Fatalf("Snapshot() desired state = %q after Start, want running", got)
	}
}
