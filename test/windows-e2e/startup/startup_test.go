//go:build windows

package startup

import (
	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
	"strings"
	"testing"
)

func TestCandidateStartupAndShutdown(t *testing.T) {
	api, ui := harness.Start(t)
	if api == ui || !strings.HasPrefix(api, "http://127.0.0.1:") || !strings.HasPrefix(ui, "http://127.0.0.1:") {
		t.Fatalf("non-loopback or shared address: %q %q", api, ui)
	}
	if !strings.Contains(harness.Get(t, api+"/v1/state"), `"version":1`) {
		t.Fatal("API did not serve v1 state")
	}
}
