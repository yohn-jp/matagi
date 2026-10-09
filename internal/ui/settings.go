package ui

import (
	_ "embed"
	"net/http"

	"github.com/yohn-jp/matagi/internal/i18n"
)

type settingsPageData struct {
	Locale               i18n.Locale
	Token                string
	Error                string
	SettingsAvailable    bool
	LayoutResetAvailable bool
	Chrome               chromeData
}

//go:embed settings.html
var settingsHTML string

var settingsTemplate = mustParseTemplates("settings", settingsHTML, templateFuncs(), chromeHTML)

func (h *handler) settingsPage(w http.ResponseWriter, message string) {
	h.renderSettingsStatus(w, http.StatusOK, message)
}

func (h *handler) renderSettingsStatus(w http.ResponseWriter, status int, message string) {
	if !h.selectTrusted(w, TrustedSettings) {
		return
	}
	locale := h.locale()
	data := settingsPageData{
		Locale: locale, Token: h.token, Error: bounded(message, maxErrorMessage),
		SettingsAvailable: h.settings != nil, LayoutResetAvailable: h.presenter != nil,
		Chrome: h.chrome("settings"),
	}
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := settingsTemplate.Execute(w, data); err != nil {
		return
	}
}

func (h *handler) resetLayout(w http.ResponseWriter, r *http.Request) {
	if h.presenter == nil {
		h.presentationUnavailable(w)
		return
	}
	if err := h.presenter.ResetSavedLayout(); err != nil {
		h.settingsPage(w, h.locale().T("The saved layout could not be reset. Workspace and open views are unchanged."))
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
