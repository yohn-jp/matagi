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

	"golang.org/x/sys/windows"

	"github.com/yohn-jp/matagi/internal/config"
	"github.com/yohn-jp/matagi/internal/registry"
	"github.com/yohn-jp/matagi/test/windows-e2e/identity"
)

var user32 = syscall.NewLazyDLL("user32.dll")
var findWindowEx = user32.NewProc("FindWindowExW")
var getWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
var postMessage = user32.NewProc("PostMessageW")

type Options struct {
	Snapshot     *registry.Snapshot
	PathPrefix   string
	UserStateDir string
	Env          map[string]string
}

// Session represents one exact production candidate process started by the
// Windows shard. Its state directory can be reused to prove persistence over
// an actual candidate restart.
type Session struct {
	APIURL  string
	UIURL   string
	cmd     *exec.Cmd
	exited  chan struct{}
	exitErr error
	hwnd    uintptr
	stderr  *strings.Builder
	closed  bool
}

// RestartedSession is a candidate process launched by the production update
// helper rather than by the Go test process.
type RestartedSession struct {
	PID    int
	APIURL string
	UIURL  string
	hwnd   uintptr
	closed bool
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
	session := StartSession(t, options)
	return session.APIURL, session.UIURL
}

// StartSession launches the exact downloaded candidate and returns a handle
// so tests can restart it with the same isolated user state.
func StartSession(t *testing.T, options Options) *Session {
	t.Helper()
	dir := os.Getenv("MATAGI_E2E_CANDIDATE")
	source := os.Getenv("MATAGI_E2E_SOURCE")
	sum := os.Getenv("MATAGI_E2E_SHA256")
	if dir == "" && source == "" && sum == "" {
		if os.Getenv("MATAGI_E2E_SHARD") != "" {
			t.Fatal("candidate shard is missing its downloaded candidate identity")
		}
		t.Skip("portable candidate test requires a downloaded candidate")
	}
	if _, err := identity.Verify(dir, source, sum); err != nil {
		t.Fatal(err)
	}

	stateHome := options.UserStateDir
	if stateHome == "" {
		stateHome = t.TempDir()
	} else if err := os.MkdirAll(stateHome, 0700); err != nil {
		t.Fatal(err)
	}
	profileHome, err := os.MkdirTemp("", "matagi-e2e-profile-*")
	if err != nil {
		t.Fatal(err)
	}
	// APPDATA owns the isolated Matagi config. LOCALAPPDATA owns the WebView2
	// profile and deliberately is not a testing.TempDir: Chromium may release
	// profile handles asynchronously after the candidate process exits, and
	// testing's immediate RemoveAll would turn that Windows sharing delay into
	// a false product failure.
	t.Setenv("APPDATA", stateHome)
	t.Setenv("LOCALAPPDATA", profileHome)
	t.Cleanup(func() { _ = os.RemoveAll(profileHome) })
	if options.Snapshot != nil {
		store, err := config.NewUserStore()
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(options.Snapshot); err != nil {
			t.Fatal(err)
		}
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
	session := &Session{cmd: cmd, exited: make(chan struct{}), stderr: out}
	go func() {
		session.exitErr = cmd.Wait()
		close(session.exited)
	}()
	t.Cleanup(func() { session.Close(t) })

	var apiURL, uiURL string
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		select {
		case <-session.exited:
			t.Fatalf("candidate exited before ready: %v (stderr: %s)", session.exitErr, out.String())
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
			if session.hwnd == 0 {
				session.hwnd = candidateWindow(cmd.Process.Pid)
			}
			if apiURL != "" && uiURL != "" && session.hwnd != 0 {
				session.APIURL, session.UIURL = apiURL, uiURL
				return session
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("candidate listeners/window not ready (stderr: %s)", out.String())
	return nil
}

// Close requests the candidate's normal Windows shutdown and waits for its
// process exit, killing only this isolated test process if its window does
// not close within the bounded deadline.
func (s *Session) Close(t *testing.T) {
	t.Helper()
	if s == nil || s.closed {
		return
	}
	s.closed = true
	select {
	case <-s.exited:
		if s.exitErr != nil {
			t.Errorf("candidate exit: %v (stderr: %s)", s.exitErr, s.stderr.String())
		}
		return
	default:
	}
	if s.hwnd != 0 {
		postMessage.Call(s.hwnd, 0x0010, 0, 0)
	}
	select {
	case <-s.exited:
		if s.exitErr != nil {
			t.Errorf("candidate exit: %v (stderr: %s)", s.exitErr, s.stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.exited
		t.Error("candidate failed bounded desktop shutdown")
	}
}

// WaitForExit waits for the candidate process to exit on its own. Update
// certification uses this after Install to prove the helper's handoff closed
// the running process before replacing its executable.
func (s *Session) WaitForExit(t *testing.T, timeout time.Duration) {
	t.Helper()
	if s == nil || s.exited == nil {
		t.Fatal("candidate session is not running")
	}
	select {
	case <-s.exited:
		if s.exitErr != nil {
			t.Fatalf("candidate exit: %v (stderr: %s)", s.exitErr, s.stderr.String())
		}
	case <-time.After(timeout):
		t.Fatalf("candidate process %d did not exit after update install", s.PID())
	}
}

// WaitForRestart finds a new Matagi desktop window running the exact target
// path and waits until its loopback API and UI are serving.
func WaitForRestart(t *testing.T, oldPID int, target string) *RestartedSession {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		hwnd, pid := candidateWindowForTarget(oldPID, target)
		if hwnd != 0 {
			apiURL, uiURL := candidateURLs(pid)
			if apiURL != "" && uiURL != "" {
				return &RestartedSession{PID: pid, APIURL: apiURL, UIURL: uiURL, hwnd: hwnd}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("production update helper did not restart Matagi from %s", target)
	return nil
}

// Close requests normal shutdown of a candidate restarted by the update
// helper and waits for that exact process to exit.
func (s *RestartedSession) Close(t *testing.T) {
	t.Helper()
	if s == nil || s.closed {
		return
	}
	s.closed = true
	process, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, uint32(s.PID))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return
		}
		t.Errorf("open restarted candidate process %d: %v", s.PID, err)
		return
	}
	defer windows.CloseHandle(process)
	postMessage.Call(s.hwnd, 0x0010, 0, 0)
	result, err := windows.WaitForSingleObject(process, 10_000)
	if err != nil {
		t.Errorf("wait for restarted candidate process %d: %v", s.PID, err)
		return
	}
	if result == uint32(windows.WAIT_TIMEOUT) {
		_ = windows.TerminateProcess(process, 1)
		_, _ = windows.WaitForSingleObject(process, 2_000)
		t.Errorf("restarted candidate process %d failed bounded desktop shutdown", s.PID)
	}
}

func candidateURLs(pid int) (apiURL, uiURL string) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for _, port := range listeningPortsForPID(pid) {
		origin := "http://127.0.0.1:" + strconv.Itoa(port)
		resp, err := client.Get(origin + "/v1/state")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"version":1`) {
				apiURL = origin
			}
		}
		resp, err = client.Get(origin + "/")
		if err == nil {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), "Matagi") {
				uiURL = origin
			}
		}
	}
	return apiURL, uiURL
}

func listeningPortsForPID(pid int) []int {
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return nil
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
	return ports
}

func candidateWindowForTarget(excludePID int, target string) (uintptr, int) {
	class, err := syscall.UTF16PtrFromString("MatagiWebView2Window")
	if err != nil {
		return 0, 0
	}
	var after uintptr
	for {
		hwnd, _, _ := findWindowEx.Call(0, after, uintptr(unsafe.Pointer(class)), 0)
		if hwnd == 0 {
			return 0, 0
		}
		var owner uint32
		getWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		pid := int(owner)
		if pid != excludePID {
			if path, err := processImagePath(pid); err == nil && strings.EqualFold(filepath.Clean(path), filepath.Clean(target)) {
				return hwnd, pid
			}
		}
		after = hwnd
	}
}

func processImagePath(pid int) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(process)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(process, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return syscall.UTF16ToString(buf[:size]), nil
}

// FormToken extracts the per-process token rendered by the local UI.
func FormToken(t *testing.T, baseURL string) string {
	t.Helper()
	body := Get(t, baseURL+"/")
	const marker = `name="token" value="`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatal("candidate page did not contain form token")
	}
	start += len(marker)
	end := strings.IndexByte(body[start:], '"')
	if end < 0 {
		t.Fatal("candidate page contained malformed form token")
	}
	token := body[start : start+end]
	if len(token) != 32 {
		t.Fatalf("unexpected form token length %d", len(token))
	}
	return token
}

// PID returns the process ID of a candidate started directly by this test.
func (s *Session) PID() int {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// CandidatePath returns the absolute path of the workflow's sole production
// candidate executable.
func CandidatePath() (string, error) {
	dir := os.Getenv("MATAGI_E2E_CANDIDATE")
	if dir == "" {
		return "", errors.New("candidate directory is not configured")
	}
	path, err := filepath.Abs(filepath.Join(dir, identity.File))
	if err != nil {
		return "", err
	}
	return filepath.Clean(path), nil
}

// RequireFixture returns the deterministic SSH boundary executable configured
// for the lifecycle shard. A missing fixture skips ordinary package tests but
// fails a candidate shard instead of recording skipped coverage as PASS.
func RequireFixture(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("MATAGI_E2E_SSH_FIXTURE_DIR")
	if dir == "" {
		if os.Getenv("MATAGI_E2E_SHARD") != "" {
			t.Fatal("candidate lifecycle shard is missing the deterministic SSH fixture")
		}
		t.Skip("candidate lifecycle test requires the deterministic SSH fixture")
	}
	return dir
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

func LaunchDuplicate(t *testing.T) {
	t.Helper()
	dir := os.Getenv("MATAGI_E2E_CANDIDATE")
	source := os.Getenv("MATAGI_E2E_SOURCE")
	sum := os.Getenv("MATAGI_E2E_SHA256")
	if _, err := identity.Verify(dir, source, sum); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, identity.File))
	cmd.Env = os.Environ()
	out := new(strings.Builder)
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("duplicate candidate exit: %v (stderr: %s)", err, out.String())
		}
	case <-time.After(12 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("duplicate candidate did not activate existing instance and exit (stderr: %s)", out.String())
	}
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
