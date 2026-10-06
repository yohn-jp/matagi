package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHandlerRendersContractStateWithoutUnknownRuntimeData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "{\"version\":1,\"environments\":[{\"id\":\"dev\",\"connectivity\":\"connected\",\"error\":\"environment error\",\"services\":[{\"id\":\"svc\",\"desiredState\":\"running\",\"state\":\"ready\",\"process\":\"running\",\"readiness\":\"ready\",\"processError\":\"process warning\",\"readinessError\":\"readiness warning\",\"runId\":\"run-secret\",\"pid\":987654,\"credentials\":\"credential-secret\",\"endpoints\":[{\"id\":\"dash\",\"label\":\"Dashboard\",\"endpointState\":\"available\",\"tunnelState\":\"ready\",\"localUrl\":\"http://127.0.0.1:43123/\",\"failure\":\"\"}]}]}]}")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(client, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		"dev", "Connection · SSH", "environment error", "Jinushi · supervisor", "running", "tone-ok", "prefers-color-scheme: light", "aria-busy", "live · 3 s", "setInterval(refresh,3000)", "Start", "Restart", "Stop",
		"process warning", "readiness warning", "Dashboard", "available", "Tunnel: ready", "Open",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, forbidden := range []string{"run-secret", "credential-secret", "987654", "43123"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page exposes %q", forbidden)
		}
	}
	if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("security headers = %#v", response.Header())
	}
}

func TestEmptyStateRegistrationSurfaceAndSubmission(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/state" {
			fmt.Fprint(w, `{"version":1,"environments":[]}`)
			return
		}
		if r.URL.Path != "/v1/environment/connect" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		requests++
		fmt.Fprint(w, `{"version":1,"environments":[]}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	h := NewHandler(client, nil)
	token := h.(*handler).token
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(w.Body.String(), "Connect a development environment") || strings.Contains(w.Body.String(), "registry JSON") || strings.Contains(w.Body.String(), "<textarea") {
		t.Fatal("onboarding surface exposes registry schema")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, formRequest("POST", "/register", url.Values{"name": {"dev"}, "host": {"dev-host"}}, token))
	if requests != 1 || w.Code != 303 {
		t.Fatal(w.Code, requests)
	}
}

func TestHandlerLifecycleActionsSendOneMatchingRequest(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		var got ServiceRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if got != (ServiceRequest{EnvironmentID: "dev", ServiceID: "svc"}) {
			t.Errorf("request body = %#v", got)
		}
		fmt.Fprint(w, "{\"version\":1,\"service\":{}}")
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(client, nil)
	token := h.(*handler).token
	for _, action := range []string{"start", "stop", "restart"} {
		form := url.Values{
			"environmentId": {"dev"},
			"serviceId":     {"svc"},
			"action":        {action},
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, formRequest(http.MethodPost, "/action", form, token))
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
			t.Fatalf("%s status = %d, location %q, body %s", action, response.Code, response.Header().Get("Location"), response.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, route := range []string{"/v1/service/start", "/v1/service/stop", "/v1/service/restart"} {
		if counts[route] != 1 {
			t.Errorf("%s request count = %d, want 1", route, counts[route])
		}
	}
}

type testAdmitter struct {
	ensured *bool
	url     string
	err     error
	calls   int
}

func (a *testAdmitter) AdmitEnsuredEndpoint(localURL string) error {
	a.calls++
	a.url = localURL
	if a.ensured != nil && !*a.ensured {
		return fmt.Errorf("admission happened before ensure")
	}
	return a.err
}

func TestHandlerEnsuresBeforeAdmittingAndRedirectingEndpoint(t *testing.T) {
	ensured := false
	const endpointURL = "http://127.0.0.1:43123/ui/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/endpoint/ensure" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		var got EndpointRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode ensure request: %v", err)
		}
		if got != (EndpointRequest{EnvironmentID: "dev", ServiceID: "svc", EndpointID: "dash"}) {
			t.Errorf("ensure body = %#v", got)
		}
		ensured = true
		fmt.Fprintf(w, "{\"version\":1,\"endpoint\":{\"id\":\"dash\",\"label\":\"Dashboard\",\"endpointState\":\"available\",\"tunnelState\":\"ready\",\"localUrl\":%q,\"failure\":\"\"}}", endpointURL)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	admitter := &testAdmitter{ensured: &ensured}
	h := NewHandler(client, admitter)
	token := h.(*handler).token
	form := url.Values{
		"environmentId": {"dev"},
		"serviceId":     {"svc"},
		"endpointId":    {"dash"},
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/open", form, token))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != endpointURL {
		t.Fatalf("status = %d, location %q, body %s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if admitter.calls != 1 || admitter.url != endpointURL {
		t.Fatalf("admitter calls=%d URL=%q", admitter.calls, admitter.url)
	}
}

func TestHandlerDoesNotOpenUnavailableOrUnadmittedEndpoint(t *testing.T) {
	for _, test := range []struct {
		name        string
		state       string
		tunnel      string
		localURL    string
		admitterErr error
	}{
		{name: "unavailable", state: "unavailable", tunnel: "failed", localURL: "http://127.0.0.1:43123/"},
		{name: "policy rejects URL", state: "available", tunnel: "ready", localURL: "https://example.com/", admitterErr: fmt.Errorf("denied")},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, "{\"version\":1,\"endpoint\":{\"id\":\"dash\",\"label\":\"Dashboard\",\"endpointState\":%q,\"tunnelState\":%q,\"localUrl\":%q,\"failure\":\"\"}}", test.state, test.tunnel, test.localURL)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			admitter := &testAdmitter{err: test.admitterErr}
			h := NewHandler(client, admitter)
			token := h.(*handler).token
			form := url.Values{"environmentId": {"dev"}, "serviceId": {"svc"}, "endpointId": {"dash"}}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, formRequest(http.MethodPost, "/open", form, token))
			if response.Code == http.StatusSeeOther {
				t.Fatalf("unexpected redirect to %q", response.Header().Get("Location"))
			}
			if admitter.calls != 0 && test.name == "unavailable" {
				t.Fatalf("unavailable endpoint admission calls = %d", admitter.calls)
			}
		})
	}
}

func TestStateChangingFormsRequireTokenAndErrorPagesKeepSecurityHeaders(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "upstream failed", http.StatusBadGateway)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	h := NewHandler(client, nil)

	response := httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/register", url.Values{"name": {"dev"}, "host": {"host"}}, "wrong"))
	if response.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("invalid token gate: status=%d calls=%d", response.Code, calls)
	}

	response = httptest.NewRecorder()
	h.(*handler).showError(response, http.StatusBadGateway, "failure")
	if response.Code != http.StatusBadGateway || response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("error response headers = %#v", response.Header())
	}
}

func formRequest(method, target string, form url.Values, token string) *http.Request {
	values := make(url.Values, len(form)+1)
	for key, items := range form {
		values[key] = append([]string(nil), items...)
	}
	values.Set("token", token)
	request := httptest.NewRequest(method, target, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}
