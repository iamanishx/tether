package task

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/iamanishx/tether/internal/sandbox"
	"github.com/iamanishx/tether/internal/store"
)

func newTestEngine(t *testing.T) (*Engine, *sandbox.Fake, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fk := sandbox.NewFake()
	return NewEngine(st, fk, Config{MaxAttempts: 3}), fk, st
}

func mustTask(t *testing.T, e *Engine) *Task {
	t.Helper()
	tk, err := e.CreateTask("job1", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestSubmitIsIdempotentOnActionID(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	ctx := context.Background()

	a1, replayed, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "a1", Code: "print(1)"})
	if err != nil || replayed || a1.Status != StatusSucceeded {
		t.Fatalf("first submit: %+v replayed=%v err=%v", a1, replayed, err)
	}
	a2, replayed, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "a1", Code: "print(1)"})
	if err != nil || !replayed || a2.Status != StatusSucceeded {
		t.Fatalf("second submit: %+v replayed=%v err=%v", a2, replayed, err)
	}
	if fk.ExecCount() != 1 {
		t.Fatalf("code ran %d times, want 1", fk.ExecCount())
	}
	if _, _, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "a1", Code: "print(2)"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("reusing an action id with different code: err=%v, want conflict", err)
	}
}

func TestLostResponseBecomesOutcomeUnknownAndBlocksCompletion(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	ctx := context.Background()
	fk.ExecFn = func(string) (sandbox.ExecResult, error) { return sandbox.ExecResult{}, errors.New("connection reset") }

	a, _, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "send-invoice", Code: "send()"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != StatusOutcomeUnknown {
		t.Fatalf("status=%s, want outcome_unknown", a.Status)
	}
	// Re-submitting must not re-run it.
	if _, replayed, _ := e.Submit(ctx, "job1", SubmitReq{ActionID: "send-invoice", Code: "send()"}); !replayed {
		t.Fatal("expected replay of recorded action")
	}
	if fk.ExecCount() != 1 {
		t.Fatalf("exec count %d, want 1", fk.ExecCount())
	}
	if _, err := e.Complete(ctx, "job1", StateSucceeded, "done"); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete with unresolved action: err=%v, want conflict", err)
	}
	// Reconcile: operator verified externally that it was sent.
	if _, err := e.Resolve(ctx, "job1", "send-invoice", "succeeded", ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("resolve without note: err=%v, want bad request", err)
	}
	if _, err := e.Resolve(ctx, "job1", "send-invoice", "succeeded", "checked billing dashboard, invoice exists"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Complete(ctx, "job1", StateSucceeded, "invoice INV-1 verified"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRetryRunsAgainAndRespectsMaxAttempts(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	ctx := context.Background()
	fk.ExecFn = func(string) (sandbox.ExecResult, error) { return sandbox.ExecResult{}, errors.New("boom") }
	if _, _, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "x", Code: "go()"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // attempts 2 and 3
		if _, err := e.Resolve(ctx, "job1", "x", "retry", "not applied, checked"); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	if _, err := e.Resolve(ctx, "job1", "x", "retry", "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("4th attempt: err=%v, want conflict", err)
	}
	if fk.ExecCount() != 3 {
		t.Fatalf("exec count %d, want 3", fk.ExecCount())
	}
}

func TestIdempotentExecErrorIsPlainFailure(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	fk.ExecFn = func(string) (sandbox.ExecResult, error) { return sandbox.ExecResult{}, errors.New("timeout") }
	a, _, _ := e.Submit(context.Background(), "job1", SubmitReq{ActionID: "r", Code: "read()", Idempotent: true})
	if a.Status != StatusFailed {
		t.Fatalf("status=%s, want failed", a.Status)
	}
}

func TestCodeErrorIsFailedWithResult(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	fk.ExecFn = func(string) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{Error: &sandbox.ExecError{Name: "ValueError", Value: "bad"}}, nil
	}
	a, _, _ := e.Submit(context.Background(), "job1", SubmitReq{ActionID: "r", Code: "raise"})
	if a.Status != StatusFailed || a.Result == nil || !strings.Contains(a.Error, "ValueError") {
		t.Fatalf("got %+v", a)
	}
}

func TestRecoverReclassifiesInFlightActions(t *testing.T) {
	e, fk, st := newTestEngine(t)
	// Simulate a crash: ledger says "running" for both, nothing recorded after.
	tk := &Task{ID: "job1", State: StateRunning, Actions: []*Action{
		{ID: "unsafe", Code: "pay()", Status: StatusRunning, Attempts: 1},
		{ID: "safe", Code: "read()", Idempotent: true, Status: StatusRunning, Attempts: 1},
		{ID: "safe-exhausted", Code: "read2()", Idempotent: true, Status: StatusRunning, Attempts: 3},
	}}
	if err := st.Save(tk.ID, tk); err != nil {
		t.Fatal(err)
	}

	replays, err := e.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if len(replays) != 1 || replays[0].ActionID != "safe" {
		t.Fatalf("replays=%+v, want only 'safe'", replays)
	}
	got, _ := e.Get("job1")
	if s := got.action("unsafe").Status; s != StatusOutcomeUnknown {
		t.Fatalf("unsafe=%s, want outcome_unknown", s)
	}
	if s := got.action("safe-exhausted").Status; s != StatusFailed {
		t.Fatalf("safe-exhausted=%s, want failed", s)
	}

	e.RunReplays(context.Background(), replays)
	got, _ = e.Get("job1")
	if s := got.action("safe").Status; s != StatusSucceeded {
		t.Fatalf("safe=%s, want succeeded after replay", s)
	}
	if got.action("safe").Attempts != 2 {
		t.Fatalf("attempts=%d, want 2", got.action("safe").Attempts)
	}
	if fk.ExecCount() != 1 || fk.Execs[0] != "read()" {
		t.Fatalf("execs=%v, want only the idempotent replay", fk.Execs)
	}
}

func TestApprovalFlow(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	ctx := context.Background()

	a, _, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "pub", Code: "publish()", RequiresApproval: true})
	if err != nil || a.Status != StatusPendingApproval {
		t.Fatalf("got %+v err=%v", a, err)
	}
	if tk, _ := e.Get("job1"); tk.State != StateWaitingForApproval {
		t.Fatalf("task state=%s", tk.State)
	}
	if fk.ExecCount() != 0 {
		t.Fatal("ran before approval")
	}
	if _, err := e.Complete(ctx, "job1", StateSucceeded, "x"); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete while pending: %v", err)
	}
	a, err = e.Approve(ctx, "job1", "pub", true, "ok by anish")
	if err != nil || a.Status != StatusSucceeded {
		t.Fatalf("approve: %+v err=%v", a, err)
	}
	if tk, _ := e.Get("job1"); tk.State != StateRunning {
		t.Fatalf("task state=%s", tk.State)
	}

	if _, _, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "del", Code: "delete()", RequiresApproval: true}); err != nil {
		t.Fatal(err)
	}
	a, err = e.Approve(ctx, "job1", "del", false, "no")
	if err != nil || a.Status != StatusRejected || fk.ExecCount() != 1 {
		t.Fatalf("reject: %+v err=%v execs=%d", a, err, fk.ExecCount())
	}
	if _, err := e.Approve(ctx, "job1", "del", true, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving a rejected action: %v", err)
	}
}

func TestCompleteNeedsEvidenceAndKillsSandbox(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	ctx := context.Background()
	if _, _, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "a", Code: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Complete(ctx, "job1", StateSucceeded, "  "); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("no evidence: %v", err)
	}
	tk, err := e.Complete(ctx, "job1", StateSucceeded, "report.pdf has 12 pages")
	if err != nil || tk.State != StateSucceeded {
		t.Fatalf("complete: %+v %v", tk, err)
	}
	if len(fk.Alive) != 0 {
		t.Fatalf("sandbox still alive: %v", fk.Alive)
	}
	if _, _, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "b", Code: "2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("submit to closed task: %v", err)
	}
}

func TestLostSandboxIsRecreatedAndRecorded(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	ctx := context.Background()
	e.Submit(ctx, "job1", SubmitReq{ActionID: "a", Code: "1"})
	first, _ := e.Get("job1")
	delete(fk.Alive, first.SandboxID) // sandbox vanished

	a, _, err := e.Submit(ctx, "job1", SubmitReq{ActionID: "b", Code: "2"})
	if err != nil || a.Status != StatusSucceeded {
		t.Fatalf("got %+v err=%v", a, err)
	}
	got, _ := e.Get("job1")
	if got.SandboxID == first.SandboxID {
		t.Fatal("sandbox id not replaced")
	}
	var lost bool
	for _, ev := range got.Events {
		lost = lost || ev.Kind == "sandbox_lost"
	}
	if !lost {
		t.Fatalf("no sandbox_lost event: %+v", got.Events)
	}
}

func TestCancelRejectsPendingAndIsIdempotent(t *testing.T) {
	e, fk, _ := newTestEngine(t)
	mustTask(t, e)
	ctx := context.Background()
	e.Submit(ctx, "job1", SubmitReq{ActionID: "a", Code: "1"})
	e.Submit(ctx, "job1", SubmitReq{ActionID: "p", Code: "2", RequiresApproval: true})
	tk, err := e.Cancel(ctx, "job1")
	if err != nil || tk.State != StateCancelled || tk.action("p").Status != StatusRejected {
		t.Fatalf("cancel: %+v %v", tk, err)
	}
	if len(fk.Alive) != 0 {
		t.Fatal("sandbox not killed")
	}
	if _, err := e.Cancel(ctx, "job1"); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestRejectsPathTraversalIDs(t *testing.T) {
	e, _, _ := newTestEngine(t)
	for _, id := range []string{"../x", "a/b", "", ".hidden", "a..b"} {
		if _, err := e.Get(id); !errors.Is(err, ErrBadRequest) {
			t.Errorf("Get(%q) err=%v, want bad request", id, err)
		}
	}
}
