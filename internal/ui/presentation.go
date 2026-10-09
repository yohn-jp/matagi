package ui

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/yohn-jp/matagi/internal/i18n"
	"github.com/yohn-jp/matagi/internal/presentation"
)

// Presenter coordinates logical service views and their desktop host. CompleteOpen
// must admit the ensured URL only for the reserved view, create its guarded
// controller, and publish that same operation generation after creation succeeds.
// The UI passes endpoint identities and URLs returned by a successful endpoint
// ensure operation; a failed generation must never admit or publish its URL.
type Presenter interface {
	ReserveOpen(presentation.EndpointKey, presentation.Location) (OpenReservation, error)
	CompleteOpen(OpenReservation, string) error
	FailOpen(OpenReservation)
	Select(presentation.ViewID) error
	// SelectTrusted displays a Matagi-owned destination and hides any selected
	// integrated service controller without changing logical view selection.
	SelectTrusted(TrustedDestination) error
	Move(presentation.ViewID, presentation.Location) error
	Close(presentation.ViewID) error
	Snapshot() presentation.Snapshot
	ResetSavedLayout() error
}

// TrustedDestination is a Matagi-owned page that replaces the selected
// integrated service controller in the main content area. It never carries a
// browser URL or service-provided destination.
type TrustedDestination string

const (
	TrustedWorkspace TrustedDestination = "workspace"
	TrustedUpdates   TrustedDestination = "updates"
	TrustedSettings  TrustedDestination = "settings"
)

// OpenReservation keeps the model's opaque operation generation with the
// view returned by ReserveOpen. Only the Presenter interprets Operation.
type OpenReservation struct {
	View      presentation.ViewSnapshot
	Operation presentation.Operation
	Ensure    bool
}

type chromeView struct {
	ID       presentation.ViewID
	Key      presentation.EndpointKey
	Location presentation.Location
	State    presentation.ViewState
	Selected bool
}

type chromeData struct {
	Locale             i18n.Locale
	Token              string
	Active             string
	PresenterAvailable bool
	Views              []chromeView
	Detached           []chromeView
	Selected           *presentation.EndpointKey
	SelectedView       *chromeView
}

type viewProjection struct {
	ID       presentation.ViewID      `json:"id"`
	Key      presentation.EndpointKey `json:"key"`
	Location presentation.Location    `json:"location"`
	State    presentation.ViewState   `json:"state"`
}

type presentationProjection struct {
	Views    []viewProjection          `json:"views"`
	Selected *presentation.EndpointKey `json:"selected,omitempty"`
}

func (h *handler) chrome(active string) chromeData {
	data := chromeData{Locale: h.locale(), Token: h.token, Active: active}
	if h.presenter == nil {
		return data
	}
	data.PresenterAvailable = true
	snapshot := h.presenter.Snapshot()
	if snapshot.Selected != nil {
		selected := *snapshot.Selected
		data.Selected = &selected
	}
	data.Views = make([]chromeView, 0, len(snapshot.Views))
	for _, view := range snapshot.Views {
		item := chromeView{ID: view.ID, Key: view.Key, Location: view.Location, State: view.State}
		item.Selected = data.Selected != nil && *data.Selected == item.Key
		if item.Location == presentation.LocationTab {
			data.Views = append(data.Views, item)
			if item.Selected {
				selectedView := item
				data.SelectedView = &selectedView
			}
		} else if item.Location == presentation.LocationWindow {
			data.Detached = append(data.Detached, item)
		}
	}
	return data
}

func (h *handler) selectTrusted(w http.ResponseWriter, destination TrustedDestination) bool {
	if h.presenter == nil {
		return true
	}
	if err := h.presenter.SelectTrusted(destination); err != nil {
		http.Error(w, h.locale().T("The service view could not be updated. Refresh the workspace and try again."), http.StatusBadGateway)
		return false
	}
	return true
}

func (h *handler) presentationProjection(w http.ResponseWriter) {
	if h.presenter == nil {
		http.Error(w, h.locale().T("Presentation controls are unavailable."), http.StatusServiceUnavailable)
		return
	}
	snapshot := h.presenter.Snapshot()
	projection := presentationProjection{Views: make([]viewProjection, 0, len(snapshot.Views))}
	for _, view := range snapshot.Views {
		projection.Views = append(projection.Views, viewProjection{ID: view.ID, Key: view.Key, Location: view.Location, State: view.State})
	}
	if snapshot.Selected != nil {
		selected := *snapshot.Selected
		projection.Selected = &selected
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(projection); err != nil {
		return
	}
}

func (h *handler) selectView(w http.ResponseWriter, r *http.Request) {
	viewID, ok := h.formViewID(w, r)
	if !ok {
		return
	}
	if h.presenter == nil {
		h.presentationUnavailable(w)
		return
	}
	view, found := h.presentationView(viewID)
	if !found {
		h.presentationFailure(w)
		return
	}
	if err := h.presenter.Select(viewID); err != nil {
		h.presentationFailure(w)
		return
	}
	http.Redirect(w, r, viewLocation(view.Location, view.Key.EnvironmentID), http.StatusSeeOther)
}

func (h *handler) moveView(w http.ResponseWriter, r *http.Request) {
	viewID, ok := h.formViewID(w, r)
	if !ok {
		return
	}
	var target presentation.Location
	switch r.PostForm.Get("target") {
	case string(presentation.LocationTab):
		target = presentation.LocationTab
	case string(presentation.LocationWindow):
		target = presentation.LocationWindow
	default:
		http.Error(w, h.locale().T("Choose a valid view location."), http.StatusBadRequest)
		return
	}
	if h.presenter == nil {
		h.presentationUnavailable(w)
		return
	}
	view, found := h.presentationView(viewID)
	if !found {
		h.presentationFailure(w)
		return
	}
	if err := h.presenter.Move(viewID, target); err != nil {
		h.presentationFailure(w)
		return
	}
	http.Redirect(w, r, viewLocation(target, view.Key.EnvironmentID), http.StatusSeeOther)
}

func (h *handler) closeView(w http.ResponseWriter, r *http.Request) {
	viewID, ok := h.formViewID(w, r)
	if !ok {
		return
	}
	if h.presenter == nil {
		h.presentationUnavailable(w)
		return
	}
	view, found := h.presentationView(viewID)
	if !found {
		h.presentationFailure(w)
		return
	}
	if err := h.presenter.Close(viewID); err != nil {
		h.presentationFailure(w)
		return
	}
	http.Redirect(w, r, workspaceLocation(view.Key.EnvironmentID), http.StatusSeeOther)
}

func (h *handler) presentationView(id presentation.ViewID) (presentation.ViewSnapshot, bool) {
	if h.presenter == nil {
		return presentation.ViewSnapshot{}, false
	}
	for _, view := range h.presenter.Snapshot().Views {
		if view.ID == id {
			return view, true
		}
	}
	return presentation.ViewSnapshot{}, false
}

func (h *handler) selectedIntegratedEnvironment() (string, bool) {
	if h.presenter == nil {
		return "", false
	}
	snapshot := h.presenter.Snapshot()
	if snapshot.Selected == nil {
		return "", false
	}
	for _, view := range snapshot.Views {
		if view.Key == *snapshot.Selected {
			return view.Key.EnvironmentID, view.Location == presentation.LocationTab
		}
	}
	return "", false
}

func viewLocation(location presentation.Location, environmentID string) string {
	if location == presentation.LocationTab {
		return serviceLocation(environmentID)
	}
	return workspaceLocation(environmentID)
}

func (h *handler) resumeView(w http.ResponseWriter, r *http.Request) {
	key := presentation.EndpointKey{
		EnvironmentID: r.PostForm.Get("environmentId"),
		ServiceID:     r.PostForm.Get("serviceId"),
		EndpointID:    r.PostForm.Get("endpointId"),
	}
	if !key.Valid() {
		http.Error(w, h.locale().T("An environment, service, and endpoint are required."), http.StatusBadRequest)
		return
	}
	if h.presenter == nil {
		h.presentationUnavailable(w)
		return
	}
	snapshot := h.presenter.Snapshot()
	var found bool
	for _, view := range snapshot.Views {
		if view.Key == key && (view.State == presentation.ViewParked || view.State == presentation.ViewFailed || view.State == presentation.ViewUnavailable) {
			found = true
			break
		}
	}
	if !found {
		h.presentationFailure(w)
		return
	}
	h.openEndpoint(w, r, key, r.PostForm.Get("location"))
}

func (h *handler) formViewID(w http.ResponseWriter, r *http.Request) (presentation.ViewID, bool) {
	value, err := strconv.ParseUint(r.PostForm.Get("viewId"), 10, 64)
	if err != nil || value == 0 {
		http.Error(w, h.locale().T("A valid service view is required."), http.StatusBadRequest)
		return 0, false
	}
	return presentation.ViewID(value), true
}

func (h *handler) presentationUnavailable(w http.ResponseWriter) {
	http.Error(w, h.locale().T("Presentation controls are unavailable."), http.StatusServiceUnavailable)
}

func (h *handler) presentationFailure(w http.ResponseWriter) {
	h.showError(w, http.StatusBadGateway, h.locale().T("The service view could not be updated. Refresh the workspace and try again."))
}

func viewStateMessage(state presentation.ViewState) string {
	switch state {
	case presentation.ViewParked:
		return "Parked"
	case presentation.ViewPending:
		return "Opening"
	case presentation.ViewCreated:
		return "Open"
	case presentation.ViewMoving:
		return "Moving"
	case presentation.ViewFailed:
		return "Open failed"
	case presentation.ViewUnavailable:
		return "Unavailable"
	default:
		return "Unavailable"
	}
}

func viewPlaceholderMessage(state presentation.ViewState) string {
	switch state {
	case presentation.ViewParked:
		return "This saved view is parked. Resume checks the registered endpoint before opening it."
	case presentation.ViewPending:
		return "This registered endpoint is being ensured."
	case presentation.ViewMoving:
		return "This service view is changing location."
	case presentation.ViewFailed:
		return "This view could not be opened. Check the workspace status, then try Resume."
	case presentation.ViewUnavailable:
		return "This view is unavailable. Resume checks the registered endpoint again."
	default:
		return "This service view is unavailable."
	}
}

//go:embed chrome.html
var chromeHTML string
