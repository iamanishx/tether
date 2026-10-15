package sandbox

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Sandbox for tests. ExecFn lets a test script behaviour,
// including failing after the "side effect" to simulate a lost response.
type Fake struct {
	mu     sync.Mutex
	next   int
	Alive  map[string]bool
	Execs  []string // code of every Exec call, in order
	ExecFn func(code string) (ExecResult, error)
}

func NewFake() *Fake { return &Fake{Alive: map[string]bool{}} }

func (f *Fake) Create(_ context.Context, _ CreateOpts) (Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("fake-%d", f.next)
	f.Alive[id] = true
	return Handle{ID: id}, nil
}

func (f *Fake) Connect(_ context.Context, id string, _ int) (Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Alive[id] {
		return Handle{}, ErrNotFound
	}
	return Handle{ID: id}, nil
}

func (f *Fake) Exec(_ context.Context, _ Handle, r ExecRequest) (ExecResult, error) {
	f.mu.Lock()
	f.Execs = append(f.Execs, r.Code)
	fn := f.ExecFn
	f.mu.Unlock()
	if fn != nil {
		return fn(r.Code)
	}
	return ExecResult{Stdout: "ok:" + r.Code}, nil
}

func (f *Fake) Pause(context.Context, string) error { return nil }

func (f *Fake) Kill(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Alive, id)
	return nil
}

// ExecCount returns how many times Exec has been called.
func (f *Fake) ExecCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Execs)
}

var _ Sandbox = (*Fake)(nil)
