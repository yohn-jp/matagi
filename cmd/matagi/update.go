package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/ui"
	"github.com/yohn-jp/matagi/internal/update"
)

// cmdApplyUpdate is the internal replacement helper. The desktop starts a
// copy of its running executable after a verified update is staged. The helper
// resolves Matagi's state from the current user's configuration directory and
// accepts only the process ID and destination already bound by the ready
// record.
func cmdApplyUpdate(args []string) error {
	if runtime.GOOS != "windows" {
		return errors.New("apply-update is supported only on Windows")
	}
	root, err := config.UserStateRoot()
	if err != nil {
		return fmt.Errorf("locate Matagi user state: %w", err)
	}
	return runApplyUpdate(args, root, update.DefaultApplyEnv(), os.Stderr)
}

func runApplyUpdate(args []string, stateRoot string, env update.ApplyEnv, out io.Writer) error {
	fs := flag.NewFlagSet("apply-update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var pid int
	var target string
	fs.IntVar(&pid, "pid", 0, "process id of the application to wait for")
	fs.StringVar(&target, "target", "", "the executable to replace")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("apply-update takes no arguments")
	}
	if stateRoot == "" {
		return errors.New("Matagi user state root is required")
	}
	res := update.Apply(update.Plan{Home: stateRoot, PID: pid, Target: target}, env)
	fmt.Fprintf(out, "matagi: update %s: %s\n", res.Outcome, res.Message)
	if res.Outcome != update.OutcomeApplied {
		return errors.New(res.Message)
	}
	return nil
}

// desktopUpdates adapts the update subsystem to the desktop Updates surface.
// It adds no update policy: every network operation starts only when the
// operator requests it.
type desktopUpdates struct {
	svc  *update.Service
	quit func()
}

const updateQuitDelay = 750 * time.Millisecond

func (m desktopUpdates) Status() update.Status             { return m.svc.Status() }
func (m desktopUpdates) SetChannel(c update.Channel) error { return m.svc.SetChannel(c) }
func (m desktopUpdates) Check() error                      { return m.svc.StartCheck() }
func (m desktopUpdates) Download(tag string) error         { return m.svc.StartDownload(tag) }

func (m desktopUpdates) Install() error {
	if err := m.svc.Install(); err != nil {
		return err
	}
	if m.quit != nil {
		time.AfterFunc(updateQuitDelay, m.quit)
	}
	return nil
}

// newDesktopUpdates composes the updater over Matagi's one user-state root.
// The helper is enabled only on Windows, where the executable replacement
// semantics are supported.
func newDesktopUpdates(root string, store update.Store, quit func()) ui.Updates {
	exe, err := os.Executable()
	if err == nil {
		exe = filepath.Clean(exe)
	} else {
		exe = ""
	}
	var startHelper func(string, []string) error
	if runtime.GOOS == "windows" {
		startHelper = update.StartDetached
	}
	return desktopUpdates{
		svc: &update.Service{
			Home:        func() string { return root },
			Exe:         exe,
			Settings:    store,
			StartHelper: startHelper,
		},
		quit: quit,
	}
}
