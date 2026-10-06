//go:build windows

package harness

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/test/windows-e2e/identity"
)

var user32 = syscall.NewLazyDLL("user32.dll")
var findWindowEx = user32.NewProc("FindWindowExW")
var getWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
var postMessage = user32.NewProc("PostMessageW")

type Options struct {
	Snapshot   *registry.Snapshot
	PathPrefix string
	Env        map[string]string
}

// Start launches the downloaded production candidate with an isolated empty
// registry. It never points the process at a real development host.
func Start(t *testing.T) (apiURL, uiURL string) {
	t.Helper()
	return StartConfigured(t, Options{})
}

// StartConfigured launches the exact downloaded candidate with a caller-owned
// validated registry and optional deterministic transport fixture boundary.
func StartConfigured(t *testing.T, options Options) (apiURL, uiURL string) {
	t.Helper()
	dir := os.Getenv("MATAGI_E2E_CANDIDATE")
	source := os.Getenv("MATAGI_E2E_SOURCE")
	sum := os.Getenv("MATAGI_E2E_SHA256")
	if dir == "" && source == "" && sum == "" {
		t.Skip("portable candidate shard runs only with a downloaded candidate")
	}
	if _, err := identity.Verify(dir, source, sum); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("APPDATA", home)
	t.Setenv("LOCALAPPDATA", home)
	store, err := config.NewUserStore()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := options.Snapshot
	if snapshot == nil {
		snapshot, err = registry.NewSnapshot(nil, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Save(snapshot); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(dir, identity.File))
	cmd.Env = os.Environ()
	if options.PathPrefix != "" {
		cmd.Env = replaceEnv(cmd.Env, "PATH", options.PathPrefix+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	for key, value := range options.Env {
		cmd.Env = replaceEnv(cmd.Env, key, value)
	}
	out := new(strings.Builder)
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var hwnd uintptr
	t.Cleanup(func() {
		if hwnd != 0 {
			postMessage.Call(hwnd, 0x0010, 0, 0)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("candidate exit: %v (stderr: %s)", err, out.String())
			}
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("candidate failed bounded desktop shutdown")
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("candidate exited before ready: %v (stderr: %s)", err, out.String())
		default:
		}
		ports, err := listeningPorts(cmd.Process.Pid)
		if err == nil {
			for _, port := range ports {
				origin := "http://127.0.0.1:" + strconv.Itoa(port)
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				req, _ := http.NewRequestWithContext(ctx, "GET", origin+"/v1/state", nil)
				resp, err := client.Do(req)
				if err == nil {
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"version":1`) {
						apiURL = origin
					}
				}
				cancel()
				resp, err = client.Get(origin + "/")
				if err == nil {
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
					resp.Body.Close()
					if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "Matagi") {
						uiURL = origin
					}
				}
			}
			if hwnd == 0 {
				hwnd = candidateWindow(cmd.Process.Pid)
			}
			if apiURL != "" && uiURL != "" && hwnd != 0 {
				return apiURL, uiURL
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("candidate listeners/window not ready (stderr: %s)", out.String())
	return "", ""
}

func candidateWindow(pid int) uintptr {
	class, err := syscall.UTF16PtrFromString("MatagiWebView2Window")
	if err != nil {
		return 0
	}
	var after uintptr
	for {
		hwnd, _, _ := findWindowEx.Call(0, after, uintptr(unsafe.Pointer(class)), 0)
		if hwnd == 0 {
			return 0
		}
		var owner uint32
		getWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) == pid {
			return hwnd
		}
		after = hwnd
	}
}

func replaceEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, entry := range env {
		if len(entry) >= len(prefix) && strings.EqualFold(entry[:len(prefix)], prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func listeningPorts(pid int) ([]int, error) {
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return nil, err
	}
	var ports []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "TCP" || f[3] != "LISTENING" || f[4] != strconv.Itoa(pid) {
			continue
		}
		parts := strings.Split(f[1], ":")
		if len(parts) != 2 || parts[0] != "127.0.0.1" {
			continue
		}
		port, err := strconv.Atoi(parts[1])
		if err == nil {
			ports = append(ports, port)
		}
	}
	if len(ports) == 0 {
		return nil, errors.New("no IPv4 loopback listeners")
	}
	return ports, nil
}

func Get(t *testing.T, url string) string {
	t.Helper()
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %s %s", url, resp.Status, body)
	}
	return string(body)
}
