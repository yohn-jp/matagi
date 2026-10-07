package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yohn-jp/matagi/internal/ssh"
)

func TestAmbiguousSubmissionAndRotation(t *testing.T) {
	r, f := fixture(t)
	owner := r.services[key("env", "svc")].CorrelationOwner()
	var submissions []string
	attempt := 0
	f.fn = func(args []string) (ssh.Result, error) {
		switch args[1] {
		case "status":
			return ssh.Result{Stdout: []byte(`{"version":1,"status":{}}`)}, nil
		case "list":
			return ssh.Result{Stdout: []byte(`{"version":1,"runs":[],"nextCursor":""}`)}, nil
		case "run":
			for _, arg := range args {
				if strings.HasPrefix(arg, "--submission-id=") {
					submissions = append(submissions, arg)
				}
			}
			if !strings.Contains(strings.Join(args, " "), "--cwd=/work") || !strings.Contains(strings.Join(args, " "), "--correlation=owner="+owner) {
				t.Fatal(args)
			}
			attempt++
			if attempt < 3 {
				return ssh.Result{ExitCode: -1}, &ssh.Error{Kind: ssh.FailureTransport, Err: errors.New("lost response")}
			}
			return ssh.Result{Stdout: []byte(`{"version":1,"run":{"runId":"run","state":"running","spec":{"correlation":{"owner":"` + owner + `"}}}}`)}, nil
		}
		return ssh.Result{}, errors.New("unexpected command")
	}
	ctx := context.Background()
	var started Service
	for i := 0; i < 3; i++ {
		result, err := r.Start(ctx, "env", "svc")
		if i < 2 && err == nil || i == 2 && err != nil {
			t.Fatal(i, err)
		}
		if i == 2 {
			started = result
		}
	}
	if started.Process != "running" || started.State != "unknown" {
		t.Fatalf("Start() result = %#v; want direct Jinushi process state with readiness still unknown", started)
	}
	snapshot := r.Snapshot()
	if observed := snapshot.Environments[0].Services[0]; observed.Process != "running" || observed.State != "unknown" {
		t.Fatalf("immediate runtime snapshot = %#v; want direct Jinushi process state without invented readiness", observed)
	}
	if environment := snapshot.Environments[0]; environment.Connectivity != "connected" || environment.Jinushi != "ready" {
		t.Fatalf("immediate environment snapshot = %#v; successful Jinushi action did not refresh its authority state", environment)
	}
	if submissions[0] != submissions[1] || submissions[1] != submissions[2] || r.pending[key("env", "svc")] != "" {
		t.Fatal(submissions)
	}
	_, err := r.Start(ctx, "env", "svc")
	if err != nil || submissions[3] == submissions[2] {
		t.Fatal(submissions, err)
	}
	_ = r.Close(ctx)
}
