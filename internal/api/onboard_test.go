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
}

func (f *onboardFake) Connect(_ context.Context, id, host string, bootstrap []string) error {
	f.called++
	f.id, f.host = id, host
	return nil
}
func (f *onboardFake) AddService(s registry.Service) error {
	f.called++
	if s.ID != "yokodori" || s.Endpoints[0].RemoteAddress != "127.0.0.1" {
		return &runtime.Failure{Code: "invalid-request"}
	}
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
		{"/v1/service/add", `{"environmentId":"dev","id":"yokodori","argv":["yokodori"],"cwd":"/work","port":3000} {}`, 400},
	} {
		w := httptest.NewRecorder()
		New(f).ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, w.Code, w.Body.String())
		}
	}
	if f.called != 2 || f.id != "dev" || f.host != "alias" {
		t.Fatal(f)
	}
}
