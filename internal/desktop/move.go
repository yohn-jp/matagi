package desktop

import (
	"errors"
	"fmt"
)

var ErrViewMoveRollbackFailed = errors.New("WebView2 view move failed and rollback failed")

type moveBounds struct {
	left, top, right, bottom int32
}

func validateViewMoveBounds(target ViewLocation, bounds DIPBounds) error {
	if target == ViewIntegrated {
		if bounds != (DIPBounds{}) {
			return errors.New("integrated WebView2 views cannot have detached-window bounds")
		}
		return nil
	}
	if bounds == (DIPBounds{}) {
		return nil
	}
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return errors.New("detached WebView2 bounds require positive width and height")
	}
	return nil
}

type controllerMoveOperations interface {
	Hide() error
	SetParent(uintptr) error
	SetBounds(moveBounds) error
	NotifyParentPosition() error
	Show() error
}

type controllerMovePlan struct {
	controller      controllerMoveOperations
	previousParent  uintptr
	targetParent    uintptr
	previousBounds  moveBounds
	targetBounds    moveBounds
	revealTarget    func() error
	beforeShow      func() error
	restorePrevious func() error
	focusTarget     func() error
	focusPrevious   func() error
	restorePeers    func() error
	commit          func() error
	undoCommit      func() error
	retirePrevious  func() error
	cleanupTarget   func() error
	closeController func() error
	cancelled       func() error
}

type viewMoveRollbackError struct {
	cause    error
	rollback error
}

func (err *viewMoveRollbackError) Error() string {
	return fmt.Sprintf("%v; close and retry the view: move error: %v; rollback error: %v", ErrViewMoveRollbackFailed, err.cause, err.rollback)
}

func (err *viewMoveRollbackError) Unwrap() []error {
	return []error{ErrViewMoveRollbackFailed, err.cause, err.rollback}
}

func runControllerMove(plan controllerMovePlan) error {
	fail := func(cause error, committed bool) error {
		var rollbackErr error
		attempt := func(stage string, operation func() error) {
			if operation == nil {
				return
			}
			if err := operation(); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("%s: %w", stage, err))
			}
		}

		attempt("hiding WebView2 controller for rollback", plan.controller.Hide)
		attempt("restoring WebView2 parent window", func() error { return plan.controller.SetParent(plan.previousParent) })
		attempt("restoring WebView2 bounds", func() error { return plan.controller.SetBounds(plan.previousBounds) })
		attempt("notifying WebView2 of its restored parent", plan.controller.NotifyParentPosition)
		if committed {
			attempt("restoring view host location", plan.undoCommit)
		}
		attempt("showing previous view host", plan.restorePrevious)
		attempt("restoring WebView2 visibility", plan.controller.Show)
		attempt("restoring sibling view visibility", plan.restorePeers)
		attempt("restoring focus to previous view host", plan.focusPrevious)
		if rollbackErr == nil {
			attempt("destroying unused target view host", plan.cleanupTarget)
		}
		if rollbackErr == nil {
			return cause
		}
		attempt("closing controller after rollback failure", plan.closeController)
		return &viewMoveRollbackError{cause: cause, rollback: rollbackErr}
	}

	if err := plan.controller.Hide(); err != nil {
		return fail(fmt.Errorf("hiding WebView2 controller before move: %w", err), false)
	}
	if err := moveCancellation(plan); err != nil {
		return fail(err, false)
	}
	if err := plan.controller.SetParent(plan.targetParent); err != nil {
		return fail(fmt.Errorf("reparenting WebView2 controller: %w", err), false)
	}
	if err := moveCancellation(plan); err != nil {
		return fail(err, false)
	}
	if err := plan.controller.SetBounds(plan.targetBounds); err != nil {
		return fail(fmt.Errorf("setting WebView2 bounds after reparent: %w", err), false)
	}
	if err := plan.controller.NotifyParentPosition(); err != nil {
		return fail(fmt.Errorf("notifying WebView2 of its new parent: %w", err), false)
	}
	if err := moveCancellation(plan); err != nil {
		return fail(err, false)
	}
	if plan.revealTarget != nil {
		if err := plan.revealTarget(); err != nil {
			return fail(fmt.Errorf("showing target view host: %w", err), false)
		}
	}
	if plan.beforeShow != nil {
		if err := plan.beforeShow(); err != nil {
			return fail(fmt.Errorf("preparing WebView2 visibility at target host: %w", err), false)
		}
	}
	if err := plan.controller.Show(); err != nil {
		return fail(fmt.Errorf("showing WebView2 controller after move: %w", err), false)
	}
	if plan.focusTarget != nil {
		if err := plan.focusTarget(); err != nil {
			return fail(fmt.Errorf("focusing moved WebView2 controller: %w", err), false)
		}
	}
	if err := moveCancellation(plan); err != nil {
		return fail(err, false)
	}
	committed := false
	if plan.commit != nil {
		if err := plan.commit(); err != nil {
			return fail(fmt.Errorf("committing WebView2 view location: %w", err), false)
		}
		committed = true
	}
	if plan.retirePrevious != nil {
		if err := plan.retirePrevious(); err != nil {
			return fail(fmt.Errorf("retiring previous view host: %w", err), committed)
		}
	}
	return nil
}

func moveCancellation(plan controllerMovePlan) error {
	if plan.cancelled == nil {
		return nil
	}
	return plan.cancelled()
}
