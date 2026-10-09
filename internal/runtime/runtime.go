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

var errRuntimeClosed = errors.New("runtime closed")

type Failure struct {
	Code     string
	Evidence string
}

func (e *Failure) Error() string { return e.Code }

const partialPersistenceEvidence = "desired service state could not be saved after the Jinushi operation succeeded"

// SafeDiagnosticCode preserves only stable Matagi classifications and known
// fixed endpoint messages. Unknown provider or remote text falls back to the
// caller's stable generic code.
func SafeDiagnosticCode(value, fallback string) string {
	switch value {
	case endpointEvidenceMissing:
		return "endpoint-evidence-missing"
	case endpointEvidenceInvalid:
		return "endpoint-evidence-invalid"
	case endpointEvidenceAmbiguous:
		return "endpoint-evidence-ambiguous"
	case endpointEvidenceStale:
		return "endpoint-evidence-stale"
	case endpointOutputIncomplete:
		return "endpoint-output-incomplete"
	case endpointApplicationDown:
		return "endpoint-application-unavailable"
	case "The SSH tunnel to the registered endpoint could not be established.":
		return "tunnel-unavailable"
	case "observation failed":
		return "remote-failure"
	}
	if isSafeDiagnosticCode(value) {
		return value
	}
	if isSafeDiagnosticCode(fallback) {
		return fallback
	}
	return ""
}

func isSafeDiagnosticCode(value string) bool {
	switch value {
	case "invalid-request", "unknown-identity", "not-found", "lifecycle-conflict", "registration-unavailable", "registration-failed", "caller-not-authorized", "unsafe-request-origin", "unsupported-media-type", "operation-canceled",
		"ssh-transport-failed", "host-unreachable", "ssh-timeout", "ssh-connectivity-failed", "ssh-client-failed", "jinushi-unavailable", "jinushi-bootstrap-failed", "jinushi-timeout", "jinushi-readiness-timeout", "jinushi-command-failed", "jinushi-readiness-failed", "jinushi-protocol-failed", "jinushi-state-uncertain", "remote-failure",
		"tunnel-unavailable", "endpoint-unavailable", "endpoint-evidence-missing", "endpoint-evidence-invalid", "endpoint-evidence-ambiguous", "endpoint-evidence-stale", "endpoint-output-incomplete", "endpoint-application-unavailable", "readiness-check-failed":
		return true
	default:
		return false
	}
}

// SafeFailureEvidence permits only fixed recovery evidence and known Jinushi
// provider codes. It never forwards arbitrary error text.
func SafeFailureEvidence(code, evidence string) string {
	if code == "lifecycle-conflict" && evidence == partialPersistenceEvidence {
		return evidence
	}
	switch code {
	case "jinushi-command-failed", "jinushi-protocol-failed", "jinushi-bootstrap-failed":
		switch evidence {
		case "invalid-run", "remote-exit", "stale-run-generation", "supervisor-unavailable":
			return evidence
		}
	}
	return ""
}

func safeProbeError(err error, fallback string) error {
	if err == nil {
		return nil
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return errors.New(SafeDiagnosticCode(failure.Code, fallback))
	}
	var providerFailure *jinushi.Failure
	if errors.As(err, &providerFailure) {
		classified := classify(providerFailure).(*Failure)
		return errors.New(SafeDiagnosticCode(classified.Code, fallback))
	}
	var sshFailure *ssh.Error
	if errors.As(err, &sshFailure) {
		return errors.New(SafeDiagnosticCode(classifySSHFailure(sshFailure.Kind), fallback))
	}
	return errors.New(SafeDiagnosticCode(err.Error(), fallback))
}

func classifiedFailureCode(err error, fallback string) string {
	if err == nil {
		return ""
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return SafeDiagnosticCode(failure.Code, fallback)
	}
	var providerFailure *jinushi.Failure
	if errors.As(err, &providerFailure) {
		classified := classify(providerFailure).(*Failure)
		return SafeDiagnosticCode(classified.Code, fallback)
	}
	var sshFailure *ssh.Error
	if errors.As(err, &sshFailure) {
		return SafeDiagnosticCode(classifySSHFailure(sshFailure.Kind), fallback)
	}
	return SafeDiagnosticCode(err.Error(), fallback)
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	var f *jinushi.Failure
	if errors.As(err, &f) {
		switch f.Kind {
		case jinushi.KindBootstrapFailed:
			return &Failure{Code: "jinushi-bootstrap-failed", Evidence: SafeFailureEvidence("jinushi-bootstrap-failed", f.Code)}
		case jinushi.KindBootstrapNotConfigured, jinushi.KindSupervisorUnavailable:
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
			return &Failure{Code: "jinushi-command-failed", Evidence: SafeFailureEvidence("jinushi-command-failed", f.Code)}
		case jinushi.KindProtocol:
			return &Failure{Code: "jinushi-protocol-failed", Evidence: SafeFailureEvidence("jinushi-protocol-failed", f.Code)}
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
type pendingSubmission struct {
	submissionID string
	restart      jinushi.RestartAttempt
}
type probeInputs struct {
	ssh              remoteRunner
	bindings         map[string]binding
	services         map[string]registry.Service
	tunnels          *tunnel.Manager
	httpClient       *http.Client
	endpointFailures map[tunnel.Identity]string
}
type Runtime struct {
	mu               sync.Mutex
	registryMu       sync.Mutex
	registry         *registry.Snapshot
	store            *config.Store
	ssh              remoteRunner
	bindings         map[string]binding
	services         map[string]registry.Service
	tunnels          *tunnel.Manager
	observer         *health.Observer
	pending          map[string]pendingSubmission
	jinushi          map[string]string
	jinushiErrors    map[string]string
	endpointFailures map[tunnel.Identity]string
	closed           bool
	httpClient       *http.Client
	shutdownCtx      context.Context
	shutdownCancel   context.CancelFunc
	activeOps        int
	activeDone       chan struct{}
	pollGate         chan struct{}
	pollCancel       context.CancelFunc
	pollVersion      uint64
	configVersion    uint64
	serviceVersions  map[string]uint64
	environmentGates map[string]chan struct{}
	serviceGates     map[string]chan struct{}
}

func key(env, service string) string { return env + "\x00" + service }

func closedSignal() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func (r *Runtime) beginOperation(ctx context.Context) (context.Context, func(), error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, errRuntimeClosed
	}
	if r.activeOps == 0 {
		r.activeDone = make(chan struct{})
	}
	r.activeOps++
	shutdownCtx := r.shutdownCtx
	r.mu.Unlock()

	opCtx, cancel := context.WithCancel(ctx)
	stopShutdown := context.AfterFunc(shutdownCtx, cancel)
	var once sync.Once
	end := func() {
		once.Do(func() {
			stopShutdown()
			cancel()
			r.mu.Lock()
			r.activeOps--
			if r.activeOps == 0 {
				close(r.activeDone)
			}
			r.mu.Unlock()
		})
	}
	return opCtx, end, nil
}

func (r *Runtime) invalidatePollLocked() context.CancelFunc {
	r.pollVersion++
	return r.pollCancel
}

func (r *Runtime) serviceGateLocked(serviceKey string) chan struct{} {
	if r.serviceGates == nil {
		r.serviceGates = map[string]chan struct{}{}
	}
	gate := r.serviceGates[serviceKey]
	if gate == nil {
		gate = make(chan struct{}, 1)
		r.serviceGates[serviceKey] = gate
	}
	return gate
}

func (r *Runtime) environmentGateLocked(environment string) chan struct{} {
	if r.environmentGates == nil {
		r.environmentGates = map[string]chan struct{}{}
	}
	gate := r.environmentGates[environment]
	if gate == nil {
		gate = make(chan struct{}, 1)
		r.environmentGates[environment] = gate
	}
	return gate
}

func acquireGate(ctx context.Context, gate chan struct{}) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Runtime) acquireServiceGate(ctx context.Context, serviceKey string) (func(), error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errRuntimeClosed
	}
	gate := r.serviceGateLocked(serviceKey)
	r.mu.Unlock()
	return acquireGate(ctx, gate)
}

func (r *Runtime) acquireEnvironmentGate(ctx context.Context, environment string) (func(), error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errRuntimeClosed
	}
	gate := r.environmentGateLocked(environment)
	r.mu.Unlock()
	return acquireGate(ctx, gate)
}

func cloneBindings(bindings map[string]binding) map[string]binding {
	result := make(map[string]binding, len(bindings))
	for id, value := range bindings {
		result[id] = value
	}
	return result
}

func cloneServices(services map[string]registry.Service) map[string]registry.Service {
	result := make(map[string]registry.Service, len(services))
	for id, value := range services {
		result[id] = value
	}
	return result
}

func cloneEndpointFailures(failures map[tunnel.Identity]string) map[tunnel.Identity]string {
	result := make(map[tunnel.Identity]string, len(failures))
	for identity, value := range failures {
		result[identity] = value
	}
	return result
}

func (r *Runtime) captureInputsLocked() probeInputs {
	return probeInputs{
		ssh:              r.ssh,
		bindings:         cloneBindings(r.bindings),
		services:         cloneServices(r.services),
		tunnels:          r.tunnels,
		httpClient:       r.httpClient,
		endpointFailures: cloneEndpointFailures(r.endpointFailures),
	}
}

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
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	r := &Runtime{
		ssh:              client,
		tunnels:          manager,
		pending:          map[string]pendingSubmission{},
		jinushi:          map[string]string{},
		endpointFailures: map[tunnel.Identity]string{},
		httpClient:       &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		shutdownCtx:      shutdownCtx,
		shutdownCancel:   shutdownCancel,
		activeDone:       closedSignal(),
		pollGate:         make(chan struct{}, 1),
		serviceVersions:  map[string]uint64{},
		serviceGates:     map[string]chan struct{}{},
		environmentGates: map[string]chan struct{}{},
	}
	if err := r.configure(snapshot); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Runtime) configure(snapshot *registry.Snapshot) error {
	bindings := map[string]binding{}
	services := map[string]registry.Service{}
	client := r.ssh
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
		bindings[string(env.ID)] = binding{env, control, observe}
	}
	for _, service := range snapshot.Services() {
		e, s := string(service.EnvironmentID), string(service.ID)
		services[key(e, s)] = service
	}
	if r.endpointFailures == nil {
		r.endpointFailures = map[tunnel.Identity]string{}
	}
	r.jinushiErrors = map[string]string{}
	inputs := probeInputs{
		ssh:              client,
		bindings:         cloneBindings(bindings),
		services:         cloneServices(services),
		tunnels:          r.tunnels,
		httpClient:       r.httpClient,
		endpointFailures: cloneEndpointFailures(r.endpointFailures),
	}
	observer, err := r.newObserver(snapshot, inputs)
	if err != nil {
		return err
	}
	r.registry = snapshot
	r.bindings = bindings
	r.services = services
	r.jinushi = map[string]string{}
	r.jinushiErrors = map[string]string{}
	r.observer = observer
	r.configVersion++
	if cancelPoll := r.invalidatePollLocked(); cancelPoll != nil {
		cancelPoll()
	}
	return nil
}

func (r *Runtime) newObserver(snapshot *registry.Snapshot, inputs probeInputs) (*health.Observer, error) {
	targets := make([]health.EnvironmentTarget, 0)
	for _, environment := range snapshot.Environments() {
		targets = append(targets, health.EnvironmentTarget{ID: string(environment.ID)})
	}
	probeTimeoutForObserver := probeTimeout
	for _, service := range snapshot.Services() {
		configuredTimeout := time.Duration(*service.Health.TimeoutMS) * time.Millisecond
		if configuredTimeout >= probeTimeoutForObserver {
			probeTimeoutForObserver = configuredTimeout + time.Second
		}
		for i := range targets {
			if targets[i].ID == string(service.EnvironmentID) {
				target := health.ServiceTarget{ID: string(service.ID)}
				for _, endpoint := range service.Endpoints {
					target.Endpoints = append(target.Endpoints, health.EndpointTarget{ID: string(endpoint.ID)})
				}
				targets[i].Services = append(targets[i].Services, target)
				break
			}
		}
	}
	probes := health.Probes{
		Connectivity: func(ctx context.Context, env string) (health.ConnectivityState, error) {
			return r.connectivityWith(ctx, inputs, env)
		},
		Process: func(ctx context.Context, env, id string) (health.ProcessState, error) {
			release, err := r.acquireServiceGate(ctx, key(env, id))
			if err != nil {
				return health.ProcessUnknown, err
			}
			defer release()
			return r.processWith(ctx, inputs, env, id)
		},
		Readiness: func(ctx context.Context, env, id string) (health.ReadinessState, error) {
			release, err := r.acquireServiceGate(ctx, key(env, id))
			if err != nil {
				return health.ReadinessUnknown, err
			}
			defer release()
			return r.readinessWith(ctx, inputs, env, id)
		},
		Endpoint: func(ctx context.Context, env, id, endpoint string) (health.EndpointState, error) {
			release, err := r.acquireServiceGate(ctx, key(env, id))
			if err != nil {
				return health.EndpointUnknown, err
			}
			defer release()
			return r.endpointStateWith(ctx, inputs, env, id, endpoint)
		},
	}
	return health.New(targets, probes, health.Options{PollInterval: 5 * time.Second, ProbeTimeout: probeTimeoutForObserver})
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
	opCtx, finish, err := r.beginOperation(context.Background())
	if err != nil {
		return &Failure{Code: "lifecycle-conflict"}
	}
	defer finish()
	r.registryMu.Lock()
	defer r.registryMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return &Failure{Code: "lifecycle-conflict"}
	}
	// An environment-only first draft may be completed with service definitions.
	// Never replace a registry that already owns services or tunnels.
	current := r.registry.Environments()
	if len(current) != 0 {
		incoming := snapshot.Environments()
		if len(current) != 1 || len(r.registry.Services()) != 0 || len(snapshot.Services()) == 0 || len(incoming) != 1 || current[0].ID != incoming[0].ID || current[0].SSHHost != incoming[0].SSHHost || current[0].Jinushi.StateDir != incoming[0].Jinushi.StateDir || !equalArgv(current[0].Jinushi.SupervisorStartCommand, incoming[0].Jinushi.SupervisorStartCommand) {
			r.mu.Unlock()
			return &Failure{Code: "lifecycle-conflict"}
		}
	}
	if r.store == nil {
		r.mu.Unlock()
		return &Failure{Code: "registration-unavailable"}
	}
	store, sshClient, tunnels, httpClient := r.store, r.ssh, r.tunnels, r.httpClient
	baseVersion := r.configVersion
	r.mu.Unlock()
	// Validate the new observer before committing persistent state. Its closures
	// capture the candidate maps and bind to the live Runtime's operation gates.
	candidate := &Runtime{ssh: sshClient, tunnels: tunnels, httpClient: httpClient}
	if err := candidate.configure(snapshot); err != nil {
		return &Failure{Code: "invalid-request"}
	}
	if err := opCtx.Err(); err != nil {
		return classify(err)
	}
	if err := store.Save(snapshot); err != nil {
		return err
	}
	// Rebind probe closures to the live runtime, not the temporary candidate.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.configVersion != baseVersion {
		return &Failure{Code: "lifecycle-conflict"}
	}
	return r.configure(snapshot)
}

// EnsureJinushi is an explicit bootstrap-capable operation; background polls
// only use the observation-only client.
func (r *Runtime) EnsureJinushi(ctx context.Context, env string) error {
	opCtx, finish, err := r.beginOperation(ctx)
	if err != nil {
		return &Failure{Code: "lifecycle-conflict"}
	}
	defer finish()

	r.mu.Lock()
	b, ok := r.bindings[env]
	if !ok {
		r.mu.Unlock()
		return &Failure{Code: "unknown-identity"}
	}
	configVersion := r.configVersion
	gate := r.environmentGateLocked(env)
	cancelPoll := r.invalidatePollLocked()
	r.mu.Unlock()
	if cancelPoll != nil {
		cancelPoll()
	}
	release, err := acquireGate(opCtx, gate)
	if err != nil {
		return classify(err)
	}
	defer release()
	r.mu.Lock()
	if r.closed || r.configVersion != configVersion || opCtx.Err() != nil {
		r.mu.Unlock()
		if err := opCtx.Err(); err != nil {
			return classify(err)
		}
		return &Failure{Code: "lifecycle-conflict"}
	}
	r.mu.Unlock()

	err = b.control.EnsureReady(opCtx)
	r.mu.Lock()
	var cancelAfterPublish context.CancelFunc
	if !r.closed && r.configVersion == configVersion {
		if _, stillRegistered := r.bindings[env]; stillRegistered {
			if err != nil {
				r.jinushi[env] = "unavailable"
				r.jinushiErrors[env] = classifiedFailureCode(err, "jinushi-unavailable")
			} else {
				r.jinushi[env] = "ready"
				delete(r.jinushiErrors, env)
			}
			cancelAfterPublish = r.invalidatePollLocked()
		}
	}
	r.mu.Unlock()
	if cancelAfterPublish != nil {
		cancelAfterPublish()
	}
	if err != nil {
		return classify(err)
	}
	return nil
}
func (r *Runtime) serviceLocked(env, id string) (registry.Service, error) {
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

func desiredStateCandidate(snapshot *registry.Snapshot, s registry.Service, desired registry.DesiredState) (*registry.Snapshot, registry.Service, error) {
	if s.DesiredState == desired {
		return snapshot, s, nil
	}

	services := snapshot.Services()
	found := false
	for i := range services {
		if services[i].EnvironmentID == s.EnvironmentID && services[i].ID == s.ID {
			services[i].DesiredState = desired
			found = true
			break
		}
	}
	if !found {
		return nil, registry.Service{}, &Failure{Code: "lifecycle-conflict"}
	}

	candidate, err := registry.NewSnapshot(snapshot.Environments(), services)
	if err != nil {
		return nil, registry.Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	var updated registry.Service
	for _, service := range candidate.Services() {
		if service.EnvironmentID == s.EnvironmentID && service.ID == s.ID {
			updated = service
			break
		}
	}
	if updated.ID == "" {
		return nil, registry.Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	return candidate, updated, nil
}

func (r *Runtime) mutate(ctx context.Context, env, id, action string) (Service, error) {
	opCtx, finish, err := r.beginOperation(ctx)
	if err != nil {
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	defer finish()

	pendingKey := key(env, id)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	if _, err := r.serviceLocked(env, id); err != nil {
		r.mu.Unlock()
		return Service{}, err
	}
	gate := r.serviceGateLocked(pendingKey)
	cancelPoll := r.invalidatePollLocked()
	r.mu.Unlock()
	if cancelPoll != nil {
		cancelPoll()
	}
	release, err := acquireGate(opCtx, gate)
	if err != nil {
		return Service{}, classify(err)
	}
	defer release()

	r.mu.Lock()
	if r.closed || opCtx.Err() != nil {
		r.mu.Unlock()
		if err := opCtx.Err(); err != nil {
			return Service{}, classify(err)
		}
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	s, err := r.serviceLocked(env, id)
	if err != nil {
		r.mu.Unlock()
		return Service{}, err
	}
	owner := s.CorrelationOwner()
	b := r.bindings[env]
	configVersion := r.configVersion
	pending := r.pending[pendingKey]
	if action != "stop" && pending.submissionID == "" {
		var token string
		token, err = jinushi.NewSubmissionID()
		if err != nil {
			r.mu.Unlock()
			return Service{}, classify(err)
		}
		pending.submissionID = token
		r.pending[pendingKey] = pending
	}
	if r.serviceVersions == nil {
		r.serviceVersions = map[string]uint64{}
	}
	r.serviceVersions[pendingKey]++
	serviceVersion := r.serviceVersions[pendingKey]
	inputs := r.captureInputsLocked()
	r.mu.Unlock()

	var run jinushi.Run
	var observedRun *jinushi.Run
	switch action {
	case "start":
		run, err = b.control.Start(opCtx, jinushi.StartRequest{Service: jinushi.Service{ID: owner, Argv: s.Execution.Argv, Cwd: s.Execution.CWD}, SubmissionID: pending.submissionID})
		observedRun = &run
	case "restart":
		run, err = b.control.RestartWithAttempt(opCtx, jinushi.StartRequest{Service: jinushi.Service{ID: owner, Argv: s.Execution.Argv, Cwd: s.Execution.CWD}, SubmissionID: pending.submissionID}, &pending.restart)
		observedRun = &run
	case "stop":
		var status jinushi.ServiceStatus
		status, err = b.control.Stop(opCtx, owner)
		observedRun = status.Run
	}
	r.mu.Lock()
	if action != "stop" {
		r.pending[pendingKey] = pending
	}
	if err != nil {
		var failure *jinushi.Failure
		if action != "stop" && errors.As(err, &failure) && (failure.Kind == jinushi.KindInvalid || failure.Kind == jinushi.KindCommand || failure.Kind == jinushi.KindBootstrapFailed) {
			delete(r.pending, pendingKey)
		}
		r.mu.Unlock()
		return Service{}, classify(err)
	}
	if action != "stop" {
		if _, stateErr := processState(run); stateErr != nil {
			r.mu.Unlock()
			return Service{}, &Failure{Code: "lifecycle-conflict"}
		}
	}
	if action == "stop" || run.ID != "" {
		delete(r.pending, pendingKey)
	}
	if r.closed || r.configVersion != configVersion {
		r.mu.Unlock()
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	r.mu.Unlock()

	desired := registry.DesiredRunning
	if action == "stop" {
		desired = registry.DesiredStopped
	}
	r.registryMu.Lock()
	r.mu.Lock()
	if r.closed || r.configVersion != configVersion {
		r.mu.Unlock()
		r.registryMu.Unlock()
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	s, err = r.serviceLocked(env, id)
	if err != nil {
		r.mu.Unlock()
		r.registryMu.Unlock()
		return Service{}, err
	}
	candidate, s, err := desiredStateCandidate(r.registry, s, desired)
	store := r.store
	r.mu.Unlock()
	if err == nil && store != nil {
		err = store.Save(candidate)
		if err != nil {
			err = &Failure{Code: "lifecycle-conflict", Evidence: "desired service state could not be saved after the Jinushi operation succeeded"}
		}
	}
	if err != nil {
		r.registryMu.Unlock()
		return Service{}, err
	}
	r.mu.Lock()
	if r.closed || r.configVersion != configVersion {
		r.mu.Unlock()
		r.registryMu.Unlock()
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	r.registry = candidate
	r.services[pendingKey] = s
	r.jinushi[env] = "ready"
	delete(r.jinushiErrors, env)
	cancelAfterPublish := r.invalidatePollLocked()
	r.mu.Unlock()
	if cancelAfterPublish != nil {
		cancelAfterPublish()
	}
	r.registryMu.Unlock()

	if action == "stop" {
		for _, ep := range s.Endpoints {
			identity := tunnel.Identity{Environment: env, Service: id, Endpoint: string(ep.ID)}
			if _, getErr := inputs.tunnels.Get(identity); getErr == nil {
				if _, stopErr := inputs.tunnels.Stop(opCtx, identity); stopErr != nil {
					continue
				}
			}
			delete(inputs.endpointFailures, identity)
		}
	} else {
		r.ensureHealth(opCtx, s, &inputs)
	}
	r.mu.Lock()
	if !r.closed && r.configVersion == configVersion && r.serviceVersions[pendingKey] == serviceVersion {
		for _, endpoint := range s.Endpoints {
			identity := tunnel.Identity{Environment: env, Service: id, Endpoint: string(endpoint.ID)}
			if failure, ok := inputs.endpointFailures[identity]; ok {
				r.endpointFailures[identity] = failure
			} else {
				delete(r.endpointFailures, identity)
			}
		}
	}
	cancelAfterPublish = r.invalidatePollLocked()
	r.mu.Unlock()
	if cancelAfterPublish != nil {
		cancelAfterPublish()
	}

	observation := r.actionObservation(opCtx, s, action, observedRun, inputs)
	r.mu.Lock()
	if r.closed || r.configVersion != configVersion {
		r.mu.Unlock()
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	if r.serviceVersions[pendingKey] != serviceVersion {
		r.mu.Unlock()
		return Service{}, &Failure{Code: "lifecycle-conflict"}
	}
	if err := opCtx.Err(); err != nil {
		r.mu.Unlock()
		return Service{}, classify(err)
	}
	r.observer.PublishServiceObservation(env, observation)
	cancelAfterPublish = r.invalidatePollLocked()
	r.mu.Unlock()
	if cancelAfterPublish != nil {
		cancelAfterPublish()
	}
	return r.serviceView(s, observation), nil
}

func (r *Runtime) actionObservation(ctx context.Context, s registry.Service, action string, run *jinushi.Run, inputs probeInputs) health.ServiceObservation {
	observation := health.ServiceObservation{ID: string(s.ID), Process: health.ProcessUnknown, Readiness: health.ReadinessUnknown}
	if run != nil {
		var err error
		observation.Process, err = processState(*run)
		if err != nil {
			observation.ProcessError = safeProbeError(err, "jinushi-state-uncertain").Error()
		}
	}
	if action != "stop" {
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		readiness, err := r.readinessWith(probeCtx, inputs, string(s.EnvironmentID), string(s.ID))
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
		state, err := r.endpointStateWith(ctx, inputs, string(s.EnvironmentID), string(s.ID), string(endpoint.ID))
		message := ""
		if err != nil {
			message = err.Error()
		}
		observation.Endpoints = append(observation.Endpoints, health.EndpointObservation{ID: string(endpoint.ID), State: state, Error: message})
	}
	return observation
}

func (r *Runtime) ensureHealth(ctx context.Context, s registry.Service, inputs *probeInputs) {
	for _, ep := range s.Endpoints {
		if ep.ID == s.Health.EndpointID {
			_, _ = r.ensure(ctx, s, ep, inputs)
			return
		}
	}
}
func (r *Runtime) ensure(ctx context.Context, s registry.Service, ep registry.Endpoint, inputs *probeInputs) (tunnel.Snapshot, error) {
	identity := tunnel.Identity{Environment: string(s.EnvironmentID), Service: string(s.ID), Endpoint: string(ep.ID)}
	if err := ctx.Err(); err != nil {
		return tunnel.Snapshot{}, err
	}
	b, exists := inputs.bindings[string(s.EnvironmentID)]
	if !exists {
		return tunnel.Snapshot{}, errors.New(endpointEvidenceMissing)
	}
	port, err := resolveEndpoint(ctx, inputs.ssh, b, s, ep)
	if err != nil {
		if ctx.Err() != nil {
			return tunnel.Snapshot{}, ctx.Err()
		}
		if current, getErr := inputs.tunnels.Get(identity); getErr == nil && (current.State == tunnel.StateReady || current.State == tunnel.StateStarting) {
			if _, stopErr := inputs.tunnels.Stop(ctx, identity); stopErr != nil {
				err = errors.New(endpointEvidenceStale)
			}
		}
		inputs.endpointFailures[identity] = err.Error()
		return tunnel.Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return tunnel.Snapshot{}, err
	}
	delete(inputs.endpointFailures, identity)
	if current, getErr := inputs.tunnels.Get(identity); getErr == nil {
		if (current.State == tunnel.StateReady || current.State == tunnel.StateStarting) && current.RemotePort == port {
			return current, nil
		}
		if current.State == tunnel.StateReady || current.State == tunnel.StateStarting {
			if _, stopErr := inputs.tunnels.Stop(ctx, identity); stopErr != nil {
				failure := errors.New(endpointEvidenceStale)
				inputs.endpointFailures[identity] = failure.Error()
				return current, failure
			}
		}
	}
	snapshot, err := inputs.tunnels.Start(ctx, tunnel.Request{Identity: identity, SSHHost: b.environment.SSHHost, RemotePort: port})
	if err != nil {
		inputs.endpointFailures[identity] = "The SSH tunnel to the registered endpoint could not be established."
		return snapshot, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), commandTimeout)
		_, cleanupErr := inputs.tunnels.Stop(cleanupCtx, identity)
		cancel()
		if cleanupErr != nil {
			return snapshot, errors.Join(ctxErr, fmt.Errorf("stop tunnel started after cancellation: %w", cleanupErr))
		}
		return snapshot, ctxErr
	}
	return snapshot, nil
}
func (r *Runtime) Ensure(ctx context.Context, env, id, endpoint string) (Endpoint, error) {
	opCtx, finish, err := r.beginOperation(ctx)
	if err != nil {
		return Endpoint{}, &Failure{Code: "lifecycle-conflict"}
	}
	defer finish()
	pendingKey := key(env, id)
	r.mu.Lock()
	s, err := r.serviceLocked(env, id)
	if err != nil {
		r.mu.Unlock()
		return Endpoint{}, err
	}
	gate := r.serviceGateLocked(pendingKey)
	configVersion := r.configVersion
	inputs := r.captureInputsLocked()
	cancelPoll := r.invalidatePollLocked()
	r.mu.Unlock()
	if cancelPoll != nil {
		cancelPoll()
	}
	release, err := acquireGate(opCtx, gate)
	if err != nil {
		return Endpoint{}, classify(err)
	}
	defer release()
	r.mu.Lock()
	if r.closed || r.configVersion != configVersion || opCtx.Err() != nil {
		r.mu.Unlock()
		if err := opCtx.Err(); err != nil {
			return Endpoint{}, classify(err)
		}
		return Endpoint{}, &Failure{Code: "lifecycle-conflict"}
	}
	s, err = r.serviceLocked(env, id)
	inputs = r.captureInputsLocked()
	r.mu.Unlock()
	if err != nil {
		return Endpoint{}, err
	}
	for _, ep := range s.Endpoints {
		if string(ep.ID) == endpoint {
			identity := tunnel.Identity{Environment: env, Service: id, Endpoint: endpoint}
			snap, err := r.ensure(opCtx, s, ep, &inputs)
			if err != nil {
				if opCtx.Err() != nil {
					return Endpoint{}, classify(opCtx.Err())
				}
				evidence := inputs.endpointFailures[identity]
				if evidence == "" {
					evidence = "The SSH tunnel to the registered endpoint could not be established."
				}
				r.mu.Lock()
				var cancelAfterPublish context.CancelFunc
				if !r.closed && r.configVersion == configVersion {
					r.endpointFailures[identity] = evidence
					cancelAfterPublish = r.invalidatePollLocked()
				}
				r.mu.Unlock()
				if cancelAfterPublish != nil {
					cancelAfterPublish()
				}
				failure := &Failure{Code: "endpoint-unavailable", Evidence: bounded(evidence)}
				return Endpoint{}, failure
			}
			state, probeErr := r.endpointStateWith(opCtx, inputs, env, id, endpoint)
			observation := health.EndpointObservation{State: state}
			if probeErr != nil {
				observation.Error = probeErr.Error()
			}
			r.mu.Lock()
			current, currentErr := r.serviceLocked(env, id)
			if r.closed || r.configVersion != configVersion || currentErr != nil {
				r.mu.Unlock()
				return Endpoint{}, &Failure{Code: "lifecycle-conflict"}
			}
			if failure := inputs.endpointFailures[identity]; failure == "" {
				delete(r.endpointFailures, identity)
			} else {
				r.endpointFailures[identity] = failure
			}
			cancelAfterPublish := r.invalidatePollLocked()
			r.mu.Unlock()
			if cancelAfterPublish != nil {
				cancelAfterPublish()
			}
			return r.endpointView(current, ep, observation, snap), nil
		}
	}
	return Endpoint{}, &Failure{Code: "unknown-identity"}
}
func (r *Runtime) connectivityWith(ctx context.Context, inputs probeInputs, env string) (health.ConnectivityState, error) {
	b, exists := inputs.bindings[env]
	if !exists {
		return health.ConnectivityUnknown, errors.New("unknown environment")
	}
	_, err := inputs.ssh.Run(ctx, b.environment.SSHHost, []string{"true"}, probeTimeout)
	if err == nil {
		return health.ConnectivityConnected, nil
	}
	var transportErr *ssh.Error
	if errors.As(err, &transportErr) && transportErr.Kind == ssh.FailureTransport {
		return health.ConnectivityUnreachable, nil
	}
	if errors.As(err, &transportErr) && transportErr.Kind == ssh.FailureRemoteCommand {
		return health.ConnectivityError, errors.New("ssh-connectivity-failed")
	}
	return health.ConnectivityError, safeProbeError(err, "ssh-connectivity-failed")
}
func (r *Runtime) connectivity(ctx context.Context, env string) (health.ConnectivityState, error) {
	r.mu.Lock()
	inputs := r.captureInputsLocked()
	r.mu.Unlock()
	return r.connectivityWith(ctx, inputs, env)
}
func (r *Runtime) processWith(ctx context.Context, inputs probeInputs, env, id string) (health.ProcessState, error) {
	s, exists := inputs.services[key(env, id)]
	b, bindingExists := inputs.bindings[env]
	if !exists || !bindingExists {
		return health.ProcessUnknown, errors.New("unknown service")
	}
	status, err := b.observe.Status(ctx, s.CorrelationOwner())
	if err != nil {
		return health.ProcessUnknown, safeProbeError(err, "remote-failure")
	}
	if status.Run == nil {
		return health.ProcessUnknown, nil
	}
	state, err := processState(*status.Run)
	if err != nil {
		return state, safeProbeError(err, "jinushi-state-uncertain")
	}
	return state, nil
}
func (r *Runtime) process(ctx context.Context, env, id string) (health.ProcessState, error) {
	r.mu.Lock()
	inputs := r.captureInputsLocked()
	r.mu.Unlock()
	return r.processWith(ctx, inputs, env, id)
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
func (r *Runtime) readinessWith(ctx context.Context, inputs probeInputs, env, id string) (health.ReadinessState, error) {
	s, exists := inputs.services[key(env, id)]
	if !exists {
		return health.ReadinessUnknown, errors.New("unknown service")
	}
	identity := tunnel.Identity{Environment: env, Service: id, Endpoint: string(s.Health.EndpointID)}
	if failure := inputs.endpointFailures[identity]; strings.HasPrefix(failure, "Dynamic endpoint") {
		return health.ReadinessError, safeProbeError(errors.New(failure), "readiness-check-failed")
	}
	snap, err := inputs.tunnels.Get(identity)
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
		return health.ReadinessError, safeProbeError(err, "readiness-check-failed")
	}
	u.Path = s.Health.Path
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(*s.Health.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return health.ReadinessError, safeProbeError(err, "readiness-check-failed")
	}
	resp, err := inputs.httpClient.Do(req)
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
func (r *Runtime) readiness(ctx context.Context, env, id string) (health.ReadinessState, error) {
	r.mu.Lock()
	inputs := r.captureInputsLocked()
	r.mu.Unlock()
	return r.readinessWith(ctx, inputs, env, id)
}
func (r *Runtime) endpointStateWith(ctx context.Context, inputs probeInputs, env, id, ep string) (health.EndpointState, error) {
	identity := tunnel.Identity{Environment: env, Service: id, Endpoint: ep}
	if failure := inputs.endpointFailures[identity]; failure != "" {
		return health.EndpointError, errors.New(SafeDiagnosticCode(failure, "remote-failure"))
	}
	snap, err := inputs.tunnels.Get(identity)
	if err != nil {
		return health.EndpointUnavailable, nil
	}
	switch snap.State {
	case tunnel.StateReady:
		if err := probeApplicationEndpoint(ctx, inputs.httpClient, snap.LocalURL); err != nil {
			return health.EndpointUnavailable, safeProbeError(err, "endpoint-application-unavailable")
		}
		return health.EndpointAvailable, nil
	case tunnel.StateFailed:
		return health.EndpointError, errors.New("tunnel-unavailable")
	case tunnel.StateStopped:
		return health.EndpointUnavailable, nil
	default:
		return health.EndpointUnknown, nil
	}
}

func (r *Runtime) endpointState(ctx context.Context, env, id, ep string) (health.EndpointState, error) {
	r.mu.Lock()
	inputs := r.captureInputsLocked()
	r.mu.Unlock()
	return r.endpointStateWith(ctx, inputs, env, id, ep)
}

func probeApplicationEndpoint(ctx context.Context, client *http.Client, localURL string) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, localURL, nil)
	if err != nil {
		return errors.New(endpointApplicationDown)
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New(endpointApplicationDown)
	}
	_ = response.Body.Close()
	return nil
}
func (r *Runtime) Poll(ctx context.Context) error {
	opCtx, finish, err := r.beginOperation(ctx)
	if err != nil {
		return errRuntimeClosed
	}
	defer finish()
	select {
	case r.pollGate <- struct{}{}:
		defer func() { <-r.pollGate }()
	case <-opCtx.Done():
		return opCtx.Err()
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errRuntimeClosed
	}
	pollCtx, cancel := context.WithCancel(opCtx)
	r.pollCancel = cancel
	pollVersion := r.pollVersion
	configVersion := r.configVersion
	registrySnapshot := r.registry
	inputs := r.captureInputsLocked()
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		r.pollCancel = nil
		r.mu.Unlock()
	}()
	internalCancellation := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.mu.Lock()
		closed := r.closed
		r.mu.Unlock()
		if closed {
			return errRuntimeClosed
		}
		if pollCtx.Err() != nil {
			return nil
		}
		return err
	}
	for _, service := range registrySnapshot.Services() {
		if err := pollCtx.Err(); err != nil {
			return internalCancellation(err)
		}
		serviceKey := key(string(service.EnvironmentID), string(service.ID))
		if service.DesiredState == registry.DesiredRunning {
			release, gateErr := r.acquireServiceGate(pollCtx, serviceKey)
			if gateErr != nil {
				return internalCancellation(gateErr)
			}
			r.ensureHealth(pollCtx, service, &inputs)
			release()
		}
		for _, endpoint := range service.Endpoints {
			if endpoint.Resolution == nil || (endpoint.ID == service.Health.EndpointID && service.DesiredState == registry.DesiredRunning) {
				continue
			}
			identity := tunnel.Identity{Environment: string(service.EnvironmentID), Service: string(service.ID), Endpoint: string(endpoint.ID)}
			current, getErr := inputs.tunnels.Get(identity)
			if !((getErr == nil && (current.State == tunnel.StateReady || current.State == tunnel.StateStarting)) || inputs.endpointFailures[identity] != "") {
				continue
			}
			release, gateErr := r.acquireServiceGate(pollCtx, serviceKey)
			if gateErr != nil {
				return internalCancellation(gateErr)
			}
			_, _ = r.ensure(pollCtx, service, endpoint, &inputs)
			release()
		}
	}
	candidate, err := r.newObserver(registrySnapshot, inputs)
	if err != nil {
		return err
	}
	if err := candidate.Poll(pollCtx); err != nil {
		return internalCancellation(err)
	}
	statuses := make(map[string]string)
	failureCodes := make(map[string]string)
	for _, environment := range candidate.Snapshot().Environments {
		if environment.State != health.ConnectivityConnected {
			statuses[environment.ID] = "unknown"
			continue
		}
		binding, exists := inputs.bindings[environment.ID]
		if !exists {
			statuses[environment.ID] = "unavailable"
			continue
		}
		release, gateErr := r.acquireEnvironmentGate(pollCtx, environment.ID)
		if gateErr != nil {
			return internalCancellation(gateErr)
		}
		readyErr := binding.observe.EnsureReady(pollCtx)
		release()
		if readyErr != nil {
			statuses[environment.ID] = "unavailable"
			failureCodes[environment.ID] = classifiedFailureCode(readyErr, "jinushi-unavailable")
		} else {
			statuses[environment.ID] = "ready"
		}
	}
	if err := pollCtx.Err(); err != nil {
		return internalCancellation(err)
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errRuntimeClosed
	}
	if pollVersion != r.pollVersion || configVersion != r.configVersion {
		r.mu.Unlock()
		return internalCancellation(context.Canceled)
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	r.observer = candidate
	r.jinushi = statuses
	r.jinushiErrors = failureCodes
	r.endpointFailures = inputs.endpointFailures
	r.mu.Unlock()
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
	if !r.closed {
		r.closed = true
		r.pollVersion++
	}
	cancelPoll := r.pollCancel
	shutdownCancel := r.shutdownCancel
	activeDone := r.activeDone
	r.mu.Unlock()
	if cancelPoll != nil {
		cancelPoll()
	}
	if shutdownCancel != nil {
		shutdownCancel()
	}
	var waitErr error
	select {
	case <-activeDone:
	case <-ctx.Done():
		waitErr = ctx.Err()
	}
	return errors.Join(waitErr, r.tunnels.Close(ctx))
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
	if message == "" {
		return ""
	}
	return SafeDiagnosticCode(message, "remote-failure")
}
func tunnelFailure(s tunnel.Snapshot) string {
	if s.State == tunnel.StateFailed {
		return "tunnel-unavailable"
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
	if failure != "" {
		failure = SafeDiagnosticCode(failure, "remote-failure")
	}
	return Endpoint{ID: string(ep.ID), Label: ep.Label, EndpointState: state, TunnelState: t, LocalURL: snap.LocalURL, Failure: failure}
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
	if result.ReadinessError == "" && result.Process != health.ProcessStopped && (result.Readiness == health.ReadinessNotReady || result.Readiness == health.ReadinessUnhealthy) {
		result.ReadinessError = "readiness-check-failed"
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
	registrySnapshot := r.registry
	observer := r.observer
	services := cloneServices(r.services)
	jinushi := make(map[string]string, len(r.jinushi))
	for id, status := range r.jinushi {
		jinushi[id] = status
	}
	jinushiErrors := make(map[string]string, len(r.jinushiErrors))
	for id, code := range r.jinushiErrors {
		jinushiErrors[id] = code
	}
	r.mu.Unlock()
	observed := observer.Snapshot()
	result := State{Version: 1, Environments: []Environment{}}
	for _, e := range registrySnapshot.Environments() {
		view := Environment{ID: string(e.ID), SSHHost: e.SSHHost, Connectivity: health.ConnectivityUnknown, Jinushi: "unknown", Services: []Service{}}
		if status := jinushi[view.ID]; status != "" {
			view.Jinushi = status
		}
		for _, o := range observed.Environments {
			if o.ID == view.ID {
				view.Connectivity = o.State
				view.Error = observationError(o.Error)
				for _, so := range o.Services {
					view.Services = append(view.Services, r.serviceView(services[key(view.ID, so.ID)], so))
				}
				break
			}
		}
		if view.Error == "" && view.Connectivity == health.ConnectivityUnreachable {
			view.Error = "host-unreachable"
		}
		if view.Error == "" {
			view.Error = SafeDiagnosticCode(jinushiErrors[view.ID], "")
		}
		if len(view.Services) == 0 {
			for _, s := range registrySnapshot.Services() {
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
