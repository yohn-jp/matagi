//go:build windows

package lifecycle

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
)

func TestCleanProfileOnboardsThroughExactCandidate(t *testing.T) {
	fixtureDir := os.Getenv("MATAGI_E2E_SSH_FIXTURE_DIR")
	if fixtureDir == "" {
		t.Skip("requires deterministic SSH fixture")
	}
	root := t.TempDir()
	apiURL, uiURL := harness.StartConfigured(t, harness.Options{PathPrefix: fixtureDir, Env: map[string]string{
		"MATAGI_E2E_SSH_LOG":   filepath.Join(root, "ssh.jsonl"),
		"MATAGI_E2E_SSH_STATE": filepath.Join(root, "state.json"),
	}})
	first := harness.Get(t, uiURL+"/")
	if !strings.Contains(first, "Connect a development environment") || strings.Contains(first, "<textarea") || strings.Contains(first, "registry JSON") {
		t.Fatal("rejected first-run surface")
	}
	token := formToken(t, uiURL)
	resp, err := http.PostForm(uiURL+"/register", url.Values{"token": {token}, "name": {"fixture-env"}, "host": {"fixture-host"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(harness.Get(t, apiURL+"/v1/state"), `"fixture-env"`) {
		t.Fatalf("onboarding failed: %d", resp.StatusCode)
	}
	workspace := harness.Get(t, uiURL+"/")
	if !strings.Contains(workspace, "Connection · SSH") || !strings.Contains(workspace, "Jinushi · supervisor") || strings.Contains(workspace, "Connect a development environment") {
		t.Fatal("workspace did not replace onboarding")
	}
	port := startRemoteHTTP(t)
	resp, err = http.PostForm(uiURL+"/service/add", url.Values{"token": {token}, "environmentId": {"fixture-env"}, "service": {"yokodori"}, "command": {"fixture-service"}, "cwd": {"/fixture"}, "port": {strconv.Itoa(port)}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(harness.Get(t, uiURL+"/"), "yokodori") {
		t.Fatalf("service registration failed: %d", resp.StatusCode)
	}
	postActionService(t, uiURL, "yokodori", "start")
	resp, err = http.PostForm(uiURL+"/open", url.Values{"token": {token}, "environmentId": {"fixture-env"}, "serviceId": {"yokodori"}, "endpointId": {"ui"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(harness.Get(t, apiURL+"/v1/state"), "yokodori") {
		t.Fatal("endpoint did not open through candidate")
	}
}

func postActionService(t *testing.T, base, id, action string) {
	t.Helper()
	resp, err := http.PostForm(base+"/action", url.Values{"token": {formToken(t, base)}, "environmentId": {"fixture-env"}, "serviceId": {id}, "action": {action}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d", action, resp.StatusCode)
	}
}
