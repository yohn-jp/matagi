package ssh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResolveExecutableUsesSystemSSHName(t *testing.T) {
	path, err := resolveExecutable(func(name string) (string, error) {
		if name != "ssh" {
			t.Fatalf("looked up %q, want ssh", name)
		}
		return "/system/bin/ssh", nil
	})
	if err != nil {
		t.Fatalf("resolveExecutable() error = %v", err)
	}
	if path != "/system/bin/ssh" {
		t.Fatalf("resolveExecutable() = %q, want /system/bin/ssh", path)
	}
}

func TestResolveExecutableReportsMissingSSH(t *testing.T) {
	_, err := resolveExecutable(func(string) (string, error) {
		return "", exec.ErrNotFound
	})
	var sshErr *Error
	if !errors.As(err, &sshErr) || sshErr.Kind != FailureExecutableLookup {
		t.Fatalf("resolveExecutable() error = %v, want executable lookup error", err)
	}
}

func TestRunPassesTargetAndCommandAsSeparateArguments(t *testing.T) {
	wantArgs := []string{"dev-box", "printf", "%s %s", "hello world", "$(touch /tmp/not-run)"}
	var gotArgs []string
	client := newClient("/system/bin/ssh", func(_ context.Context, path string, args []string) processResult {
		if path != "/system/bin/ssh" {
			t.Errorf("executable = %q, want /system/bin/ssh", path)
		}
		gotArgs = append([]string(nil), args...)
		return processResult{stdout: []byte("ok\n"), stderr: []byte("notice\n"), exitCode: 0}
	})

	result, err := client.Run(context.Background(), "dev-box", []string{"printf", "%s %s", "hello world", "$(touch /tmp/not-run)"}, time.Second)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("process args = %#v, want %#v", gotArgs, wantArgs)
	}
	if string(result.Stdout) != "ok\n" || string(result.Stderr) != "notice\n" || result.ExitCode != 0 {
		t.Fatalf("Run() result = %#v", result)
	}
}

func TestRunClassifiesRemoteAndTransportFailures(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		wantKind FailureKind
	}{
		{name: "remote command", exitCode: 7, wantKind: FailureRemoteCommand},
		{name: "OpenSSH transport", exitCode: 255, wantKind: FailureTransport},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newClient("ssh", func(context.Context, string, []string) processResult {
				return processResult{stdout: []byte("partial"), stderr: []byte("failure details"), exitCode: test.exitCode, err: fmt.Errorf("exit status %d", test.exitCode)}
			})
			result, err := client.Run(context.Background(), "dev-box", []string{"false"}, time.Second)
			var sshErr *Error
			if !errors.As(err, &sshErr) || sshErr.Kind != test.wantKind {
				t.Fatalf("Run() error = %v, want kind %q", err, test.wantKind)
			}
			if result.ExitCode != test.exitCode || string(result.Stdout) != "partial" || string(result.Stderr) != "failure details" {
				t.Fatalf("Run() result = %#v", result)
			}
		})
	}
}

func TestRunRejectsUnboundedOrOptionLikeRequests(t *testing.T) {
	client := newClient("ssh", func(context.Context, string, []string) processResult {
		t.Fatal("process runner called for invalid request")
		return processResult{}
	})
	for _, test := range []struct {
		name    string
		target  string
		command []string
		timeout time.Duration
	}{
		{name: "empty target", target: "", command: []string{"true"}, timeout: time.Second},
		{name: "option-like target", target: "-oStrictHostKeyChecking=no", command: []string{"true"}, timeout: time.Second},
		{name: "empty command", target: "dev-box", timeout: time.Second},
		{name: "no timeout", target: "dev-box", command: []string{"true"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.Run(context.Background(), test.target, test.command, test.timeout)
			var sshErr *Error
			if !errors.As(err, &sshErr) || sshErr.Kind != FailureInvalidRequest {
				t.Fatalf("Run() error = %v, want invalid request", err)
			}
		})
	}
}

func TestRunTimeoutAndCancellationStopTheOwnedProcess(t *testing.T) {
	tests := []struct {
		name       string
		cancel     bool
		wantKind   FailureKind
		requestTTL time.Duration
	}{
		{name: "timeout", wantKind: FailureTimeout, requestTTL: 2 * time.Second},
		{name: "caller cancellation", cancel: true, wantKind: FailureCanceled, requestTTL: 10 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			startedFile := filepath.Join(t.TempDir(), "started")
			client := newClient(os.Args[0], func(ctx context.Context, _ string, args []string) processResult {
				helperArgs := []string{"-test.run=^TestSSHProcessHelper$", "--", "wait", startedFile}
				return runProcess(ctx, os.Args[0], helperArgs)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancel {
				go func() {
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					timeout := time.NewTimer(5 * time.Second)
					defer timeout.Stop()
					for {
						select {
						case <-ticker.C:
							if _, err := os.Stat(startedFile); err == nil {
								cancel()
								return
							}
						case <-timeout.C:
							cancel()
							return
						}
					}
				}()
			}

			started := time.Now()
			_, err := client.Run(ctx, "dev-box", []string{"wait", startedFile}, test.requestTTL)
			elapsed := time.Since(started)
			var sshErr *Error
			if !errors.As(err, &sshErr) || sshErr.Kind != test.wantKind {
				t.Fatalf("Run() error = %v, want kind %q", err, test.wantKind)
			}
			if _, statErr := os.Stat(startedFile); statErr != nil {
				t.Fatalf("helper process did not start: %v", statErr)
			}
			if elapsed > 6*time.Second {
				t.Fatalf("Run() took %s to stop the owned process", elapsed)
			}
		})
	}
}

func TestSSHProcessHelper(t *testing.T) {
	marker := -1
	for index, arg := range os.Args {
		if arg == "--" {
			marker = index
			break
		}
	}
	if marker < 0 || marker+1 >= len(os.Args) {
		return
	}

	mode := os.Args[marker+1]
	switch mode {
	case "wait":
		if marker+2 < len(os.Args) {
			if err := os.WriteFile(os.Args[marker+2], []byte("started"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		for {
			time.Sleep(time.Hour)
		}
	case "exit":
		fmt.Fprint(os.Stdout, "helper stdout")
		fmt.Fprint(os.Stderr, "helper stderr")
		os.Exit(17)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode:", strings.Join(os.Args[marker+1:], " "))
		os.Exit(2)
	}
}

func TestRunProcessCapturesOutputAndExitCode(t *testing.T) {
	result := runProcess(context.Background(), os.Args[0], []string{"-test.run=^TestSSHProcessHelper$", "--", "exit"})
	if result.exitCode != 17 || result.err == nil {
		t.Fatalf("runProcess() exitCode = %d, err = %v; want exit status 17", result.exitCode, result.err)
	}
	if string(result.stdout) != "helper stdout" || string(result.stderr) != "helper stderr" {
		t.Fatalf("runProcess() output = stdout %q, stderr %q", result.stdout, result.stderr)
	}
}
