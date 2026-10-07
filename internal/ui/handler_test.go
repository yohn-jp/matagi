package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/settings"
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
	handler := testHandler(t, client, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		"dev", "Connection · SSH", "environment error", "Jinushi · process supervisor", "Desired", "Process", "Readiness", "running", "tone-ok", "prefers-color-scheme: light", "aria-busy", "Live · updates every 3 seconds", "setInterval(refresh,3000)", "Start", "Restart", "Stop",
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

func TestNewHandlerRemainsACompatibleHostLocaleConstructor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"version":1,"environments":[]}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(client, nil)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Matagi") {
		t.Fatalf("compatibility constructor = %d, %s", response.Code, response.Body.String())
	}
}

func TestWorkspaceTemplateHasJapaneseCopyForEveryLiteralMessage(t *testing.T) {
	messageID := regexp.MustCompile(`\{\{t \$?\.Locale "([^"]+)"`)
	for _, match := range messageID.FindAllStringSubmatch(pageHTML, -1) {
		if !i18n.Japanese.Has(match[1]) {
			t.Errorf("workspace message %q has no Japanese catalog entry", match[1])
		}
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
	h := testHandler(t, client, nil)
	token := h.token
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

func TestGenericServiceFormAndBoundedArgvRegistration(t *testing.T) {
	var registered []AddServiceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/state":
			fmt.Fprint(w, `{"version":1,"environments":[{"id":"dev","connectivity":"connected","jinushi":"ready","services":[]}]}`)
		case "/v1/service/add":
			var request AddServiceRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode service registration: %v", err)
			}
			registered = append(registered, request)
			fmt.Fprint(w, `{"version":1,"environments":[]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h := testHandler(t, client, nil)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, want := range []string{
		"Service name", "Remote UI port (static endpoint)", "Dynamic endpoint descriptor path (optional)", "managed Run output", "Readiness path", "Working directory on the development host", "Start command and arguments", "Matagi does not parse shell quoting.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("generic service form lacks %q", want)
		}
	}
	for _, product := range []string{"Yokodori", "Inari", "Hachidori", "yokodori serve"} {
		if strings.Contains(pageHTML, product) {
			t.Errorf("generic page includes sibling product copy %q", product)
		}
	}
	form := url.Values{
		"environmentId": {"dev"},
		"service":       {"local-dashboard"},
		"port":          {"43123"},
		"healthPath":    {"/healthz"},
		"cwd":           {"/srv/dashboard"},
		"command":       {"python   -m http.server 8080"},
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/service/add", form, h.token))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("service registration = %d, body=%s", response.Code, response.Body.String())
	}
	want := AddServiceRequest{EnvironmentID: "dev", ID: "local-dashboard", Argv: []string{"python", "-m", "http.server", "8080"}, CWD: "/srv/dashboard", Port: 43123, HealthPath: "/healthz"}
	if len(registered) != 1 || registered[0].EnvironmentID != want.EnvironmentID || registered[0].ID != want.ID || registered[0].CWD != want.CWD || registered[0].Port != want.Port || registered[0].HealthPath != want.HealthPath || strings.Join(registered[0].Argv, "\x00") != strings.Join(want.Argv, "\x00") {
		t.Fatalf("registered service = %#v, want %#v", registered, want)
	}
	dynamicForm := url.Values{
		"environmentId":  {"dev"},
		"service":        {"ephemeral-dashboard"},
		"resolutionPath": {"/home/dev/.cache/service/endpoint.json"},
		"cwd":            {"/srv/dashboard"},
		"command":        {"dashboard --serve"},
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/service/add", dynamicForm, h.token))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("dynamic service registration = %d, body=%s", response.Code, response.Body.String())
	}
	if len(registered) != 2 || registered[1].Port != 0 || registered[1].ResolutionPath != dynamicForm.Get("resolutionPath") {
		t.Fatalf("dynamic registration request = %#v", registered)
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
	h := testHandler(t, client, nil)
	token := h.token
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
	h := testHandler(t, client, admitter)
	token := h.token
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
			h := testHandler(t, client, admitter)
			token := h.token
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
	h := testHandler(t, client, nil)

	response := httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/register", url.Values{"name": {"dev"}, "host": {"host"}}, "wrong"))
	if response.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("invalid token gate: status=%d calls=%d", response.Code, calls)
	}
	if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("rejected form headers = %#v", response.Header())
	}

	response = httptest.NewRecorder()
	h.showError(response, http.StatusBadGateway, "failure")
	if response.Code != http.StatusBadGateway || response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("error response headers = %#v", response.Header())
	}
	if body := response.Body.String(); !strings.Contains(body, "Request could not be completed") || !strings.Contains(body, `href="/">Return to Workspace</a>`) || !strings.Contains(body, "Workspace status is unavailable.") || strings.Contains(body, "Loading the Matagi workspace") || strings.Contains(body, `id="sync-text"`) {
		t.Fatalf("error page should offer a return without implying an endless load: %s", body)
	}
}

func TestLocaleSelectionPersistsAndRendersCompleteJapaneseWorkspace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"version":1,"environments":[{"id":"dev","sshHost":"dev-host","connectivity":"connected","jinushi":"ready","services":[{"id":"svc","desiredState":"running","state":"ready","process":"running","readiness":"ready","processError":"process warning","readinessError":"","endpoints":[{"id":"ui","label":"Dashboard","endpointState":"available","tunnelState":"ready","localUrl":"http://127.0.0.1:43123/","failure":""}]}]}]}`)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	store, err := settings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLocale("en"); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithOptions(client, nil, Options{Settings: store}).(*handler)

	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `<html lang="en">`) {
		t.Fatalf("English workspace did not render: status=%d body=%s", response.Code, response.Body.String())
	}
	form := url.Values{"locale": {"ja"}, "returnTo": {"/updates"}}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/settings/locale", form, h.token))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/updates" {
		t.Fatalf("locale POST = %d, Location=%q, body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	for _, want := range []string{`<html lang="ja">`, "開発環境", "接続 · SSH", "目標状態", "プロセス", "準備状態", "開始", "Dashboard", "process warning", `name="environmentId"`, `name="serviceId"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Japanese workspace lacks %q", want)
		}
	}
	if !strings.Contains(body, "<h2>svc</h2>") || !strings.Contains(body, "dev-host") || !strings.Contains(body, `name="environmentId"`) || !strings.Contains(body, `name="serviceId"`) {
		t.Error("service/environment identities or API field names were changed")
	}
	if !strings.Contains(body, `toLocaleTimeString(lang)`) || !strings.Contains(body, `"Live · updated ":"ライブ · 更新時刻 "`) {
		t.Error("browser-generated operator copy did not use the page locale")
	}
	if got, err := store.Locale(); err != nil || got != "ja" {
		t.Fatalf("saved locale = %q, %v", got, err)
	}
}

func TestLocaleReturnPathIsRestrictedAndUnsupportedSelectionIsRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"version":1,"environments":[]}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	h := testHandler(t, client, nil)
	form := url.Values{"locale": {"ja"}, "returnTo": {"https://example.invalid/"}}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/settings/locale", form, h.token))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("unsafe return path = %d, %q", response.Code, response.Header().Get("Location"))
	}
	form.Set("locale", "fr")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/settings/locale", form, h.token))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "English または 日本語を選択してください。") {
		t.Fatalf("unsupported locale response = %d, %s", response.Code, response.Body.String())
	}
}

func TestLifecycleActionConflictPreservesEvidenceAndDoesNotClaimNoChange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"version":1,"error":{"code":"lifecycle-conflict","message":"submission-123"}}`, http.StatusConflict)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	h := testHandler(t, client, nil)
	form := url.Values{"environmentId": {"dev"}, "serviceId": {"svc"}, "action": {"start"}}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/action", form, h.token))
	body := response.Body.String()
	for _, want := range []string{"lifecycle-conflict", "submission-123", "could not be confirmed", "inspect the managed process"} {
		if !strings.Contains(body, want) {
			t.Errorf("action conflict lacks %q: %s", want, body)
		}
	}
	if strings.Contains(body, "No changes were saved.") {
		t.Fatal("ambiguous lifecycle result was represented as no change")
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

func testHandler(t *testing.T, client *Client, admitter EndpointAdmitter) *handler {
	t.Helper()
	store, err := settings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLocale("en"); err != nil {
		t.Fatal(err)
	}
	return NewHandlerWithOptions(client, admitter, Options{Settings: store}).(*handler)
}
