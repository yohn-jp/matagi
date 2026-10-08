// Package api projects runtime state onto an IPv4 loopback HTTP interface.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/runtime"
)

type Runtime interface {
	Snapshot() runtime.State
	Start(context.Context, string, string) (runtime.Service, error)
	Stop(context.Context, string, string) (runtime.Service, error)
	Restart(context.Context, string, string) (runtime.Service, error)
	Ensure(context.Context, string, string, string) (runtime.Endpoint, error)
}
type Handler struct {
	runtime    Runtime
	capability Capability
}

// New creates a read-only API handler. Mutating v1 routes require a handler
// created with NewWithCapability.
func New(r Runtime) http.Handler { return NewWithCapability(r, Capability{}) }

// NewWithCapability creates an API handler that admits authorized v1
// mutations while preserving the existing routes and JSON payloads. A
// programmatic caller must hold the same in-memory Capability and add it to
// each loopback POST with Capability.AddToRequest.
func NewWithCapability(r Runtime, capability Capability) http.Handler {
	return &Handler{runtime: r, capability: capability}
}

// Registration is available only on the production runtime, not on read-only API fakes.
type registrar interface {
	Register(*registry.Snapshot) error
}
type registration struct {
	Environments []registry.Environment `json:"environments"`
	Services     []registry.Service     `json:"services"`
}

type request struct {
	EnvironmentID string `json:"environmentId"`
	ServiceID     string `json:"serviceId"`
	EndpointID    string `json:"endpointId"`
}

func write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int, code string) {
	failureWithMessage(w, status, code, code)
}
func failureWithMessage(w http.ResponseWriter, status int, code, message string) {
	write(w, status, struct {
		Version int `json:"version"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Version: 1, Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}})
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && isMutationPath(r.URL.Path) && !h.admitMutation(w, r) {
		return
	}
	if r.URL.Path == "/v1/environment/connect" && r.Method == http.MethodPost {
		owner, ok := h.runtime.(interface {
			Connect(context.Context, string, string, []string) error
		})
		if !ok {
			failure(w, 409, "registration-unavailable")
			return
		}
		var input struct {
			ID        string   `json:"id"`
			SSHHost   string   `json:"sshHost"`
			Bootstrap []string `json:"bootstrap"`
		}
		if !decodeRequest(r, &input) {
			failure(w, 400, "invalid-request")
			return
		}
		if _, err := registry.NewSnapshot([]registry.Environment{{ID: registry.EnvironmentID(input.ID), SSHHost: input.SSHHost, Jinushi: registry.JinushiDefinition{SupervisorStartCommand: input.Bootstrap}}}, nil); err != nil {
			failure(w, 400, "invalid-request")
			return
		}
		if err := owner.Connect(r.Context(), input.ID, input.SSHHost, input.Bootstrap); err != nil {
			operationFailure(w, err)
			return
		}
		write(w, 200, h.runtime.Snapshot())
		return
	}
	if r.URL.Path == "/v1/service/add" && r.Method == http.MethodPost {
		owner, ok := h.runtime.(interface{ AddService(registry.Service) error })
		if !ok {
			failure(w, 409, "registration-unavailable")
			return
		}
		var input struct {
			EnvironmentID  string   `json:"environmentId"`
			ID             string   `json:"id"`
			Argv           []string `json:"argv"`
			CWD            string   `json:"cwd"`
			Port           int      `json:"port"`
			ResolutionPath string   `json:"resolutionPath"`
			HealthPath     string   `json:"healthPath"`
		}
		if !decodeRequest(r, &input) {
			failure(w, 400, "invalid-request")
			return
		}
		if input.HealthPath == "" {
			input.HealthPath = "/"
		}
		endpoint := registry.Endpoint{ID: "ui", Label: "User interface", RemoteAddress: registry.RemoteLoopbackAddress, RemotePort: input.Port}
		if input.ResolutionPath != "" {
			endpoint.Resolution = &registry.EndpointResolution{Type: registry.EndpointResolutionJSONURLFile, Path: input.ResolutionPath}
		}
		service := registry.Service{ID: registry.ServiceID(input.ID), EnvironmentID: registry.EnvironmentID(input.EnvironmentID), DesiredState: registry.DesiredStopped,
			Execution: registry.ExecutionIntent{Argv: input.Argv, CWD: input.CWD, Lifetime: registry.LifetimeDetached},
			Endpoints: []registry.Endpoint{endpoint},
			Health:    registry.HealthDefinition{Type: registry.HealthHTTP, EndpointID: "ui", Path: input.HealthPath}}
		if _, err := registry.NewSnapshot([]registry.Environment{{ID: registry.EnvironmentID(input.EnvironmentID), SSHHost: "validation"}}, []registry.Service{service}); err != nil {
			failure(w, 400, "invalid-request")
			return
		}
		if err := owner.AddService(service); err != nil {
			operationFailure(w, err)
			return
		}
		write(w, 200, h.runtime.Snapshot())
		return
	}
	if r.URL.Path == "/v1/environment/ensure-jinushi" && r.Method == http.MethodPost {
		owner, ok := h.runtime.(interface {
			EnsureJinushi(context.Context, string) error
		})
		if !ok {
			failure(w, 409, "registration-unavailable")
			return
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4097))
		decoder.DisallowUnknownFields()
		var input struct {
			EnvironmentID string `json:"environmentId"`
		}
		var trailing any
		if decoder.Decode(&input) != nil || decoder.Decode(&trailing) != io.EOF || strings.TrimSpace(input.EnvironmentID) == "" {
			failure(w, 400, "invalid-request")
			return
		}
		if err := owner.EnsureJinushi(r.Context(), input.EnvironmentID); err != nil {
			var f *runtime.Failure
			if errors.As(err, &f) {
				failure(w, 502, f.Code)
			} else {
				failure(w, 502, "remote-failure")
			}
			return
		}
		write(w, 200, struct {
			Version int `json:"version"`
		}{1})
		return
	}
	if r.URL.Path == "/v1/environment/register" && r.Method == http.MethodPost {
		owner, ok := h.runtime.(registrar)
		if !ok {
			failure(w, 409, "registration-unavailable")
			return
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 65537))
		decoder.DisallowUnknownFields()
		var input registration
		var trailing any
		if decoder.Decode(&input) != nil || decoder.Decode(&trailing) != io.EOF || len(input.Environments) != 1 {
			failure(w, 400, "invalid-request")
			return
		}
		snapshot, err := registry.NewSnapshot(input.Environments, input.Services)
		if err != nil {
			failure(w, 400, "invalid-request")
			return
		}
		if err := owner.Register(snapshot); err != nil {
			var f *runtime.Failure
			if errors.As(err, &f) && f.Code == "lifecycle-conflict" {
				failure(w, 409, f.Code)
			} else {
				failure(w, 500, "registration-failed")
			}
			return
		}
		write(w, 200, h.runtime.Snapshot())
		return
	}
	if r.URL.Path == "/v1/state" && r.Method == http.MethodGet {
		write(w, 200, h.runtime.Snapshot())
		return
	}
	if r.Method != http.MethodPost || (r.URL.Path != "/v1/service/start" && r.URL.Path != "/v1/service/stop" && r.URL.Path != "/v1/service/restart" && r.URL.Path != "/v1/endpoint/ensure") {
		failure(w, 404, "invalid-request")
		return
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4097))
	decoder.DisallowUnknownFields()
	var input request
	if err := decoder.Decode(&input); err != nil {
		failure(w, 400, "invalid-request")
		return
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || strings.TrimSpace(input.EnvironmentID) == "" || strings.TrimSpace(input.ServiceID) == "" || (r.URL.Path == "/v1/endpoint/ensure") != (strings.TrimSpace(input.EndpointID) != "") {
		failure(w, 400, "invalid-request")
		return
	}
	var result any
	var err error
	switch r.URL.Path {
	case "/v1/service/start":
		result, err = h.runtime.Start(r.Context(), input.EnvironmentID, input.ServiceID)
	case "/v1/service/stop":
		result, err = h.runtime.Stop(r.Context(), input.EnvironmentID, input.ServiceID)
	case "/v1/service/restart":
		result, err = h.runtime.Restart(r.Context(), input.EnvironmentID, input.ServiceID)
	default:
		result, err = h.runtime.Ensure(r.Context(), input.EnvironmentID, input.ServiceID, input.EndpointID)
	}
	if err != nil {
		code := "remote-failure"
		status := 502
		var f *runtime.Failure
		if errors.As(err, &f) {
			code = f.Code
		}
		switch code {
		case "unknown-identity":
			status = 404
		case "invalid-request":
			status = 400
		case "lifecycle-conflict":
			status = 409
		case "ssh-timeout", "jinushi-timeout":
			status = 504
		}
		message := code
		if errors.As(err, &f) && f.Evidence != "" {
			message = f.Evidence
		}
		failureWithMessage(w, status, code, message)
		return
	}
	if r.URL.Path == "/v1/endpoint/ensure" {
		write(w, 200, struct {
			Version  int `json:"version"`
			Endpoint any `json:"endpoint"`
		}{1, result})
	} else {
		write(w, 200, struct {
			Version int `json:"version"`
			Service any `json:"service"`
		}{1, result})
	}
}

func isMutationPath(path string) bool {
	switch path {
	case "/v1/environment/connect", "/v1/service/add", "/v1/environment/ensure-jinushi", "/v1/environment/register",
		"/v1/service/start", "/v1/service/stop", "/v1/service/restart", "/v1/endpoint/ensure":
		return true
	default:
		return false
	}
}

func (h *Handler) admitMutation(w http.ResponseWriter, r *http.Request) bool {
	if !safeMutationOrigin(r) {
		failure(w, http.StatusForbidden, "unsafe-request-origin")
		return false
	}
	if !jsonContentType(r.Header.Values("Content-Type")) {
		failure(w, http.StatusUnsupportedMediaType, "unsupported-media-type")
		return false
	}
	if !h.capability.permits(r.Header.Values(capabilityHeader), r.Host) {
		failure(w, http.StatusForbidden, "caller-not-authorized")
		return false
	}
	return true
}

func safeMutationOrigin(r *http.Request) bool {
	requestPort, ok := loopbackRequestPort(r.Host)
	if !ok {
		return false
	}
	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return true
	}
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.Scheme != "http" || origin.User != nil || origin.Opaque != "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" {
		return false
	}
	originPort, ok := loopbackRequestPort(origin.Host)
	return ok && originPort == requestPort
}

func loopbackRequestPort(host string) (string, bool) {
	address, portText, err := net.SplitHostPort(host)
	if err != nil || address != "127.0.0.1" {
		return "", false
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", false
	}
	return strconv.FormatUint(port, 10), true
}

func jsonContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(values[0])
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return false
	}
	for name, value := range params {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	return true
}

func decodeRequest(r *http.Request, dst any) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 65537))
	decoder.DisallowUnknownFields()
	var trailing any
	return decoder.Decode(dst) == nil && decoder.Decode(&trailing) == io.EOF
}

func operationFailure(w http.ResponseWriter, err error) {
	var f *runtime.Failure
	if errors.As(err, &f) {
		status := 502
		if f.Code == "invalid-request" {
			status = 400
		}
		if f.Code == "lifecycle-conflict" {
			status = 409
		}
		message := f.Code
		if f.Evidence != "" {
			message = f.Evidence
		}
		failureWithMessage(w, status, f.Code, message)
		return
	}
	failure(w, 500, "registration-failed")
}

func Listen() (net.Listener, error) { return net.Listen("tcp4", "127.0.0.1:0") }
func Serve(ctx context.Context, rt Runtime) error {
	listener, err := Listen()
	if err != nil {
		return err
	}
	server := &http.Server{Handler: New(rt), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
