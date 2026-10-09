package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/runtime"
	"github.com/yohn-jp/matagi/internal/settings"
)

const maxFormBytes = 16 << 10

// EndpointAdmitter admits a loopback URL after the API has successfully
// ensured the corresponding registered endpoint.
type EndpointAdmitter interface {
	AdmitEnsuredEndpoint(localURL string) error
}

type page struct {
	State  *State
	Error  string
	Token  string
	Locale i18n.Locale
}

// Options injects Matagi-owned settings and update services into the local
// operator surface.
type Options struct {
	Settings *settings.Store
	Updates  Updates
}

// NewHandler serves Matagi's local HTML surface. It talks to the runtime only
// through Client and delegates endpoint navigation admission to the desktop
// shell after a successful ensure response.
func NewHandler(client *Client, admitter EndpointAdmitter) http.Handler {
	return NewHandlerWithOptions(client, admitter, Options{})
}

// NewHandlerWithOptions serves the local operator surface with settings and
// optional update services, preserving NewHandler for existing callers.
func NewHandlerWithOptions(client *Client, admitter EndpointAdmitter, options Options) http.Handler {
	return &handler{client: client, admitter: admitter, settings: options.Settings, updates: options.Updates, token: newFormToken()}
}

type handler struct {
	client   *Client
	admitter EndpointAdmitter
	settings *settings.Store
	updates  Updates
	token    string
}

func (h *handler) locale() i18n.Locale {
	if h.settings == nil {
		return i18n.Resolve("", i18n.HostLocales()...)
	}
	saved, err := h.settings.Locale()
	if err != nil {
		saved = ""
	}
	return i18n.Resolve(string(saved), i18n.HostLocales()...)
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
			http.Error(w, h.locale().T("Invalid form submission."), http.StatusBadRequest)
			return
		}
		if !h.validFormToken(r.PostForm.Get("token")) {
			http.Error(w, h.locale().T("Invalid form token."), http.StatusForbidden)
			return
		}
	}
	switch r.URL.Path {
	case "/":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet, h.locale())
			return
		}
		h.index(w, r, "")
	case "/jinushi":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		if r.PostForm.Get("environmentId") == "" {
			http.Error(w, h.locale().T("An environment is required."), http.StatusBadRequest)
			return
		}
		if err := h.client.EnsureJinushi(r.Context(), r.PostForm.Get("environmentId")); err != nil {
			h.showError(w, apiStatus(err), h.describeFailure(err))
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	case "/settings/locale":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.setLocale(w, r)
	case "/updates":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet, h.locale())
			return
		}
		h.updatesPage(w, r)
	case "/updates/channel":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.updatesChannel(w, r)
	case "/updates/check":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.updatesCheck(w, r)
	case "/updates/download":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.updatesDownload(w, r)
	case "/updates/install":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.updatesInstall(w, r)
	case "/service/add":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.addService(w, r)
	case "/register":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.register(w, r)
	case "/action":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.action(w, r)
	case "/open":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost, h.locale())
			return
		}
		h.open(w, r)
	default:
		http.Error(w, h.locale().T("Page not found."), http.StatusNotFound)
	}
}

func (h *handler) index(w http.ResponseWriter, r *http.Request, message string) {
	state, err := h.client.GetState(r.Context())
	if err != nil {
		if message == "" {
			message = h.describeFailure(err)
		}
		h.showError(w, http.StatusBadGateway, message)
		return
	}
	for i := range state.Environments {
		env := &state.Environments[i]
		env.Error = h.diagnosticHint(env.Error)
		for j := range env.Services {
			service := &env.Services[j]
			service.ProcessError = h.diagnosticHint(service.ProcessError)
			service.ReadinessError = h.diagnosticHint(service.ReadinessError)
			for k := range service.Endpoints {
				endpoint := &service.Endpoints[k]
				endpoint.Failure = h.diagnosticHint(endpoint.Failure)
				endpoint.LocalURL = localAvailabilityURL(endpoint)
			}
		}
	}
	h.render(w, page{State: &state, Error: message, Token: h.token, Locale: h.locale()})
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	request := ConnectRequest{ID: strings.TrimSpace(r.PostForm.Get("name")), SSHHost: strings.TrimSpace(r.PostForm.Get("host"))}
	if cmd := strings.TrimSpace(r.PostForm.Get("bootstrap")); cmd != "" {
		request.Bootstrap = strings.Fields(cmd)
	}
	if err := h.client.Connect(r.Context(), request); err != nil {
		h.index(w, r, h.describeFailure(err))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *handler) addService(w http.ResponseWriter, r *http.Request) {
	portText := strings.TrimSpace(r.PostForm.Get("port"))
	resolutionPath := strings.TrimSpace(r.PostForm.Get("resolutionPath"))
	port := 0
	if portText != "" {
		var err error
		port, err = strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			h.index(w, r, h.locale().T("Enter a valid UI port (1–65535)."))
			return
		}
	}
	if port == 0 && resolutionPath == "" {
		h.index(w, r, h.locale().T("Enter a valid UI port (1–65535)."))
		return
	}
	req := AddServiceRequest{EnvironmentID: r.PostForm.Get("environmentId"), ID: strings.TrimSpace(r.PostForm.Get("service")), Argv: strings.Fields(r.PostForm.Get("command")), CWD: strings.TrimSpace(r.PostForm.Get("cwd")), Port: port, ResolutionPath: resolutionPath, HealthPath: strings.TrimSpace(r.PostForm.Get("healthPath"))}
	if req.ID == "" {
		h.index(w, r, h.locale().T("The service name is required."))
		return
	}
	if err := h.client.AddService(r.Context(), req); err != nil {
		h.index(w, r, h.describeFailure(err))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *handler) describeFailure(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		code := runtime.SafeDiagnosticCode(apiErr.Code, "remote-failure")
		switch code {
		case "registration-failed", "unsafe-request-origin", "caller-not-authorized", "unsupported-media-type":
			return code + ": " + h.locale().T("The local API rejected this request. Refresh the workspace before retrying.")
		}
		hint := h.errorHint(code)
		if hint == "" {
			hint = h.errorHint("remote-failure")
		}
		if evidence := runtime.SafeFailureEvidence(code, apiErr.Message); evidence != "" {
			return hint + " " + evidence
		}
		return hint
	}
	return h.errorHint("remote-failure")
}

func (h *handler) diagnosticHint(value string) string {
	code := runtime.SafeDiagnosticCode(value, "")
	if code == "" {
		return ""
	}
	return h.errorHint(code)
}

func (h *handler) errorHint(code string) string {
	var message string
	switch code {
	case "invalid-request":
		message = "Check the entered details and try again. No changes were saved."
	case "ssh-transport-failed", "host-unreachable":
		message = "SSH transport or authentication failed. Check your OpenSSH host alias and credentials."
	case "ssh-timeout":
		message = "The SSH host did not respond before the connection timed out."
	case "ssh-connectivity-failed":
		message = "SSH connected, but the remote connectivity check failed."
	case "ssh-client-failed":
		message = "The local OpenSSH client could not complete the request. Check its installation and configuration."
	case "jinushi-unavailable", "jinushi-bootstrap-failed":
		message = "Jinushi is unavailable. Check that it is installed, or specify its bootstrap command under Advanced."
	case "jinushi-timeout", "jinushi-readiness-timeout":
		message = "Jinushi did not respond before the request timed out. Check Jinushi on the development host."
	case "jinushi-command-failed", "jinushi-readiness-failed":
		message = "Jinushi rejected the lifecycle action or returned a failed result. Check its response and the service registration."
	case "jinushi-protocol-failed":
		message = "Jinushi returned an invalid response. Check Jinushi on the development host."
	case "lifecycle-conflict", "registration-unavailable":
		message = "This environment or service is already registered or changing state. Refresh the workspace."
	case "registration-failed", "unsafe-request-origin", "caller-not-authorized", "unsupported-media-type":
		message = "The local API rejected this request. Refresh the workspace before retrying."
	case "operation-canceled":
		message = "The operation was canceled."
	case "unknown-identity", "not-found":
		message = "The environment or service is no longer registered. Refresh the workspace."
	case "tunnel-unavailable":
		message = "The endpoint could not be ensured. Check Jinushi, service readiness, and the tunnel status."
	case "endpoint-unavailable":
		message = "The application endpoint could not be resolved or reached. Check its descriptor, managed Run output, and service status."
	case "endpoint-evidence-missing", "endpoint-evidence-invalid", "endpoint-evidence-ambiguous", "endpoint-evidence-stale", "endpoint-output-incomplete", "endpoint-application-unavailable":
		message = "The application endpoint could not be resolved or reached. Check its descriptor, managed Run output, and service status."
	case "readiness-check-failed":
		message = "Check the service health endpoint and service logs."
	case "remote-failure":
		message = "This failure could not be classified. Refresh the workspace and inspect the service status before retrying."
	default:
		return ""
	}
	return h.locale().T(message)
}

func (h *handler) describeActionFailure(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "lifecycle-conflict" {
		hint := h.locale().T("The lifecycle action could not be confirmed. Refresh the workspace and inspect the managed process before retrying this action.")
		if evidence := runtime.SafeFailureEvidence("lifecycle-conflict", apiErr.Message); evidence != "" {
			return hint + " " + evidence
		}
		return hint
	}
	return h.describeFailure(err)
}

func (h *handler) action(w http.ResponseWriter, r *http.Request) {
	envID, serviceID, action := r.PostForm.Get("environmentId"), r.PostForm.Get("serviceId"), r.PostForm.Get("action")
	if envID == "" || serviceID == "" {
		http.Error(w, h.locale().T("An environment and service are required."), http.StatusBadRequest)
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
		http.Error(w, h.locale().T("Unknown service action."), http.StatusBadRequest)
		return
	}
	if err != nil {
		h.showError(w, apiStatus(err), h.describeActionFailure(err))
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
		http.Error(w, h.locale().T("An environment, service, and endpoint are required."), http.StatusBadRequest)
		return
	}
	result, err := h.client.EnsureEndpoint(r.Context(), request)
	if err != nil {
		h.showError(w, apiStatus(err), h.describeFailure(err))
		return
	}
	endpoint := result.Endpoint
	if endpoint.ID != request.EndpointID {
		h.showError(w, http.StatusBadGateway, h.locale().T("The ensured endpoint did not match the requested endpoint."))
		return
	}
	if endpoint.EndpointState != "available" || endpoint.TunnelState != "ready" {
		message := endpoint.Failure
		if message == "" {
			message = h.locale().T("The endpoint is not locally available.")
		}
		h.showError(w, http.StatusBadGateway, bounded(message, maxErrorMessage))
		return
	}
	if h.admitter == nil {
		h.showError(w, http.StatusBadGateway, h.locale().T("The desktop navigation policy is unavailable."))
		return
	}
	if err := h.admitter.AdmitEnsuredEndpoint(endpoint.LocalURL); err != nil {
		h.showError(w, http.StatusBadGateway, h.locale().T("The ensured endpoint did not return a permitted loopback URL."))
		return
	}
	// The destination comes only from the successful ensure response after the
	// desktop policy has admitted its loopback origin.
	http.Redirect(w, r, endpoint.LocalURL, http.StatusSeeOther)
}

func (h *handler) showError(w http.ResponseWriter, status int, message string) {
	h.renderStatus(w, status, page{Error: bounded(message, maxErrorMessage), Token: h.token, Locale: h.locale()})
}

func (h *handler) setLocale(w http.ResponseWriter, r *http.Request) {
	selected := r.PostForm.Get("locale")
	if !i18n.Valid(selected) {
		h.showError(w, http.StatusBadRequest, h.locale().T("Choose English or Japanese."))
		return
	}
	if h.settings == nil {
		h.showError(w, http.StatusServiceUnavailable, h.locale().T("Language preference settings are unavailable."))
		return
	}
	if err := h.settings.SetLocale(selected); err != nil {
		evidence := bounded(err.Error(), maxErrorMessage-100)
		h.showError(w, http.StatusInternalServerError, h.locale().T("The language preference could not be saved.")+" "+evidence)
		return
	}
	returnTo := r.PostForm.Get("returnTo")
	if returnTo != "/updates" {
		returnTo = "/"
	}
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
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

func methodNotAllowed(w http.ResponseWriter, allowed string, locale i18n.Locale) {
	w.Header().Set("Allow", allowed)
	http.Error(w, locale.T("method not allowed"), http.StatusMethodNotAllowed)
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

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"systemCSS": func() template.CSS { return CSS() },
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
		"t": func(locale i18n.Locale, message string, args ...any) string {
			return locale.T(message, args...)
		},
		"messagesJS": func(locale i18n.Locale) template.JS {
			data, _ := json.Marshal(locale.Table(i18n.BrowserMessages))
			return template.JS(data)
		},
		"localeCode": func(locale i18n.Locale) string { return string(locale) },
		"isLocale":   func(locale i18n.Locale, value string) bool { return string(locale) == value },
		"locales":    func() []i18n.Locale { return i18n.Supported },
		"operatorLabel": func(locale i18n.Locale, label string) string {
			if label == "User interface" {
				return locale.T(label)
			}
			return label
		},
	}
}

var pageTemplate = template.Must(template.New("matagi").Funcs(templateFuncs()).Parse(pageHTML))
