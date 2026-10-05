package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLauncher struct {
	mu        sync.Mutex
	specs     []ForwardSpec
	processes []*fakeProcess
	startErr  error
	entered   chan struct{}
	release   chan struct{}
}

func (f *fakeLauncher) Start(ctx context.Context, spec ForwardSpec) (Process, error) {
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	if f.startErr != nil {
		return nil, f.startErr
	}
	process := newFakeProcess()
	f.processes = append(f.processes, process)
	return process, nil
}

func (f *fakeLauncher) recorded() ([]ForwardSpec, []*fakeProcess) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ForwardSpec(nil), f.specs...), append([]*fakeProcess(nil), f.processes...)
}

type fakeProcess struct {
	done      chan struct{}
	once      sync.Once
	waitMu    sync.Mutex
	waitErr   error
	stopErr   error
	stopCalls atomic.Int32
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{done: make(chan struct{})}
}

func (p *fakeProcess) Wait() error {
	<-p.done
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}

func (p *fakeProcess) Stop() error {
	p.stopCalls.Add(1)
	if p.stopErr != nil {
		return p.stopErr
	}
	p.exit(nil)
	return nil
}

func (p *fakeProcess) exit(err error) {
	p.once.Do(func() {
		p.waitMu.Lock()
		p.waitErr = err
		p.waitMu.Unlock()
		close(p.done)
	})
}

func TestStartAllocatesLoopbackEndpointAndTracksIdentity(t *testing.T) {
	launcher := &fakeLauncher{}
	manager := mustManager(t, launcher)
	remoteListener, err := net.Listen("tcp", net.JoinHostPort(loopbackAddress, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer remoteListener.Close()
	remotePort := uint16(remoteListener.Addr().(*net.TCPAddr).Port)
	request := testRequest("ui", remotePort)

	snapshot, err := manager.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != StateReady {
		t.Fatalf("state = %q, want %q", snapshot.State, StateReady)
	}
	if snapshot.LocalPort == 0 || snapshot.LocalPort == request.RemotePort {
		t.Fatalf("local port = %d, want a nonzero port distinct from remote %d", snapshot.LocalPort, request.RemotePort)
	}
	if snapshot.LocalURL != fmt.Sprintf("http://127.0.0.1:%d", snapshot.LocalPort) {
		t.Fatalf("local URL = %q", snapshot.LocalURL)
	}
	if snapshot.Identity != request.Identity {
		t.Fatalf("identity = %+v, want %+v", snapshot.Identity, request.Identity)
	}
	if snapshot.RemoteAddress != loopbackAddress || snapshot.RemotePort != request.RemotePort {
		t.Fatalf("remote target = %s:%d", snapshot.RemoteAddress, snapshot.RemotePort)
	}

	specs, _ := launcher.recorded()
	if len(specs) != 1 {
		t.Fatalf("launcher calls = %d, want 1", len(specs))
	}
	if specs[0].SSHHost != request.SSHHost || specs[0].LocalAddress != loopbackAddress || specs[0].RemoteAddress != loopbackAddress || specs[0].LocalPort != snapshot.LocalPort || specs[0].RemotePort != request.RemotePort {
		t.Fatalf("launcher spec = %+v", specs[0])
	}
}

func TestConcurrentEndpointsReceiveDistinctLocalPorts(t *testing.T) {
	launcher := &fakeLauncher{}
	manager := mustManager(t, launcher)
	const count = 24
	ports := make(chan uint16, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snapshot, err := manager.Start(context.Background(), testRequest(fmt.Sprintf("ui-%02d", i), 3000+uint16(i)))
			if err != nil {
				errs <- err
				return
			}
			ports <- snapshot.LocalPort
		}(i)
	}
	wg.Wait()
	close(ports)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	seen := make(map[uint16]bool)
	for port := range ports {
		if seen[port] {
			t.Fatalf("local port %d was allocated more than once", port)
		}
		seen[port] = true
	}
	if len(seen) != count {
		t.Fatalf("allocated %d ports, want %d", len(seen), count)
	}
}

func TestLauncherFailureLeavesFailedEndpointWithoutPort(t *testing.T) {
	failure := errors.New("forward could not bind")
	manager := mustManager(t, &fakeLauncher{startErr: failure})

	snapshot, err := manager.Start(context.Background(), testRequest("broken", 9090))
	if !errors.Is(err, failure) {
		t.Fatalf("Start error = %v, want %v", err, failure)
	}
	if snapshot.State != StateFailed || snapshot.LocalPort != 0 || snapshot.LocalURL != "" {
		t.Fatalf("failed snapshot = %+v", snapshot)
	}
	current, err := manager.Get(snapshot.Identity)
	if err != nil || current.State != StateFailed {
		t.Fatalf("Get after failed launch = %+v, %v", current, err)
	}
	if len(manager.ports) != 0 {
		t.Fatalf("failed launch retained ports: %+v", manager.ports)
	}
}

func TestStartingStateIsObservableAndStopWaitsForLaunch(t *testing.T) {
	launcher := &fakeLauncher{entered: make(chan struct{}, 1), release: make(chan struct{})}
	manager := mustManager(t, launcher)
	request := testRequest("slow", 7070)
	started := make(chan error, 1)
	go func() {
		_, err := manager.Start(context.Background(), request)
		started <- err
	}()
	<-launcher.entered

	snapshot, err := manager.Get(request.Identity)
	if err != nil || snapshot.State != StateStarting {
		t.Fatalf("Get during launch = %+v, %v", snapshot, err)
	}
	stopped := make(chan Snapshot, 1)
	stopErr := make(chan error, 1)
	go func() {
		snapshot, err := manager.Stop(context.Background(), request.Identity)
		stopped <- snapshot
		stopErr <- err
	}()
	close(launcher.release)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := <-stopErr; err != nil {
		t.Fatal(err)
	}
	if snapshot := <-stopped; snapshot.State != StateStopped {
		t.Fatalf("Stop during launch returned state %q", snapshot.State)
	}
	_, processes := launcher.recorded()
	if len(processes) != 1 || processes[0].stopCalls.Load() != 1 {
		t.Fatalf("owned process stop calls = %+v", processes)
	}
}

func TestStopRestartAndCloseOnlyReapOwnedProcesses(t *testing.T) {
	launcher := &fakeLauncher{}
	manager := mustManager(t, launcher)
	request := testRequest("owned", 6060)
	first, err := manager.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_, processes := launcher.recorded()
	unrelated := newFakeProcess()

	stopped, err := manager.Stop(context.Background(), request.Identity)
	if err != nil || stopped.State != StateStopped {
		t.Fatalf("Stop = %+v, %v", stopped, err)
	}
	if processes[0].stopCalls.Load() != 1 || unrelated.stopCalls.Load() != 0 {
		t.Fatalf("owned stops = %d, unrelated stops = %d", processes[0].stopCalls.Load(), unrelated.stopCalls.Load())
	}

	second, err := manager.Restart(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != StateReady || second.LocalPort == 0 {
		t.Fatalf("restart snapshot = %+v", second)
	}
	if second.Identity != first.Identity {
		t.Fatalf("restart changed logical identity: %+v", second.Identity)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, processes = launcher.recorded()
	if len(processes) != 2 || processes[1].stopCalls.Load() != 1 {
		t.Fatalf("close did not stop second owned process: %+v", processes)
	}
	if _, err := manager.Start(context.Background(), testRequest("after-close", 6061)); err == nil {
		t.Fatal("Start succeeded after manager Close")
	}
}

func TestUnexpectedProcessExitMarksEndpointFailed(t *testing.T) {
	launcher := &fakeLauncher{}
	manager := mustManager(t, launcher)
	request := testRequest("crashed", 5050)
	if _, err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	_, processes := launcher.recorded()
	processes[0].exit(errors.New("ssh exited"))

	waitFor(t, func() bool {
		snapshot, err := manager.Get(request.Identity)
		return err == nil && snapshot.State == StateFailed
	})
	snapshot, err := manager.Get(request.Identity)
	if err != nil || snapshot.Failure != "ssh exited" || snapshot.LocalPort != 0 || snapshot.LocalURL != "" {
		t.Fatalf("failed endpoint snapshot = %+v, %v", snapshot, err)
	}
}

func mustManager(t *testing.T, launcher Launcher) *Manager {
	t.Helper()
	manager, err := NewManager(launcher)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return manager
}

func testRequest(endpoint string, remotePort uint16) Request {
	return Request{
		Identity:   Identity{Environment: "dev", Service: "dashboard", Endpoint: endpoint},
		SSHHost:    "dev-host",
		RemotePort: remotePort,
	}
}

func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}
