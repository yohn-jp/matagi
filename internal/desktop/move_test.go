package desktop

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type fakeMoveOperations struct {
	events []string
	failAt map[string]int
	calls  map[string]int
}

func newFakeMoveOperations() *fakeMoveOperations {
	return &fakeMoveOperations{failAt: make(map[string]int), calls: make(map[string]int)}
}

func (operations *fakeMoveOperations) step(name string) error {
	operations.events = append(operations.events, name)
	operations.calls[name]++
	if operations.failAt[name] == operations.calls[name] {
		return fmt.Errorf("injected %s failure", name)
	}
	return nil
}

func (operations *fakeMoveOperations) Hide() error { return operations.step("hide") }

func (operations *fakeMoveOperations) SetParent(parent uintptr) error {
	return operations.step(fmt.Sprintf("parent:%d", parent))
}

func (operations *fakeMoveOperations) SetBounds(bounds moveBounds) error {
	return operations.step(fmt.Sprintf("bounds:%d,%d,%d,%d", bounds.left, bounds.top, bounds.right, bounds.bottom))
}

func (operations *fakeMoveOperations) NotifyParentPosition() error { return operations.step("notify") }

func (operations *fakeMoveOperations) Show() error { return operations.step("show") }

func successfulMovePlan(operations *fakeMoveOperations) controllerMovePlan {
	return controllerMovePlan{
		controller:     operations,
		previousParent: 10,
		targetParent:   20,
		previousBounds: moveBounds{left: 1, top: 2, right: 301, bottom: 202},
		targetBounds:   moveBounds{left: 3, top: 4, right: 403, bottom: 304},
		revealTarget:   func() error { operations.events = append(operations.events, "reveal-target"); return nil },
		restorePrevious: func() error {
			operations.events = append(operations.events, "restore-previous")
			return nil
		},
		focusTarget: func() error { operations.events = append(operations.events, "focus-target"); return nil },
		focusPrevious: func() error {
			operations.events = append(operations.events, "focus-previous")
			return nil
		},
		restorePeers: func() error { operations.events = append(operations.events, "restore-peers"); return nil },
		commit:       func() error { operations.events = append(operations.events, "commit"); return nil },
		undoCommit: func() error {
			operations.events = append(operations.events, "undo-commit")
			return nil
		},
		retirePrevious: func() error { operations.events = append(operations.events, "retire-previous"); return nil },
		cleanupTarget:  func() error { operations.events = append(operations.events, "cleanup-target"); return nil },
		closeController: func() error {
			operations.events = append(operations.events, "close-controller")
			return nil
		},
	}
}

func TestControllerMoveCommitsBeforeRetiringPreviousHost(t *testing.T) {
	operations := newFakeMoveOperations()
	if err := runControllerMove(successfulMovePlan(operations)); err != nil {
		t.Fatalf("move failed: %v", err)
	}
	if got, want := operations.events, []string{
		"hide",
		"parent:20",
		"bounds:3,4,403,304",
		"notify",
		"reveal-target",
		"show",
		"focus-target",
		"commit",
		"retire-previous",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("move events = %v, want %v", got, want)
	}
}

func TestControllerMoveFailureRestoresPreviousHost(t *testing.T) {
	operations := newFakeMoveOperations()
	operations.failAt["notify"] = 1
	err := runControllerMove(successfulMovePlan(operations))
	if err == nil || errors.Is(err, ErrViewMoveRollbackFailed) {
		t.Fatalf("move error = %v, want recoverable target failure", err)
	}
	if got, want := operations.events[len(operations.events)-9:], []string{
		"hide",
		"parent:10",
		"bounds:1,2,301,202",
		"notify",
		"restore-previous",
		"show",
		"restore-peers",
		"focus-previous",
		"cleanup-target",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rollback events = %v, want %v", got, want)
	}
	if containsMoveEvent(operations.events, "close-controller") {
		t.Fatal("controller closed after a successful rollback")
	}
	if !containsMoveEvent(operations.events, "cleanup-target") {
		t.Fatal("target host was not cleaned up after a successful rollback")
	}
}

func TestControllerMoveCommitCancellationRestoresPreviousHost(t *testing.T) {
	operations := newFakeMoveOperations()
	cancelled := errors.New("move cancelled before commit")
	plan := successfulMovePlan(operations)
	plan.commit = func() error {
		operations.events = append(operations.events, "commit-cancelled")
		return cancelled
	}
	err := runControllerMove(plan)
	if !errors.Is(err, cancelled) || errors.Is(err, ErrViewMoveRollbackFailed) {
		t.Fatalf("commit cancellation error = %v, want restored original host", err)
	}
	if containsMoveEvent(operations.events, "commit") || containsMoveEvent(operations.events, "retire-previous") {
		t.Fatalf("move committed or retired prior host after cancellation: %v", operations.events)
	}
	if !containsMoveEvent(operations.events, "cleanup-target") {
		t.Fatalf("target host was not cleaned after commit cancellation: %v", operations.events)
	}
}

func TestControllerMoveRollbackFailureClosesOnceAndReportsRetryableError(t *testing.T) {
	operations := newFakeMoveOperations()
	operations.failAt["notify"] = 1
	operations.failAt["parent:10"] = 1
	plan := successfulMovePlan(operations)
	plan.closeController = func() error {
		operations.events = append(operations.events, "close-controller", "cleanup-target")
		return nil
	}
	err := runControllerMove(plan)
	if !errors.Is(err, ErrViewMoveRollbackFailed) {
		t.Fatalf("move error = %v, want explicit rollback failure", err)
	}
	if got := countMoveEvent(operations.events, "close-controller"); got != 1 {
		t.Fatalf("controller close count = %d, want 1", got)
	}
	if !containsMoveEvent(operations.events, "cleanup-target") {
		t.Fatal("target host was not cleaned up after rollback failure")
	}
	if closeIndex, cleanupIndex := eventIndex(operations.events, "close-controller"), eventIndex(operations.events, "cleanup-target"); closeIndex < 0 || cleanupIndex < closeIndex {
		t.Fatalf("controller and target cleanup order = %v, want controller closed before target host destruction", operations.events)
	}
}

func containsMoveEvent(events []string, want string) bool { return countMoveEvent(events, want) != 0 }

func countMoveEvent(events []string, want string) int {
	count := 0
	for _, event := range events {
		if event == want {
			count++
		}
	}
	return count
}

func eventIndex(events []string, want string) int {
	for index, event := range events {
		if event == want {
			return index
		}
	}
	return -1
}
