package task

import (
	"context"
	"errors"
	"testing"

	"github.com/iamanishx/tether/internal/sandbox"
)

func TestLostProcessStreamPreservesEvidenceWithoutSuccess(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	fk.ExecFn = func(string) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ProcessID: 123, Stdout: "side effect started\n", OutputTruncated: true}, errors.New("truncated Connect stream")
	}
	a, _, err := e.Submit(context.Background(), "job1", SubmitReq{ActionID: "unsafe", Code: "send()"})
	if err != nil || a.Status != StatusOutcomeUnknown || a.Result == nil || a.Result.ProcessID != 123 || a.Result.Stdout == "" || !a.Result.OutputTruncated {
		t.Fatalf("lost evidence or false success: %+v err=%v", a, err)
	}
	persisted, _ := e.Get("job1")
	if persisted.action("unsafe").Result.ProcessID != 123 {
		t.Fatal("process receipt not persisted")
	}
	if _, err := e.Complete(context.Background(), "job1", StateSucceeded, "pretend done"); !errors.Is(err, ErrConflict) {
		t.Fatal("unknown process outcome did not block completion")
	}
}

func TestActionIDBindsReplayAndApprovalPolicy(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	base := SubmitReq{ActionID: "write", Code: "send()", RequiresApproval: true}
	if _, _, err := e.Submit(context.Background(), "job1", base); err != nil {
		t.Fatal(err)
	}
	for _, altered := range []SubmitReq{
		{ActionID: "write", Code: "send()"},
		{ActionID: "write", Code: "send()", RequiresApproval: true, Idempotent: true},
	} {
		if _, _, err := e.Submit(context.Background(), "job1", altered); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed action policy accepted: %+v err=%v", altered, err)
		}
	}
	if fk.ExecCount() != 0 {
		t.Fatal("approval bypass executed a side effect")
	}
}
