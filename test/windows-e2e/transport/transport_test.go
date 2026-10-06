//go:build windows

package transport

import (
	"github.com/yohn-jp/matagi/internal/desktop"
	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
	"os/exec"
	"strings"
	"testing"
)

func TestOpenSSHAndNavigationBoundary(t *testing.T) {
	_, url := harness.Start(t)
	path, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal("system OpenSSH unavailable: ", err)
	}
	out, err := exec.Command(path, "-G", "localhost").CombinedOutput()
	if err != nil {
		t.Fatalf("OpenSSH config resolution: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(out)), "hostname localhost") {
		t.Fatal("OpenSSH did not resolve fixture target")
	}
	policy, err := desktop.NewPolicy(url)
	if err != nil {
		t.Fatal(err)
	}
	if !policy.AllowNavigation(url+"/") || policy.AllowNavigation("https://example.com") || policy.AllowNavigation("http://127.0.0.1:1/") || policy.AllowNewWindow(url) {
		t.Fatal("navigation policy violated")
	}
}
