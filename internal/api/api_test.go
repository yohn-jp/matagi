package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/runtime"
)

type fake struct {
	action       string
	env, svc, ep string
}

func (f *fake) Snapshot() runtime.State {
	return runtime.State{Version: 1, Environments: []runtime.Environment{}}
}
func (f *fake) Start(_ context.Context, e, s string) (runtime.Service, error) {
	f.action = "start"
	f.env = e
	f.svc = s
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
func TestContract(t *testing.T) {
	f := &fake{}
	for _, path := range []string{"start", "stop", "restart"} {
		w := httptest.NewRecorder()
		New(f).ServeHTTP(w, httptest.NewRequest("POST", "/v1/service/"+path, strings.NewReader(`{"environmentId":"e","serviceId":"s"}`)))
		if w.Code != 200 || f.action != path || f.env != "e" || f.svc != "s" || !strings.Contains(w.Body.String(), `"version":1,"service"`) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	New(f).ServeHTTP(w, httptest.NewRequest("POST", "/v1/endpoint/ensure", strings.NewReader(`{"environmentId":"e","serviceId":"s","endpointId":"ui"}`)))
	if w.Code != 200 || f.ep != "ui" || !strings.Contains(w.Body.String(), `"endpoint":{"id":"ui"`) {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	New(f).ServeHTTP(w, httptest.NewRequest("GET", "/v1/state", nil))
	var state runtime.State
	if json.Unmarshal(w.Body.Bytes(), &state) != nil || state.Version != 1 {
		t.Fatal(w.Body.String())
	}
}
func TestInvalidRequests(t *testing.T) {
	for _, body := range []string{`{}`, `{"environmentId":"e","serviceId":"s","endpointId":"ui"}`, `{"environmentId":"e","serviceId":"s","extra":1}`, `{"environmentId":"e","serviceId":"s"} {}`} {
		w := httptest.NewRecorder()
		New(&fake{}).ServeHTTP(w, httptest.NewRequest("POST", "/v1/service/start", strings.NewReader(body)))
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid-request"`) {
			t.Fatal(w.Code, w.Body.String())
		}
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
