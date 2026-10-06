package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

const maxFormBytes = 16 << 10

// EndpointAdmitter admits a loopback URL after the API has successfully
// ensured the corresponding registered endpoint.
type EndpointAdmitter interface {
	AdmitEnsuredEndpoint(localURL string) error
}

type page struct {
	State *State
	Error string
	Token string
}

// NewHandler serves Matagi's local HTML surface. It talks to the runtime only
// through Client and delegates endpoint navigation admission to the desktop
// shell after a successful ensure response.
func NewHandler(client *Client, admitter EndpointAdmitter) http.Handler {
	return &handler{client: client, admitter: admitter, token: newFormToken()}
}

type handler struct {
	client   *Client
	admitter EndpointAdmitter
	token    string
}

func newFormToken() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return hex.EncodeToString(value)
}

func (h *handler) validFormToken(value string) bool {
	return len(value) == len(h.token) && subtle.ConstantTimeCompare([]byte(value), []byte(h.token)) == 1
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method == http.MethodPost {
		if err := parseForm(w, r); err != nil {
			http.Error(w, "invalid form submission", http.StatusBadRequest)
			return
		}
		if !h.validFormToken(r.PostForm.Get("token")) {
			http.Error(w, "invalid form token", http.StatusForbidden)
			return
		}
	}
	switch r.URL.Path {
	case "/":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.index(w, r, "")
	case "/jinushi":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		if r.PostForm.Get("environmentId") == "" {
			http.Error(w, "invalid environment", http.StatusBadRequest)
			return
		}
		if err := h.client.EnsureJinushi(r.Context(), r.PostForm.Get("environmentId")); err != nil {
			h.showError(w, apiStatus(err), bounded(err.Error(), maxErrorMessage))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "/service/add":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.addService(w, r)
	case "/register":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.register(w, r)
	case "/action":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.action(w, r)
	case "/open":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		h.open(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *handler) index(w http.ResponseWriter, r *http.Request, message string) {
	state, err := h.client.GetState(r.Context())
	if err != nil {
		if message == "" {
			message = bounded(err.Error(), maxErrorMessage)
		}
		h.showError(w, http.StatusBadGateway, message)
		return
	}
	for i := range state.Environments {
		env := &state.Environments[i]
		env.Error = bounded(env.Error, maxErrorMessage)
		for j := range env.Services {
			service := &env.Services[j]
			service.ProcessError = bounded(service.ProcessError, maxErrorMessage)
			service.ReadinessError = bounded(service.ReadinessError, maxErrorMessage)
			for k := range service.Endpoints {
				endpoint := &service.Endpoints[k]
				endpoint.Failure = bounded(endpoint.Failure, maxErrorMessage)
				endpoint.LocalURL = localAvailabilityURL(endpoint)
			}
		}
	}
	h.render(w, page{State: &state, Error: message, Token: h.token})
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	request := ConnectRequest{ID: strings.TrimSpace(r.PostForm.Get("name")), SSHHost: strings.TrimSpace(r.PostForm.Get("host"))}
	if cmd := strings.TrimSpace(r.PostForm.Get("bootstrap")); cmd != "" {
		request.Bootstrap = strings.Fields(cmd)
	}
	if err := h.client.Connect(r.Context(), request); err != nil {
		h.index(w, r, describeFailure(err))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *handler) addService(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PostForm.Get("port"))
	if err != nil {
		h.index(w, r, "Enter a valid UI port (1–65535).")
		return
	}
	req := AddServiceRequest{EnvironmentID: r.PostForm.Get("environmentId"), ID: strings.TrimSpace(r.PostForm.Get("service")), Argv: strings.Fields(r.PostForm.Get("command")), CWD: strings.TrimSpace(r.PostForm.Get("cwd")), Port: port, HealthPath: strings.TrimSpace(r.PostForm.Get("healthPath"))}
	if err := h.client.AddService(r.Context(), req); err != nil {
		h.index(w, r, describeFailure(err))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func describeFailure(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case "invalid-request":
			return "Check the entered details and try again. No changes were saved."
		case "ssh-transport-failed", "host-unreachable":
			return "SSH transport or authentication failed. Check your OpenSSH host alias and credentials."
		case "ssh-timeout":
			return "SSH host did not respond before the connection timed out."
		case "ssh-connectivity-failed":
			return "SSH connected, but the remote connectivity check failed."
		case "jinushi-unavailable":
			return "Jinushi is unavailable. Check that it is installed, or specify its bootstrap command under Advanced."
		case "jinushi-bootstrap-failed":
			return "Jinushi bootstrap failed. Check the configured command on the development host."
		case "jinushi-readiness-timeout", "jinushi-readiness-failed":
			return "Jinushi did not become ready. Check its status on the development host."
		case "lifecycle-conflict":
			return "This environment or service is already registered. Refresh the workspace."
		}
	}
	return bounded(err.Error(), maxErrorMessage)
}

func (h *handler) action(w http.ResponseWriter, r *http.Request) {
	envID, serviceID, action := r.PostForm.Get("environmentId"), r.PostForm.Get("serviceId"), r.PostForm.Get("action")
	if envID == "" || serviceID == "" {
		http.Error(w, "environment and service are required", http.StatusBadRequest)
		return
	}
	request := ServiceRequest{EnvironmentID: envID, ServiceID: serviceID}
	var err error
	switch action {
	case "start":
		_, err = h.client.Start(r.Context(), request)
	case "stop":
		_, err = h.client.Stop(r.Context(), request)
	case "restart":
		_, err = h.client.Restart(r.Context(), request)
	default:
		http.Error(w, "unknown service action", http.StatusBadRequest)
		return
	}
	if err != nil {
		h.showError(w, apiStatus(err), bounded(err.Error(), maxErrorMessage))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *handler) open(w http.ResponseWriter, r *http.Request) {
	request := EndpointRequest{
		EnvironmentID: r.PostForm.Get("environmentId"),
		ServiceID:     r.PostForm.Get("serviceId"),
		EndpointID:    r.PostForm.Get("endpointId"),
	}
	if request.EnvironmentID == "" || request.ServiceID == "" || request.EndpointID == "" {
		http.Error(w, "environment, service, and endpoint are required", http.StatusBadRequest)
		return
	}
	result, err := h.client.EnsureEndpoint(r.Context(), request)
	if err != nil {
		h.showError(w, apiStatus(err), bounded(err.Error(), maxErrorMessage))
		return
	}
	endpoint := result.Endpoint
	if endpoint.ID != request.EndpointID {
		h.showError(w, http.StatusBadGateway, "The ensured endpoint did not match the requested endpoint.")
		return
	}
	if endpoint.EndpointState != "available" || endpoint.TunnelState != "ready" {
		message := endpoint.Failure
		if message == "" {
			message = "The endpoint is not locally available."
		}
		h.showError(w, http.StatusBadGateway, bounded(message, maxErrorMessage))
		return
	}
	if h.admitter == nil {
		h.showError(w, http.StatusBadGateway, "The desktop navigation policy is unavailable.")
		return
	}
	if err := h.admitter.AdmitEnsuredEndpoint(endpoint.LocalURL); err != nil {
		h.showError(w, http.StatusBadGateway, "The ensured endpoint did not return a permitted loopback URL.")
		return
	}
	// The destination comes only from the successful ensure response after the
	// desktop policy has admitted its loopback origin.
	http.Redirect(w, r, endpoint.LocalURL, http.StatusSeeOther)
}

func (h *handler) showError(w http.ResponseWriter, status int, message string) {
	h.renderStatus(w, status, page{Error: bounded(message, maxErrorMessage), Token: h.token})
}

func apiStatus(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusGatewayTimeout {
		return http.StatusGatewayTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func parseForm(w http.ResponseWriter, r *http.Request) error {
	if r.URL.RawQuery != "" {
		return errors.New("query parameters are not accepted")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		return err
	}
	return nil
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func (h *handler) render(w http.ResponseWriter, data page) {
	h.renderStatus(w, http.StatusOK, data)
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
}

func (h *handler) renderStatus(w http.ResponseWriter, status int, data page) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := pageTemplate.Execute(w, data); err != nil {
		return
	}
}

func localAvailabilityURL(endpoint *Endpoint) string {
	if endpoint.EndpointState != "available" || endpoint.TunnelState != "ready" || endpoint.LocalURL == "" {
		return ""
	}
	if _, err := LoopbackHTTPOrigin(endpoint.LocalURL); err != nil {
		return ""
	}
	return "available"
}

//go:embed page.html
var pageHTML string

var pageTemplate = template.Must(template.New("matagi").Funcs(template.FuncMap{
	"systemCSS": CSS,
	"tone": func(state string) string {
		switch state {
		case "ready", "running", "connected", "available":
			return "tone-ok"
		case "starting", "ensuring":
			return "tone-active"
		case "unhealthy", "unreachable", "unavailable", "error", "failed":
			return "tone-bad"
		default:
			return "tone-idle"
		}
	},
}).Parse(pageHTML))
