//go:build windows

package updates

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/settings"
	"github.com/yohn-jp/matagi/internal/statefile"
	"github.com/yohn-jp/matagi/internal/update"
	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
)

func TestProductionHelperReplacesAndRestartsOnlyTheCertifiedCandidate(t *testing.T) {
	session := harness.StartSession(t, harness.Options{})
	target, err := harness.CandidatePath()
	if err != nil {
		t.Fatal(err)
	}
	root, err := config.UserStateRoot()
	if err != nil {
		t.Fatal(err)
	}

	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := update.FileSHA256(target)
	if err != nil {
		t.Fatal(err)
	}

	version, err := update.ParseVersion("99.0.0")
	if err != nil {
		t.Fatal(err)
	}
	staged := update.StagedPath(root, version)
	if err := os.MkdirAll(filepath.Dir(staged), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, original, 0700); err != nil {
		t.Fatal(err)
	}
	settingsStore, err := settings.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := settingsStore.ModifyUpdateSettings(func(s *update.Settings) error {
		s.Channel = update.Stable
		s.Installed = &update.Installed{Version: "0.1.0", SHA256: sum}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(update.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	ready := update.Ready{
		Schema:  "matagi.update-ready/1",
		Tag:     version.String(),
		Asset:   update.ExeAsset,
		SHA256:  sum,
		Size:    int64(len(original)),
		Target:  target,
		Created: time.Now().UTC(),
	}
	if err := statefile.WriteJSON(filepath.Join(update.Dir(root), "ready.json"), ready); err != nil {
		t.Fatal(err)
	}

	page := harness.Get(t, session.UIURL+"/updates")
	if !strings.Contains(page, `id="updates-install"`) || !strings.Contains(page, version.String()) || !strings.Contains(page, target) {
		t.Fatal("the exact candidate did not expose the locally verified, path-bound update")
	}

	tampered := append([]byte(nil), original...)
	tampered[len(tampered)-1] ^= 0xff
	if err := os.WriteFile(staged, tampered, 0700); err != nil {
		t.Fatal(err)
	}
	status, failurePage := postInstall(t, session.UIURL)
	if status != http.StatusOK || !strings.Contains(failurePage, "failed re-verification") {
		t.Fatalf("tampered candidate was not refused with an actionable error: status=%d", status)
	}
	if got, err := update.FileSHA256(target); err != nil || got != sum {
		t.Fatalf("tampered staged bytes changed the running executable: digest=%q err=%v", got, err)
	}
	if body := harness.Get(t, session.APIURL+"/v1/state"); !strings.Contains(body, `"version":1`) {
		t.Fatal("candidate stopped after refusing tampered staged bytes")
	}
	if result, err := update.LoadResult(root); err == nil {
		t.Fatalf("tampered staged bytes unexpectedly started replacement helper: %#v", result)
	}

	if err := os.WriteFile(staged, original, 0700); err != nil {
		t.Fatal(err)
	}
	oldPID := session.PID()
	status, _ = postInstall(t, session.UIURL)
	if status != http.StatusSeeOther {
		t.Fatalf("verified candidate install action returned HTTP status %d", status)
	}
	session.WaitForExit(t, 15*time.Second)

	restarted := harness.WaitForRestart(t, oldPID, target)
	t.Cleanup(func() { restarted.Close(t) })
	if restarted.PID == oldPID {
		t.Fatalf("update restarted the same process ID %d", oldPID)
	}
	if got, err := update.FileSHA256(target); err != nil || got != sum {
		t.Fatalf("replacement bytes differ from the sole certified candidate: digest=%q err=%v", got, err)
	}
	old := target + ".old"
	if got, err := update.FileSHA256(old); err != nil || got != sum {
		t.Fatalf("recoverable previous executable is missing or changed: digest=%q err=%v", got, err)
	}
	result, err := update.LoadResult(root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != update.OutcomeApplied || result.Tag != version.String() || result.SHA256 != sum || result.Target != target {
		t.Fatalf("helper result did not bind the applied candidate: %#v", result)
	}
	page = harness.Get(t, restarted.UIURL+"/updates")
	if !strings.Contains(page, `id="updates-result"`) || !strings.Contains(page, version.String()) || !strings.Contains(page, "UPDATED") {
		t.Fatal("restarted candidate did not present the persisted applied result")
	}
}

func postInstall(t *testing.T, uiURL string) (int, string) {
	t.Helper()
	form := url.Values{"token": {harness.FormToken(t, uiURL)}}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(uiURL+"/updates/install", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}
