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
		if !strings.Contains(body, `action="/updates/check"`) || !strings.Contains(body, `action="/updates/download"`) || !strings.Contains(body, `id="updates-install"`) {
			t.Errorf("%s page lacks explicit update actions", locale)
		}
	}
	if f.checks != 0 || len(f.downloads) != 0 || f.installs != 0 || len(f.channels) != 0 {
		t.Fatalf("rendering performed an update action: checks=%d downloads=%v installs=%d channels=%v", f.checks, f.downloads, f.installs, f.channels)
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
	h, _ := newUpdatesTestHandler(t, f, i18n.English)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/updates", nil))
	body := response.Body.String()
	for _, want := range []string{`id="updates-busy-note"`, `id="updates-active-operation"`, `role="progressbar"`, `aria-valuenow="50"`, "8.0 MiB of 16.0 MiB", `id="updates-check"`, `<select name="channel" disabled>`} {
		if !strings.Contains(body, want) {
			t.Errorf("progress page lacks %q", want)
		}
	}
	start := strings.Index(body, `id="updates-download-0.2.6"`)
	if start < 0 {
		t.Fatal("download candidate form is missing")
	}
	if end := strings.Index(body[start:], "</form>"); end < 0 || !strings.Contains(body[start:start+end], "disabled") {
		t.Error("Download remains enabled while an update is in progress")
	}
	if !strings.Contains(body, `data-pending="Checking…" disabled`) || !strings.Contains(body, `data-pending="Restarting…" disabled`) {
		t.Error("Check or Restart & update remains enabled while an update is in progress")
	}
	if f.checks != 0 || len(f.downloads) != 0 || f.installs != 0 {
		t.Fatal("reading progress performed a network or install action")
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
	if response.Code != http.StatusOK || !strings.Contains(body, i18n.Japanese.T("Another update action is in progress. Wait for it to finish.")) || !strings.Contains(body, "another update action is in progress") {
		t.Fatalf("refused check lacks a translated hint and literal evidence: %d %s", response.Code, body)
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
