package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/internal/runtime"
)

type onboardFake struct {
	fake
	called   int
	id, host string
	services []registry.Service
}

func (f *onboardFake) Connect(_ context.Context, id, host string, bootstrap []string) error {
	f.called++
	f.id, f.host = id, host
	return nil
}
func (f *onboardFake) AddService(s registry.Service) error {
	f.called++
	if s.ID == "" || s.Endpoints[0].RemoteAddress != "127.0.0.1" {
		return &runtime.Failure{Code: "invalid-request"}
	}
	f.services = append(f.services, s)
	return nil
}
func TestTypedOnboardingAndServiceBoundary(t *testing.T) {
	f := &onboardFake{}
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/v1/environment/connect", `{"id":"dev","sshHost":"alias","bootstrap":[]}`, 200},
		{"/v1/environment/connect", `{"id":"dev","sshHost":"alias","rogue":true}`, 400},
		{"/v1/service/add", `{"environmentId":"dev","id":"yokodori","argv":["yokodori"],"cwd":"/work","port":3000}`, 200},
		{"/v1/service/add", `{"environmentId":"dev","id":"ephemeral","argv":["service"],"cwd":"/work","resolutionPath":"/home/dev/.cache/service/endpoint.json"}`, 200},
		{"/v1/service/add", `{"environmentId":"dev","id":"missing-endpoint","argv":["service"],"cwd":"/work"}`, 400},
		{"/v1/service/add", `{"environmentId":"dev","id":"yokodori","argv":["yokodori"],"cwd":"/work","port":3000} {}`, 400},
	} {
		w := httptest.NewRecorder()
		New(f).ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, w.Code, w.Body.String())
		}
	}
	if f.called != 3 || f.id != "dev" || f.host != "alias" || len(f.services) != 2 {
		t.Fatal(f)
	}
	static := f.services[0].Endpoints[0]
	dynamic := f.services[1].Endpoints[0]
	if static.RemotePort != 3000 || static.Resolution != nil {
		t.Fatalf("static service endpoint = %#v", static)
	}
	if dynamic.RemotePort != 0 || dynamic.Resolution == nil || dynamic.Resolution.Type != registry.EndpointResolutionJSONURLFile || dynamic.Resolution.Path != "/home/dev/.cache/service/endpoint.json" {
		t.Fatalf("dynamic service endpoint = %#v", dynamic)
	}
}
