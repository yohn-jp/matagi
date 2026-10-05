// Package tunnel manages Windows-local endpoints backed by owned forwarding
// processes. The launcher is injected so this package does not define SSH
// configuration or process-construction policy.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
)

const loopbackAddress = "127.0.0.1"

// State describes the lifecycle of a managed tunnel.
type State string

const (
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateFailed   State = "failed"
	StateStopped  State = "stopped"
)

// Identity names a logical endpoint independently of its allocated local port.
type Identity struct {
	Environment string
	Service     string
	Endpoint    string
}

// Request describes the registered endpoint and the SSH destination that can
// reach it. The remote service is always addressed through remote loopback.
type Request struct {
	Identity   Identity
	SSHHost    string
	RemotePort uint16
}

// ForwardSpec is the transport-neutral request passed to a forwarding
// launcher. LocalAddress is always loopbackAddress and RemoteAddress is the
// remote development host's loopback address.
type ForwardSpec struct {
	SSHHost       string
	LocalAddress  string
	LocalPort     uint16
	RemoteAddress string
	RemotePort    uint16
}

// Process represents only a forwarding process returned by Launcher.Start.
// Wait must return after the process exits and has been reaped. Stop requests
// termination; the manager waits for Wait before releasing the local port.
type Process interface {
	Wait() error
	Stop() error
}

// Launcher starts one forwarding process. A nil error means the forward has
// been established. If Start returns an error, it must stop and reap any
// process it created before returning.
type Launcher interface {
	Start(context.Context, ForwardSpec) (Process, error)
}

// Snapshot is the public state of a logical endpoint. LocalURL is present only
// while a local port is allocated to this endpoint.
type Snapshot struct {
	Identity      Identity
	State         State
	LocalPort     uint16
	LocalURL      string
	RemoteAddress string
	RemotePort    uint16
	Failure       string
}

type managedTunnel struct {
	snapshot        Snapshot
	process         Process
	startDone       chan struct{}
	waitDone        chan struct{}
	stopRequested   bool
	stopInProgress  bool
	stopAttemptDone chan struct{}
}

// Manager allocates loopback ports and tracks only processes returned by its
// injected launcher.
type Manager struct {
	mu        sync.Mutex
	launcher  Launcher
	endpoints map[Identity]*managedTunnel
	ports     map[uint16]Identity
	closed    bool
}

// NewManager creates a tunnel manager backed by launcher.
func NewManager(launcher Launcher) (*Manager, error) {
	if launcher == nil {
		return nil, errors.New("tunnel launcher is required")
	}
	return &Manager{
		launcher:  launcher,
		endpoints: make(map[Identity]*managedTunnel),
		ports:     make(map[uint16]Identity),
	}, nil
}

// Start allocates a loopback port and starts the requested forwarding process.
// A successfully returned launcher process moves the endpoint to ready; a
// launch failure is retained as failed state and does not hold the port.
func (m *Manager) Start(ctx context.Context, request Request) (Snapshot, error) {
	if err := request.validate(); err != nil {
		return Snapshot{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Snapshot{}, errors.New("tunnel manager is closed")
	}
	if existing := m.endpoints[request.Identity]; existing != nil {
		if existing.snapshot.State == StateStarting || existing.snapshot.State == StateReady {
			snapshot := existing.snapshot
			m.mu.Unlock()
			return snapshot, fmt.Errorf("tunnel %s is already %s", request.Identity, snapshot.State)
		}
	}

	port, err := m.allocatePortLocked()
	if err != nil {
		m.mu.Unlock()
		return Snapshot{}, err
	}
	entry := &managedTunnel{
		snapshot: Snapshot{
			Identity:      request.Identity,
			State:         StateStarting,
			LocalPort:     port,
			LocalURL:      localURL(port),
			RemoteAddress: loopbackAddress,
			RemotePort:    request.RemotePort,
		},
		startDone: make(chan struct{}),
	}
	m.endpoints[request.Identity] = entry
	m.ports[port] = request.Identity
	m.mu.Unlock()

	process, launchErr := m.launcher.Start(ctx, ForwardSpec{
		SSHHost:       request.SSHHost,
		LocalAddress:  loopbackAddress,
		LocalPort:     port,
		RemoteAddress: loopbackAddress,
		RemotePort:    request.RemotePort,
	})
	if launchErr == nil && process == nil {
		launchErr = errors.New("tunnel launcher returned no process")
	}

	m.mu.Lock()
	if launchErr != nil {
		entry.snapshot.State = StateFailed
		entry.snapshot.Failure = launchErr.Error()
		m.releasePortLocked(entry)
	} else {
		entry.process = process
		entry.waitDone = make(chan struct{})
		entry.snapshot.State = StateReady
	}
	close(entry.startDone)
	snapshot := entry.snapshot
	m.mu.Unlock()

	if launchErr != nil {
		return snapshot, launchErr
	}
	go m.watch(entry)
	return m.Get(request.Identity)
}

// Stop stops and reaps the forwarding process owned by identity. Stopping an
// endpoint while its launcher is running waits for that launch to finish.
func (m *Manager) Stop(ctx context.Context, identity Identity) (Snapshot, error) {
	for {
		m.mu.Lock()
		entry := m.endpoints[identity]
		if entry == nil {
			m.mu.Unlock()
			return Snapshot{}, fmt.Errorf("tunnel %s is not registered", identity)
		}
		switch entry.snapshot.State {
		case StateStarting:
			started := entry.startDone
			m.mu.Unlock()
			select {
			case <-started:
				continue
			case <-ctx.Done():
				snapshot, _ := m.Get(identity)
				return snapshot, ctx.Err()
			}
		case StateStopped:
			snapshot := entry.snapshot
			m.mu.Unlock()
			return snapshot, nil
		case StateFailed:
			entry.snapshot.State = StateStopped
			entry.snapshot.Failure = ""
			m.releasePortLocked(entry)
			snapshot := entry.snapshot
			m.mu.Unlock()
			return snapshot, nil
		case StateReady:
			if entry.stopInProgress {
				attemptDone := entry.stopAttemptDone
				m.mu.Unlock()
				select {
				case <-attemptDone:
					continue
				case <-ctx.Done():
					snapshot, _ := m.Get(identity)
					return snapshot, ctx.Err()
				}
			}
			if entry.stopRequested {
				waitDone := entry.waitDone
				m.mu.Unlock()
				select {
				case <-waitDone:
					return m.Get(identity)
				case <-ctx.Done():
					snapshot, _ := m.Get(identity)
					return snapshot, ctx.Err()
				}
			}
			process := entry.process
			if process == nil {
				m.mu.Unlock()
				return Snapshot{}, errors.New("ready tunnel has no owned process")
			}
			entry.stopRequested = true
			entry.stopInProgress = true
			entry.stopAttemptDone = make(chan struct{})
			attemptDone := entry.stopAttemptDone
			waitDone := entry.waitDone
			m.mu.Unlock()

			stopErr := process.Stop()
			m.mu.Lock()
			entry.stopInProgress = false
			if stopErr != nil {
				select {
				case <-waitDone:
				default:
					entry.stopRequested = false
				}
			}
			close(attemptDone)
			snapshot := entry.snapshot
			m.mu.Unlock()
			if stopErr != nil {
				return snapshot, stopErr
			}

			select {
			case <-waitDone:
				return m.Get(identity)
			case <-ctx.Done():
				snapshot, _ := m.Get(identity)
				return snapshot, ctx.Err()
			}
		default:
			m.mu.Unlock()
			return Snapshot{}, fmt.Errorf("tunnel %s has invalid state", identity)
		}
	}
}

// Restart stops any currently owned process before starting the endpoint
// again. The new local port is allocated independently of the prior port.
func (m *Manager) Restart(ctx context.Context, request Request) (Snapshot, error) {
	if snapshot, err := m.Get(request.Identity); err == nil && snapshot.State != StateStopped {
		if _, err := m.Stop(ctx, request.Identity); err != nil {
			return snapshot, err
		}
	}
	return m.Start(ctx, request)
}

// Get returns the current state for identity.
func (m *Manager) Get(identity Identity) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.endpoints[identity]
	if entry == nil {
		return Snapshot{}, fmt.Errorf("tunnel %s is not registered", identity)
	}
	return entry.snapshot, nil
}

// Close stops all forwarding processes owned by the manager. It prevents new
// starts even when one or more owned processes fail to stop.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	identities := make([]Identity, 0, len(m.endpoints))
	for identity := range m.endpoints {
		identities = append(identities, identity)
	}
	m.mu.Unlock()

	var failures []string
	for _, identity := range identities {
		if _, err := m.Stop(ctx, identity); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", identity, err))
		}
	}
	if len(failures) != 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (m *Manager) watch(entry *managedTunnel) {
	err := entry.process.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.endpoints[entry.snapshot.Identity] != entry {
		close(entry.waitDone)
		return
	}
	if entry.stopRequested {
		entry.snapshot.State = StateStopped
		entry.snapshot.Failure = ""
	} else {
		entry.snapshot.State = StateFailed
		if err == nil {
			entry.snapshot.Failure = "forwarding process exited"
		} else {
			entry.snapshot.Failure = err.Error()
		}
	}
	entry.process = nil
	m.releasePortLocked(entry)
	close(entry.waitDone)
}

func (m *Manager) allocatePortLocked() (uint16, error) {
	for attempt := 0; attempt < 128; attempt++ {
		listener, err := net.Listen("tcp", net.JoinHostPort(loopbackAddress, "0"))
		if err != nil {
			return 0, fmt.Errorf("allocate loopback port: %w", err)
		}
		port := uint16(listener.Addr().(*net.TCPAddr).Port)
		closeErr := listener.Close()
		if closeErr != nil {
			return 0, fmt.Errorf("release allocated loopback port: %w", closeErr)
		}
		if _, inUse := m.ports[port]; !inUse {
			return port, nil
		}
	}
	return 0, errors.New("could not allocate a unique loopback port")
}

func (m *Manager) releasePortLocked(entry *managedTunnel) {
	port := entry.snapshot.LocalPort
	if owner, exists := m.ports[port]; exists && owner == entry.snapshot.Identity {
		delete(m.ports, port)
	}
	entry.snapshot.LocalPort = 0
	entry.snapshot.LocalURL = ""
}

func localURL(port uint16) string {
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(loopbackAddress, fmt.Sprint(port))}).String()
}

func (r Request) validate() error {
	if strings.TrimSpace(r.Identity.Environment) == "" || strings.TrimSpace(r.Identity.Service) == "" || strings.TrimSpace(r.Identity.Endpoint) == "" {
		return errors.New("environment, service, and endpoint identity are required")
	}
	if strings.TrimSpace(r.SSHHost) == "" {
		return errors.New("SSH host is required")
	}
	if r.RemotePort == 0 {
		return errors.New("remote port must be between 1 and 65535")
	}
	return nil
}

func (i Identity) String() string {
	return i.Environment + "/" + i.Service + "/" + i.Endpoint
}
