//go:build windows

package surface

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/test/windows-e2e/harness"
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

func TestLocaleAndUpdateSurfaceOnProductionCandidate(t *testing.T) {
	_, uiURL := harness.Start(t)
	postLocale(t, uiURL, "en", "/")
	english := harness.Get(t, uiURL+"/")
	if !strings.Contains(english, `<html lang="en">`) {
		t.Fatal("candidate did not render the English locale selector")
	}
	assertLocaleForm(t, english, "/")

	updates := harness.Get(t, uiURL+"/updates")
	if !strings.Contains(updates, `<html lang="en">`) {
		t.Fatal("Updates page did not preserve the explicit English locale")
	}
	assertLocaleForm(t, updates, "/updates")
	for _, selector := range []string{`id="updates-region"`, `id="updates-installed-version"`, `id="updates-last-check"`, `id="updates-releases-section"`, `id="updates-op"`, `id="updates-ready"`, `id="updates-check"`} {
		if !strings.Contains(updates, selector) {
			t.Fatalf("candidate Updates surface omitted %s", selector)
		}
	}
	lastCheckMarker := `<dd id="updates-last-check">`
	lastCheckStart := strings.Index(updates, lastCheckMarker)
	if lastCheckStart < 0 {
		t.Fatal("candidate Updates surface omitted the last-check value")
	}
	lastCheckText := updates[lastCheckStart+len(lastCheckMarker):]
	lastCheckEnd := strings.Index(lastCheckText, `</dd>`)
	if lastCheckEnd < 0 || strings.TrimSpace(lastCheckText[:lastCheckEnd]) != "never" {
		t.Fatal("opening the Updates surface performed an implicit release check")
	}
	if !strings.Contains(updates, `action="/updates/check"`) || !strings.Contains(updates, `method="post"`) {
		t.Fatal("update metadata check is not an explicit POST action")
	}

	postLocale(t, uiURL, "ja", "/updates")
	japanese := harness.Get(t, uiURL+"/updates")
	if !strings.Contains(japanese, `<html lang="ja">`) || !hasJapaneseText(japanese) {
		t.Fatal("candidate Updates surface did not render Japanese operator copy")
	}
	assertLocaleForm(t, japanese, "/updates")
	if !strings.Contains(japanese, `id="updates-check"`) || !strings.Contains(japanese, `action="/updates/check"`) {
		t.Fatal("Japanese Updates surface lost its explicit check control")
	}
}

func assertLocaleForm(t *testing.T, page, returnTo string) {
	t.Helper()
	const selectMarker = `<select id="ui-locale" name="locale">`
	selectStart := strings.Index(page, selectMarker)
	if selectStart < 0 {
		t.Fatal("page omitted the language selector")
	}
	formStart := strings.LastIndex(page[:selectStart], `<form`)
	formEnd := strings.Index(page[selectStart:], `</form>`)
	if formStart < 0 || formEnd < 0 {
		t.Fatal("language selector is not inside a complete form")
	}
	formEnd += selectStart + len(`</form>`)
	form := page[formStart:formEnd]
	if !strings.Contains(form, `method="post" action="/settings/locale"`) ||
		!strings.Contains(form, `name="token"`) ||
		!strings.Contains(form, `name="returnTo" value="`+returnTo+`"`) ||
		!strings.Contains(form, `<button`) {
		t.Fatalf("language form does not preserve its destination and token: %s", form)
	}
	selectEnd := strings.Index(page[selectStart:], `</select>`)
	if selectEnd < 0 {
		t.Fatal("language selector is malformed")
	}
	control := page[selectStart : selectStart+selectEnd]
	if strings.Count(control, `<option`) != 2 ||
		!strings.Contains(control, `<option value="en"`) ||
		!strings.Contains(control, `>English</option>`) ||
		!strings.Contains(control, `<option value="ja"`) ||
		!strings.Contains(control, `>日本語</option>`) {
		t.Fatalf("language selector must offer exactly English and Japanese: %s", control)
	}
}

func postLocale(t *testing.T, uiURL, locale, returnTo string) {
	t.Helper()
	form := url.Values{
		"token":    {harness.FormToken(t, uiURL)},
		"locale":   {locale},
		"returnTo": {returnTo},
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(uiURL+"/settings/locale", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != returnTo {
		t.Fatalf("select locale %q: status=%s location=%q", locale, resp.Status, resp.Header.Get("Location"))
	}
}

func hasJapaneseText(s string) bool {
	for _, r := range s {
		if (r >= '\u3040' && r <= '\u30ff') || (r >= '\u3400' && r <= '\u9fff') {
			return true
		}
	}
	return false
}
