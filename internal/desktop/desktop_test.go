package desktop

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/ui"
)

func TestPolicyAllowsOnlyUIAndEnsuredLoopbackOrigins(t *testing.T) {
	policy, err := NewPolicy("http://127.0.0.1:43100/")
	if err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		"http://127.0.0.1:43100/",
		"http://127.0.0.1:43100/settings",
	} {
		if !policy.AllowNavigation(uri) {
			t.Errorf("AllowNavigation(%q) = false", uri)
		}
	}
	for _, uri := range []string{
		"http://127.0.0.1:43101/",
		"http://localhost:43100/",
		"http://[::1]:43100/",
		"https://127.0.0.1:43100/",
		"https://example.com/",
		"file:///tmp/page.html",
		"javascript:alert(1)",
		"http://user:pass@127.0.0.1:43100/",
	} {
		if policy.AllowNavigation(uri) {
			t.Errorf("AllowNavigation(%q) = true before endpoint ensure", uri)
		}
	}

	const ensuredURL = "http://127.0.0.1:43123/dashboard/"
	if err := policy.AdmitEnsuredEndpoint(ensuredURL); err != nil {
		t.Fatal(err)
	}
	for _, uri := range []string{
		ensuredURL,
		"http://127.0.0.1:43123/other/path",
	} {
		if !policy.AllowNavigation(uri) {
			t.Errorf("AllowNavigation(%q) = false after endpoint ensure", uri)
		}
	}
	for _, uri := range []string{
		"http://127.0.0.1:43124/",
		"http://localhost:43123/",
		"https://127.0.0.1:43123/",
	} {
		if policy.AllowNavigation(uri) {
			t.Errorf("AllowNavigation(%q) = true after endpoint ensure", uri)
		}
	}
	if policy.AllowNewWindow(ensuredURL) {
		t.Fatal("AllowNewWindow allowed a popup")
	}
}

func TestPolicyRejectsNonLoopbackInitialUIURL(t *testing.T) {
	for _, uri := range []string{
		"https://127.0.0.1:43100/",
		"http://localhost:43100/",
		"http://127.0.0.1/",
		"http://0.0.0.0:43100/",
	} {
		if _, err := NewPolicy(uri); err == nil {
			t.Errorf("NewPolicy(%q) succeeded", uri)
		}
	}
}

type fakePlatform struct {
	opened int
	window Window
	err    error
}

func (*fakePlatform) RuntimeVersion() (string, error)  { return "test", nil }
func (*fakePlatform) AcquireInstance() (func(), error) { return func() {}, nil }
func (*fakePlatform) Activate() error                  { return nil }

func (p *fakePlatform) Open(_ context.Context, w Window) error {
	p.opened++
	p.window = w
	return p.err
}

func (*fakePlatform) ReportError(string, string) {}

func TestOpenRequiresPermittedInitialURL(t *testing.T) {
	policy, err := NewPolicy("http://127.0.0.1:43100/")
	if err != nil {
		t.Fatal(err)
	}
	platform := &fakePlatform{}
	if err := Open(context.Background(), platform, Window{URL: "http://127.0.0.1:43101/", Policy: policy}); err == nil {
		t.Fatal("Open accepted an unpermitted origin")
	}
	if platform.opened != 0 {
		t.Fatalf("platform opened %d times", platform.opened)
	}
	if err := Open(context.Background(), platform, Window{URL: "http://127.0.0.1:43100/", Policy: policy}); err != nil {
		t.Fatal(err)
	}
	if platform.opened != 1 || platform.window.URL != "http://127.0.0.1:43100/" {
		t.Fatalf("platform state = %#v", platform)
	}
}

func TestOpenReturnsPlatformFailure(t *testing.T) {
	policy, err := NewPolicy("http://127.0.0.1:43100/")
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("WebView2 startup failed")
	platform := &fakePlatform{err: want}
	err = Open(context.Background(), platform, Window{URL: "http://127.0.0.1:43100/", Policy: policy})
	if !errors.Is(err, want) {
		t.Fatalf("Open error = %v", err)
	}
}

func TestEnsureResponseIsTheOnlySourceOfAdmittedEndpointNavigation(t *testing.T) {
	const endpointURL = "http://127.0.0.1:43123/registered-ui/"
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/endpoint/ensure" {
			t.Errorf("API request = %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprintf(w, "{\"version\":1,\"endpoint\":{\"id\":\"dash\",\"label\":\"Dashboard\",\"endpointState\":\"available\",\"tunnelState\":\"ready\",\"localUrl\":%q,\"failure\":\"\"}}", endpointURL)
	}))
	defer apiServer.Close()
	client, err := ui.NewClient(apiServer.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewPolicy("http://127.0.0.1:43100/")
	if err != nil {
		t.Fatal(err)
	}
	if policy.AllowNavigation(endpointURL) {
		t.Fatal("endpoint was admitted before ensure")
	}
	handler := ui.NewHandler(client, policy)
	form := url.Values{
		"environmentId": {"dev"},
		"serviceId":     {"svc"},
		"endpointId":    {"dash"},
	}
	request := httptest.NewRequest(http.MethodPost, "/open", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != endpointURL {
		t.Fatalf("status = %d, location = %q, body = %s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	if !policy.AllowNavigation(endpointURL) || !policy.AllowNavigation("http://127.0.0.1:43123/another-path") {
		t.Fatal("successful ensure did not admit its endpoint origin")
	}
	if policy.AllowNavigation("http://127.0.0.1:43124/") {
		t.Fatal("ensure admitted an unrelated loopback origin")
	}
}

func TestPreflightChecksRuntimeBeforeInstance(t *testing.T) {
	p := &preflightPlatform{}
	_, release, err := Preflight(p)
	if !errors.Is(err, ErrWebView2Missing) || release != nil || p.acquired {
		t.Fatalf("missing runtime: release=%v err=%v acquired=%v", release, err, p.acquired)
	}

	p.version = "1.0"
	version, release, err := Preflight(p)
	if err != nil || version != "1.0" || release == nil || !p.acquired {
		t.Fatalf("preflight: version=%q release=%v err=%v acquired=%v", version, release, err, p.acquired)
	}
	release()
}

type preflightPlatform struct {
	version  string
	acquired bool
}

func (p *preflightPlatform) RuntimeVersion() (string, error) { return p.version, nil }
func (p *preflightPlatform) AcquireInstance() (func(), error) {
	p.acquired = true
	return func() {}, nil
}
func (*preflightPlatform) Activate() error                    { return nil }
func (*preflightPlatform) Open(context.Context, Window) error { return nil }
func (*preflightPlatform) ReportError(string, string)         {}

func TestInstanceNameIsBoundedAndUserSpecific(t *testing.T) {
	a := InstanceName("S-1-5-21-a")
	b := InstanceName("S-1-5-21-b")
	if a == b || !strings.HasPrefix(a, instancePrefix) || strings.Contains(a, "S-1-5-21-a") {
		t.Fatalf("instance names not opaque/user-specific: %q %q", a, b)
	}
}

func TestOwnedConsolePolicy(t *testing.T) {
	if !shouldHideOwnedConsole(10, []uint32{10}) {
		t.Fatal("sole owned console should be hidden")
	}
	for _, attached := range [][]uint32{nil, {10, 20}, {20}} {
		if shouldHideOwnedConsole(10, attached) {
			t.Fatalf("shared/unowned console would be hidden: %v", attached)
		}
	}
}
