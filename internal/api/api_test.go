package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/runtime"
)

type fake struct {
	action       string
	env, svc, ep string
	startErr     error
	ensureErr    error
	state        *runtime.State
}

func (f *fake) Snapshot() runtime.State {
	if f.state != nil {
		return *f.state
	}
	return runtime.State{Version: 1, Environments: []runtime.Environment{}}
}
func (f *fake) Start(_ context.Context, e, s string) (runtime.Service, error) {
	f.action = "start"
	f.env = e
	f.svc = s
	if f.startErr != nil {
		return runtime.Service{}, f.startErr
	}
	return runtime.Service{ID: s, Endpoints: []runtime.Endpoint{}}, nil
}
func (f *fake) Stop(_ context.Context, e, s string) (runtime.Service, error) {
	f.action = "stop"
	f.env = e
	f.svc = s
	return runtime.Service{ID: s}, nil
}
func (f *fake) Restart(_ context.Context, e, s string) (runtime.Service, error) {
	f.action = "restart"
	f.env = e
	f.svc = s
	return runtime.Service{ID: s}, nil
}
func (f *fake) Ensure(_ context.Context, e, s, p string) (runtime.Endpoint, error) {
	f.action = "ensure"
	f.env = e
	f.svc = s
	f.ep = p
	return runtime.Endpoint{ID: p}, nil
}
func (f *fake) EnsureJinushi(context.Context, string) error { return f.ensureErr }

func TestContract(t *testing.T) {
	f := &fake{}
	handler, capability := newAuthorizedAPI(t, f)
	for _, path := range []string{"start", "stop", "restart"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/service/"+path, `{"environmentId":"e","serviceId":"s"}`))
		if w.Code != 200 || f.action != path || f.env != "e" || f.svc != "s" || !strings.Contains(w.Body.String(), `"version":1,"service"`) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/endpoint/ensure", `{"environmentId":"e","serviceId":"s","endpointId":"ui"}`))
	if w.Code != 200 || f.ep != "ui" || !strings.Contains(w.Body.String(), `"endpoint":{"id":"ui"`) {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedRequest(capability, "GET", "/v1/state", ""))
	var state runtime.State
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Version != 1 {
		t.Fatal(w.Body.String())
	}
}

func TestStateProjectionSanitizesDiagnosticsAtAPIBoundary(t *testing.T) {
	const secret = "bearer-secret-abc123"
	const endpointEvidence = "Dynamic endpoint evidence is missing. Start the managed service and check its descriptor and Run output."
	state := runtime.State{Version: 1, Environments: []runtime.Environment{{
		ID: "dev", Error: secret,
		Services: []runtime.Service{{
			ID: "svc", ProcessError: "host-unreachable", ReadinessError: secret,
			Endpoints: []runtime.Endpoint{{ID: "ui", Failure: endpointEvidence}},
		}},
	}}}
	handler := New(&fake{state: &state})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/v1/state", nil))
	if w.Code != 200 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), `"error":"remote-failure"`) || !strings.Contains(w.Body.String(), `"processError":"host-unreachable"`) || !strings.Contains(w.Body.String(), `"failure":"endpoint-evidence-missing"`) {
		t.Fatalf("state DTO diagnostic projection = %d %s", w.Code, w.Body.String())
	}
	if state.Environments[0].Error != secret || state.Environments[0].Services[0].ReadinessError != secret || state.Environments[0].Services[0].Endpoints[0].Failure != endpointEvidence {
		t.Fatal("API projection mutated the runtime-owned snapshot")
	}
}

func TestStateGETPreservesNilAndEmptyArrayShapes(t *testing.T) {
	for _, test := range []struct {
		name  string
		state runtime.State
		array string
	}{
		{
			name:  "empty state",
			state: runtime.State{Version: 1, Environments: []runtime.Environment{}},
			array: `"environments":[]`,
		},
		{
			name: "environment with no services",
			state: runtime.State{Version: 1, Environments: []runtime.Environment{{
				ID: "dev", Services: []runtime.Service{},
			}}},
			array: `"services":[]`,
		},
		{
			name: "service with no endpoints",
			state: runtime.State{Version: 1, Environments: []runtime.Environment{{
				ID: "dev", Services: []runtime.Service{{ID: "svc", Endpoints: []runtime.Endpoint{}}},
			}}},
			array: `"endpoints":[]`,
		},
		{
			name:  "nil collections",
			state: runtime.State{Version: 1},
			array: `"environments":null`,
		},
		{
			name:  "nil services",
			state: runtime.State{Version: 1, Environments: []runtime.Environment{{ID: "dev"}}},
			array: `"services":null`,
		},
		{
			name: "nil endpoints",
			state: runtime.State{Version: 1, Environments: []runtime.Environment{{
				ID: "dev", Services: []runtime.Service{{ID: "svc"}},
			}}},
			array: `"endpoints":null`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := test.state
			before, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			handler := New(&fake{state: &state})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest("GET", "/v1/state", nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), test.array) {
				t.Fatalf("state response lost slice shape %s: %d %s", test.array, w.Code, w.Body.String())
			}
			after, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatalf("API projection mutated the runtime-owned snapshot: before %s, after %s", before, after)
			}
		})
	}
}
func TestInvalidRequests(t *testing.T) {
	handler, capability := newAuthorizedAPI(t, &fake{})
	for _, body := range []string{`{}`, `{"environmentId":"e","serviceId":"s","endpointId":"ui"}`, `{"environmentId":"e","serviceId":"s","extra":1}`, `{"environmentId":"e","serviceId":"s"} {}`} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/service/start", body))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid-request"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestActionFailureKeepsStableClassAndBoundedJinushiEvidence(t *testing.T) {
	f := &fake{startErr: &runtime.Failure{Code: "jinushi-command-failed", Evidence: "invalid-run"}}
	handler, capability := newAuthorizedAPI(t, f)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/service/start", `{"environmentId":"e","serviceId":"s"}`))
	var response struct {
		Version int `json:"version"`
		Error   struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 502 || response.Version != 1 || response.Error.Code != "jinushi-command-failed" || response.Error.Message != "invalid-run" {
		t.Fatalf("action failure = status %d, %#v; want stable class plus literal Jinushi code", w.Code, response)
	}
}

func TestActionFailureDoesNotPublishUntrustedEvidence(t *testing.T) {
	const secret = "bearer-secret-abc123"
	for _, test := range []struct {
		name string
		code string
	}{
		{name: "known code with untrusted evidence", code: "jinushi-command-failed"},
		{name: "unknown code", code: secret},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &fake{startErr: &runtime.Failure{Code: test.code, Evidence: secret}}
			handler, capability := newAuthorizedAPI(t, f)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/service/start", `{"environmentId":"e","serviceId":"s"}`))
			if w.Code != 502 || strings.Contains(w.Body.String(), secret) {
				t.Fatalf("action failure exposed untrusted diagnostic: %d %s", w.Code, w.Body.String())
			}
			wantCode := test.code
			if test.code == secret {
				wantCode = "remote-failure"
			}
			if !strings.Contains(w.Body.String(), `"version":1`) || !strings.Contains(w.Body.String(), `"code":"`+wantCode+`"`) || !strings.Contains(w.Body.String(), `"message":"`+wantCode+`"`) {
				t.Fatalf("action failure changed stable envelope: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestEnsureJinushiFailureKeepsStableClassAndHidesUntrustedEvidence(t *testing.T) {
	const secret = "bearer-secret-abc123"
	f := &fake{ensureErr: &runtime.Failure{Code: "jinushi-unavailable", Evidence: secret}}
	handler, capability := newAuthorizedAPI(t, f)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/environment/ensure-jinushi", `{"environmentId":"e"}`))
	if w.Code != 502 || strings.Contains(w.Body.String(), secret) || !strings.Contains(w.Body.String(), `"version":1`) || !strings.Contains(w.Body.String(), `"code":"jinushi-unavailable"`) || !strings.Contains(w.Body.String(), `"message":"jinushi-unavailable"`) {
		t.Fatalf("EnsureJinushi failure exposed unsafe evidence or changed envelope: %d %s", w.Code, w.Body.String())
	}
}

func TestEnsureJinushiFailurePreservesFixedPartialPersistenceEvidence(t *testing.T) {
	const evidence = "desired service state could not be saved after the Jinushi operation succeeded"
	f := &fake{ensureErr: &runtime.Failure{Code: "lifecycle-conflict", Evidence: evidence}}
	handler, capability := newAuthorizedAPI(t, f)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/environment/ensure-jinushi", `{"environmentId":"e"}`))
	if w.Code != 502 || !strings.Contains(w.Body.String(), `"code":"lifecycle-conflict"`) || !strings.Contains(w.Body.String(), `"message":"`+evidence+`"`) {
		t.Fatalf("EnsureJinushi failure lost fixed evidence or changed status/envelope: %d %s", w.Code, w.Body.String())
	}
}

func TestActionFailureMapsTimeoutAndConflictStatuses(t *testing.T) {
	for _, test := range []struct {
		code string
		want int
	}{
		{code: "ssh-timeout", want: 504},
		{code: "jinushi-timeout", want: 504},
		{code: "lifecycle-conflict", want: 409},
		{code: "host-unreachable", want: 502},
	} {
		t.Run(test.code, func(t *testing.T) {
			f := &fake{startErr: &runtime.Failure{Code: test.code}}
			handler, capability := newAuthorizedAPI(t, f)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/service/start", `{"environmentId":"e","serviceId":"s"}`))
			if w.Code != test.want {
				t.Fatalf("status = %d, want %d; response %s", w.Code, test.want, w.Body.String())
			}
		})
	}
}

type registeringFake struct {
	fake
	registrations int
}

func (f *registeringFake) Register(s *registry.Snapshot) error {
	if len(s.Environments()) != 1 {
		panic("unvalidated snapshot")
	}
	f.registrations++
	return nil
}
func TestRegistrationValidatesBeforeMutation(t *testing.T) {
	f := &registeringFake{}
	handler, capability := newAuthorizedAPI(t, f)
	for _, body := range []string{`{}`, `{"environments":[{"id":"dev","sshHost":"-unsafe"}]}`, `{"environments":[],"services":[]}`} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/environment/register", body))
		if w.Code != 400 || f.registrations != 0 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	body := `{"environments":[{"id":"dev","sshHost":"host","jinushi":{"supervisorStartCommand":["start"]}}],"services":[]}`
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedRequest(capability, "POST", "/v1/environment/register", body))
	if w.Code != 200 || f.registrations != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	readOnlyHandler, readOnlyCapability := newAuthorizedAPI(t, &fake{})
	readOnlyHandler.ServeHTTP(w, authorizedRequest(readOnlyCapability, "POST", "/v1/environment/register", body))
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
}
func TestLoopback(t *testing.T) {
	listener, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if listener.Addr().(*net.TCPAddr).IP.String() != "127.0.0.1" {
		t.Fatal(listener.Addr())
	}
}
