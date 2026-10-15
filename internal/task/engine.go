package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iamanishx/tether/internal/sandbox"
	"github.com/iamanishx/tether/internal/store"
)

type Config struct {
	SandboxTTLSec int           // sandbox lifetime before E2B auto-pauses it
	ExecTimeout   time.Duration // budget for one action, including sandbox (re)connect
	MaxAttempts   int           // per action, persisted, survives restarts
	MaxActions    int           // per task
}

func (c Config) withDefaults() Config {
	if c.SandboxTTLSec <= 0 {
		c.SandboxTTLSec = 300
	}
	if c.ExecTimeout <= 0 {
		c.ExecTimeout = 2 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.MaxActions <= 0 {
		c.MaxActions = 200
	}
	return c
}

type Engine struct {
	st  *store.Store
	sb  sandbox.Sandbox
	cfg Config
	now func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewEngine(st *store.Store, sb sandbox.Sandbox, cfg Config) *Engine {
	return &Engine{st: st, sb: sb, cfg: cfg.withDefaults(), now: time.Now, locks: map[string]*sync.Mutex{}}
}

// lock serialises all mutations of one task (sandbox state is sequential too).
func (e *Engine) lock(id string) func() {
	e.mu.Lock()
	m, ok := e.locks[id]
	if !ok {
		m = &sync.Mutex{}
		e.locks[id] = m
	}
	e.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (e *Engine) load(id string) (*Task, error) {
	var t Task
	if err := e.st.Load(id, &t); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: task %s", ErrNotFound, id)
		}
		return nil, err
	}
	return &t, nil
}

func (e *Engine) save(t *Task) error {
	t.UpdatedAt = e.now()
	return e.st.Save(t.ID, t)
}

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// CreateTask is idempotent on id: creating an existing task returns it.
func (e *Engine) CreateTask(id, title string, meta map[string]string) (*Task, error) {
	if id == "" {
		id = newID("t_")
	}
	if !store.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid task id", ErrBadRequest)
	}
	defer e.lock(id)()
	if t, err := e.load(id); err == nil {
		return t, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	now := e.now()
	t := &Task{ID: id, Title: title, State: StateQueued, Metadata: meta, Actions: []*Action{}, CreatedAt: now}
	return t, e.save(t)
}

func (e *Engine) Get(id string) (*Task, error) {
	if !store.ValidID(id) {
		return nil, fmt.Errorf("%w: invalid task id", ErrBadRequest)
	}
	return e.load(id)
}

func (e *Engine) List() ([]*Task, error) {
	ids, err := e.st.IDs()
	if err != nil {
		return nil, err
	}
	out := make([]*Task, 0, len(ids))
	for _, id := range ids {
		if t, err := e.load(id); err == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

type SubmitReq struct {
	ActionID         string
	Language         string
	Code             string
	Idempotent       bool // safe to replay automatically after a crash
	RequiresApproval bool
}

// Submit records the intent, then (unless approval is required) runs the
// action. Submitting an existing ActionID returns the recorded action and
// never re-executes it; replayed is true in that case.
func (e *Engine) Submit(ctx context.Context, taskID string, r SubmitReq) (a *Action, replayed bool, err error) {
	if !store.ValidID(taskID) {
		return nil, false, fmt.Errorf("%w: invalid task id", ErrBadRequest)
	}
	defer e.lock(taskID)()
	t, err := e.load(taskID)
	if err != nil {
		return nil, false, err
	}
	if !store.ValidID(r.ActionID) {
		return nil, false, fmt.Errorf("%w: action_id is required and must match [A-Za-z0-9._-]", ErrBadRequest)
	}
	if existing := t.action(r.ActionID); existing != nil {
		if existing.Code != r.Code || existing.Language != r.Language {
			return nil, false, fmt.Errorf("%w: action_id %s already used with a different request", ErrConflict, r.ActionID)
		}
		return existing, true, nil
	}
	if strings.TrimSpace(r.Code) == "" {
		return nil, false, fmt.Errorf("%w: code is required", ErrBadRequest)
	}
	if t.State.Terminal() {
		return nil, false, fmt.Errorf("%w: task is %s", ErrConflict, t.State)
	}
	if len(t.Actions) >= e.cfg.MaxActions {
		return nil, false, fmt.Errorf("%w: task reached max_actions (%d)", ErrConflict, e.cfg.MaxActions)
	}

	now := e.now()
	a = &Action{
		ID: r.ActionID, Language: r.Language, Code: r.Code,
		Idempotent: r.Idempotent, RequiresApproval: r.RequiresApproval,
		CreatedAt: now, UpdatedAt: now,
	}
	if r.RequiresApproval {
		a.Status = StatusPendingApproval
	} else {
		a.Status, a.Attempts = StatusRunning, 1
	}
	t.Actions = append(t.Actions, a)
	t.recompute()
	if err := e.save(t); err != nil { // intent is durable before anything runs
		return nil, false, err
	}
	if a.Status == StatusPendingApproval {
		return a, false, nil
	}
	return a, false, e.execute(ctx, t, a)
}

// Approve runs or rejects an action that was submitted with requires_approval.
func (e *Engine) Approve(ctx context.Context, taskID, actionID string, approve bool, note string) (*Action, error) {
	defer e.lock(taskID)()
	t, a, err := e.find(taskID, actionID)
	if err != nil {
		return nil, err
	}
	if a.Status != StatusPendingApproval {
		return nil, fmt.Errorf("%w: action is %s, not pending_approval", ErrConflict, a.Status)
	}
	a.Note, a.UpdatedAt = note, e.now()
	if !approve {
		a.Status = StatusRejected
		t.recompute()
		return a, e.save(t)
	}
	a.Status, a.Attempts = StatusRunning, 1
	t.recompute()
	if err := e.save(t); err != nil {
		return nil, err
	}
	return a, e.execute(ctx, t, a)
}

// Resolve reconciles an action whose outcome is unknown (or that failed):
// record what really happened, or explicitly retry it.
func (e *Engine) Resolve(ctx context.Context, taskID, actionID, outcome, note string) (*Action, error) {
	defer e.lock(taskID)()
	t, a, err := e.find(taskID, actionID)
	if err != nil {
		return nil, err
	}
	if t.State.Terminal() {
		return nil, fmt.Errorf("%w: task is %s", ErrConflict, t.State)
	}
	if a.Status != StatusOutcomeUnknown && a.Status != StatusFailed {
		return nil, fmt.Errorf("%w: action is %s; only outcome_unknown or failed can be resolved", ErrConflict, a.Status)
	}
	if strings.TrimSpace(note) == "" {
		return nil, fmt.Errorf("%w: note is required (say how you verified the outcome)", ErrBadRequest)
	}
	a.Note, a.UpdatedAt = note, e.now()
	switch outcome {
	case "succeeded":
		a.Status = StatusSucceeded
	case "failed":
		a.Status = StatusFailed
	case "retry":
		if a.Attempts >= e.cfg.MaxAttempts {
			return nil, fmt.Errorf("%w: max attempts (%d) reached", ErrConflict, e.cfg.MaxAttempts)
		}
		a.Status, a.Error, a.Result = StatusRunning, "", nil
		a.Attempts++
	default:
		return nil, fmt.Errorf("%w: outcome must be succeeded, failed or retry", ErrBadRequest)
	}
	t.recompute()
	if err := e.save(t); err != nil {
		return nil, err
	}
	if a.Status == StatusRunning {
		return a, e.execute(ctx, t, a)
	}
	return a, nil
}

// Complete closes a task. It refuses while any action is unresolved and
// requires evidence for success, so "the agent says done" is not enough.
func (e *Engine) Complete(ctx context.Context, taskID string, state State, evidence string) (*Task, error) {
	if state != StateSucceeded && state != StateFailed {
		return nil, fmt.Errorf("%w: state must be succeeded or failed", ErrBadRequest)
	}
	defer e.lock(taskID)()
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if t.State.Terminal() {
		return nil, fmt.Errorf("%w: task is already %s", ErrConflict, t.State)
	}
	if u := t.unresolved(); len(u) > 0 {
		return nil, fmt.Errorf("%w: unresolved actions: %s", ErrConflict, strings.Join(u, ", "))
	}
	if state == StateSucceeded && strings.TrimSpace(evidence) == "" {
		return nil, fmt.Errorf("%w: evidence is required to mark a task succeeded", ErrBadRequest)
	}
	t.State, t.Evidence = state, evidence
	if err := e.save(t); err != nil {
		return nil, err
	}
	e.releaseSandbox(ctx, t)
	return t, nil
}

// Cancel stops a task, rejects pending approvals and kills its sandbox.
func (e *Engine) Cancel(ctx context.Context, taskID string) (*Task, error) {
	defer e.lock(taskID)()
	t, err := e.load(taskID)
	if err != nil {
		return nil, err
	}
	if t.State.Terminal() {
		return t, nil
	}
	for _, a := range t.Actions {
		if a.Status == StatusPendingApproval {
			a.Status, a.Note, a.UpdatedAt = StatusRejected, "task cancelled", e.now()
		}
	}
	t.State = StateCancelled
	if err := e.save(t); err != nil {
		return nil, err
	}
	e.releaseSandbox(ctx, t)
	return t, nil
}

func (e *Engine) find(taskID, actionID string) (*Task, *Action, error) {
	t, err := e.load(taskID)
	if err != nil {
		return nil, nil, err
	}
	a := t.action(actionID)
	if a == nil {
		return nil, nil, fmt.Errorf("%w: action %s", ErrNotFound, actionID)
	}
	return t, a, nil
}

// execute runs a (already persisted as running) action and records the outcome.
// The task lock must be held. The call is detached from the caller's
// cancellation so a dropped HTTP client cannot leave the ledger without an outcome.
func (e *Engine) execute(ctx context.Context, t *Task, a *Action) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cfg.ExecTimeout)
	defer cancel()

	h, err := e.ensureSandbox(ctx, t)
	if err != nil {
		// Nothing was sent to the sandbox, so this is a definite, retryable failure.
		a.Status, a.Error = StatusFailed, "sandbox unavailable, nothing was executed: "+err.Error()
		return e.finish(t, a)
	}

	res, err := e.sb.Exec(ctx, h, sandbox.ExecRequest{Code: a.Code, Language: a.Language})
	switch {
	case err != nil && a.Idempotent:
		a.Status, a.Error = StatusFailed, "execution error (action is idempotent, safe to retry): "+err.Error()
	case err != nil:
		a.Status = StatusOutcomeUnknown
		a.Error = "no confirmed result; the action may have taken effect, reconcile before retrying: " + err.Error()
	case res.Error != nil:
		a.Status, a.Result = StatusFailed, &res
		a.Error = strings.TrimSpace(res.Error.Name + ": " + res.Error.Value)
	default:
		a.Status, a.Result, a.Error = StatusSucceeded, &res, ""
	}
	return e.finish(t, a)
}

func (e *Engine) finish(t *Task, a *Action) error {
	a.UpdatedAt = e.now()
	t.recompute()
	return e.save(t)
}

// ensureSandbox connects to (resuming if paused) or creates the task's sandbox
// and persists its id before first use.
func (e *Engine) ensureSandbox(ctx context.Context, t *Task) (sandbox.Handle, error) {
	if t.SandboxID != "" {
		h, err := e.sb.Connect(ctx, t.SandboxID, e.cfg.SandboxTTLSec)
		if err == nil {
			return h, nil
		}
		if !errors.Is(err, sandbox.ErrNotFound) {
			return sandbox.Handle{}, err
		}
		t.Events = append(t.Events, Event{At: e.now(), Kind: "sandbox_lost",
			Detail: "sandbox " + t.SandboxID + " no longer exists; in-sandbox files and variables are gone"})
	}
	h, err := e.sb.Create(ctx, sandbox.CreateOpts{TaskID: t.ID, TimeoutSec: e.cfg.SandboxTTLSec})
	if err != nil {
		return sandbox.Handle{}, err
	}
	t.SandboxID = h.ID
	t.Events = append(t.Events, Event{At: e.now(), Kind: "sandbox_created", Detail: h.ID})
	if err := e.save(t); err != nil {
		_ = e.sb.Kill(ctx, h.ID)
		return sandbox.Handle{}, err
	}
	return h, nil
}

func (e *Engine) releaseSandbox(ctx context.Context, t *Task) {
	if t.SandboxID == "" {
		return
	}
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	err := e.sb.Kill(kctx, t.SandboxID)
	if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
		log.Printf("task %s: kill sandbox %s: %v", t.ID, t.SandboxID, err)
		return
	}
	t.Events = append(t.Events, Event{At: e.now(), Kind: "sandbox_killed", Detail: t.SandboxID})
	_ = e.save(t)
}

// Replay identifies an idempotent action to re-run after recovery.
type Replay struct{ TaskID, ActionID string }

// Recover must run once at startup, before serving requests. Actions that
// were in flight when the process stopped are reclassified: idempotent ones
// are queued for replay (bounded by MaxAttempts); all others become
// outcome_unknown and wait for reconciliation.
func (e *Engine) Recover() ([]Replay, error) {
	ids, err := e.st.IDs()
	if err != nil {
		return nil, err
	}
	var replays []Replay
	for _, id := range ids {
		if err := func() error {
			defer e.lock(id)()
			t, err := e.load(id)
			if err != nil {
				return err
			}
			changed := false
			for _, a := range t.Actions {
				if a.Status != StatusRunning {
					continue
				}
				changed = true
				switch {
				case a.Idempotent && a.Attempts < e.cfg.MaxAttempts:
					a.Attempts++
					a.Note = "interrupted by restart; replaying (idempotent)"
					replays = append(replays, Replay{TaskID: t.ID, ActionID: a.ID})
				case a.Idempotent:
					a.Status, a.Error = StatusFailed, "interrupted by restart; max attempts reached"
				default:
					a.Status = StatusOutcomeUnknown
					a.Error = "process stopped while the action was in flight; outcome unknown, reconcile before retrying"
				}
				a.UpdatedAt = e.now()
			}
			if changed {
				t.Events = append(t.Events, Event{At: e.now(), Kind: "recovered", Detail: "reclassified in-flight actions after restart"})
				return e.save(t)
			}
			return nil
		}(); err != nil {
			return replays, fmt.Errorf("recover %s: %w", id, err)
		}
	}
	return replays, nil
}

// RunReplays executes the replays returned by Recover.
func (e *Engine) RunReplays(ctx context.Context, rs []Replay) {
	for _, r := range rs {
		func() {
			defer e.lock(r.TaskID)()
			t, a, err := e.find(r.TaskID, r.ActionID)
			if err != nil || a.Status != StatusRunning {
				return
			}
			if err := e.execute(ctx, t, a); err != nil {
				log.Printf("replay %s/%s: %v", r.TaskID, r.ActionID, err)
			}
		}()
	}
}
