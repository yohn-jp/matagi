package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yohn-jp/matagi/internal/statefile"
	"github.com/yohn-jp/matagi/internal/update"
)

func TestApplyUpdateHelperReplacesAndRestartsWithoutArguments(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "matagi.exe")
	oldBytes := []byte("MZ previous Matagi executable")
	newBytes := []byte("MZ verified Matagi executable")
	if err := os.WriteFile(target, oldBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	version, err := update.ParseVersion("0.1.5-dev")
	if err != nil {
		t.Fatal(err)
	}
	staged := update.StagedPath(root, version)
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, newBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(newBytes)
	ready := update.Ready{
		Schema: "matagi.update-ready/1", Tag: version.String(), Asset: update.ExeAsset,
		SHA256: hex.EncodeToString(digest[:]), Size: int64(len(newBytes)), Target: target,
		Created: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC),
	}
	if err := os.MkdirAll(update.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := statefile.WriteJSON(filepath.Join(update.Dir(root), "ready.json"), ready); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid() + 1
	var restartExe string
	var restartArgs []string
	env := update.ApplyEnv{
		WaitExit: func(got int, _ time.Duration) error {
			if got != pid {
				t.Errorf("wait PID = %d, want %d", got, pid)
			}
			return nil
		},
		Restart: func(exe string, args []string) error {
			restartExe, restartArgs = exe, append([]string(nil), args...)
			return nil
		},
		Rename:      os.Rename,
		Sleep:       func(time.Duration) {},
		Now:         func() time.Time { return time.Date(2026, 10, 7, 0, 0, 1, 0, time.UTC) },
		ExitTimeout: time.Second,
	}
	var output bytes.Buffer
	args := []string{"--pid", strconv.Itoa(pid), "--target", target}
	if err := runApplyUpdate(args, root, env, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "matagi: update applied:") {
		t.Fatalf("helper output %q", output.String())
	}
	if restartExe != target || len(restartArgs) != 0 {
		t.Fatalf("restart = (%q, %q), want executable with no arguments", restartExe, restartArgs)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(newBytes) {
		t.Fatalf("replacement = %q, %v", got, err)
	}
	if got, err := os.ReadFile(target + ".old"); err != nil || string(got) != string(oldBytes) {
		t.Fatalf("recovery copy = %q, %v", got, err)
	}
	result, err := update.LoadResult(root)
	if err != nil || result.Outcome != update.OutcomeApplied || result.Tag != version.String() {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestApplyUpdateHelperRejectsUnexpectedFlagsAndArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--home", t.TempDir(), "--pid", "12", "--target", "matagi.exe"},
		{"--pid", "12", "--target", "matagi.exe", "extra"},
	} {
		var output bytes.Buffer
		if err := runApplyUpdate(args, t.TempDir(), update.ApplyEnv{}, &output); err == nil {
			t.Errorf("accepted helper arguments %q", args)
		}
		if output.Len() != 0 {
			t.Errorf("invalid arguments ran helper: %q", output.String())
		}
	}
}
