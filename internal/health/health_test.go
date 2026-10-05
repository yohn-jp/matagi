package health

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollAggregatesDeterministicIndependentStates(t *testing.T) {
	observer := mustObserver(t,
		[]EnvironmentTarget{
			{ID: "zeta", Services: []ServiceTarget{
				{ID: "worker", Endpoints: []EndpointTarget{{ID: "z-endpoint"}, {ID: "a-endpoint"}}},
				{ID: "api", Endpoints: []EndpointTarget{{ID: "ui"}}},
				{ID: "unhealthy"},
			}},
			{ID: "alpha", Services: []ServiceTarget{{ID: "stopped"}}},
		},
		Probes{
			Connectivity: func(context.Context, string) (ConnectivityState, error) { return ConnectivityConnected, nil },
			Process: func(_ context.Context, _, serviceID string) (ProcessState, error) {
				switch serviceID {
				case "stopped":
					return ProcessStopped, nil
				case "worker":
					return ProcessStarting, nil
				default:
					return ProcessRunning, nil
				}
			},
			Readiness: func(_ context.Context, _, serviceID string) (ReadinessState, error) {
				switch serviceID {
				case "api":
					return ReadinessReady, nil
				case "worker":
					return ReadinessNotReady, nil
				case "unhealthy":
					return ReadinessUnhealthy, nil
				default:
					return ReadinessUnknown, nil
				}
			},
			Endpoint: func(_ context.Context, _, _, endpointID string) (EndpointState, error) {
				if endpointID == "ui" {
					return EndpointUnavailable, nil
				}
				return EndpointAvailable, nil
			},
		},
		Options{PollInterval: time.Second, ProbeTimeout: time.Second},
	)

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := observer.Snapshot()
	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := observer.Snapshot()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same probe results produced different snapshots:\nfirst:  %#v\nsecond: %#v", first, second)
	}
	if got, want := []string{first.Environments[0].ID, first.Environments[1].ID}, []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("environment order = %v, want %v", got, want)
	}
	alpha := first.Environments[0]
	if got := alpha.Services[0].State; got != ServiceStopped {
		t.Fatalf("stopped service state = %q, want %q", got, ServiceStopped)
	}
	zeta := first.Environments[1]
	if got, want := []string{zeta.Services[0].ID, zeta.Services[1].ID, zeta.Services[2].ID}, []string{"api", "unhealthy", "worker"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("service order = %v, want %v", got, want)
	}
	api := zeta.Services[0]
	if api.State != ServiceReady || api.Endpoints[0].State != EndpointUnavailable {
		t.Fatalf("service readiness and endpoint state were conflated: service=%q endpoint=%q", api.State, api.Endpoints[0].State)
	}
	if unhealthy := zeta.Services[1]; unhealthy.State != ServiceUnhealthy || unhealthy.Readiness != ReadinessUnhealthy {
		t.Fatalf("unhealthy service observation = %#v", unhealthy)
	}
	worker := zeta.Services[2]
	if worker.State != ServiceStarting || worker.Process != ProcessStarting || worker.Readiness != ReadinessNotReady {
		t.Fatalf("worker observation = %#v, want starting with not-ready evidence", worker)
	}
	if got, want := []string{worker.Endpoints[0].ID, worker.Endpoints[1].ID}, []string{"a-endpoint", "z-endpoint"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoint order = %v, want %v", got, want)
	}
}

func TestUnreachableEnvironmentDoesNotFabricateRemoteState(t *testing.T) {
	processCalled := false
	readinessCalled := false
	endpointCalled := false
	observer := mustObserver(t,
		[]EnvironmentTarget{{ID: "dev", Services: []ServiceTarget{{ID: "api", Endpoints: []EndpointTarget{{ID: "dashboard"}}}}}},
		Probes{
			Connectivity: func(context.Context, string) (ConnectivityState, error) { return ConnectivityUnreachable, nil },
			Process: func(context.Context, string, string) (ProcessState, error) {
				processCalled = true
				return ProcessRunning, nil
			},
			Readiness: func(context.Context, string, string) (ReadinessState, error) {
				readinessCalled = true
				return ReadinessReady, nil
			},
			Endpoint: func(context.Context, string, string, string) (EndpointState, error) {
				endpointCalled = true
				return EndpointUnavailable, nil
			},
		},
		Options{PollInterval: time.Second, ProbeTimeout: time.Second},
	)

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := observer.Snapshot().Environments[0]
	if got.State != ConnectivityUnreachable || got.Services[0].State != ServiceUnknown {
		t.Fatalf("unreachable observation fabricated service state: %#v", got)
	}
	if got.Services[0].Process != ProcessUnknown || got.Services[0].Readiness != ReadinessUnknown {
		t.Fatalf("unreachable environment produced remote observations: %#v", got.Services[0])
	}
	if processCalled || readinessCalled {
		t.Fatal("remote probes ran while the environment was unreachable")
	}
	if !endpointCalled || got.Services[0].Endpoints[0].State != EndpointUnavailable {
		t.Fatalf("local endpoint observation was not kept independent: %#v", got.Services[0].Endpoints)
	}
}

func TestProbeErrorsRemainOnTheirOwnObservation(t *testing.T) {
	observer := mustObserver(t,
		[]EnvironmentTarget{{ID: "dev", Services: []ServiceTarget{{ID: "api", Endpoints: []EndpointTarget{{ID: "dashboard"}}}}}},
		Probes{
			Connectivity: func(context.Context, string) (ConnectivityState, error) { return ConnectivityConnected, nil },
			Process: func(context.Context, string, string) (ProcessState, error) {
				return ProcessUnknown, errors.New("Jinushi status unavailable")
			},
			Readiness: func(context.Context, string, string) (ReadinessState, error) { return ReadinessReady, nil },
			Endpoint: func(context.Context, string, string, string) (EndpointState, error) {
				return EndpointUnknown, errors.New("local connection refused")
			},
		},
		Options{PollInterval: time.Second, ProbeTimeout: time.Second},
	)

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	environment := observer.Snapshot().Environments[0]
	service := environment.Services[0]
	if environment.State != ConnectivityConnected || environment.Error != "" {
		t.Fatalf("process error changed connectivity observation: %#v", environment)
	}
	if service.Process != ProcessError || service.ProcessError != "Jinushi status unavailable" || service.State != ServiceReady {
		t.Fatalf("process failure was not represented separately: %#v", service)
	}
	if service.Readiness != ReadinessReady || service.ReadinessError != "" {
		t.Fatalf("process error overwrote readiness evidence: %#v", service)
	}
	endpoint := service.Endpoints[0]
	if endpoint.State != EndpointError || endpoint.Error != "local connection refused" {
		t.Fatalf("endpoint failure was not represented separately: %#v", endpoint)
	}
}

func TestProbeTimeoutBecomesObservationError(t *testing.T) {
	observer := mustObserver(t,
		[]EnvironmentTarget{{ID: "dev", Services: []ServiceTarget{{ID: "api", Endpoints: []EndpointTarget{{ID: "dashboard"}}}}}},
		Probes{
			Connectivity: func(context.Context, string) (ConnectivityState, error) { return ConnectivityConnected, nil },
			Process: func(ctx context.Context, _, _ string) (ProcessState, error) {
				<-ctx.Done()
				return ProcessUnknown, ctx.Err()
			},
			Readiness: func(context.Context, string, string) (ReadinessState, error) { return ReadinessNotReady, nil },
			Endpoint:  func(context.Context, string, string, string) (EndpointState, error) { return EndpointAvailable, nil },
		},
		Options{PollInterval: time.Second, ProbeTimeout: 10 * time.Millisecond},
	)

	if err := observer.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := observer.Snapshot().Environments[0].Services[0]
	if service.Process != ProcessError || !strings.Contains(service.ProcessError, context.DeadlineExceeded.Error()) {
		t.Fatalf("timed out process probe = %#v", service)
	}
	if service.Readiness != ReadinessNotReady || service.Endpoints[0].State != EndpointAvailable {
		t.Fatalf("process timeout contaminated other observations: %#v", service)
	}
}

func TestParentCancellationDoesNotPublishPartialSnapshot(t *testing.T) {
	started := make(chan struct{})
	observer := mustObserver(t,
		[]EnvironmentTarget{{ID: "dev", Services: []ServiceTarget{{ID: "api"}}}},
		Probes{
			Connectivity: func(ctx context.Context, _ string) (ConnectivityState, error) {
				close(started)
				<-ctx.Done()
				return ConnectivityUnknown, ctx.Err()
			},
			Process:   func(context.Context, string, string) (ProcessState, error) { return ProcessRunning, nil },
			Readiness: func(context.Context, string, string) (ReadinessState, error) { return ReadinessReady, nil },
			Endpoint:  func(context.Context, string, string, string) (EndpointState, error) { return EndpointAvailable, nil },
		},
		Options{PollInterval: time.Second, ProbeTimeout: time.Second},
	)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- observer.Poll(ctx) }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Poll error = %v, want context canceled", err)
	}
	if snapshot := observer.Snapshot(); len(snapshot.Environments) != 0 {
		t.Fatalf("canceled poll published a partial snapshot: %#v", snapshot)
	}
}

func TestRunPollsUntilCanceled(t *testing.T) {
	var polls atomic.Int32
	observer := mustObserver(t,
		[]EnvironmentTarget{{ID: "dev"}},
		Probes{
			Connectivity: func(context.Context, string) (ConnectivityState, error) {
				polls.Add(1)
				return ConnectivityConnected, nil
			},
			Process:   func(context.Context, string, string) (ProcessState, error) { return ProcessUnknown, nil },
			Readiness: func(context.Context, string, string) (ReadinessState, error) { return ReadinessUnknown, nil },
			Endpoint:  func(context.Context, string, string, string) (EndpointState, error) { return EndpointUnknown, nil },
		},
		Options{PollInterval: time.Millisecond, ProbeTimeout: time.Second},
	)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- observer.Run(ctx) }()
	deadline := time.After(time.Second)
	for polls.Load() < 2 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("Run did not perform a periodic poll")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("Run returned after cancellation: %v", err)
	}
}

func TestConcurrentPollAndSnapshotAccess(t *testing.T) {
	observer := mustObserver(t,
		[]EnvironmentTarget{{ID: "dev", Services: []ServiceTarget{{ID: "api", Endpoints: []EndpointTarget{{ID: "ui"}}}}}},
		Probes{
			Connectivity: func(context.Context, string) (ConnectivityState, error) { return ConnectivityConnected, nil },
			Process:      func(context.Context, string, string) (ProcessState, error) { return ProcessRunning, nil },
			Readiness:    func(context.Context, string, string) (ReadinessState, error) { return ReadinessReady, nil },
			Endpoint:     func(context.Context, string, string, string) (EndpointState, error) { return EndpointAvailable, nil },
		},
		Options{PollInterval: time.Second, ProbeTimeout: time.Second},
	)

	var workers sync.WaitGroup
	workers.Add(5)
	for i := 0; i < 4; i++ {
		go func() {
			defer workers.Done()
			for j := 0; j < 100; j++ {
				snapshot := observer.Snapshot()
				if len(snapshot.Environments) > 0 && len(snapshot.Environments[0].Services) > 0 {
					snapshot.Environments[0].Services[0].Endpoints[0].State = EndpointError
				}
			}
		}()
	}
	go func() {
		defer workers.Done()
		for i := 0; i < 100; i++ {
			if err := observer.Poll(context.Background()); err != nil {
				t.Errorf("Poll failed: %v", err)
				return
			}
		}
	}()
	workers.Wait()
	if got := observer.Snapshot().Environments[0].Services[0].Endpoints[0].State; got != EndpointAvailable {
		t.Fatalf("caller mutation changed published snapshot: %q", got)
	}
}

func mustObserver(t *testing.T, environments []EnvironmentTarget, probes Probes, options Options) *Observer {
	t.Helper()
	observer, err := New(environments, probes, options)
	if err != nil {
		t.Fatal(err)
	}
	return observer
}
