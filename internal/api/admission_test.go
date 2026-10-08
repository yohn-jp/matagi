package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/runtime"
)

type admissionFake struct{ mutations []string }

func (f *admissionFake) Snapshot() runtime.State {
	return runtime.State{Version: 1, Environments: []runtime.Environment{}}
}
func (f *admissionFake) Start(context.Context, string, string) (runtime.Service, error) {
	f.mutations = append(f.mutations, "start")
	return runtime.Service{}, nil
}
func (f *admissionFake) Stop(context.Context, string, string) (runtime.Service, error) {
	f.mutations = append(f.mutations, "stop")
	return runtime.Service{}, nil
}
func (f *admissionFake) Restart(context.Context, string, string) (runtime.Service, error) {
	f.mutations = append(f.mutations, "restart")
	return runtime.Service{}, nil
}
func (f *admissionFake) Ensure(context.Context, string, string, string) (runtime.Endpoint, error) {
	f.mutations = append(f.mutations, "endpoint-ensure")
	return runtime.Endpoint{}, nil
}
func (f *admissionFake) Connect(context.Context, string, string, []string) error {
	f.mutations = append(f.mutations, "connect")
	return nil
}
func (f *admissionFake) AddService(registry.Service) error {
	f.mutations = append(f.mutations, "service-add")
	return nil
}
func (f *admissionFake) EnsureJinushi(context.Context, string) error {
	f.mutations = append(f.mutations, "ensure-jinushi")
	return nil
}
func (f *admissionFake) Register(*registry.Snapshot) error {
	f.mutations = append(f.mutations, "register")
	return nil
}

func newAuthorizedAPI(t *testing.T, rt Runtime) (http.Handler, Capability) {
	t.Helper()
	capability, err := NewCapability("127.0.0.1:43123")
	if err != nil {
		t.Fatal(err)
	}
	return NewWithCapability(rt, capability), capability
}

func authorizedRequest(capability Capability, method, path, body string) *http.Request {
	req := httptest.NewRequest(method, "http://127.0.0.1:43123"+path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		capability.AddToRequest(req)
	}
	return req
}

func TestEveryMutationRouteRequiresCapability(t *testing.T) {
	f := &admissionFake{}
	handler := New(f)
	requests := []struct{ path, body string }{
		{"/v1/environment/connect", `{"id":"dev","sshHost":"host","bootstrap":[]}`},
		{"/v1/service/add", `{"environmentId":"dev","id":"svc","argv":["svc"],"cwd":"/work","port":3000}`},
		{"/v1/environment/ensure-jinushi", `{"environmentId":"dev"}`},
		{"/v1/environment/register", `{"environments":[{"id":"dev","sshHost":"host","jinushi":{"supervisorStartCommand":["start"]}}],"services":[]}`},
		{"/v1/service/start", `{"environmentId":"dev","serviceId":"svc"}`},
		{"/v1/service/stop", `{"environmentId":"dev","serviceId":"svc"}`},
		{"/v1/service/restart", `{"environmentId":"dev","serviceId":"svc"}`},
		{"/v1/endpoint/ensure", `{"environmentId":"dev","serviceId":"svc","endpointId":"ui"}`},
	}
	for _, test := range requests {
		t.Run(test.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43123"+test.path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body %s; want forbidden", w.Code, w.Body.String())
			}
		})
	}
	if len(f.mutations) != 0 {
		t.Fatalf("mutations = %v; unauthorized requests had side effects", f.mutations)
	}
}

func TestTokenlessCrossOriginSimpleRegistrationHasNoSideEffects(t *testing.T) {
	f := &admissionFake{}
	body := `{"environments":[{"id":"dev","sshHost":"host","jinushi":{"supervisorStartCommand":["start"]}}],"services":[]}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43123/v1/environment/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()

	New(f).ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body %s; want forbidden", w.Code, w.Body.String())
	}
	if len(f.mutations) != 0 {
		t.Fatalf("mutations = %v; unauthorized request mutated the registry", f.mutations)
	}
}

func TestReadOnlyStateRemainsAvailableWithoutCapability(t *testing.T) {
	f := &admissionFake{}
	w := httptest.NewRecorder()
	New(f).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:43123/v1/state", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"version":1`) {
		t.Fatalf("state response = %d %s", w.Code, w.Body.String())
	}
}

func TestMutationRejectsUnsafeHostOriginAndMediaType(t *testing.T) {
	f := &registeringFake{}
	handler, capability := newAuthorizedAPI(t, f)
	body := `{"environments":[{"id":"dev","sshHost":"host","jinushi":{"supervisorStartCommand":["start"]}}],"services":[]}`
	for _, test := range []struct {
		name   string
		status int
		mutate func(*http.Request)
	}{
		{name: "untrusted Host", status: http.StatusForbidden, mutate: func(r *http.Request) { r.Host = "attacker.example:43123" }},
		{name: "cross-origin Origin", status: http.StatusForbidden, mutate: func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }},
		{name: "simple media type", status: http.StatusUnsupportedMediaType, mutate: func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		{name: "missing media type", status: http.StatusUnsupportedMediaType, mutate: func(r *http.Request) { r.Header.Del("Content-Type") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := authorizedRequest(capability, http.MethodPost, "/v1/environment/register", body)
			test.mutate(req)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != test.status {
				t.Fatalf("status = %d, body %s; want %d", w.Code, w.Body.String(), test.status)
			}
		})
	}
	if f.registrations != 0 {
		t.Fatalf("registrations = %d; unsafe requests mutated the registry", f.registrations)
	}
}

func TestAuthorizedV1RegistrationKeepsItsExistingBodyAndRoute(t *testing.T) {
	f := &registeringFake{}
	handler, capability := newAuthorizedAPI(t, f)
	body := `{"environments":[{"id":"dev","sshHost":"host","jinushi":{"supervisorStartCommand":["start"]}}],"services":[]}`
	req := authorizedRequest(capability, http.MethodPost, "/v1/environment/register", body)
	req.Header.Set("Origin", "http://127.0.0.1:43123")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || f.registrations != 1 {
		t.Fatalf("authorized v1 registration = %d %s; registrations = %d", w.Code, w.Body.String(), f.registrations)
	}
}

func TestWrongCapabilityCannotMutate(t *testing.T) {
	f := &registeringFake{}
	handler, _ := newAuthorizedAPI(t, f)
	wrongCapability, err := NewCapability("127.0.0.1:43123")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"environments":[{"id":"dev","sshHost":"host","jinushi":{"supervisorStartCommand":["start"]}}],"services":[]}`
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authorizedRequest(wrongCapability, http.MethodPost, "/v1/environment/register", body))
	if w.Code != http.StatusForbidden || f.registrations != 0 {
		t.Fatalf("wrong capability request = %d %s; registrations = %d", w.Code, w.Body.String(), f.registrations)
	}
}

func TestCapabilityIsBoundToItsAPIListener(t *testing.T) {
	capability, err := NewCapability("127.0.0.1:43123")
	if err != nil {
		t.Fatal(err)
	}
	body := `{"environments":[{"id":"dev","sshHost":"host","jinushi":{"supervisorStartCommand":["start"]}}],"services":[]}`
	apiRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43123/v1/environment/register", strings.NewReader(body))
	apiRequest.Header.Set("Content-Type", "application/json")
	capability.AddToRequest(apiRequest)
	if apiRequest.Header.Get("Authorization") == "" {
		t.Fatal("capability was not attached to its API listener")
	}
	tunneledRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43124/", nil)
	capability.AddToRequest(tunneledRequest)
	if tunneledRequest.Header.Get("Authorization") != "" {
		t.Fatal("capability was attached to another loopback port")
	}

	f := &registeringFake{}
	apiRequest.Host = "127.0.0.1:43124"
	w := httptest.NewRecorder()
	NewWithCapability(f, capability).ServeHTTP(w, apiRequest)
	if w.Code != http.StatusForbidden || f.registrations != 0 {
		t.Fatalf("wrong listener request = %d %s; registrations = %d", w.Code, w.Body.String(), f.registrations)
	}
}

func TestNewCapabilityRequiresAnIPv4LoopbackListener(t *testing.T) {
	for _, address := range []string{"localhost:43123", "0.0.0.0:43123", "127.0.0.1", "127.0.0.1:0"} {
		if _, err := NewCapability(address); err == nil {
			t.Errorf("NewCapability(%q) succeeded", address)
		}
	}
}

func TestProgrammaticV1CallerUsesSharedCapabilityWithoutChangingPayload(t *testing.T) {
	f := &registeringFake{}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	capability, err := NewCapability(listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(NewWithCapability(f, capability))
	server.Listener = listener
	server.Start()
	defer server.Close()
	body := `{"environments":[{"id":"dev","sshHost":"host","jinushi":{"supervisorStartCommand":["start"]}}],"services":[]}`
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/environment/register", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	capability.AddToRequest(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || f.registrations != 1 || !strings.Contains(string(response), `"version":1`) {
		t.Fatalf("authorized raw v1 request = %d %s; registrations = %d", resp.StatusCode, response, f.registrations)
	}
}

func TestCapabilityCannotBeFormattedAsASecret(t *testing.T) {
	_, capability := newAuthorizedAPI(t, &fake{})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:43123/v1/service/start", nil)
	capability.AddToRequest(req)
	if strings.Contains(fmt.Sprintf("%v %#v %+v", capability, capability, capability), strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")) {
		t.Fatal("formatting capability exposed its authorization token")
	}
}
