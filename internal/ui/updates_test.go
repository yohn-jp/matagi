package ui

import (
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/settings"
	"github.com/yohn-jp/matagi/internal/update"
)

type fakeUIUpdates struct {
	status      update.Status
	channels    []update.Channel
	checks      int
	downloads   []string
	installs    int
	setErr      error
	checkErr    error
	downloadErr error
	installErr  error
}

func (f *fakeUIUpdates) Status() update.Status { return f.status }
func (f *fakeUIUpdates) SetChannel(channel update.Channel) error {
	f.channels = append(f.channels, channel)
	if f.setErr == nil {
		f.status.Channel = channel
		f.status.Check = nil
	}
	return f.setErr
}
func (f *fakeUIUpdates) Check() error {
	f.checks++
	return f.checkErr
}
func (f *fakeUIUpdates) Download(tag string) error {
	f.downloads = append(f.downloads, tag)
	return f.downloadErr
}
func (f *fakeUIUpdates) Install() error {
	f.installs++
	return f.installErr
}

func newUpdatesTestHandler(t *testing.T, updates Updates, locale i18n.Locale) (*handler, *settings.Store) {
	t.Helper()
	store, err := settings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetLocale(string(locale)); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithOptions(nil, nil, Options{Settings: store, Updates: updates}).(*handler)
	return h, store
}

func testCandidate(t *testing.T, tag string, relation string) update.Candidate {
	t.Helper()
	version, err := update.ParseVersion(tag)
	if err != nil {
		t.Fatal(err)
	}
	return update.Candidate{Release: update.Release{
		Tag:       tag,
		Version:   version,
		Published: time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC),
	}, Relation: relation}
}

func baseUpdatesStatus(t *testing.T) update.Status {
	t.Helper()
	now := time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)
	return update.Status{
		Supported: true,
		Channel:   update.Stable,
		Installed: update.InstalledView{Version: "0.2.5", Known: true, SHA256: strings.Repeat("a", 64)},
		LastCheck: &update.LastCheck{Time: now, Channel: update.Stable, OK: true, Latest: "0.2.6"},
		Ready:     &update.Ready{Tag: "0.2.6", SHA256: strings.Repeat("b", 64), Target: `C:\Matagi\matagi.exe`, Created: now},
		Check: &update.CheckResult{
			Time:      now,
			Channel:   update.Stable,
			Releases:  []update.Candidate{testCandidate(t, "0.2.6", update.RelNewer)},
			Installed: update.InstalledView{Version: "0.2.5", Known: true},
		},
	}
}

func TestUpdatesPageRendersLocalStateInEnglishAndJapanese(t *testing.T) {
	f := &fakeUIUpdates{status: baseUpdatesStatus(t)}
	h, store := newUpdatesTestHandler(t, f, i18n.English)
	for _, locale := range []i18n.Locale{i18n.English, i18n.Japanese} {
		if err := store.SetLocale(string(locale)); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/updates", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET /updates (%s) status = %d: %s", locale, response.Code, response.Body.String())
		}
		body := response.Body.String()
		for _, id := range []string{
			"Updates",
			"Official Matagi GitHub Releases",
			"Installed version",
			"Check for updates",
			"Ready to install",
		} {
			want := html.EscapeString(locale.T(id))
			if !strings.Contains(body, want) {
				t.Errorf("%s page lacks translated %q (%q)", locale, id, want)
			}
		}
		for _, literal := range []string{"0.2.5", "0.2.6", "SHA-256", "stable"} {
			if !strings.Contains(body, literal) {
				t.Errorf("%s page lacks literal update data %q", locale, literal)
			}
		}
		for _, forbidden := range []string{"Hachidori", "Yokodori", "Inari"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s update UI contains sibling product text %q", locale, forbidden)
			}
		}
		if !strings.Contains(body, `href="/settings"`) || !strings.Contains(body, html.EscapeString(locale.T("Settings"))) {
			t.Errorf("%s updates page does not use the shared Settings navigation", locale)
		}
		for _, want := range []string{`id="ui-locale-form"`, `action="/settings/locale"`, `name="returnTo" value="/updates"`, `id="ui-locale"`, `<option value="en"`, `<option value="ja"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s nil-Presenter Updates page lacks legacy language control %q", locale, want)
			}
		}
		if !strings.Contains(body, `action="/updates/check"`) || !strings.Contains(body, `action="/updates/download"`) || !strings.Contains(body, `id="updates-install"`) {
			t.Errorf("%s page lacks explicit update actions", locale)
		}
	}
	if f.checks != 0 || len(f.downloads) != 0 || f.installs != 0 || len(f.channels) != 0 {
		t.Fatalf("rendering performed an update action: checks=%d downloads=%v installs=%d channels=%v", f.checks, f.downloads, f.installs, f.channels)
	}
}

func TestUpdatesPageLocalizesChannelLabelsAndRetainsChannelEnums(t *testing.T) {
	for _, item := range []struct {
		channel update.Channel
		label   string
	}{
		{channel: update.Stable, label: "Stable"},
		{channel: update.Development, label: "Development"},
	} {
		status := baseUpdatesStatus(t)
		status.Channel = item.channel
		status.LastCheck.Channel = item.channel
		status.Check.Channel = item.channel
		f := &fakeUIUpdates{status: status}
		h, store := newUpdatesTestHandler(t, f, i18n.English)
		for _, locale := range []i18n.Locale{i18n.English, i18n.Japanese} {
			if err := store.SetLocale(string(locale)); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/updates", nil))
			body := response.Body.String()
			lastStart := strings.Index(body, `id="updates-last-check">`)
			if lastStart < 0 {
				t.Fatalf("%s page lacks the last-check channel", locale)
			}
			lastEnd := strings.Index(body[lastStart:], "</dd>")
			if lastEnd < 0 {
				t.Fatalf("%s page lacks the last-check channel", locale)
			}
			lastCheck := body[lastStart : lastStart+lastEnd]
			if !strings.Contains(lastCheck, locale.T(item.label)) || strings.Contains(lastCheck, "· "+string(item.channel)+" ·") {
				t.Errorf("%s last-check channel is not localized: %s", locale, lastCheck)
			}
			resultsStart := strings.Index(body, `id="updates-results-note">`)
			if resultsStart < 0 {
				t.Fatalf("%s page lacks the check-result channel", locale)
			}
			resultsEnd := strings.Index(body[resultsStart:], "</p>")
			if resultsEnd < 0 {
				t.Fatalf("%s page lacks the check-result channel", locale)
			}
			resultsNote := body[resultsStart : resultsStart+resultsEnd]
			if !strings.Contains(resultsNote, locale.T(item.label)) || strings.Contains(resultsNote, locale.T("channel")+" "+string(item.channel)) {
				t.Errorf("%s result channel is not localized: %s", locale, resultsNote)
			}
			if !strings.Contains(body, `value="`+string(item.channel)+`" selected`) {
				t.Errorf("%s page changed the updater's channel enum %q", locale, item.channel)
			}
		}
	}
}

func TestUpdatesPageExplainsReleaseAssetProblemsInBothLocales(t *testing.T) {
	status := baseUpdatesStatus(t)
	status.Check.Releases[0].Problem = "two Windows executable assets were found"
	h, store := newUpdatesTestHandler(t, &fakeUIUpdates{status: status}, i18n.English)
	for _, locale := range []i18n.Locale{i18n.English, i18n.Japanese} {
		if err := store.SetLocale(string(locale)); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/updates", nil))
		body := response.Body.String()
		hint := update.Hints[update.ClassAsset]
		if !strings.Contains(body, locale.T(hint)) || !strings.Contains(body, "two Windows executable assets were found") {
			t.Errorf("%s page lacks localized asset guidance or literal evidence: %s", locale, body)
		}
	}
}

func TestUpdatesPageShowsDownloadProgressAndDisablesConflictingActions(t *testing.T) {
	status := baseUpdatesStatus(t)
	status.Busy = &update.Operation{
		Kind:    update.KindDownload,
		Tag:     "0.2.6",
		Plan:    []string{update.PhaseChecksum, update.PhaseDownload, update.PhaseVerify},
		Phases:  []string{update.PhaseChecksum, update.PhaseDownload},
		Phase:   update.PhaseDownload,
		Step:    update.StepDownloading,
		Detail:  update.ExeAsset,
		Done:    8 << 20,
		Total:   16 << 20,
		Started: time.Now().Add(-time.Second),
	}
	f := &fakeUIUpdates{status: status}
	h, store := newUpdatesTestHandler(t, f, i18n.English)
	for _, locale := range []i18n.Locale{i18n.English, i18n.Japanese} {
		if err := store.SetLocale(string(locale)); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/updates", nil))
		body := response.Body.String()
		for _, want := range []string{`id="updates-busy-note"`, `id="updates-active-operation"`, `role="progressbar"`, `aria-valuenow="50"`, `id="updates-check"`, `<select name="channel" disabled>`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s progress page lacks %q", locale, want)
			}
		}
		position := locale.T("Phase %d of %d", 2, 3)
		if !strings.Contains(body, html.EscapeString(position)) {
			t.Errorf("%s progress page lacks localized phase position %q", locale, position)
		}
		if locale == i18n.English && !strings.Contains(body, "8.0 MiB of 16.0 MiB") {
			t.Error("English progress page lacks the byte total")
		}
		if locale == i18n.Japanese && strings.Contains(body, "Phase 2 of 3") {
			t.Error("Japanese progress page contains the English phase position")
		}
		start := strings.Index(body, `id="updates-download-0.2.6"`)
		if start < 0 {
			t.Fatal("download candidate form is missing")
		}
		if end := strings.Index(body[start:], "</form>"); end < 0 || !strings.Contains(body[start:start+end], "disabled") {
			t.Error("Download remains enabled while an update is in progress")
		}
		checking := html.EscapeString(locale.T("Checking…"))
		restarting := html.EscapeString(locale.T("Restarting…"))
		if !strings.Contains(body, `data-pending="`+checking+`" disabled`) || !strings.Contains(body, `data-pending="`+restarting+`" disabled`) {
			t.Error("Check or Restart & update remains enabled while an update is in progress")
		}
	}
	if f.checks != 0 || len(f.downloads) != 0 || f.installs != 0 {
		t.Fatal("reading progress performed a network or install action")
	}
}

func TestUpdatesPageLocalizesSuccessfulCheckWithNoEligibleReleases(t *testing.T) {
	status := baseUpdatesStatus(t)
	status.LastCheck = &update.LastCheck{
		Time:    status.LastCheck.Time,
		Channel: update.Stable,
		OK:      true,
		Message: "success-message-must-not-be-shown",
	}
	status.Check = &update.CheckResult{Time: status.LastCheck.Time, Channel: update.Stable}
	h, store := newUpdatesTestHandler(t, &fakeUIUpdates{status: status}, i18n.English)
	for _, locale := range []i18n.Locale{i18n.English, i18n.Japanese} {
		if err := store.SetLocale(string(locale)); err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/updates", nil))
		body := response.Body.String()
		want := html.EscapeString(locale.T("No eligible releases on this channel."))
		if !strings.Contains(body, want) {
			t.Errorf("%s page lacks localized empty-release message %q", locale, want)
		}
		if strings.Contains(body, "success-message-must-not-be-shown") {
			t.Errorf("%s page displayed a raw success message instead of deriving empty state", locale)
		}
	}
}

func TestUpdatesPostsForwardOnlyExplicitActionsAndSurfaceRefusal(t *testing.T) {
	f := &fakeUIUpdates{status: baseUpdatesStatus(t)}
	h, _ := newUpdatesTestHandler(t, f, i18n.Japanese)
	for _, action := range []struct {
		path string
		form url.Values
	}{
		{path: "/updates/channel", form: url.Values{"channel": {"development"}}},
		{path: "/updates/check"},
		{path: "/updates/download", form: url.Values{"tag": {" 0.2.6 "}}},
		{path: "/updates/install"},
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, formRequest(http.MethodPost, action.path, action.form, h.token))
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/updates" {
			t.Fatalf("POST %s = %d, location %q, body %s", action.path, response.Code, response.Header().Get("Location"), response.Body.String())
		}
	}
	if len(f.channels) != 1 || f.channels[0] != update.Development || f.checks != 1 || len(f.downloads) != 1 || f.downloads[0] != "0.2.6" || f.installs != 1 {
		t.Fatalf("explicit update actions were not forwarded once: channels=%v checks=%d downloads=%v installs=%d", f.channels, f.checks, f.downloads, f.installs)
	}

	f.checkErr = &update.Error{Class: update.ClassRefused, Msg: "another update action is in progress"}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, formRequest(http.MethodPost, "/updates/check", nil, h.token))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, i18n.Japanese.T("This action is unavailable in the current update state. Review the selected channel, ready candidate and operation status before retrying.")) || !strings.Contains(body, "another update action is in progress") {
		t.Fatalf("refused check lacks a translated hint and literal evidence: %d %s", response.Code, body)
	}
}

func TestUpdatesPageAddsLocalizedHintsBesideUpdateStateEvidence(t *testing.T) {
	status := baseUpdatesStatus(t)
	status.ReadyProblem = "staged executable failed verification"
	status.Err = "could not load update state"
	status.Result = &update.Result{Outcome: update.OutcomeRefused, Message: "install refused because the candidate is unavailable"}
	h, _ := newUpdatesTestHandler(t, &fakeUIUpdates{status: status}, i18n.Japanese)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/updates", nil))
	body := response.Body.String()
	for _, pair := range [][2]string{
		{"updates-ready-problem", "This candidate is not installable. Review the status and error detail above, then check for updates or download another verified candidate."},
		{"updates-state-error", "The updater could not read or save local update state. Check Matagi's state directory and review the error detail."},
		{"updates-result", "The update attempt was refused. Review the current update status and error detail before retrying."},
	} {
		if !strings.Contains(body, `id="`+pair[0]+`"`) || !strings.Contains(body, html.EscapeString(i18n.Japanese.T(pair[1]))) {
			t.Errorf("Japanese page lacks translated hint %q near %s", pair[1], pair[0])
		}
	}
	for _, detail := range []string{"staged executable failed verification", "could not load update state", "install refused because the candidate is unavailable"} {
		if !strings.Contains(body, detail) {
			t.Errorf("Japanese page lost update diagnostic %q", detail)
		}
	}
}

func TestDownloadOutcomeExplainsRemainingVerifiedState(t *testing.T) {
	failure := &update.Failure{Class: update.ClassDownload, Phase: update.PhaseDownload}
	withoutReady := downloadOutcome(failure, update.Status{})
	if strings.Join(withoutReady, " ") != "The partial download was discarded. No update is ready to install." {
		t.Fatalf("outcome without ready update = %v", withoutReady)
	}
	withReady := downloadOutcome(failure, update.Status{Ready: &update.Ready{}, ReadyProblem: ""})
	if strings.Join(withReady, " ") != "The partial download was discarded. A previously verified update is still ready to install." {
		t.Fatalf("outcome with previously verified update = %v", withReady)
	}
	if updateErrorHint(errors.New("unclassified")) != "The update action could not be completed." {
		t.Fatal("unclassified update errors lack a bounded generic hint")
	}
}
