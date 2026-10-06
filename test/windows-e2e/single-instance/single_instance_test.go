//go:build windows

package singleinstance

import (
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
)

func TestSecondLaunchActivatesExistingCandidate(t *testing.T) {
	apiURL, _ := harness.Start(t)
	harness.LaunchDuplicate(t)
	if !strings.Contains(harness.Get(t, apiURL+"/v1/state"), `"version":1`) {
		t.Fatal("first candidate stopped after duplicate launch")
	}
}
