package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/presentation"
	"github.com/yohn-jp/matagi/internal/settings"
)

type modelPresenter struct {
	mu            sync.Mutex
	model         *presentation.Model
	reservations  []OpenReservation
	completeCalls int
	completedURLs []string
	failCalls     int
	selected      []presentation.ViewID
	moved         []presentation.Location
	closed        []presentation.ViewID
	resetCalls    int
	trusted       []TrustedDestination
	completeErr   error
	resetErr      error
}

func newModelPresenter() *modelPresenter {
	return &modelPresenter{model: presentation.NewModel()}
}

func (p *modelPresenter) ReserveOpen(key presentation.EndpointKey, location presentation.Location) (OpenReservation, error) {
	result, err := p.model.ReserveOpen(key, location)
	if err != nil {
		return OpenReservation{}, err
	}
	reservation := OpenReservation{View: result.View, Operation: result.Operation, Ensure: result.Ensure}
	p.mu.Lock()
	p.reservations = append(p.reservations, reservation)
	p.mu.Unlock()
	return reservation, nil
}

func (p *modelPresenter) CompleteOpen(reservation OpenReservation, localURL string) error {
	p.mu.Lock()
	p.completeCalls++
	err := p.completeErr
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if _, ok := p.model.CompleteOpen(reservation.Operation); !ok {
		return errors.New("stale presentation generation")
	}
	p.mu.Lock()
	p.completedURLs = append(p.completedURLs, localURL)
	p.mu.Unlock()
	return nil
}

func (p *modelPresenter) FailOpen(reservation OpenReservation) {
	p.model.FailOpen(reservation.Operation)
	p.mu.Lock()
	p.failCalls++
	p.mu.Unlock()
}

func (p *modelPresenter) Select(id presentation.ViewID) error {
	if !p.model.Select(id) {
		return errors.New("view not found")
	}
	p.mu.Lock()
	p.selected = append(p.selected, id)
	p.mu.Unlock()
	return nil
}

func (p *modelPresenter) SelectTrusted(destination TrustedDestination) error {
	switch destination {
	case TrustedWorkspace, TrustedUpdates, TrustedSettings:
	default:
		return errors.New("unknown trusted destination")
	}
	p.mu.Lock()
	p.trusted = append(p.trusted, destination)
	p.mu.Unlock()
	return nil
}

func (p *modelPresenter) Move(id presentation.ViewID, target presentation.Location) error {
	operation, err := p.model.BeginMove(id, target, nil)
	if err != nil {
		return err
	}
	if _, ok := p.model.CommitMove(operation); !ok {
		return errors.New("move generation expired")
	}
	p.mu.Lock()
	p.moved = append(p.moved, target)
	p.mu.Unlock()
	return nil
}

func (p *modelPresenter) Close(id presentation.ViewID) error {
	if !p.model.Close(id) {
		return errors.New("view not found")
	}
	p.mu.Lock()
	p.closed = append(p.closed, id)
	p.mu.Unlock()
	return nil
}

func (p *modelPresenter) Snapshot() presentation.Snapshot { return p.model.Snapshot() }

func (p *modelPresenter) ResetSavedLayout() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resetCalls++
	return p.resetErr
}

func (p *modelPresenter) recorded() (reservations []OpenReservation, completeCalls int, completedURLs []string, failCalls int, selected []presentation.ViewID, moved []presentation.Location, closed []presentation.ViewID, resetCalls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]OpenReservation(nil), p.reservations...), p.completeCalls, append([]string(nil), p.completedURLs...), p.failCalls,
		append([]presentation.ViewID(nil), p.selected...), append([]presentation.Location(nil), p.moved...), append([]presentation.ViewID(nil), p.closed...), p.resetCalls
}

func (p *modelPresenter) trustedDestinations() []TrustedDestination {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]TrustedDestination(nil), p.trusted...)
}

func TestPresenterOpenReservesBeforeEnsureAndUsesTrustedReturn(t *testing.T) {
	const endpointURL = "http://127.0.0.1:43123/ui/"
	var ensures int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/endpoint/ensure" {
			t.Errorf("unexpected runtime path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		ensures++
		var request EndpointRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode ensure request: %v", err)
		}
		if request != (EndpointRequest{EnvironmentID: "dev", ServiceID: "svc", EndpointID: "dash"}) {
			t.Errorf("ensure request = %#v", request)
		}
		fmt.Fprintf(w, `{"version":1,"endpoint":{"id":"dash","label":"Dashboard","endpointState":"available","tunnelState":"ready","localUrl":%q}}`, endpointURL)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	presenter := newModelPresenter()
	h := testHandlerWithPresenter(t, client, nil, presenter)
	form := url.Values{
		"environmentId": {"dev"}, "serviceId": {"svc"}, "endpointId": {"dash"}, "location": {"window"},
		"url": {"https://attacker.invalid/"},
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/open", form, h.token))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?environmentId=dev" {
		t.Fatalf("Open response = %d, %q, %s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if ensures != 1 {
		t.Fatalf("ensure calls = %d, want 1", ensures)
	}
	reservations, completeCalls, completedURLs, failCalls, selected, _, _, _ := presenter.recorded()
	if len(reservations) != 1 || !reservations[0].Ensure || reservations[0].View.Location != presentation.LocationWindow {
		t.Fatalf("reservation = %#v", reservations)
	}
	if completeCalls != 1 || len(completedURLs) != 1 || completedURLs[0] != endpointURL || failCalls != 0 {
		t.Fatalf("completion calls=%d urls=%q failures=%d", completeCalls, completedURLs, failCalls)
	}
	if len(selected) != 1 || selected[0] != reservations[0].View.ID {
		t.Fatalf("selected views = %#v", selected)
	}
	views := presenter.Snapshot().Views
	if len(views) != 1 || views[0].State != presentation.ViewCreated || views[0].Location != presentation.LocationWindow {
		t.Fatalf("published views = %#v", views)
	}
}

func TestConcurrentPresenterOpenCoalescesPendingEnsure(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	ensures := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ensures++
		mu.Unlock()
		started <- struct{}{}
		<-release
		fmt.Fprint(w, `{"version":1,"endpoint":{"id":"dash","endpointState":"available","tunnelState":"ready","localUrl":"http://127.0.0.1:43123/"}}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	presenter := newModelPresenter()
	h := testHandlerWithPresenter(t, client, nil, presenter)
	form := url.Values{"environmentId": {"dev"}, "serviceId": {"svc"}, "endpointId": {"dash"}}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, formRequest(http.MethodPost, "/open", form, h.token))
		firstDone <- response
	}()
	<-started
	second := httptest.NewRecorder()
	h.ServeHTTP(second, formRequest(http.MethodPost, "/open", form, h.token))
	if second.Code != http.StatusSeeOther || second.Header().Get("Location") != "/service?environmentId=dev" {
		t.Fatalf("coalesced Open = %d, %q, %s", second.Code, second.Header().Get("Location"), second.Body.String())
	}
	close(release)
	first := <-firstDone
	if first.Code != http.StatusSeeOther || first.Header().Get("Location") != "/service?environmentId=dev" {
		t.Fatalf("first Open = %d, %q, %s", first.Code, first.Header().Get("Location"), first.Body.String())
	}
	mu.Lock()
	ensureCount := ensures
	mu.Unlock()
	reservations, completeCalls, _, failCalls, _, _, _, _ := presenter.recorded()
	if ensureCount != 1 || len(reservations) != 2 || !reservations[0].Ensure || reservations[1].Ensure || completeCalls != 1 || failCalls != 0 {
		t.Fatalf("ensures=%d reservations=%#v completions=%d failures=%d", ensureCount, reservations, completeCalls, failCalls)
	}
}

func TestClosingPendingOpenRejectsLateGeneration(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		fmt.Fprint(w, `{"version":1,"endpoint":{"id":"dash","endpointState":"available","tunnelState":"ready","localUrl":"http://127.0.0.1:43123/"}}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	presenter := newModelPresenter()
	h := testHandlerWithPresenter(t, client, nil, presenter)
	openForm := url.Values{"environmentId": {"dev"}, "serviceId": {"svc"}, "endpointId": {"dash"}}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, formRequest(http.MethodPost, "/open", openForm, h.token))
		firstDone <- response
	}()
	<-started
	views := presenter.Snapshot().Views
	if len(views) != 1 || views[0].State != presentation.ViewPending {
		t.Fatalf("pending views = %#v", views)
	}
	closeResponse := httptest.NewRecorder()
	h.ServeHTTP(closeResponse, formRequest(http.MethodPost, "/presentation/close", url.Values{"viewId": {fmt.Sprint(views[0].ID)}}, h.token))
	if closeResponse.Code != http.StatusSeeOther {
		t.Fatalf("close pending view = %d, %s", closeResponse.Code, closeResponse.Body.String())
	}
	close(release)
	openResponse := <-firstDone
	if openResponse.Code != http.StatusBadGateway || strings.Contains(openResponse.Body.String(), "stale presentation generation") {
		t.Fatalf("late Open response = %d, %s", openResponse.Code, openResponse.Body.String())
	}
	if views := presenter.Snapshot().Views; len(views) != 0 {
		t.Fatalf("canceled generation was published: %#v", views)
	}
	_, completeCalls, completedURLs, _, _, _, closed, _ := presenter.recorded()
	if completeCalls != 1 || len(completedURLs) != 0 || len(closed) != 1 {
		t.Fatalf("completion attempts=%d completed=%q closed=%v", completeCalls, completedURLs, closed)
	}
}

func TestPresenterOpenRejectsWrongTokenEndpointStateURLAndFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		endpointID string
		state      string
		tunnel     string
		localURL   string
		fail       error
		wrongToken bool
	}{
		{name: "mismatched endpoint", endpointID: "other", state: "available", tunnel: "ready", localURL: "http://127.0.0.1:43123/"},
		{name: "unavailable tunnel", endpointID: "dash", state: "available", tunnel: "failed", localURL: "http://127.0.0.1:43123/"},
		{name: "unavailable endpoint", endpointID: "dash", state: "unavailable", tunnel: "ready", localURL: "http://127.0.0.1:43123/"},
		{name: "non-loopback URL", endpointID: "dash", state: "available", tunnel: "ready", localURL: "https://attacker.invalid/"},
		{name: "presenter failure", endpointID: "dash", state: "available", tunnel: "ready", localURL: "http://127.0.0.1:43123/", fail: errors.New("private path secret")},
		{name: "wrong token", endpointID: "dash", state: "available", tunnel: "ready", localURL: "http://127.0.0.1:43123/", wrongToken: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var ensures int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ensures++
				fmt.Fprintf(w, `{"version":1,"endpoint":{"id":%q,"endpointState":%q,"tunnelState":%q,"localUrl":%q,"failure":"private path secret"}}`, test.endpointID, test.state, test.tunnel, test.localURL)
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, time.Second)
			presenter := newModelPresenter()
			presenter.completeErr = test.fail
			h := testHandlerWithPresenter(t, client, nil, presenter)
			form := url.Values{"environmentId": {"dev"}, "serviceId": {"svc"}, "endpointId": {"dash"}}
			token := h.token
			if test.wrongToken {
				token = "wrong"
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, formRequest(http.MethodPost, "/open", form, token))
			if response.Code == http.StatusSeeOther {
				t.Fatalf("unauthorized Open succeeded: %d %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private path secret") {
				t.Fatal("unsafe endpoint or presenter detail reached the page")
			}
			reservations, completeCalls, completedURLs, failCalls, _, _, _, _ := presenter.recorded()
			if test.wrongToken {
				if ensures != 0 || len(reservations) != 0 {
					t.Fatalf("wrong-token request reached presenter/runtime: ensures=%d reservations=%d", ensures, len(reservations))
				}
				return
			}
			if len(reservations) != 1 || !reservations[0].Ensure {
				t.Fatalf("reservation = %#v", reservations)
			}
			if test.endpointID != "dash" || test.state != "available" || test.tunnel != "ready" || !strings.HasPrefix(test.localURL, "http://127.0.0.1:") {
				if completeCalls != 0 || len(completedURLs) != 0 || failCalls != 1 {
					t.Fatalf("rejected endpoint completion=%d urls=%q failures=%d", completeCalls, completedURLs, failCalls)
				}
			} else if test.fail != nil {
				if completeCalls != 1 || len(completedURLs) != 0 || failCalls != 1 {
					t.Fatalf("failed completion=%d urls=%q failures=%d", completeCalls, completedURLs, failCalls)
				}
				if views := presenter.Snapshot().Views; len(views) != 1 || views[0].State != presentation.ViewFailed {
					t.Fatalf("presenter failure state = %#v", views)
				}
			}
		})
	}
}

func TestPresentationControlsAndSettingsDoNotCallRuntime(t *testing.T) {
	var runtimeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtimeCalls++
		if r.URL.Path != "/v1/state" {
			t.Errorf("presentation action called runtime route %s", r.URL.Path)
			http.Error(w, "unexpected runtime call", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"version":1,"environments":[]}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	presenter := newModelPresenter()
	key := presentation.EndpointKey{EnvironmentID: "dev", ServiceID: "svc", EndpointID: "dash"}
	reserved, err := presenter.ReserveOpen(key, presentation.LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	if err := presenter.CompleteOpen(reserved, "http://127.0.0.1:43123/"); err != nil {
		t.Fatal(err)
	}
	h := testHandlerWithPresenter(t, client, nil, presenter)

	projection := httptest.NewRecorder()
	h.ServeHTTP(projection, httptest.NewRequest(http.MethodGet, "/presentation", nil))
	if projection.Code != http.StatusOK || projection.Header().Get("Cache-Control") != "no-store" || strings.Contains(projection.Body.String(), "localUrl") || !strings.Contains(projection.Body.String(), `"endpointId":"dash"`) {
		t.Fatalf("presentation projection = %d %#v %s", projection.Code, projection.Header(), projection.Body.String())
	}

	for _, action := range []struct {
		path string
		form url.Values
	}{
		{path: "/presentation/select", form: url.Values{"viewId": {fmt.Sprint(reserved.View.ID)}}},
		{path: "/presentation/move", form: url.Values{"viewId": {fmt.Sprint(reserved.View.ID)}, "target": {"window"}}},
		{path: "/presentation/move", form: url.Values{"viewId": {fmt.Sprint(reserved.View.ID)}, "target": {"tab"}}},
		{path: "/presentation/close", form: url.Values{"viewId": {fmt.Sprint(reserved.View.ID)}}},
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, formRequest(http.MethodPost, action.path, action.form, h.token))
		if response.Code != http.StatusSeeOther {
			t.Fatalf("%s = %d, %s", action.path, response.Code, response.Body.String())
		}
	}
	settingsPage := httptest.NewRecorder()
	h.ServeHTTP(settingsPage, httptest.NewRequest(http.MethodGet, "/settings", nil))
	for _, want := range []string{"Saved service views", "parked placeholders", `action="/settings/locale"`, `name="returnTo" value="/settings"`, `name="locale"`, `<option value="ja"`, `action="/presentation/reset"`, `name="token" value="` + h.token + `"`} {
		if !strings.Contains(settingsPage.Body.String(), want) {
			t.Errorf("Settings page lacks %q", want)
		}
	}
	localeResponse := httptest.NewRecorder()
	h.ServeHTTP(localeResponse, formRequest(http.MethodPost, "/settings/locale", url.Values{"locale": {"ja"}, "returnTo": {"/settings"}}, h.token))
	if localeResponse.Code != http.StatusSeeOther || localeResponse.Header().Get("Location") != "/settings" {
		t.Fatalf("Settings locale return = %d %q", localeResponse.Code, localeResponse.Header().Get("Location"))
	}
	updatesLocaleReturn := httptest.NewRecorder()
	h.ServeHTTP(updatesLocaleReturn, formRequest(http.MethodPost, "/settings/locale", url.Values{"locale": {"en"}, "returnTo": {"/updates"}}, h.token))
	if updatesLocaleReturn.Code != http.StatusSeeOther || updatesLocaleReturn.Header().Get("Location") != "/updates" {
		t.Fatalf("Updates locale return = %d %q", updatesLocaleReturn.Code, updatesLocaleReturn.Header().Get("Location"))
	}
	reset := httptest.NewRecorder()
	h.ServeHTTP(reset, formRequest(http.MethodPost, "/presentation/reset", nil, h.token))
	if reset.Code != http.StatusSeeOther || reset.Header().Get("Location") != "/settings" {
		t.Fatalf("layout reset = %d %q %s", reset.Code, reset.Header().Get("Location"), reset.Body.String())
	}
	_, _, _, _, selected, moved, closed, resets := presenter.recorded()
	if len(selected) != 1 || len(moved) != 2 || len(closed) != 1 || resets != 1 {
		t.Fatalf("presentation actions selected=%v moved=%v closed=%v reset=%d", selected, moved, closed, resets)
	}
	if runtimeCalls != 0 {
		t.Fatalf("presentation and Settings actions called runtime %d times", runtimeCalls)
	}
}

func TestPresentationPostRoutesRequireFormToken(t *testing.T) {
	presenter := newModelPresenter()
	h := testHandlerWithPresenter(t, nil, nil, presenter)
	for _, path := range []string{"/presentation/select", "/presentation/move", "/presentation/close", "/presentation/resume", "/presentation/reset"} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, formRequest(http.MethodPost, path, url.Values{"viewId": {"1"}, "target": {"window"}, "environmentId": {"dev"}, "serviceId": {"svc"}, "endpointId": {"dash"}}, "wrong"))
		if response.Code != http.StatusForbidden {
			t.Errorf("%s wrong-token status = %d, body=%s", path, response.Code, response.Body.String())
		}
	}
	_, _, _, _, selected, moved, closed, resets := presenter.recorded()
	if len(selected) != 0 || len(moved) != 0 || len(closed) != 0 || resets != 0 {
		t.Fatalf("wrong-token calls reached presenter: select=%v move=%v close=%v reset=%d", selected, moved, closed, resets)
	}
}

func TestWorkspaceEnvironmentSelectionAndChromeAccessibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/state" {
			t.Errorf("unexpected runtime path %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"version":1,"environments":[{"id":"dev-one","sshHost":"alias-one","connectivity":"connected","jinushi":"ready","services":[{"id":"svc","endpoints":[{"id":"dash","label":"Dashboard","endpointState":"available","tunnelState":"ready"}]}]},{"id":"dev-two","sshHost":"alias-two","connectivity":"connected","jinushi":"ready","services":[{"id":"svc","endpoints":[{"id":"editor","label":"Editor","endpointState":"available","tunnelState":"ready"}]}]}]}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	presenter := newModelPresenter()
	key := presentation.EndpointKey{EnvironmentID: "dev-two", ServiceID: "svc", EndpointID: "editor"}
	reservation, err := presenter.ReserveOpen(key, presentation.LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	if err := presenter.CompleteOpen(reservation, "http://127.0.0.1:43123/"); err != nil {
		t.Fatal(err)
	}
	h := testHandlerWithPresenter(t, client, nil, presenter)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workspace?environmentId=dev-two", nil))
	body := response.Body.String()
	for _, want := range []string{`href="/updates"`, `href="/settings"`, `aria-selected="true"`, `data-integrated-view="true"`, "svc · dev-two", "editor", "alias-two", `Select environment: dev-two · OpenSSH alias alias-two · connectivity connected`, `height: 96px`, `grid-template-rows: 48px 48px`, `.shell.app-content { padding-top:`, `role="tablist"`, `aria-label="Select view: dev-two · svc · editor"`, `name="location" value="window"`, `name="target" value="window"`, `data-close-selected`, `querySelectorAll('[role="tab"][data-integrated-view="true"]')`} {
		if !strings.Contains(body, want) {
			t.Errorf("workspace chrome lacks %q", want)
		}
	}
	if !strings.Contains(body, `aria-current="page"`) || !strings.Contains(body, `Select environment: dev-two · OpenSSH alias alias-two · connectivity connected`) || !strings.Contains(body, "alias-two") || strings.Contains(body, "dev-one"+`</h2>`) || strings.Contains(body, `action="/settings/locale"`) {
		t.Fatalf("environment selection did not stay presentation-only: %s", body)
	}
}

func TestTrustedNavigationHidesServiceAndServiceResponseKeepsItsSelection(t *testing.T) {
	var runtimeCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtimeCalls++
		if r.URL.Path != "/v1/state" {
			t.Errorf("unexpected runtime path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"version":1,"environments":[]}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	presenter := newModelPresenter()
	view := reserveAndCompleteView(t, presenter, "dev", "svc", "dash", presentation.LocationTab)
	if err := presenter.Select(view.ID); err != nil {
		t.Fatal(err)
	}
	h := testHandlerWithPresenterAndUpdates(t, client, nil, presenter, &fakeUIUpdates{})

	workspace := httptest.NewRecorder()
	h.ServeHTTP(workspace, httptest.NewRequest(http.MethodGet, "/workspace", nil))
	workspaceBody := workspace.Body.String()
	if workspace.Code != http.StatusOK || !strings.Contains(workspaceBody, `href="/workspace" aria-current="page"`) || strings.Contains(workspaceBody, `action="/settings/locale"`) {
		t.Fatalf("trusted Workspace response = %d, %s", workspace.Code, workspace.Body.String())
	}
	trusted := presenter.trustedDestinations()
	if len(trusted) != 1 || trusted[0] != TrustedWorkspace {
		t.Fatalf("Workspace trusted selection = %v", trusted)
	}
	workspacePoll := httptest.NewRecorder()
	h.ServeHTTP(workspacePoll, httptest.NewRequest(http.MethodGet, "/workspace/refresh?environmentId=dev", nil))
	if workspacePoll.Code != http.StatusOK || fmt.Sprint(presenter.trustedDestinations()) != fmt.Sprint(trusted) {
		t.Fatalf("observational Workspace poll changed destination: status=%d destinations=%v", workspacePoll.Code, presenter.trustedDestinations())
	}

	service := httptest.NewRecorder()
	h.ServeHTTP(service, httptest.NewRequest(http.MethodGet, "/service?environmentId=dev", nil))
	serviceBody := service.Body.String()
	if service.Code != http.StatusOK || strings.Contains(serviceBody, `href="/workspace" aria-current="page"`) || !strings.Contains(serviceBody, `data-close-selected`) || !strings.Contains(serviceBody, `aria-selected="true"`) {
		t.Fatalf("service response did not preserve its selected view: %d, %s", service.Code, serviceBody)
	}
	if got := presenter.trustedDestinations(); len(got) != 1 {
		t.Fatalf("service response selected a trusted destination: %v", got)
	}
	servicePoll := httptest.NewRecorder()
	h.ServeHTTP(servicePoll, httptest.NewRequest(http.MethodGet, "/service/refresh?environmentId=dev", nil))
	if servicePoll.Code != http.StatusOK || len(presenter.trustedDestinations()) != 1 {
		t.Fatalf("observational service poll changed destination: status=%d destinations=%v", servicePoll.Code, presenter.trustedDestinations())
	}

	updates := httptest.NewRecorder()
	h.ServeHTTP(updates, httptest.NewRequest(http.MethodGet, "/updates", nil))
	updatesBody := updates.Body.String()
	if updates.Code != http.StatusOK || !strings.Contains(updatesBody, `href="/updates" aria-current="page"`) || strings.Contains(updatesBody, `href="/workspace" aria-current="page"`) || strings.Contains(updatesBody, `action="/settings/locale"`) {
		t.Fatalf("Updates trusted response = %d, %s", updates.Code, updates.Body.String())
	}
	updatesPoll := httptest.NewRecorder()
	h.ServeHTTP(updatesPoll, httptest.NewRequest(http.MethodGet, "/updates/refresh", nil))
	if updatesPoll.Code != http.StatusOK || fmt.Sprint(presenter.trustedDestinations()) != fmt.Sprint([]TrustedDestination{TrustedWorkspace, TrustedUpdates}) {
		t.Fatalf("observational Updates poll changed destination: status=%d destinations=%v", updatesPoll.Code, presenter.trustedDestinations())
	}
	settings := httptest.NewRecorder()
	h.ServeHTTP(settings, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if settings.Code != http.StatusOK || !strings.Contains(settings.Body.String(), `href="/settings" aria-current="page"`) || strings.Contains(settings.Body.String(), `href="/workspace" aria-current="page"`) {
		t.Fatalf("Settings trusted response = %d, %s", settings.Code, settings.Body.String())
	}
	trusted = presenter.trustedDestinations()
	want := []TrustedDestination{TrustedWorkspace, TrustedUpdates, TrustedSettings}
	if fmt.Sprint(trusted) != fmt.Sprint(want) {
		t.Fatalf("trusted destinations = %v, want %v", trusted, want)
	}
	if runtimeCalls != 4 {
		t.Fatalf("Workspace and service pages/poll called runtime %d times, want four state reads", runtimeCalls)
	}
	if len(presenter.Snapshot().Views) != 1 || presenter.Snapshot().Views[0].ID != view.ID {
		t.Fatal("trusted navigation changed the selected service view")
	}
}

func TestChromeTabsAndCloseControlsExcludeDetachedViews(t *testing.T) {
	presenter := newModelPresenter()
	integrated := reserveAndCompleteView(t, presenter, "env-a", "same", "tab", presentation.LocationTab)
	detached := reserveAndCompleteView(t, presenter, "env-b", "same", "window", presentation.LocationWindow)
	if !presenter.model.MarkUnavailable(detached.ID) {
		t.Fatal("could not mark detached view unavailable")
	}
	if err := presenter.Select(integrated.ID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"version":1,"environments":[]}`)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	h := testHandlerWithPresenter(t, client, nil, presenter)
	chrome := h.chrome("service")
	if len(chrome.Views) != 1 || chrome.Views[0].ID != integrated.ID || chrome.SelectedView == nil || chrome.SelectedView.ID != integrated.ID {
		t.Fatalf("integrated chrome views = %#v", chrome)
	}
	if err := presenter.Select(detached.ID); err != nil {
		t.Fatal(err)
	}
	chrome = h.chrome("service")
	if len(chrome.Views) != 1 || len(chrome.Detached) != 1 || chrome.Detached[0].ID != detached.ID || chrome.SelectedView != nil {
		t.Fatalf("detached selection leaked into integrated controls: %#v", chrome)
	}
	workspace := httptest.NewRecorder()
	h.ServeHTTP(workspace, httptest.NewRequest(http.MethodGet, "/workspace", nil))
	body := workspace.Body.String()
	for _, want := range []string{`data-integrated-view="true"`, `aria-label="Select view: env-a · same · tab"`, "Window view", "same · env-b · window", `action="/presentation/resume"`, `name="location" value="window"`, `action="/presentation/close"`} {
		if !strings.Contains(body, want) {
			t.Errorf("Workspace lacks detached placeholder control %q", want)
		}
	}
	if strings.Count(body, `data-integrated-view="true"`) != 2 || strings.Count(body, `data-close-selected`) != 1 {
		t.Fatalf("detached placeholder entered integrated tab controls (integrated marker count %d, close marker count %d)", strings.Count(body, `data-integrated-view="true"`), strings.Count(body, `data-close-selected`))
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/service", nil))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?environmentId=env-b" {
		t.Fatalf("detached view response = %d, %q", response.Code, response.Header().Get("Location"))
	}
	servicePoll := httptest.NewRecorder()
	h.ServeHTTP(servicePoll, httptest.NewRequest(http.MethodGet, "/service/refresh", nil))
	if servicePoll.Code != http.StatusOK || fmt.Sprint(presenter.trustedDestinations()) != fmt.Sprint([]TrustedDestination{TrustedWorkspace}) {
		t.Fatalf("detached service poll selected a trusted destination: status=%d destinations=%v", servicePoll.Code, presenter.trustedDestinations())
	}
}

func TestRestoredWindowViewStaysParkedUntilExplicitResume(t *testing.T) {
	presenter := newModelPresenter()
	key := presentation.EndpointKey{EnvironmentID: "dev", ServiceID: "svc", EndpointID: "dash"}
	if err := presenter.model.Restore(presentation.Layout{
		Views:    []presentation.LayoutEntry{{Key: key, Location: presentation.LocationWindow}},
		Selected: &key,
	}); err != nil {
		t.Fatal(err)
	}
	var stateReads, ensures int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/state":
			stateReads++
			fmt.Fprint(w, `{"version":1,"environments":[]}`)
		case "/v1/endpoint/ensure":
			ensures++
			http.Error(w, "unexpected ensure", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, time.Second)
	h := testHandlerWithPresenter(t, client, nil, presenter)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workspace", nil))
	body := response.Body.String()
	for _, want := range []string{"Window view", "svc · dev · dash", "This saved view is parked", `action="/presentation/resume"`, `name="location" value="window"`, `action="/presentation/close"`} {
		if !strings.Contains(body, want) {
			t.Errorf("restored window placeholder lacks %q", want)
		}
	}
	if response.Code != http.StatusOK || stateReads != 1 || ensures != 0 || presenter.Snapshot().Views[0].State != presentation.ViewParked {
		t.Fatalf("restored window response=%d state reads=%d ensures=%d views=%#v", response.Code, stateReads, ensures, presenter.Snapshot().Views)
	}
}

func reserveAndCompleteView(t *testing.T, presenter *modelPresenter, environmentID, serviceID, endpointID string, location presentation.Location) presentation.ViewSnapshot {
	t.Helper()
	reservation, err := presenter.ReserveOpen(presentation.EndpointKey{EnvironmentID: environmentID, ServiceID: serviceID, EndpointID: endpointID}, location)
	if err != nil {
		t.Fatal(err)
	}
	if err := presenter.CompleteOpen(reservation, "http://127.0.0.1:43123/"); err != nil {
		t.Fatal(err)
	}
	return reservation.View
}

func testHandlerWithPresenter(t *testing.T, client *Client, admitter EndpointAdmitter, presenter Presenter) *handler {
	return testHandlerWithPresenterAndUpdates(t, client, admitter, presenter, nil)
}

func testHandlerWithPresenterAndUpdates(t *testing.T, client *Client, admitter EndpointAdmitter, presenter Presenter, updates Updates) *handler {
	t.Helper()
	store, err := settings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLocale(string(i18n.English)); err != nil {
		t.Fatal(err)
	}
	return NewHandlerWithOptions(client, admitter, Options{Settings: store, Updates: updates, Presenter: presenter}).(*handler)
}
