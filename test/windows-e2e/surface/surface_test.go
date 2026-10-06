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
	if !strings.Contains(body, "Matagi") || !strings.Contains(body, "Connect a development environment") ||
		strings.Contains(body, "<textarea") || strings.Contains(body, "registry JSON") ||
		!strings.Contains(body, "prefers-color-scheme: light") {
		t.Fatal("candidate did not present the canonical first-run surface")
	}
	const marker = `name="token" value="`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatal("candidate page did not contain form token")
	}
	start += len(marker)
	end := strings.IndexByte(body[start:], '"')
	if end < 0 {
		t.Fatal("candidate page contained malformed form token")
	}
	token := body[start : start+end]
	resp, err := http.PostForm(url+"/action", map[string][]string{"token": {token}, "action": {"start"}, "environmentId": {"not-registered"}, "serviceId": {"not-registered"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("unregistered lifecycle operation reported success")
	}
}
