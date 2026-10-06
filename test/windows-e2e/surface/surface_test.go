//go:build windows

package surface

import (
	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
	"net/http"
	"strings"
	"testing"
)

func TestLocalUIAndFailurePresentation(t *testing.T) {
	_, url := harness.Start(t)
	body := harness.Get(t, url+"/")
	if !strings.Contains(body, "Matagi") {
		t.Fatal("missing Matagi UI")
	}
	resp, err := http.PostForm(url+"/action", map[string][]string{"action": {"start"}, "environmentId": {"not-registered"}, "serviceId": {"not-registered"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("unregistered lifecycle operation reported success")
	}
}
