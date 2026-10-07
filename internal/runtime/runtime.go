package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/health"
	"github.com/yohn-jp/matagi/internal/jinushi"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/ssh"
	"github.com/yohn-jp/matagi/internal/tunnel"
)

const commandTimeout = 10 * time.Second
const probeTimeout = 3 * time.Second

type Failure struct {
	Code     string
	Evidence string
}

func (e *Failure) Error() string { return e.Code }
func classify(err error) error {
	if err == nil {
		return nil
	}
	var f *jinushi.Failure
	if errors.As(err, &f) {
		switch f.Kind {
		case jinushi.KindBootstrapFailed, jinushi.KindBootstrapNotConfigured, jinushi.KindSupervisorUnavailable:
			return &Failure{Code: "jinushi-unavailable"}
		case jinushi.KindTimeout:
			return &Failure{Code: classifyJinushiTimeout(f)}
		case jinushi.KindTransport:
			return &Failure{Code: classifyJinushiTransport(f)}
		case jinushi.KindCanceled:
			return &Failure{Code: "operation-canceled"}
		case jinushi.KindUncertain, jinushi.KindAmbiguous, jinushi.KindNotStopped:
			return &Failure{Code: "lifecycle-conflict"}
		case jinushi.KindInvalid:
			return &Failure{Code: "invalid-request"}
		case jinushi.KindCommand:
			return &Failure{Code: "jinushi-command-failed", Evidence: boundedJinushiCode(f.Code)}
		case jinushi.KindProtocol:
			return &Failure{Code: "jinushi-protocol-failed", Evidence: boundedJinushiCode(f.Code)}
		}
	}
	var transport *ssh.Error
	if errors.As(err, &transport) {
		return &Failure{Code: classifySSHFailure(transport.Kind)}
	}
	if errors.Is(err, context.Canceled) {
		return &Failure{Code: "operation-canceled"}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Failure{Code: "jinushi-timeout"}
	}
	return &Failure{Code: "remote-failure"}
}

func classifyJinushiTimeout(f *jinushi.Failure) string {
	var execution *jinushi.ExecutionError
	if errors.As(f, &execution) && execution.Kind == jinushi.ExecutionTimeout {
		var sshFailure *ssh.Error
		if errors.As(execution, &sshFailure) && sshFailure.Kind == ssh.FailureTimeout {
			return "ssh-timeout"
		}
	}
	return "jinushi-timeout"
}

func classifyJinushiTransport(f *jinushi.Failure) string {
	var execution *jinushi.ExecutionError
	if errors.As(f, &execution) {
		var sshFailure *ssh.Error
		if errors.As(execution, &sshFailure) {
			return classifySSHFailure(sshFailure.Kind)
		}
	}
	return "host-unreachable"
}

func classifySSHFailure(kind ssh.FailureKind) string {
	switch kind {
	case ssh.FailureTransport:
		return "host-unreachable"
	case ssh.FailureTimeout:
		return "ssh-timeout"
	case ssh.FailureCanceled:
		return "operation-canceled"
	case ssh.FailureInvalidRequest:
		return "invalid-request"
	case ssh.FailureRemoteCommand:
		return "jinushi-command-failed"
	default:
		return "ssh-client-failed"
	}
}

func boundedJinushiCode(code string) string {
	if code == "" || len(code) > 96 {
		return ""
	}
	for _, char := range code {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
			return ""
		}
	}
	return code
}

type binding struct {
	environment      registry.Environment
	control, observe *jinushi.Client
}
type Runtime struct {
	mu               sync.Mutex
	registry         *registry.Snapshot
	store            *config.Store
	ssh              remoteRunner
	bindings         map[string]binding
	services         map[string]registry.Service
	tunnels          *tunnel.Manager
	observer         *health.Observer
	pending          map[string]string
	jinushi          map[string]string
	endpointFailures map[tunnel.Identity]string
	closed           bool
	httpClient       *http.Client
}

func key(env, service string) string { return env + "\x00" + service }
func New(store *config.Store) (*Runtime, error) {
	snapshot, err := loadSnapshot(store)
	if err != nil {
		return nil, err
	}
	client, err := ssh.New()
	if err != nil {
		return nil, err
	}
	launcher, err := NewLauncher()
	if err != nil {
		return nil, err
	}
	manager, err := tunnel.NewManager(launcher)
	if err != nil {
		return nil, err
	}
	r, err := Compose(snapshot, client, manager)
	if err != nil {
		return nil, err
	}
	r.store = store
	return r, nil
}

func loadSnapshot(store *config.Store) (*registry.Snapshot, error) {
	snapshot, err := store.Load()
	if err == nil {
		return snapshot, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return registry.NewSnapshot(nil, nil)
}

// Compose accepts validated producer state and external execution seams.
func Compose(snapshot *registry.Snapshot, client remoteRunner, manager *tunnel.Manager) (*Runtime, error) {
	if snapshot == nil || client == nil || manager == nil {
		return nil, errors.New("registry, SSH client and tunnel manager are required")
	}
	r := &Runtime{ssh: client, tunnels: manager, pending: map[string]string{}, jinushi: map[string]string{}, endpointFailures: map[tunnel.Identity]string{}, httpClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if err := r.configure(snapshot); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Runtime) configure(snapshot *registry.Snapshot) error {
	r.registry = snapshot
	r.jinushi = map[string]string{}
	if r.endpointFailures == nil {
		r.endpointFailures = map[tunnel.Identity]string{}
	}
	r.bindings = map[string]binding{}
	r.services = map[string]registry.Service{}
	client := r.ssh
	targets := []health.EnvironmentTarget{}
	observerProbeTimeout := probeTimeout
	for _, env := range snapshot.Environments() {
		ex := executor{client: client, host: env.SSHHost}
		opts := jinushi.Options{StateDir: env.Jinushi.StateDir, SupervisorStartCommand: env.Jinushi.SupervisorStartCommand, CommandTimeout: commandTimeout}
		control, err := jinushi.New(ex, opts)
		if err != nil {
			return err
		}
		opts.SupervisorStartCommand = nil
		observe, err := jinushi.New(ex, opts)
		if err != nil {
			return err
		}
		r.bindings[string(env.ID)] = binding{env, control, observe}
		targets = append(targets, health.EnvironmentTarget{ID: string(env.ID)})
	}
	for _, service := range snapshot.Services() {
		configuredTimeout := time.Duration(*service.Health.TimeoutMS) * time.Millisecond
		if configuredTimeout >= observerProbeTimeout {
			observerProbeTimeout = configuredTimeout + time.Second
		}
		e, s := string(service.EnvironmentID), string(service.ID)
		r.services[key(e, s)] = service
		for i := range targets {
			if targets[i].ID == e {
				target := health.ServiceTarget{ID: s}
				for _, endpoint := range service.Endpoints {
					target.Endpoints = append(target.Endpoints, health.EndpointTarget{ID: string(endpoint.ID)})
				}
				targets[i].Services = append(targets[i].Services, target)
				break
			}
		}
	}
	observer, err := health.New(targets, health.Probes{Connectivity: r.connectivity, Process: r.process, Readiness: r.readiness, Endpoint: r.endpointState}, health.Options{PollInterval: 5 * time.Second, ProbeTimeout: observerProbeTimeout})
	if err != nil {
		return err
	}
	r.observer = observer
	return nil
}

func equalArgv(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Register installs the first validated registry atomically in the sole runtime.
// Existing registrations are deliberately not replaced while tunnels may be owned.
func (r *Runtime) Register(snapshot *registry.Snapshot) error {
	if snapshot == nil {
		return &Failure{Code: "invalid-request"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return &Failure{Code: "lifecycle-conflict"}
	}
	// An environment-only first draft may be completed with service definitions.
	// Never replace a registry that already owns services or tunnels.
	current := r.registry.Environments()
	if len(current) != 0 {
		incoming := snapshot.Environments()
		if len(current) != 1 || len(r.registry.Services()) != 0 || len(snapshot.Services()) == 0 || len(incoming) != 1 || current[0].ID != incoming[0].ID || current[0].SSHHost != incoming[0].SSHHost || current[0].Jinushi.StateDir != incoming[0].Jinushi.StateDir || !equalArgv(current[0].Jinushi.SupervisorStartCommand, incoming[0].Jinushi.SupervisorStartCommand) {
			return &Failure{Code: "lifecycle-conflict"}
		}
	}
	if r.store == nil {
		return &Failure{Code: "registration-unavailable"}
	}
	// Prepare clients and observer before committing the persistent state.
	candidate := &Runtime{ssh: r.ssh, tunnels: r.tunnels, httpClient: r.httpClient}
	if err := candidate.configure(snapshot); err != nil {
		return &Failure{Code: "invalid-request"}
	}
	if err := r.store.Save(snapshot); err != nil {
		return err
	}
	// Rebind probe closures to the live runtime, not the temporary candidate.
	return r.configure(snapshot)
}

// EnsureJinushi is an explicit bootstrap-capable operation; background polls
// only use the observation-only client.
func (r *Runtime) EnsureJinushi(ctx context.Context, env string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return &Failure{Code: "lifecycle-conflict"}
	}
	b, ok := r.bindings[env]
	if !ok {
		return &Failure{Code: "unknown-identity"}
	}
	if err := b.control.EnsureReady(ctx); err != nil {
		r.jinushi[env] = "unavailable"
		return classify(err)
	}
	r.jinushi[env] = "ready"
	return nil
}
func (r *Runtime) service(env, id string) (registry.Service, error) {
	s, ok := r.services[key(env, id)]
	if !ok {
		return s, &Failure{Code: "unknown-identity"}
	}
	return s, nil
}
func (r *Runtime) Start(ctx context.Context, env, id string) (Service, error) {
	return r.mutate(ctx, env, id, "start")
}
func (r *Runtime) Stop(ctx context.Context, env, id string) (Service, error) {
	return r.mutate(ctx, env, id, "stop")
}
func (r *Runtime) Restart(ctx context.Context, env, id string) (Service, error) {
	return r.mutate(ctx, env, id, "restart")
}
func (r *Runtime) mutate(ctx context.Context, env, id, action string) (Service, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.service(env, id)
	if err != nil {
		return Service{}, err
	}
	if r.closed {
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	owner := s.CorrelationOwner()
	b := r.bindings[env]
	token := r.pending[key(env, id)]
	if action != "stop" && token == "" {
		token, err = jinushi.NewSubmissionID()
		if err != nil {
			return Service{}, classify(err)
		}
		r.pending[key(env, id)] = token
	}
	var run jinushi.Run
	var observedRun *jinushi.Run
	switch action {
	case "start":
		run, err = b.control.Start(ctx, jinushi.StartRequest{Service: jinushi.Service{ID: owner, Argv: s.Execution.Argv, Cwd: s.Execution.CWD}, SubmissionID: token})
		observedRun = &run
	case "restart":
		run, err = b.control.Restart(ctx, jinushi.StartRequest{Service: jinushi.Service{ID: owner, Argv: s.Execution.Argv, Cwd: s.Execution.CWD}, SubmissionID: token})
		observedRun = &run
	case "stop":
		var status jinushi.ServiceStatus
		status, err = b.control.Stop(ctx, owner)
		observedRun = status.Run
	}
	if err != nil {
		var failure *jinushi.Failure
		if action != "stop" && errors.As(err, &failure) && (failure.Kind == jinushi.KindInvalid || failure.Kind == jinushi.KindCommand || failure.Kind == jinushi.KindBootstrapFailed) {
			delete(r.pending, key(env, id))
		}
		return Service{}, classify(err)
	}
	r.jinushi[env] = "ready"
	if action == "stop" || run.ID != "" {
		delete(r.pending, key(env, id))
	}
	if action == "stop" {
		for _, ep := range s.Endpoints {
			identity := tunnel.Identity{Environment: env, Service: id, Endpoint: string(ep.ID)}
			if _, getErr := r.tunnels.Get(identity); getErr == nil {
				if _, stopErr := r.tunnels.Stop(ctx, identity); stopErr != nil {
					continue
				}
			}
			delete(r.endpointFailures, identity)
		}
	} else {
		r.ensureHealth(ctx, s)
	}
	observation := r.actionObservation(ctx, s, action, observedRun)
	r.observer.PublishServiceObservation(env, observation)
	return r.serviceView(s, observation), nil
}

func (r *Runtime) actionObservation(ctx context.Context, s registry.Service, action string, run *jinushi.Run) health.ServiceObservation {
	observation := health.ServiceObservation{ID: string(s.ID), Process: health.ProcessUnknown, Readiness: health.ReadinessUnknown}
	if run != nil {
		var err error
		observation.Process, err = processState(*run)
		if err != nil {
			observation.ProcessError = err.Error()
		}
	}
	if action != "stop" {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		readiness, err := r.readiness(probeCtx, string(s.EnvironmentID), string(s.ID))
		cancel()
		observation.Readiness = readiness
		if ctx.Err() != nil {
			observation.Readiness = health.ReadinessUnknown
		} else if err != nil {
			observation.ReadinessError = err.Error()
		}
	}
	observation.State = health.AggregateServiceState(observation.Process, observation.Readiness)
	observation.Endpoints = make([]health.EndpointObservation, 0, len(s.Endpoints))
	for _, endpoint := range s.Endpoints {
		state, err := r.endpointState(ctx, string(s.EnvironmentID), string(s.ID), string(endpoint.ID))
		message := ""
		if err != nil {
			message = err.Error()
		}
		observation.Endpoints = append(observation.Endpoints, health.EndpointObservation{ID: string(endpoint.ID), State: state, Error: message})
	}
	return observation
}

func (r *Runtime) ensureHealth(ctx context.Context, s registry.Service) {
	for _, ep := range s.Endpoints {
		if ep.ID == s.Health.EndpointID {
			_, _ = r.ensure(ctx, s, ep)
			return
		}
	}
}
func (r *Runtime) ensure(ctx context.Context, s registry.Service, ep registry.Endpoint) (tunnel.Snapshot, error) {
	identity := tunnel.Identity{Environment: string(s.EnvironmentID), Service: string(s.ID), Endpoint: string(ep.ID)}
	port, err := r.resolveEndpoint(ctx, s, ep)
	if err != nil {
		if current, getErr := r.tunnels.Get(identity); getErr == nil && (current.State == tunnel.StateReady || current.State == tunnel.StateStarting) {
			if _, stopErr := r.tunnels.Stop(ctx, identity); stopErr != nil {
				err = errors.New(endpointEvidenceStale)
			}
		}
		r.endpointFailures[identity] = err.Error()
		return tunnel.Snapshot{}, err
	}
	delete(r.endpointFailures, identity)
	if current, getErr := r.tunnels.Get(identity); getErr == nil {
		if (current.State == tunnel.StateReady || current.State == tunnel.StateStarting) && current.RemotePort == port {
			return current, nil
		}
		if current.State == tunnel.StateReady || current.State == tunnel.StateStarting {
			if _, stopErr := r.tunnels.Stop(ctx, identity); stopErr != nil {
				failure := errors.New(endpointEvidenceStale)
				r.endpointFailures[identity] = failure.Error()
				return current, failure
			}
		}
	}
	snapshot, err := r.tunnels.Start(ctx, tunnel.Request{Identity: identity, SSHHost: r.bindings[string(s.EnvironmentID)].environment.SSHHost, RemotePort: port})
	if err != nil {
		r.endpointFailures[identity] = "The SSH tunnel to the registered endpoint could not be established."
		return snapshot, err
	}
	return snapshot, nil
}
func (r *Runtime) Ensure(ctx context.Context, env, id, endpoint string) (Endpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := r.service(env, id)
	if err != nil {
		return Endpoint{}, err
	}
	if r.closed {
		return Endpoint{}, &Failure{Code: "lifecycle-conflict"}
	}
	for _, ep := range s.Endpoints {
		if string(ep.ID) == endpoint {
			snap, err := r.ensure(ctx, s, ep)
			if err != nil {
				identity := tunnel.Identity{Environment: env, Service: id, Endpoint: endpoint}
				evidence := r.endpointFailures[identity]
				if evidence == "" {
					evidence = "The SSH tunnel to the registered endpoint could not be established."
				}
				failure := &Failure{Code: "endpoint-unavailable", Evidence: bounded(evidence)}
				return Endpoint{}, failure
			}
			state, probeErr := r.endpointState(ctx, env, id, endpoint)
			observation := health.EndpointObservation{State: state}
			if probeErr != nil {
				observation.Error = probeErr.Error()
			}
			return r.endpointView(s, ep, observation, snap), nil
		}
	}
	return Endpoint{}, &Failure{Code: "unknown-identity"}
}
func (r *Runtime) connectivity(ctx context.Context, env string) (health.ConnectivityState, error) {
	_, err := r.ssh.Run(ctx, r.bindings[env].environment.SSHHost, []string{"true"}, probeTimeout)
	if err == nil {
		return health.ConnectivityConnected, nil
	}
	var transportErr *ssh.Error
	if errors.As(err, &transportErr) && transportErr.Kind == ssh.FailureTransport {
		return health.ConnectivityUnreachable, nil
	}
	return health.ConnectivityError, err
}
func (r *Runtime) process(ctx context.Context, env, id string) (health.ProcessState, error) {
	s := r.services[key(env, id)]
	status, err := r.bindings[env].observe.Status(ctx, s.CorrelationOwner())
	if err != nil {
		return health.ProcessUnknown, err
	}
	if status.Run == nil {
		return health.ProcessUnknown, nil
	}
	return processState(*status.Run)
}

func processState(run jinushi.Run) (health.ProcessState, error) {
	switch run.State {
	case jinushi.StateTerminal:
		return health.ProcessStopped, nil
	case jinushi.StateRunning:
		return health.ProcessRunning, nil
	case jinushi.StateAccepted, jinushi.StateStarting, jinushi.StateTerminating, jinushi.StateReconciling:
		return health.ProcessStarting, nil
	default:
		return health.ProcessUnknown, fmt.Errorf("Jinushi state uncertain")
	}
}
func (r *Runtime) readiness(ctx context.Context, env, id string) (health.ReadinessState, error) {
	s := r.services[key(env, id)]
	identity := tunnel.Identity{Environment: env, Service: id, Endpoint: string(s.Health.EndpointID)}
	if failure := r.endpointFailures[identity]; strings.HasPrefix(failure, "Dynamic endpoint") {
		return health.ReadinessError, errors.New(failure)
	}
	snap, err := r.tunnels.Get(identity)
	if err != nil {
		return health.ReadinessUnknown, nil
	}
	if snap.State == tunnel.StateFailed {
		return health.ReadinessUnknown, nil
	}
	if snap.State != tunnel.StateReady {
		return health.ReadinessUnknown, nil
	}
	u, err := url.Parse(snap.LocalURL)
	if err != nil {
		return health.ReadinessError, err
	}
	u.Path = s.Health.Path
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(*s.Health.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return health.ReadinessError, err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return health.ReadinessNotReady, nil
	}
	defer resp.Body.Close()
	for _, status := range *s.Health.ExpectedStatus {
		if resp.StatusCode == status {
			return health.ReadinessReady, nil
		}
	}
	return health.ReadinessUnhealthy, nil
}
func (r *Runtime) endpointState(ctx context.Context, env, id, ep string) (health.EndpointState, error) {
	identity := tunnel.Identity{Environment: env, Service: id, Endpoint: ep}
	if failure := r.endpointFailures[identity]; failure != "" {
		return health.EndpointError, errors.New(failure)
	}
	snap, err := r.tunnels.Get(identity)
	if err != nil {
		return health.EndpointUnavailable, nil
	}
	switch snap.State {
	case tunnel.StateReady:
		if err := r.probeApplicationEndpoint(ctx, snap.LocalURL); err != nil {
			return health.EndpointUnavailable, err
		}
		return health.EndpointAvailable, nil
	case tunnel.StateFailed:
		return health.EndpointError, errors.New("The SSH tunnel to the registered endpoint could not be established.")
	case tunnel.StateStopped:
		return health.EndpointUnavailable, nil
	default:
		return health.EndpointUnknown, nil
	}
}

func (r *Runtime) probeApplicationEndpoint(ctx context.Context, localURL string) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, localURL, nil)
	if err != nil {
		return errors.New(endpointApplicationDown)
	}
	response, err := r.httpClient.Do(request)
	if err != nil {
		return errors.New(endpointApplicationDown)
	}
	_ = response.Body.Close()
	return nil
}
func (r *Runtime) Poll(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("runtime closed")
	}
	for _, s := range r.registry.Services() {
		if s.DesiredState == registry.DesiredRunning {
			r.ensureHealth(ctx, s)
		}
		for _, endpoint := range s.Endpoints {
			if endpoint.Resolution == nil || (endpoint.ID == s.Health.EndpointID && s.DesiredState == registry.DesiredRunning) {
				continue
			}
			identity := tunnel.Identity{Environment: string(s.EnvironmentID), Service: string(s.ID), Endpoint: string(endpoint.ID)}
			current, err := r.tunnels.Get(identity)
			if (err == nil && (current.State == tunnel.StateReady || current.State == tunnel.StateStarting)) || r.endpointFailures[identity] != "" {
				_, _ = r.ensure(ctx, s, endpoint)
			}
		}
	}
	defer r.mu.Unlock()
	if err := r.observer.Poll(ctx); err != nil {
		return err
	}
	for _, env := range r.observer.Snapshot().Environments {
		if env.State != health.ConnectivityConnected {
			r.jinushi[env.ID] = "unknown"
			continue
		}
		if err := r.bindings[env.ID].observe.EnsureReady(ctx); err != nil {
			r.jinushi[env.ID] = "unavailable"
		} else {
			r.jinushi[env.ID] = "ready"
		}
	}
	return nil
}
func (r *Runtime) Run(ctx context.Context) error {
	if err := r.Poll(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.Poll(ctx); err != nil {
				return err
			}
		}
	}
}
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return r.tunnels.Close(ctx)
}

type State struct {
	Version      int           `json:"version"`
	Environments []Environment `json:"environments"`
}
type Environment struct {
	ID           string                   `json:"id"`
	SSHHost      string                   `json:"sshHost"`
	Connectivity health.ConnectivityState `json:"connectivity"`
	Jinushi      string                   `json:"jinushi"`
	Error        string                   `json:"error"`
	Services     []Service                `json:"services"`
}
type Service struct {
	ID             string                `json:"id"`
	DesiredState   registry.DesiredState `json:"desiredState"`
	State          health.ServiceState   `json:"state"`
	Process        health.ProcessState   `json:"process"`
	Readiness      health.ReadinessState `json:"readiness"`
	ProcessError   string                `json:"processError"`
	ReadinessError string                `json:"readinessError"`
	Endpoints      []Endpoint            `json:"endpoints"`
}
type Endpoint struct {
	ID            string               `json:"id"`
	Label         string               `json:"label"`
	EndpointState health.EndpointState `json:"endpointState"`
	TunnelState   tunnel.State         `json:"tunnelState"`
	LocalURL      string               `json:"localUrl"`
	Failure       string               `json:"failure"`
}

func observationError(message string) string {
	if message != "" {
		return "observation failed"
	}
	return ""
}
func tunnelFailure(s tunnel.Snapshot) string {
	if s.State == tunnel.StateFailed {
		return "tunnel unavailable"
	}
	return ""
}
func bounded(s string) string {
	if len(s) > 160 {
		return s[:160]
	}
	return s
}
func (r *Runtime) endpointView(s registry.Service, ep registry.Endpoint, obs health.EndpointObservation, snap tunnel.Snapshot) Endpoint {
	state := obs.State
	if state == "" {
		state = health.EndpointUnknown
	}
	t := snap.State
	if t == "" {
		t = tunnel.StateStopped
	}
	failure := obs.Error
	if failure == "" {
		failure = tunnelFailure(snap)
	}
	return Endpoint{ID: string(ep.ID), Label: ep.Label, EndpointState: state, TunnelState: t, LocalURL: snap.LocalURL, Failure: bounded(failure)}
}
func (r *Runtime) serviceView(s registry.Service, obs health.ServiceObservation) Service {
	result := Service{ID: string(s.ID), DesiredState: s.DesiredState, State: obs.State, Process: obs.Process, Readiness: obs.Readiness, ProcessError: observationError(obs.ProcessError), ReadinessError: observationError(obs.ReadinessError), Endpoints: []Endpoint{}}
	if result.State == "" {
		result.State = health.ServiceUnknown
	}
	if result.Process == "" {
		result.Process = health.ProcessUnknown
	}
	if result.Readiness == "" {
		result.Readiness = health.ReadinessUnknown
	}
	for _, ep := range s.Endpoints {
		var observation health.EndpointObservation
		for _, o := range obs.Endpoints {
			if o.ID == string(ep.ID) {
				observation = o
			}
		}
		snap, _ := r.tunnels.Get(tunnel.Identity{Environment: string(s.EnvironmentID), Service: string(s.ID), Endpoint: string(ep.ID)})
		result.Endpoints = append(result.Endpoints, r.endpointView(s, ep, observation, snap))
	}
	return result
}
func (r *Runtime) Snapshot() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	observed := r.observer.Snapshot()
	result := State{Version: 1, Environments: []Environment{}}
	for _, e := range r.registry.Environments() {
		view := Environment{ID: string(e.ID), SSHHost: e.SSHHost, Connectivity: health.ConnectivityUnknown, Jinushi: "unknown", Services: []Service{}}
		if status := r.jinushi[view.ID]; status != "" {
			view.Jinushi = status
		}
		for _, o := range observed.Environments {
			if o.ID == view.ID {
				view.Connectivity = o.State
				view.Error = observationError(o.Error)
				for _, so := range o.Services {
					view.Services = append(view.Services, r.serviceView(r.services[key(view.ID, so.ID)], so))
				}
				break
			}
		}
		if len(view.Services) == 0 {
			for _, s := range r.registry.Services() {
				if s.EnvironmentID == e.ID {
					view.Services = append(view.Services, r.serviceView(s, health.ServiceObservation{}))
				}
			}
		}
		sort.Slice(view.Services, func(i, j int) bool { return view.Services[i].ID < view.Services[j].ID })
		result.Environments = append(result.Environments, view)
	}
	return result
}
