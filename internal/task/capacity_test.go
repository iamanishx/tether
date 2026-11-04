package task

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iamanishx/tether/internal/sandbox"
	"github.com/iamanishx/tether/internal/store"
)

type countedPause struct {
	*sandbox.Fake
	pauses atomic.Int32
}

func (p *countedPause) Pause(context.Context, string) error { p.pauses.Add(1); return nil }

func TestGlobalAdmissionAndPausePreserveDistinctTasks(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fk := &countedPause{Fake: sandbox.NewFake()}
	var active, peak atomic.Int32
	fk.ExecFn = func(code string) (sandbox.ExecResult, error) {
		n := active.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return sandbox.ExecResult{Stdout: code}, nil
	}
	e := NewEngine(st, fk, Config{MaxConcurrentActions: 1, PauseAfterAction: true})
	var wg sync.WaitGroup
	for _, id := range []string{"alpha", "beta", "gamma"} {
		if _, err := e.CreateTask(id, id, nil); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if a, _, err := e.Submit(context.Background(), id, SubmitReq{ActionID: "a", Code: id}); err != nil || a.Status != StatusSucceeded {
				t.Errorf("%s: %+v %v", id, a, err)
			}
		}(id)
	}
	wg.Wait()
	if peak.Load() != 1 || fk.pauses.Load() != 3 {
		t.Fatalf("peak=%d pauses=%d", peak.Load(), fk.pauses.Load())
	}
	ids := map[string]bool{}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		task, _ := e.Get(id)
		ids[task.SandboxID] = true
		if task.Actions[0].Result.Stdout != id || task.Events[len(task.Events)-1].Kind != "sandbox_paused" {
			t.Fatalf("task/evidence mixup %+v", task)
		}
	}
	if len(ids) != 3 {
		t.Fatal("distinct tasks shared a sandbox")
	}
}

func TestCapacityTimeoutIsDefinitelyNotDispatched(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fk := sandbox.NewFake()
	entered := make(chan struct{})
	release := make(chan struct{})
	fk.ExecFn = func(string) (sandbox.ExecResult, error) { close(entered); <-release; return sandbox.ExecResult{}, nil }
	e := NewEngine(st, fk, Config{MaxConcurrentActions: 1, ExecTimeout: 30 * time.Millisecond})
	e.CreateTask("first", "", nil)
	e.CreateTask("second", "", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Submit(context.Background(), "first", SubmitReq{ActionID: "a", Code: "hold"})
	}()
	<-entered
	a, _, err := e.Submit(context.Background(), "second", SubmitReq{ActionID: "a", Code: "must not run"})
	if err != nil || a.Status != StatusFailed || fk.ExecCount() != 1 {
		t.Fatalf("a=%+v err=%v count=%d", a, err, fk.ExecCount())
	}
	close(release)
	<-done
}
