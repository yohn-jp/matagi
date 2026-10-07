// Package health provides side-effect-free, typed health observations.
package health

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type ConnectivityState string

const (
	ConnectivityUnknown     ConnectivityState = "unknown"
	ConnectivityConnected   ConnectivityState = "connected"
	ConnectivityUnreachable ConnectivityState = "unreachable"
	ConnectivityError       ConnectivityState = "error"
)

type ProcessState string

const (
	ProcessUnknown  ProcessState = "unknown"
	ProcessStopped  ProcessState = "stopped"
	ProcessStarting ProcessState = "starting"
	ProcessRunning  ProcessState = "running"
	ProcessError    ProcessState = "error"
)

type ReadinessState string

const (
	ReadinessUnknown   ReadinessState = "unknown"
	ReadinessNotReady  ReadinessState = "not-ready"
	ReadinessReady     ReadinessState = "ready"
	ReadinessUnhealthy ReadinessState = "unhealthy"
	ReadinessError     ReadinessState = "error"
)

type EndpointState string

const (
	EndpointUnknown     EndpointState = "unknown"
	EndpointAvailable   EndpointState = "available"
	EndpointUnavailable EndpointState = "unavailable"
	EndpointError       EndpointState = "error"
)

type ServiceState string

const (
	ServiceUnknown   ServiceState = "unknown"
	ServiceStopped   ServiceState = "stopped"
	ServiceStarting  ServiceState = "starting"
	ServiceReady     ServiceState = "ready"
	ServiceUnhealthy ServiceState = "unhealthy"
)

type EnvironmentTarget struct {
	ID       string
	Services []ServiceTarget
}

type ServiceTarget struct {
	ID        string
	Endpoints []EndpointTarget
}

type EndpointTarget struct {
	ID string
}

type Probes struct {
	Connectivity func(context.Context, string) (ConnectivityState, error)
	Process      func(context.Context, string, string) (ProcessState, error)
	Readiness    func(context.Context, string, string) (ReadinessState, error)
	Endpoint     func(context.Context, string, string, string) (EndpointState, error)
}

type Options struct {
	PollInterval time.Duration
	ProbeTimeout time.Duration
}

type EndpointObservation struct {
	ID    string
	State EndpointState
	Error string
}

type ServiceObservation struct {
	ID             string
	State          ServiceState
	Process        ProcessState
	ProcessError   string
	Readiness      ReadinessState
	ReadinessError string
	Endpoints      []EndpointObservation
}

type EnvironmentObservation struct {
	ID       string
	State    ConnectivityState
	Error    string
	Services []ServiceObservation
}

type Snapshot struct {
	Environments []EnvironmentObservation
}

type Observer struct {
	environments []EnvironmentTarget
	probes       Probes
	options      Options

	pollGate chan struct{}
	mu       sync.RWMutex
	snapshot Snapshot
}

func New(environments []EnvironmentTarget, probes Probes, options Options) (*Observer, error) {
	if probes.Connectivity == nil || probes.Process == nil || probes.Readiness == nil || probes.Endpoint == nil {
		return nil, errors.New("all health probes are required")
	}
	if options.PollInterval <= 0 {
		return nil, errors.New("poll interval must be positive")
	}
	if options.ProbeTimeout <= 0 {
		return nil, errors.New("probe timeout must be positive")
	}
	targets, err := normalizeTargets(environments)
	if err != nil {
		return nil, err
	}
	return &Observer{
		environments: targets,
		probes:       probes,
		options:      options,
		pollGate:     make(chan struct{}, 1),
	}, nil
}

func normalizeTargets(environments []EnvironmentTarget) ([]EnvironmentTarget, error) {
	result := make([]EnvironmentTarget, len(environments))
	seenEnvironments := make(map[string]struct{}, len(environments))
	for i, environment := range environments {
		if environment.ID == "" {
			return nil, errors.New("environment ID must not be empty")
		}
		if _, exists := seenEnvironments[environment.ID]; exists {
			return nil, fmt.Errorf("duplicate environment ID %q", environment.ID)
		}
		seenEnvironments[environment.ID] = struct{}{}
		result[i] = EnvironmentTarget{ID: environment.ID, Services: append([]ServiceTarget(nil), environment.Services...)}
		seenServices := make(map[string]struct{}, len(environment.Services))
		for j := range result[i].Services {
			service := &result[i].Services[j]
			if service.ID == "" {
				return nil, fmt.Errorf("service ID must not be empty in environment %q", environment.ID)
			}
			if _, exists := seenServices[service.ID]; exists {
				return nil, fmt.Errorf("duplicate service ID %q in environment %q", service.ID, environment.ID)
			}
			seenServices[service.ID] = struct{}{}
			service.Endpoints = append([]EndpointTarget(nil), service.Endpoints...)
			seenEndpoints := make(map[string]struct{}, len(service.Endpoints))
			for _, endpoint := range service.Endpoints {
				if endpoint.ID == "" {
					return nil, fmt.Errorf("endpoint ID must not be empty in service %q", service.ID)
				}
				if _, exists := seenEndpoints[endpoint.ID]; exists {
					return nil, fmt.Errorf("duplicate endpoint ID %q in service %q", endpoint.ID, service.ID)
				}
				seenEndpoints[endpoint.ID] = struct{}{}
			}
			sort.Slice(service.Endpoints, func(a, b int) bool { return service.Endpoints[a].ID < service.Endpoints[b].ID })
		}
		sort.Slice(result[i].Services, func(a, b int) bool { return result[i].Services[a].ID < result[i].Services[b].ID })
	}
	sort.Slice(result, func(a, b int) bool { return result[a].ID < result[b].ID })
	return result, nil
}

// Poll performs one bounded observation cycle and publishes its complete snapshot.
// A parent cancellation leaves the previously published snapshot unchanged.
func (o *Observer) Poll(ctx context.Context) error {
	select {
	case o.pollGate <- struct{}{}:
		defer func() { <-o.pollGate }()
	case <-ctx.Done():
		return ctx.Err()
	}

	next := Snapshot{Environments: make([]EnvironmentObservation, 0, len(o.environments))}
	for _, environment := range o.environments {
		connectivity, probeErr := callProbe(ctx, o.options.ProbeTimeout, func(probeCtx context.Context) (ConnectivityState, error) {
			return o.probes.Connectivity(probeCtx, environment.ID)
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		connectivity, connectionErr := normalizeConnectivity(connectivity, probeErr)
		envObservation := EnvironmentObservation{ID: environment.ID, State: connectivity, Error: connectionErr}
		for _, service := range environment.Services {
			serviceObservation := ServiceObservation{ID: service.ID, Process: ProcessUnknown, Readiness: ReadinessUnknown}
			if connectivity == ConnectivityConnected {
				process, err := callProbe(ctx, o.options.ProbeTimeout, func(probeCtx context.Context) (ProcessState, error) {
					return o.probes.Process(probeCtx, environment.ID, service.ID)
				})
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				serviceObservation.Process, serviceObservation.ProcessError = normalizeProcess(process, err)

				readiness, err := callProbe(ctx, o.options.ProbeTimeout, func(probeCtx context.Context) (ReadinessState, error) {
					return o.probes.Readiness(probeCtx, environment.ID, service.ID)
				})
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				serviceObservation.Readiness, serviceObservation.ReadinessError = normalizeReadiness(readiness, err)
			}
			serviceObservation.State = AggregateServiceState(serviceObservation.Process, serviceObservation.Readiness)
			serviceObservation.Endpoints = make([]EndpointObservation, 0, len(service.Endpoints))
			for _, endpoint := range service.Endpoints {
				state, err := callProbe(ctx, o.options.ProbeTimeout, func(probeCtx context.Context) (EndpointState, error) {
					return o.probes.Endpoint(probeCtx, environment.ID, service.ID, endpoint.ID)
				})
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				state, message := normalizeEndpoint(state, err)
				serviceObservation.Endpoints = append(serviceObservation.Endpoints, EndpointObservation{ID: endpoint.ID, State: state, Error: message})
			}
			envObservation.Services = append(envObservation.Services, serviceObservation)
		}
		next.Environments = append(next.Environments, envObservation)
	}

	o.mu.Lock()
	o.snapshot = cloneSnapshot(next)
	o.mu.Unlock()
	return nil
}

// Run observes immediately, then polls at the configured interval until canceled.
func (o *Observer) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return nil
	}
	if err := o.Poll(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	ticker := time.NewTicker(o.options.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := o.Poll(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

func (o *Observer) Snapshot() Snapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return cloneSnapshot(o.snapshot)
}

// PublishServiceObservation records fresh evidence returned by an explicit
// lifecycle action. The next Poll replaces it with a complete observation.
func (o *Observer) PublishServiceObservation(environmentID string, observation ServiceObservation) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	next := cloneSnapshot(o.snapshot)
	environmentIndex := -1
	for i := range next.Environments {
		if next.Environments[i].ID == environmentID {
			environmentIndex = i
			break
		}
	}
	if environmentIndex < 0 {
		var target *EnvironmentTarget
		for i := range o.environments {
			if o.environments[i].ID == environmentID {
				target = &o.environments[i]
				break
			}
		}
		if target == nil {
			return false
		}
		next.Environments = append(next.Environments, unknownEnvironmentObservation(*target))
		sort.Slice(next.Environments, func(i, j int) bool { return next.Environments[i].ID < next.Environments[j].ID })
		for i := range next.Environments {
			if next.Environments[i].ID == environmentID {
				environmentIndex = i
				break
			}
		}
	}

	environment := &next.Environments[environmentIndex]
	environment.State = ConnectivityConnected
	environment.Error = ""
	for i := range environment.Services {
		if environment.Services[i].ID == observation.ID {
			observation.Endpoints = append([]EndpointObservation(nil), observation.Endpoints...)
			environment.Services[i] = observation
			o.snapshot = next
			return true
		}
	}
	return false
}

func unknownEnvironmentObservation(target EnvironmentTarget) EnvironmentObservation {
	environment := EnvironmentObservation{ID: target.ID, State: ConnectivityUnknown, Services: make([]ServiceObservation, 0, len(target.Services))}
	for _, service := range target.Services {
		observation := ServiceObservation{ID: service.ID, State: ServiceUnknown, Process: ProcessUnknown, Readiness: ReadinessUnknown, Endpoints: make([]EndpointObservation, 0, len(service.Endpoints))}
		for _, endpoint := range service.Endpoints {
			observation.Endpoints = append(observation.Endpoints, EndpointObservation{ID: endpoint.ID, State: EndpointUnknown})
		}
		environment.Services = append(environment.Services, observation)
	}
	return environment
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	cloned := Snapshot{Environments: make([]EnvironmentObservation, len(snapshot.Environments))}
	for i, environment := range snapshot.Environments {
		cloned.Environments[i] = environment
		cloned.Environments[i].Services = make([]ServiceObservation, len(environment.Services))
		for j, service := range environment.Services {
			cloned.Environments[i].Services[j] = service
			cloned.Environments[i].Services[j].Endpoints = append([]EndpointObservation(nil), service.Endpoints...)
		}
	}
	return cloned
}

func callProbe[T any](ctx context.Context, timeout time.Duration, probe func(context.Context) (T, error)) (T, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return probe(probeCtx)
}

func normalizeConnectivity(state ConnectivityState, err error) (ConnectivityState, string) {
	if err != nil {
		return ConnectivityError, err.Error()
	}
	switch state {
	case ConnectivityUnknown, ConnectivityConnected, ConnectivityUnreachable, ConnectivityError:
		return state, ""
	default:
		return ConnectivityError, fmt.Sprintf("invalid connectivity state %q", state)
	}
}

func normalizeProcess(state ProcessState, err error) (ProcessState, string) {
	if err != nil {
		return ProcessError, err.Error()
	}
	switch state {
	case ProcessUnknown, ProcessStopped, ProcessStarting, ProcessRunning, ProcessError:
		return state, ""
	default:
		return ProcessError, fmt.Sprintf("invalid process state %q", state)
	}
}

func normalizeReadiness(state ReadinessState, err error) (ReadinessState, string) {
	if err != nil {
		return ReadinessError, err.Error()
	}
	switch state {
	case ReadinessUnknown, ReadinessNotReady, ReadinessReady, ReadinessUnhealthy, ReadinessError:
		return state, ""
	default:
		return ReadinessError, fmt.Sprintf("invalid readiness state %q", state)
	}
}

func normalizeEndpoint(state EndpointState, err error) (EndpointState, string) {
	if err != nil {
		return EndpointError, err.Error()
	}
	switch state {
	case EndpointUnknown, EndpointAvailable, EndpointUnavailable, EndpointError:
		return state, ""
	default:
		return EndpointError, fmt.Sprintf("invalid endpoint state %q", state)
	}
}

// AggregateServiceState projects lifecycle and readiness evidence onto the
// operator-facing service state. Readiness alone cannot imply that a managed
// launch is in progress.
func AggregateServiceState(process ProcessState, readiness ReadinessState) ServiceState {
	switch {
	case process == ProcessStopped:
		return ServiceStopped
	case readiness == ReadinessError || readiness == ReadinessUnhealthy:
		return ServiceUnhealthy
	case readiness == ReadinessReady:
		return ServiceReady
	case (process == ProcessStarting || process == ProcessRunning) && readiness == ReadinessNotReady:
		return ServiceStarting
	default:
		return ServiceUnknown
	}
}
