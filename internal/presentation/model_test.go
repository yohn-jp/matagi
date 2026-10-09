package presentation

import (
	"sync"
	"testing"
)

func TestConcurrentOpenReservesOneEnsurePerEndpoint(t *testing.T) {
	model := NewModel()
	key := testKey("env-a", "same-service", "web")
	const callers = 64
	results := make([]OpenResult, callers)
	errs := make([]error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for index := 0; index < callers; index++ {
		go func(index int) {
			defer wait.Done()
			results[index], errs[index] = model.ReserveOpen(key, LocationTab)
		}(index)
	}
	wait.Wait()

	ensureCount := 0
	viewID := ViewID(0)
	for index, result := range results {
		if errs[index] != nil {
			t.Fatalf("ReserveOpen() error = %v", errs[index])
		}
		if result.Ensure {
			ensureCount++
		}
		if viewID == 0 {
			viewID = result.View.ID
		}
		if result.View.ID != viewID {
			t.Fatalf("reservation view IDs differ: %d and %d", result.View.ID, viewID)
		}
	}
	if ensureCount != 1 {
		t.Fatalf("ensure owners = %d, want 1", ensureCount)
	}
	if got := len(model.Snapshot().Views); got != 1 {
		t.Fatalf("view count = %d, want 1", got)
	}
}

func TestEndpointIdentityUsesEnvironmentAndEndpointTuple(t *testing.T) {
	model := NewModel()
	first, err := model.ReserveOpen(testKey("env-a", "same-service", "web"), LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	second, err := model.ReserveOpen(testKey("env-b", "same-service", "web"), LocationWindow)
	if err != nil {
		t.Fatal(err)
	}
	third, err := model.ReserveOpen(testKey("env-b", "same-service", "admin"), LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	if first.View.ID == second.View.ID || second.View.ID == third.View.ID || first.View.ID == third.View.ID {
		t.Fatal("distinct endpoint tuples shared a ViewID")
	}
	if got := len(model.Snapshot().Views); got != 3 {
		t.Fatalf("view count = %d, want 3", got)
	}
}

func TestCloseInvalidatesLateOpenCompletion(t *testing.T) {
	model := NewModel()
	reserved, err := model.ReserveOpen(testKey("env", "svc", "web"), LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	if !model.IsCurrent(reserved.Operation) {
		t.Fatal("new reservation is not current")
	}
	if !model.Close(reserved.View.ID) {
		t.Fatal("Close() did not remove the pending view")
	}
	if model.IsCurrent(reserved.Operation) {
		t.Fatal("close did not invalidate the open generation")
	}
	if _, ok := model.CompleteOpen(reserved.Operation); ok {
		t.Fatal("late completion after close was accepted")
	}
	if got := len(model.Snapshot().Views); got != 0 {
		t.Fatalf("late completion resurrected %d views", got)
	}
}

func TestOpenGenerationCannotBeReusedAfterFailure(t *testing.T) {
	model := NewModel()
	first, err := model.ReserveOpen(testKey("env", "svc", "web"), LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	if !model.FailOpen(first.Operation) {
		t.Fatal("FailOpen() rejected current operation")
	}
	if model.IsCurrent(first.Operation) {
		t.Fatal("failed open generation remained current")
	}
	second, err := model.ReserveOpen(first.View.Key, LocationWindow)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Ensure || second.Operation.generation == first.Operation.generation {
		t.Fatal("explicit retry did not reserve a new generation")
	}
	if _, ok := model.CompleteOpen(first.Operation); ok {
		t.Fatal("stale first completion was accepted after retry")
	}
}

func TestMoveCommitAndFailurePreserveCommittedLocation(t *testing.T) {
	model := NewModel()
	reserved, err := model.ReserveOpen(testKey("env", "svc", "web"), LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	view, ok := model.CompleteOpen(reserved.Operation)
	if !ok {
		t.Fatal("CompleteOpen() rejected current operation")
	}

	bounds := &DIPBounds{X: -1400.5, Y: 30, Width: 960, Height: 720}
	move, err := model.BeginMove(view.ID, LocationWindow, bounds)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Snapshot().Views[0]; got.Location != LocationTab || got.Bounds != nil || got.State != ViewMoving {
		t.Fatalf("uncommitted move changed the saved location: %#v", got)
	}
	if !model.FailMove(move) {
		t.Fatal("FailMove() rejected current move")
	}
	failed := model.Snapshot().Views[0]
	if failed.Location != LocationTab || failed.Bounds != nil || failed.State != ViewCreated {
		t.Fatalf("failed move did not preserve its original location: %#v", failed)
	}
	if model.IsCurrent(move) {
		t.Fatal("failed move generation remained current")
	}

	move, err = model.BeginMove(view.ID, LocationWindow, bounds)
	if err != nil {
		t.Fatal(err)
	}
	committed, ok := model.CommitMove(move)
	if !ok {
		t.Fatal("CommitMove() rejected current move")
	}
	if committed.Location != LocationWindow || committed.Bounds == nil || *committed.Bounds != *bounds {
		t.Fatalf("committed window placement = %#v", committed)
	}
	if model.IsCurrent(move) {
		t.Fatal("committed move generation remained current")
	}
}

func TestSnapshotsAndLayoutsAreDetachedCopies(t *testing.T) {
	model := NewModel()
	reserved, err := model.ReserveOpen(testKey("env", "svc", "web"), LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := model.CompleteOpen(reserved.Operation); !ok {
		t.Fatal("CompleteOpen() rejected current operation")
	}

	snapshot := model.Snapshot()
	snapshot.Views[0].Key.ServiceID = "changed"
	if snapshot.Selected != nil {
		snapshot.Selected.ServiceID = "changed"
	}
	layout := model.Layout()
	layout.Views[0].Key.ServiceID = "also-changed"
	if layout.Selected != nil {
		layout.Selected.ServiceID = "also-changed"
	}
	current := model.Snapshot()
	if current.Views[0].Key.ServiceID != "svc" || current.Selected == nil || current.Selected.ServiceID != "svc" {
		t.Fatalf("mutating a snapshot changed model state: %#v", current)
	}
	if current.Views[0].State != ViewCreated {
		t.Fatalf("snapshot contains non-presentation state: %#v", current.Views[0])
	}
}

func TestRestoreCreatesParkedViewsWithoutEnsuring(t *testing.T) {
	key := testKey("env", "svc", "web")
	selected := key
	layout := Layout{
		Views:    []LayoutEntry{{Key: key, Location: LocationWindow, Bounds: &DIPBounds{X: -100, Y: 25, Width: 800, Height: 600}}},
		Selected: &selected,
	}
	model := NewModel()
	if err := model.Restore(layout); err != nil {
		t.Fatal(err)
	}
	view := model.Snapshot().Views[0]
	if view.State != ViewParked || view.Location != LocationWindow {
		t.Fatalf("restored view = %#v", view)
	}
	resumed, err := model.ReserveOpen(key, LocationTab)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Ensure || resumed.View.Location != LocationWindow {
		t.Fatalf("restored resume = %#v", resumed)
	}
	if got := model.Layout().Views[0].Location; got != LocationWindow {
		t.Fatalf("pending resume discarded saved location: %q", got)
	}
}

func testKey(environment, service, endpoint string) EndpointKey {
	return EndpointKey{EnvironmentID: environment, ServiceID: service, EndpointID: endpoint}
}
