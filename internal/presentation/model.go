// Package presentation owns logical service-view identity and saved layout.
// It has no authority over service health, transport, or lifecycle.
package presentation

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
)

// EndpointKey identifies a registered endpoint by its exact logical identity.
// It deliberately contains no URL or transport location.
type EndpointKey struct {
	EnvironmentID string `json:"environmentId"`
	ServiceID     string `json:"serviceId"`
	EndpointID    string `json:"endpointId"`
}

// Valid reports whether every identity component is a non-empty registered
// identifier with no surrounding whitespace or NUL byte.
func (key EndpointKey) Valid() bool {
	return validIdentity(key.EnvironmentID) && validIdentity(key.ServiceID) && validIdentity(key.EndpointID)
}

func validIdentity(value string) bool {
	return strings.TrimSpace(value) != "" && value == strings.TrimSpace(value) && !strings.ContainsRune(value, '\x00')
}

// ViewID is an opaque process-local identity. It is never persisted.
type ViewID uint64

// Location is the current presentation location of a service view.
type Location string

const (
	LocationTab    Location = "tab"
	LocationWindow Location = "window"
)

func (location Location) valid() bool {
	return location == LocationTab || location == LocationWindow
}

// ViewState describes presentation progress only; it carries no service
// readiness, desired-state, process, or tunnel information.
type ViewState string

const (
	ViewParked      ViewState = "parked"
	ViewPending     ViewState = "pending"
	ViewCreated     ViewState = "created"
	ViewMoving      ViewState = "moving"
	ViewFailed      ViewState = "failed"
	ViewUnavailable ViewState = "unavailable"
)

// DIPBounds records normal detached-window bounds in device-independent
// pixels. Negative origins are valid for monitors arranged left or above the
// primary display.
type DIPBounds struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

func (bounds DIPBounds) valid() bool {
	return finite(bounds.X) && finite(bounds.Y) && finite(bounds.Width) && finite(bounds.Height) && bounds.Width > 0 && bounds.Height > 0
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// ViewSnapshot is a detached copy of one logical view. Mutating it cannot
// mutate the model.
type ViewSnapshot struct {
	ID       ViewID
	Key      EndpointKey
	Location Location
	State    ViewState
	Bounds   *DIPBounds
}

// Snapshot is a detached, ordered copy of current presentation state.
type Snapshot struct {
	Views    []ViewSnapshot
	Selected *EndpointKey
}

// Operation is an opaque generation token returned by ReserveOpen or
// BeginMove. A completion is accepted only while this exact operation remains
// current for its view.
type Operation struct {
	viewID     ViewID
	key        EndpointKey
	generation uint64
	kind       operationKind
	target     Location
	bounds     *DIPBounds
}

type operationKind uint8

const (
	operationNone operationKind = iota
	operationOpen
	operationMove
)

// OpenResult identifies the reserved or existing view. Ensure is true only
// for the caller that owns a newly-created open operation; coalesced callers
// must not issue a second ensure.
type OpenResult struct {
	View      ViewSnapshot
	Operation Operation
	Ensure    bool
}

// Model owns logical view ordering, selection, locations, and operation
// generations. It never calls a runtime, host, or lifecycle API.
type Model struct {
	mu       sync.RWMutex
	views    []*viewRecord
	byID     map[ViewID]*viewRecord
	byKey    map[EndpointKey]*viewRecord
	selected *EndpointKey
	nextID   ViewID
}

type viewRecord struct {
	view       ViewSnapshot
	generation uint64
	op         operationKind
	moveTarget Location
	moveBounds *DIPBounds
	persisted  bool
}

// NewModel creates an empty presentation model.
func NewModel() *Model {
	return &Model{
		byID:  make(map[ViewID]*viewRecord),
		byKey: make(map[EndpointKey]*viewRecord),
	}
}

// ReserveOpen reserves one operation for key before its caller performs an
// endpoint ensure. Repeated calls for a pending or ready key coalesce and
// never request another ensure. A parked, failed, or unavailable view can be
// resumed by one explicit call.
func (m *Model) ReserveOpen(key EndpointKey, initial Location) (OpenResult, error) {
	if m == nil {
		return OpenResult{}, errors.New("presentation model is nil")
	}
	if !key.Valid() {
		return OpenResult{}, errors.New("endpoint key is invalid")
	}
	if !initial.valid() {
		return OpenResult{}, fmt.Errorf("presentation location %q is invalid", initial)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.byKey[key]; existing != nil {
		m.selectKey(key)
		switch existing.view.State {
		case ViewPending:
			return OpenResult{View: cloneView(existing.view), Operation: m.operation(existing), Ensure: false}, nil
		case ViewCreated, ViewMoving:
			return OpenResult{View: cloneView(existing.view)}, nil
		case ViewParked, ViewFailed, ViewUnavailable:
			if err := m.beginOpen(existing); err != nil {
				return OpenResult{}, err
			}
			return OpenResult{View: cloneView(existing.view), Operation: m.operation(existing), Ensure: true}, nil
		default:
			return OpenResult{}, errors.New("presentation view has an invalid state")
		}
	}

	if m.nextID == ViewID(math.MaxUint64) {
		return OpenResult{}, errors.New("presentation view identifier space exhausted")
	}
	m.nextID++
	view := &viewRecord{
		view: ViewSnapshot{
			ID:       m.nextID,
			Key:      key,
			Location: initial,
			State:    ViewPending,
		},
	}
	if err := m.beginOpen(view); err != nil {
		return OpenResult{}, err
	}
	m.views = append(m.views, view)
	m.byID[view.view.ID] = view
	m.byKey[key] = view
	m.selectKey(key)
	return OpenResult{View: cloneView(view.view), Operation: m.operation(view), Ensure: true}, nil
}

// CompleteOpen publishes a reserved view after its caller has completed
// endpoint admission and host creation. Late or duplicate completions return
// false and do not change model state.
func (m *Model) CompleteOpen(operation Operation) (ViewSnapshot, bool) {
	if m == nil {
		return ViewSnapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.current(operation, operationOpen)
	if view == nil || view.view.State != ViewPending {
		return ViewSnapshot{}, false
	}
	view.op = operationNone
	view.view.State = ViewCreated
	view.persisted = true
	return cloneView(view.view), true
}

// FailOpen marks the reserved operation as failed without losing its logical
// identity or location. A later explicit ReserveOpen may retry it.
func (m *Model) FailOpen(operation Operation) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.current(operation, operationOpen)
	if view == nil || view.view.State != ViewPending {
		return false
	}
	view.op = operationNone
	view.view.State = ViewFailed
	return true
}

// IsCurrent reports whether operation is still the active generation for its
// view. Close, failure, completion, or a newer operation invalidates it.
func (m *Model) IsCurrent(operation Operation) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current(operation, operation.kind) != nil
}

// Select changes the selected logical view without affecting service state.
func (m *Model) Select(id ViewID) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.byID[id]
	if view == nil {
		return false
	}
	m.selectKey(view.view.Key)
	return true
}

// BeginMove starts a location change but leaves the committed location and
// bounds untouched until CommitMove succeeds.
func (m *Model) BeginMove(id ViewID, target Location, bounds *DIPBounds) (Operation, error) {
	if m == nil {
		return Operation{}, errors.New("presentation model is nil")
	}
	if !target.valid() {
		return Operation{}, fmt.Errorf("presentation location %q is invalid", target)
	}
	if bounds != nil && !bounds.valid() {
		return Operation{}, errors.New("detached-window bounds are invalid")
	}
	if target == LocationTab && bounds != nil {
		return Operation{}, errors.New("integrated tabs cannot have detached-window bounds")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.byID[id]
	if view == nil {
		return Operation{}, errors.New("presentation view was not found")
	}
	if view.view.State != ViewCreated || view.op != operationNone {
		return Operation{}, errors.New("presentation view cannot move in its current state")
	}
	if err := advanceGeneration(view); err != nil {
		return Operation{}, err
	}
	view.op = operationMove
	view.moveTarget = target
	view.moveBounds = cloneBounds(bounds)
	view.view.State = ViewMoving
	return m.operation(view), nil
}

// CommitMove applies a completed host move. It rejects stale generations.
func (m *Model) CommitMove(operation Operation) (ViewSnapshot, bool) {
	if m == nil {
		return ViewSnapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.current(operation, operationMove)
	if view == nil || view.view.State != ViewMoving {
		return ViewSnapshot{}, false
	}
	view.view.Location = view.moveTarget
	view.view.Bounds = cloneBounds(view.moveBounds)
	view.moveTarget = ""
	view.moveBounds = nil
	view.op = operationNone
	view.view.State = ViewCreated
	return cloneView(view.view), true
}

// FailMove aborts a location change while retaining the old committed
// location and bounds.
func (m *Model) FailMove(operation Operation) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.current(operation, operationMove)
	if view == nil || view.view.State != ViewMoving {
		return false
	}
	view.moveTarget = ""
	view.moveBounds = nil
	view.op = operationNone
	view.view.State = ViewCreated
	return true
}

// MarkUnavailable changes presentation state after a host-level failure. It
// does not infer or change service readiness.
func (m *Model) MarkUnavailable(id ViewID) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.byID[id]
	if view == nil || (view.view.State != ViewCreated && view.view.State != ViewMoving) {
		return false
	}
	if err := advanceGeneration(view); err != nil {
		return false
	}
	view.op = operationNone
	view.moveTarget = ""
	view.moveBounds = nil
	view.view.State = ViewUnavailable
	return true
}

// Close removes only the presentation view and invalidates its outstanding
// operation generations. It has no service or tunnel side effects.
func (m *Model) Close(id ViewID) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view := m.byID[id]
	if view == nil {
		return false
	}
	delete(m.byID, id)
	delete(m.byKey, view.view.Key)
	for index, candidate := range m.views {
		if candidate == view {
			m.views = append(m.views[:index], m.views[index+1:]...)
			break
		}
	}
	if m.selected != nil && *m.selected == view.view.Key {
		m.selected = nil
	}
	return true
}

// Snapshot returns the current views in order with no mutable model aliases.
func (m *Model) Snapshot() Snapshot {
	if m == nil {
		return Snapshot{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := Snapshot{Views: make([]ViewSnapshot, 0, len(m.views))}
	for _, view := range m.views {
		result.Views = append(result.Views, cloneView(view.view))
	}
	if m.selected != nil {
		selected := *m.selected
		result.Selected = &selected
	}
	return result
}

// Layout returns the logical saved layout in view order. New views are
// included after successful host creation; a restored placeholder remains in
// the layout while an explicit Resume is pending.
func (m *Model) Layout() Layout {
	if m == nil {
		return Layout{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := Layout{Views: make([]LayoutEntry, 0, len(m.views))}
	for _, view := range m.views {
		if !view.persisted && view.view.State != ViewCreated && view.view.State != ViewMoving {
			continue
		}
		result.Views = append(result.Views, LayoutEntry{
			Key:      view.view.Key,
			Location: view.view.Location,
			Bounds:   cloneBounds(view.view.Bounds),
		})
	}
	if m.selected != nil {
		for _, entry := range result.Views {
			if entry.Key == *m.selected {
				selected := *m.selected
				result.Selected = &selected
				break
			}
		}
	}
	return result
}

// Restore installs saved entries as parked placeholders without issuing any
// endpoint operation. It is allowed only on an empty model.
func (m *Model) Restore(layout Layout) error {
	if m == nil {
		return errors.New("presentation model is nil")
	}
	if err := validateLayout(layout, nil, false); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.views) != 0 {
		return errors.New("presentation model is not empty")
	}
	for _, entry := range layout.Views {
		if m.nextID == ViewID(math.MaxUint64) {
			return errors.New("presentation view identifier space exhausted")
		}
		m.nextID++
		view := &viewRecord{
			view: ViewSnapshot{
				ID:       m.nextID,
				Key:      entry.Key,
				Location: entry.Location,
				State:    ViewParked,
				Bounds:   cloneBounds(entry.Bounds),
			},
			persisted: true,
		}
		m.views = append(m.views, view)
		m.byID[view.view.ID] = view
		m.byKey[entry.Key] = view
	}
	if layout.Selected != nil {
		selected := *layout.Selected
		m.selected = &selected
	}
	return nil
}

func (m *Model) beginOpen(view *viewRecord) error {
	if err := advanceGeneration(view); err != nil {
		return err
	}
	view.op = operationOpen
	view.view.State = ViewPending
	return nil
}

func advanceGeneration(view *viewRecord) error {
	if view.generation == math.MaxUint64 {
		return errors.New("presentation operation generation space exhausted")
	}
	view.generation++
	return nil
}

func (m *Model) operation(view *viewRecord) Operation {
	return Operation{
		viewID:     view.view.ID,
		key:        view.view.Key,
		generation: view.generation,
		kind:       view.op,
		target:     view.moveTarget,
		bounds:     cloneBounds(view.moveBounds),
	}
}

func (m *Model) current(operation Operation, kind operationKind) *viewRecord {
	if operation.kind != kind || kind == operationNone || operation.generation == 0 {
		return nil
	}
	view := m.byID[operation.viewID]
	if view == nil || view.view.Key != operation.key || view.generation != operation.generation || view.op != kind {
		return nil
	}
	return view
}

func (m *Model) selectKey(key EndpointKey) {
	selected := key
	m.selected = &selected
}

func cloneView(view ViewSnapshot) ViewSnapshot {
	view.Bounds = cloneBounds(view.Bounds)
	return view
}

func cloneBounds(bounds *DIPBounds) *DIPBounds {
	if bounds == nil {
		return nil
	}
	copy := *bounds
	return &copy
}
