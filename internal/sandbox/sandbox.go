// Package sandbox abstracts the remote code-execution sandbox. The production
// implementation is E2B (see e2b.go); tests use Fake.
package sandbox

import (
	"context"
	"errors"
)

// ErrNotFound means the sandbox no longer exists (expired, killed, or lost).
var ErrNotFound = errors.New("sandbox not found")

type Handle struct {
	ID string
	// Opaque, in-memory-only connection details. Never persisted.
	Domain       string
	EnvdToken    string
	TrafficToken string
}

type CreateOpts struct {
	TaskID     string
	TimeoutSec int
}

type ExecRequest struct {
	Code     string
	Language string // "python" (default), "bash", "javascript", ...
}

type ExecError struct {
	Name      string `json:"name,omitempty"`
	Value     string `json:"value,omitempty"`
	Traceback string `json:"traceback,omitempty"`
}

type ExecResult struct {
	Stdout   string     `json:"stdout"`
	Stderr   string     `json:"stderr"`
	Results  []string   `json:"results,omitempty"`
	ExitCode int        `json:"exit_code,omitempty"`
	Error    *ExecError `json:"error,omitempty"`
}

type Sandbox interface {
	// Create starts a new sandbox that auto-pauses (not kills) when idle.
	Create(ctx context.Context, o CreateOpts) (Handle, error)
	// Connect returns a handle for an existing sandbox, resuming it if paused.
	Connect(ctx context.Context, id string, timeoutSec int) (Handle, error)
	Exec(ctx context.Context, h Handle, r ExecRequest) (ExecResult, error)
	Pause(ctx context.Context, id string) error
	Kill(ctx context.Context, id string) error
}
