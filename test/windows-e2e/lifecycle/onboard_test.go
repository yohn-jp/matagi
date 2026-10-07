//go:build windows

package lifecycle

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
)

func TestCleanProfileOnboardsThroughExactCandidate(t *testing.T) {
	fixtureDir := harness.RequireFixture(t)
	root := t.TempDir()
	options := harness.Options{PathPrefix: fixtureDir, UserStateDir: filepath.Join(root, "user-state"), Env: map[string]string{
		"MATAGI_E2E_SSH_LOG":   filepath.Join(root, "ssh.jsonl"),
		"MATAGI_E2E_SSH_STATE": filepath.Join(root, "state.json"),
	}}
	session := harness.StartSession(t, options)
	apiURL, uiURL := session.APIURL, session.UIURL
	postLocale(t, uiURL, "en", "/")
	first := harness.Get(t, uiURL+"/")
	if !strings.Contains(first, `<html lang="en">`) || !strings.Contains(first, "Connect a development environment") || strings.Contains(first, "<textarea") || strings.Contains(first, "registry JSON") {
		t.Fatal("rejected first-run surface")
	}
	localeStart := strings.Index(first, `id="ui-locale"`)
	if localeStart < 0 {
		t.Fatal("first-run surface omitted the English/Japanese language selector")
	}
	localeEnd := strings.Index(first[localeStart:], `</select>`)
	if localeEnd < 0 {
		t.Fatal("first-run language selector was malformed")
	}
	localeControl := first[localeStart : localeStart+localeEnd]
	if strings.Count(localeControl, `<option`) != 2 || !strings.Contains(localeControl, `value="en"`) || !strings.Contains(localeControl, `value="ja"`) {
		t.Fatalf("language selector must offer exactly en and ja: %s", localeControl)
	}
	postLocale(t, uiURL, "ja", "/")
	if body := harness.Get(t, uiURL+"/"); !strings.Contains(body, `<html lang="ja">`) {
		t.Fatal("locale selection did not render Japanese")
	}
	session.Close(t)
	session = harness.StartSession(t, options)
	apiURL, uiURL = session.APIURL, session.UIURL
	first = harness.Get(t, uiURL+"/")
	if !strings.Contains(first, `<html lang="ja">`) {
		t.Fatal("explicit locale did not survive a production-candidate restart")
	}
	token := harness.FormToken(t, uiURL)
	resp, err := http.PostForm(uiURL+"/register", url.Values{"token": {token}, "name": {"fixture-env"}, "host": {"fixture-host"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(harness.Get(t, apiURL+"/v1/state"), `"fixture-env"`) {
		t.Fatalf("onboarding failed: %d", resp.StatusCode)
	}
	workspace := harness.Get(t, uiURL+"/")
	if !strings.Contains(workspace, `<html lang="ja">`) || !strings.Contains(workspace, "開発環境") || !strings.Contains(workspace, "fixture-env") || strings.Contains(workspace, "Connect a development environment") {
		t.Fatal("workspace did not replace onboarding")
	}
	port, markReady := startRemoteHTTP(t)
	markReady()
	resp, err = http.PostForm(uiURL+"/service/add", url.Values{"token": {token}, "environmentId": {"fixture-env"}, "service": {"fixture-service"}, "command": {"fixture-service"}, "cwd": {"/fixture"}, "port": {strconv.Itoa(port)}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	registered := harness.Get(t, uiURL+"/")
	if resp.StatusCode != 200 || !strings.Contains(registered, "fixture-service") {
		t.Fatalf("service registration failed: %d", resp.StatusCode)
	}
	for _, sibling := range []string{"yokodori", "inari", "hachidori"} {
		if strings.Contains(strings.ToLower(registered), sibling) {
			t.Fatalf("generic service registration UI contains sibling-product copy %q", sibling)
		}
	}
	for _, field := range []string{`name="service"`, `name="port"`, `name="healthPath"`, `name="cwd"`, `name="command"`} {
		if !strings.Contains(registered, field) {
			t.Fatalf("generic service registration omitted field %s", field)
		}
	}
	for _, label := range []string{"サービス名", "リモート UI ポート", "準備状態の確認パス", "開発ホスト上の作業ディレクトリ", "起動コマンドと引数"} {
		if !strings.Contains(registered, label) {
			t.Fatalf("Japanese service registration omitted localized field %q", label)
		}
	}
	postActionService(t, uiURL, "fixture-service", "start")
	resp, err = http.PostForm(uiURL+"/open", url.Values{"token": {token}, "environmentId": {"fixture-env"}, "serviceId": {"fixture-service"}, "endpointId": {"ui"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	state := harness.Get(t, apiURL+"/v1/state")
	if resp.StatusCode != 200 || !strings.Contains(state, `"fixture-env"`) || !strings.Contains(state, `"fixture-host"`) || !strings.Contains(state, `"fixture-service"`) {
		t.Fatal("endpoint did not open through candidate")
	}
}

func postLocale(t *testing.T, uiURL, locale, from string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(uiURL+"/settings/locale", url.Values{"token": {harness.FormToken(t, uiURL)}, "locale": {locale}, "returnTo": {from}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != from {
		t.Fatalf("select locale %q: status=%s location=%q", locale, resp.Status, resp.Header.Get("Location"))
	}
}

func postActionService(t *testing.T, base, id, action string) {
	t.Helper()
	resp, err := http.PostForm(base+"/action", url.Values{"token": {harness.FormToken(t, base)}, "environmentId": {"fixture-env"}, "serviceId": {id}, "action": {action}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d", action, resp.StatusCode)
	}
}
