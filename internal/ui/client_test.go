package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/api"
	"github.com/yohn-jp/matagi/internal/runtime"
)

type compatibilityRuntime struct{ starts int }

func (*compatibilityRuntime) Snapshot() runtime.State {
	return runtime.State{Version: 1, Environments: []runtime.Environment{}}
}
func (r *compatibilityRuntime) Start(context.Context, string, string) (runtime.Service, error) {
	r.starts++
	return runtime.Service{}, nil
}
func (*compatibilityRuntime) Stop(context.Context, string, string) (runtime.Service, error) {
	return runtime.Service{}, nil
}
func (*compatibilityRuntime) Restart(context.Context, string, string) (runtime.Service, error) {
	return runtime.Service{}, nil
}
func (*compatibilityRuntime) Ensure(context.Context, string, string, string) (runtime.Endpoint, error) {
	return runtime.Endpoint{}, nil
}

func TestClientImplementsFrozenV1RoutesAndBodies(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/state":
			if r.Header.Get("Accept") != "application/json" {
				t.Errorf("Accept = %q, want application/json", r.Header.Get("Accept"))
			}
			fmt.Fprint(w, "{\"version\":1,\"environments\":[{\"id\":\"dev\",\"connectivity\":\"connected\",\"error\":\"\",\"services\":[{\"id\":\"svc\",\"desiredState\":\"running\",\"state\":\"ready\",\"process\":\"running\",\"readiness\":\"ready\",\"processError\":\"\",\"readinessError\":\"\",\"endpoints\":[{\"id\":\"dash\",\"label\":\"Dashboard\",\"endpointState\":\"available\",\"tunnelState\":\"ready\",\"localUrl\":\"http://127.0.0.1:43123/\",\"failure\":\"\"}]}]}]}")
		case "POST /v1/service/start", "POST /v1/service/stop", "POST /v1/service/restart":
			var got ServiceRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode service request: %v", err)
			}
			if got != (ServiceRequest{EnvironmentID: "dev", ServiceID: "svc"}) {
				t.Errorf("service body = %#v", got)
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
			}
			fmt.Fprint(w, "{\"version\":1,\"service\":{}}")
		case "POST /v1/endpoint/ensure":
			var got EndpointRequest
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode endpoint request: %v", err)
			}
			if got != (EndpointRequest{EnvironmentID: "dev", ServiceID: "svc", EndpointID: "dash"}) {
				t.Errorf("endpoint body = %#v", got)
			}
			fmt.Fprint(w, "{\"version\":1,\"endpoint\":{\"id\":\"dash\",\"label\":\"Dashboard\",\"endpointState\":\"available\",\"tunnelState\":\"ready\",\"localUrl\":\"http://127.0.0.1:43123/\",\"failure\":\"\"}}")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	state, err := client.GetState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 || len(state.Environments) != 1 || state.Environments[0].Services[0].Endpoints[0].LocalURL != "http://127.0.0.1:43123/" {
		t.Fatalf("state decode = %#v", state)
	}
	serviceRequest := ServiceRequest{EnvironmentID: "dev", ServiceID: "svc"}
	if _, err := client.Start(ctx, serviceRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stop(ctx, serviceRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Restart(ctx, serviceRequest); err != nil {
		t.Fatal(err)
	}
	endpointResult, err := client.EnsureEndpoint(ctx, EndpointRequest{EnvironmentID: "dev", ServiceID: "svc", EndpointID: "dash"})
	if err != nil {
		t.Fatal(err)
	}
	if endpointResult.Endpoint.ID != "dash" || endpointResult.Endpoint.LocalURL != "http://127.0.0.1:43123/" {
		t.Fatalf("endpoint decode = %#v", endpointResult)
	}
	mu.Lock()
	gotPaths := append([]string(nil), paths...)
	mu.Unlock()
	wantPaths := []string{
		"GET /v1/state",
		"POST /v1/service/start",
		"POST /v1/service/stop",
		"POST /v1/service/restart",
		"POST /v1/endpoint/ensure",
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("requests = %#v, want %#v", gotPaths, wantPaths)
	}
}

func TestClientDecodesBoundedV1Errors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, "{\"version\":1,\"error\":{\"code\":\"transport_failure\",\"message\":%q}}", strings.Repeat("x", 700))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetState(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want *APIError", err, err)
	}
	if apiErr.Code != "transport_failure" || apiErr.StatusCode != http.StatusBadGateway || len([]rune(apiErr.Message)) != maxErrorMessage {
		t.Fatalf("API error = %#v", apiErr)
	}
}

func TestClientTimeoutAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	t.Run("client timeout", func(t *testing.T) {
		client, err := NewClient(server.URL, 40*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.GetState(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context deadline exceeded", err)
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		client, err := NewClient(server.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
		_, err = client.GetState(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	})
}

func TestClientDoesNotReplayMutationAfterRedirect(t *testing.T) {
	var original, redirected int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/service/start":
			original++
			http.Redirect(w, r, "/replayed", http.StatusTemporaryRedirect)
		case "/replayed":
			redirected++
			fmt.Fprint(w, "{\"version\":1,\"service\":{}}")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Start(context.Background(), ServiceRequest{EnvironmentID: "dev", ServiceID: "svc"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("error = %v, want redirect API error", err)
	}
	if original != 1 || redirected != 0 {
		t.Fatalf("request counts: original=%d redirected=%d", original, redirected)
	}
}

func TestAuthorizedClientKeepsCapabilityOnLoopbackMutationOnly(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	capability, err := api.NewCapability(listener.Addr().String())
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	var mutationAuthorization string
	var readAuthorization string
	var tunneledRequests int
	tunneled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tunneledRequests++
		fmt.Fprint(w, "product UI")
	}))
	defer tunneled.Close()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/state":
			readAuthorization = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"version":1,"environments":[]}`)
		case "POST /v1/service/start":
			mutationAuthorization = r.Header.Get("Authorization")
			http.Redirect(w, r, tunneled.URL+"/", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, r)
		}
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	client, err := NewClientWithCapability(server.URL, time.Second, capability)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetState(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = client.Start(context.Background(), ServiceRequest{EnvironmentID: "dev", ServiceID: "svc"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("start error = %v; want redirect API error", err)
	}
	if readAuthorization != "" {
		t.Fatalf("read-only state request carried Authorization %q", readAuthorization)
	}
	if !strings.HasPrefix(mutationAuthorization, "Bearer ") {
		t.Fatalf("mutation Authorization = %q; want capability bearer", mutationAuthorization)
	}
	if tunneledRequests != 0 {
		t.Fatalf("tunneled product origin received %d redirected requests", tunneledRequests)
	}
}

func TestLegacyClientRetainsReadOnlyV1AndFailsMutationsClosed(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rt := &compatibilityRuntime{}
	server := httptest.NewUnstartedServer(api.New(rt))
	server.Listener = listener
	server.Start()
	defer server.Close()
	client, err := NewClient(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	state, err := client.GetState(context.Background())
	if err != nil || state.Version != apiVersion {
		t.Fatalf("legacy v1 state = %#v, %v", state, err)
	}
	_, err = client.Start(context.Background(), ServiceRequest{EnvironmentID: "dev", ServiceID: "svc"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "caller-not-authorized" {
		t.Fatalf("tokenless legacy mutation = %v; want caller-not-authorized", err)
	}
	if rt.starts != 0 {
		t.Fatalf("legacy mutation calls = %d; want zero", rt.starts)
	}
}

func TestNewClientRequiresLoopbackHTTPOrigin(t *testing.T) {
	for _, address := range []string{
		"https://127.0.0.1:1234",
		"http://localhost:1234",
		"http://127.0.0.1",
		"http://127.0.0.1:1234/api",
		"http://user@127.0.0.1:1234",
	} {
		if _, err := NewClient(address, time.Second); err == nil {
			t.Errorf("NewClient(%q) succeeded", address)
		}
	}
}
