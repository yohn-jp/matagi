package ui

import (
	"context"
	"errors"
	"html/template"
	"net/http"
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
}

// NewHandler serves Matagi's local HTML surface. It talks to the runtime only
// through Client and delegates endpoint navigation admission to the desktop
// shell after a successful ensure response.
func NewHandler(client *Client, admitter EndpointAdmitter) http.Handler {
	return &handler{client: client, admitter: admitter}
}

type handler struct {
	client   *Client
	admitter EndpointAdmitter
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		h.index(w, r, "")
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
	h.render(w, page{State: &state, Error: message})
}

func (h *handler) action(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(w, r); err != nil {
		http.Error(w, "invalid action request", http.StatusBadRequest)
		return
	}
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
	if err := parseForm(w, r); err != nil {
		http.Error(w, "invalid endpoint request", http.StatusBadRequest)
		return
	}
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
	w.WriteHeader(status)
	h.render(w, page{Error: bounded(message, maxErrorMessage)})
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
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

var pageTemplate = template.Must(template.New("matagi").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Matagi</title>
<style>
:root { color-scheme: light dark; font: 16px system-ui, sans-serif; }
body { margin: 0 auto; max-width: 72rem; padding: 1.5rem; }
h1 { margin-top: 0; }
section.environment { border-top: 1px solid #8886; margin-top: 1.5rem; padding-top: 1rem; }
article.service { border: 1px solid #8886; border-radius: .5rem; margin: 1rem 0; padding: 1rem; }
.facts { display: grid; gap: .4rem 1rem; grid-template-columns: max-content 1fr; }
.error { color: #b42318; }
form { display: inline-block; margin: .3rem .4rem .3rem 0; }
button { cursor: pointer; padding: .45rem .8rem; }
</style>
</head>
<body>
<main>
<h1>Matagi</h1>
{{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
{{if .State}}
{{range .State.Environments}}
<section class="environment">
{{ $environmentID := .ID }}
<h2>Environment {{.ID}}</h2>
<p>Connectivity: {{.Connectivity}}</p>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
{{range .Services}}
<article class="service">
{{ $serviceID := .ID }}
<h3>{{.ID}}</h3>
<div class="facts">
<span>Desired</span><span>{{.DesiredState}}</span>
<span>Service</span><span>{{.State}}</span>
<span>Process</span><span>{{.Process}}</span>
<span>Readiness</span><span>{{.Readiness}}</span>
</div>
{{if .ProcessError}}<p class="error">Process: {{.ProcessError}}</p>{{end}}
{{if .ReadinessError}}<p class="error">Readiness: {{.ReadinessError}}</p>{{end}}
<form method="post" action="/action"><input type="hidden" name="environmentId" value="{{$environmentID}}"><input type="hidden" name="serviceId" value="{{$serviceID}}"><button name="action" value="start">Start</button></form>
<form method="post" action="/action"><input type="hidden" name="environmentId" value="{{$environmentID}}"><input type="hidden" name="serviceId" value="{{$serviceID}}"><button name="action" value="stop">Stop</button></form>
<form method="post" action="/action"><input type="hidden" name="environmentId" value="{{$environmentID}}"><input type="hidden" name="serviceId" value="{{$serviceID}}"><button name="action" value="restart">Restart</button></form>
{{range .Endpoints}}
<section class="endpoint">
<h4>{{.Label}}</h4>
<p>Endpoint: {{.EndpointState}} · Tunnel: {{.TunnelState}} · Local: {{if .LocalURL}}{{.LocalURL}}{{else}}unavailable{{end}}</p>
{{if .Failure}}<p class="error">{{.Failure}}</p>{{end}}
<form method="post" action="/open"><input type="hidden" name="environmentId" value="{{$environmentID}}"><input type="hidden" name="serviceId" value="{{$serviceID}}"><input type="hidden" name="endpointId" value="{{.ID}}"><button type="submit">Open</button></form>
</section>
{{end}}
</article>
{{end}}
</section>
{{else}}
<p>No environments are registered.</p>
{{end}}
{{else}}
<p>Waiting for the Matagi service API.</p>
{{end}}
</main>
</body>
</html>`))
