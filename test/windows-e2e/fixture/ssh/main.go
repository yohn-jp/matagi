package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

type receipt struct {
	Outcome  string `json:"outcome"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

type runSpec struct {
	Correlation map[string]string `json:"correlation"`
}

type run struct {
	ID         string   `json:"runId"`
	State      string   `json:"state"`
	Generation uint64   `json:"generation"`
	CreatedAt  string   `json:"createdAt"`
	Spec       runSpec  `json:"spec"`
	Receipt    *receipt `json:"receipt,omitempty"`
}

type fixtureState struct {
	Next   int    `json:"next"`
	Run    *run   `json:"run,omitempty"`
	Stdout string `json:"stdout,omitempty"`
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fail("missing arguments")
	}
	if args[0] == "-N" {
		logArgs(args)
		if err := forward(args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(255)
		}
		return
	}
	if len(args) < 2 {
		fail("missing remote command")
	}
	command, err := parseRemoteCommand(strings.Join(args[1:], " "))
	if err != nil {
		fail("invalid serialized remote command: " + err.Error())
	}
	// Log the effective remote argv after emulating the shell OpenSSH invokes
	// for its command string. This is not the raw process argv OpenSSH receives.
	logArgs(append([]string{args[0]}, command...))
	if len(command) == 1 && command[0] == "true" {
		return
	}
	if command[0] == "head" {
		readEndpointDescriptor(command)
		return
	}
	if command[0] != "jinushi" || len(command) < 2 {
		fail("unsupported remote command")
	}
	statePath := os.Getenv("MATAGI_E2E_SSH_STATE")
	if statePath == "" {
		fail("missing fixture state path")
	}
	state := loadState(statePath)
	switch command[1] {
	case "status":
		emit(map[string]any{"version": 1, "status": map[string]any{"state": "ready"}})
	case "list":
		runs := []run{}
		if state.Run != nil {
			runs = append(runs, *state.Run)
		}
		emit(map[string]any{"version": 1, "runs": runs, "nextCursor": ""})
	case "run":
		owner := correlationOwner(command[2:])
		if owner == "" {
			fail("missing owner correlation")
		}
		state.Next++
		state.Run = &run{
			ID:         fmt.Sprintf("fixture-run-%d", state.Next),
			State:      "running",
			Generation: 1,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			Spec:       runSpec{Correlation: map[string]string{"owner": owner}},
		}
		state.Stdout = "Dashboard: " + os.Getenv("MATAGI_E2E_ENDPOINT_URL") + "\n"
		saveState(statePath, state)
		emit(map[string]any{"version": 1, "run": state.Run})
	case "output":
		requireRun(state)
		if command[len(command)-1] != state.Run.ID {
			fail("output requested for a Run other than the managed Run")
		}
		emit(map[string]any{"version": 1, "data": base64.StdEncoding.EncodeToString([]byte(state.Stdout))})
	case "inspect":
		requireRun(state)
		emit(map[string]any{"version": 1, "run": state.Run})
	case "cancel":
		requireRun(state)
		code := 0
		state.Run.State = "terminal"
		state.Run.Receipt = &receipt{Outcome: "canceled", ExitCode: &code}
		saveState(statePath, state)
		emit(map[string]any{"version": 1, "run": state.Run})
	case "await":
		requireRun(state)
		emit(map[string]any{"version": 1, "run": state.Run})
	default:
		fail("unsupported Jinushi command")
	}
}

func parseRemoteCommand(serialized string) ([]string, error) {
	var arguments []string
	var current strings.Builder
	quoted := false
	started := false
	for index := 0; index < len(serialized); {
		character := serialized[index]
		if quoted {
			if character == '\'' {
				quoted = false
				index++
				continue
			}
			current.WriteByte(character)
			index++
			continue
		}

		switch character {
		case '\'':
			quoted = true
			started = true
			index++
		case '\\':
			if !started || index+1 >= len(serialized) || serialized[index+1] != '\'' {
				return nil, fmt.Errorf("unexpected escape at byte %d", index)
			}
			current.WriteByte('\'')
			index += 2
		case ' ':
			if !started {
				return nil, fmt.Errorf("unexpected separator at byte %d", index)
			}
			arguments = append(arguments, current.String())
			current.Reset()
			started = false
			index++
		default:
			return nil, fmt.Errorf("unexpected unquoted character %q at byte %d", character, index)
		}
	}
	if quoted {
		return nil, errors.New("unterminated single-quoted argument")
	}
	if !started {
		if len(arguments) == 0 {
			return nil, errors.New("empty remote command")
		}
		return nil, errors.New("trailing argument separator")
	}
	arguments = append(arguments, current.String())
	return arguments, nil
}

func readEndpointDescriptor(command []string) {
	if len(command) < 5 || command[1] != "-c" || command[3] != "--" || command[4] != os.Getenv("MATAGI_E2E_ENDPOINT_PATH") {
		fail("unexpected endpoint descriptor request")
	}
	url := os.Getenv("MATAGI_E2E_ENDPOINT_URL")
	if url == "" {
		fail("missing managed endpoint URL")
	}
	emit(map[string]any{"url": url})
}

func logArgs(args []string) {
	path := os.Getenv("MATAGI_E2E_SSH_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(args)
}

func loadState(path string) fixtureState {
	for attempt := 0; attempt < 20; attempt++ {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return fixtureState{}
		}
		if err == nil {
			var state fixtureState
			if json.Unmarshal(data, &state) == nil {
				return state
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	fail("could not read fixture state")
	return fixtureState{}
}

func saveState(path string, state fixtureState) {
	data, err := json.Marshal(state)
	if err != nil {
		fail(err.Error())
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		fail(err.Error())
	}
}

func requireRun(state fixtureState) {
	if state.Run == nil {
		fail("fixture has no run")
	}
}

func correlationOwner(args []string) string {
	const prefix = "--correlation=owner="
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return strings.TrimPrefix(arg, prefix)
		}
	}
	return ""
}

func emit(value any) {
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fail(err.Error())
	}
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(2)
}

func forward(args []string) error {
	var spec string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-L" {
			spec = args[i+1]
			break
		}
	}
	parts := strings.Split(spec, ":")
	if len(parts) != 4 || parts[0] != "127.0.0.1" || parts[2] != "127.0.0.1" {
		return errors.New("invalid fixture forwarding specification")
	}
	local := net.JoinHostPort(parts[0], parts[1])
	remote := net.JoinHostPort(parts[2], parts[3])
	listener, err := net.Listen("tcp4", local)
	if err != nil {
		return err
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go bridge(conn, remote)
	}
}

func bridge(client net.Conn, remote string) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp4", remote, 2*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
}
