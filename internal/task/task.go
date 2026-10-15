// Package task holds the durable task model and the engine that runs actions
// in a sandbox while keeping an exactly-recorded ledger of what happened.
package task

import (
	"errors"
	"time"

	"github.com/iamanishx/tether/internal/sandbox"
)

type State string

const (
	StateQueued             State = "queued"
	StateRunning            State = "running"
	StateWaitingForApproval State = "waiting_for_approval"
	StateSucceeded          State = "succeeded"
	StateFailed             State = "failed"
	StateCancelled          State = "cancelled"
)

func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

type Status string

const (
	// StatusRunning is written (and fsynced) BEFORE the sandbox call. If the
	// process dies while an action is in this state, the outcome is unknown.
	StatusRunning         Status = "running"
	StatusPendingApproval Status = "pending_approval"
	StatusSucceeded       Status = "succeeded"
	StatusFailed          Status = "failed"
	// StatusOutcomeUnknown: the call may or may not have taken effect. A human
	// or the agent must reconcile it before the task can complete.
	StatusOutcomeUnknown Status = "outcome_unknown"
	StatusRejected       Status = "rejected"
)

// Action is one ledger entry. ID is chosen by the caller and is the
// idempotency key: submitting the same ID again never runs the code twice.
type Action struct {
	ID               string              `json:"id"`
	Language         string              `json:"language,omitempty"`
	Code             string              `json:"code"`
	Idempotent       bool                `json:"idempotent"`
	RequiresApproval bool                `json:"requires_approval"`
	Status           Status              `json:"status"`
	Attempts         int                 `json:"attempts"`
	Result           *sandbox.ExecResult `json:"result,omitempty"`
	Error            string              `json:"error,omitempty"`
	Note             string              `json:"note,omitempty"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

type Event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail,omitempty"`
}

type Task struct {
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	State     State             `json:"state"`
	SandboxID string            `json:"sandbox_id,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Actions   []*Action         `json:"actions"`
	Events    []Event           `json:"events,omitempty"`
	Evidence  string            `json:"evidence,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

func (t *Task) action(id string) *Action {
	for _, a := range t.Actions {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// recompute derives the non-terminal state from the ledger.
func (t *Task) recompute() {
	if t.State.Terminal() {
		return
	}
	switch {
	case t.hasStatus(StatusPendingApproval):
		t.State = StateWaitingForApproval
	case len(t.Actions) > 0:
		t.State = StateRunning
	default:
		t.State = StateQueued
	}
}

func (t *Task) hasStatus(s Status) bool {
	for _, a := range t.Actions {
		if a.Status == s {
			return true
		}
	}
	return false
}

// unresolved lists actions that block completion.
func (t *Task) unresolved() []string {
	var ids []string
	for _, a := range t.Actions {
		switch a.Status {
		case StatusRunning, StatusPendingApproval, StatusOutcomeUnknown:
			ids = append(ids, a.ID)
		}
	}
	return ids
}

var (
	ErrBadRequest = errors.New("bad request")
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
)
