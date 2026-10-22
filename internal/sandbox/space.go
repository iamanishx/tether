package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SPACE talks to a self-hosted Perplexity SPACE deployment
// (https://github.com/perplexityai/SPACE). Lifecycle calls return an
// operation that is polled to completion; command output arrives as SSE from
// the data plane, authenticated with a per-command attachment token.
type SPACE struct {
	apiURL       string // control plane origin, e.g. https://space.internal
	dataPlaneURL string // data plane origin, e.g. https://space-data.internal
	token        string
	template     string
	pollEvery    time.Duration
	http         *http.Client
}

// NewSPACE returns a SPACE client. Both URLs must be bare origins.
func NewSPACE(apiURL, dataPlaneURL, token, template string) (*SPACE, error) {
	for _, u := range []string{apiURL, dataPlaneURL} {
		if err := checkOrigin(u); err != nil {
			return nil, err
		}
	}
	if template == "" {
		template = "seed:latest"
	}
	return &SPACE{
		apiURL:       strings.TrimRight(apiURL, "/"),
		dataPlaneURL: strings.TrimRight(dataPlaneURL, "/"),
		token:        token,
		template:     template,
		pollEvery:    250 * time.Millisecond,
		http: &http.Client{
			Timeout: 0, // per-call deadlines come from ctx
			// Never follow a redirect with a bearer token attached.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func checkOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || strings.TrimRight(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("SPACE url %q must be an http(s) origin without credentials, path or query", raw)
	}
	return nil
}

type spaceOp struct {
	OperationID string `json:"operation_id"`
	SandboxID   string `json:"sandbox_id"`
}

type spaceOpRecord struct {
	State           string `json:"state"`
	ProgressMessage string `json:"progress_message"`
	TerminalError   any    `json:"terminal_error"`
}

type spaceSandbox struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	FailureCode *string `json:"failure_code"`
}

type spaceCommand struct {
	SessionID string `json:"session_id"`
	Attach    *struct {
		URL       string  `json:"url"`
		EventsURL *string `json:"events_url"`
		Token     string  `json:"token"`
	} `json:"attach"`
}

func (s *SPACE) Create(ctx context.Context, o CreateOpts) (Handle, error) {
	body := map[string]any{
		"template": s.template,
		"runtime":  "fc",
		"metadata": map[string]string{"tether_task_id": o.TaskID},
	}
	var op spaceOp
	if _, err := s.api(ctx, http.MethodPost, "/v1/sandboxes", body, &op, true); err != nil {
		return Handle{}, fmt.Errorf("create sandbox: %w", err)
	}
	if err := s.waitOp(ctx, op.OperationID); err != nil {
		return Handle{}, fmt.Errorf("create sandbox %s: %w", op.SandboxID, err)
	}
	if err := s.waitRunning(ctx, op.SandboxID); err != nil {
		return Handle{}, err
	}
	return Handle{ID: op.SandboxID}, nil
}

// Connect checks the sandbox and wakes it if it was paused or suspended.
func (s *SPACE) Connect(ctx context.Context, id string, _ int) (Handle, error) {
	sb, err := s.get(ctx, id)
	if err != nil {
		return Handle{}, err
	}
	switch sb.State {
	case "running":
		return Handle{ID: id}, nil
	case "paused", "suspended", "stopped":
		var op spaceOp
		status, err := s.api(ctx, http.MethodPost, "/v1/sandboxes/"+id+":wake", nil, &op, true)
		if status == http.StatusNotFound {
			return Handle{}, ErrNotFound
		}
		if err != nil {
			return Handle{}, fmt.Errorf("wake sandbox: %w", err)
		}
		if err := s.waitOp(ctx, op.OperationID); err != nil {
			return Handle{}, fmt.Errorf("wake sandbox %s: %w", id, err)
		}
	}
	if err := s.waitRunning(ctx, id); err != nil {
		return Handle{}, err
	}
	return Handle{ID: id}, nil
}

// Pause suspends the sandbox: VM state and filesystem are saved and resources released.
func (s *SPACE) Pause(ctx context.Context, id string) error {
	var op spaceOp
	status, err := s.api(ctx, http.MethodPost, "/v1/sandboxes/"+id+":suspend", nil, &op, true)
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.waitOp(ctx, op.OperationID)
}

func (s *SPACE) Kill(ctx context.Context, id string) error {
	var op spaceOp
	status, err := s.api(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, &op, true)
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.waitOp(ctx, op.OperationID)
}

// argv maps tether's language names onto a guest command line.
func argv(language, code string) ([]string, error) {
	switch strings.ToLower(language) {
	case "", "python", "python3":
		return []string{"python3", "-c", code}, nil
	case "bash", "sh", "shell":
		return []string{"sh", "-c", code}, nil
	case "javascript", "js", "node":
		return []string{"node", "-e", code}, nil
	}
	return nil, fmt.Errorf("SPACE backend does not support language %q (use python, bash or javascript)", language)
}

// Exec starts a command and consumes its SSE event stream to completion.
func (s *SPACE) Exec(ctx context.Context, h Handle, r ExecRequest) (ExecResult, error) {
	cmd, err := argv(r.Language, r.Code)
	if err != nil {
		return ExecResult{}, err
	}
	body := map[string]any{"command": cmd}
	if dl, ok := ctx.Deadline(); ok {
		if secs := int(time.Until(dl).Seconds()); secs > 0 {
			body["timeout"] = fmt.Sprintf("%ds", secs)
		}
	}
	var c spaceCommand
	if _, err := s.api(ctx, http.MethodPost, "/v1/sandboxes/"+h.ID+"/commands", body, &c, true); err != nil {
		return ExecResult{}, fmt.Errorf("start command: %w", err)
	}
	if c.Attach == nil || c.Attach.Token == "" {
		return ExecResult{}, errors.New("start command: response has no attachment token")
	}
	eventsPath := "/v1/sessions/" + c.SessionID + "/events"
	eventsURL := s.dataPlaneURL + eventsPath
	// Only ever send the attachment token to the configured data plane.
	if got := c.Attach.EventsURL; got != nil && *got != eventsPath && *got != eventsURL {
		return ExecResult{}, fmt.Errorf("start command: unexpected events url %q", *got)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventsURL, nil)
	if err != nil {
		return ExecResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Attach.Token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := s.http.Do(req)
	if err != nil {
		return ExecResult{}, fmt.Errorf("attach to command: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
		return ExecResult{}, fmt.Errorf("attach to command: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return parseSSE(resp.Body)
}

type spaceFrame struct {
	Type       string `json:"type"`
	DataBase64 string `json:"data_base64"`
	ExitCode   *int   `json:"exit_code"`
	Status     string `json:"status"`
	Message    string `json:"message"`
}

// parseSSE folds a SPACE process event stream into one ExecResult. A stream
// that ends without process_exited is an error: the outcome is unknown.
func parseSSE(r io.Reader) (ExecResult, error) {
	var (
		res  ExecResult
		out  bytes.Buffer
		errb bytes.Buffer
		data [][]byte
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	handle := func(raw []byte) (done bool, err error) {
		var f spaceFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			return false, fmt.Errorf("bad event frame: %w", err)
		}
		switch f.Type {
		case "stdout", "stderr":
			b, err := base64.StdEncoding.DecodeString(f.DataBase64)
			if err != nil {
				return false, fmt.Errorf("bad %s payload: %w", f.Type, err)
			}
			if f.Type == "stdout" {
				out.Write(b)
			} else {
				errb.Write(b)
			}
		case "process_exited":
			code := -1
			if f.ExitCode != nil {
				code = *f.ExitCode
			}
			res.ExitCode = code
			if f.Status != "exited" || code != 0 {
				res.Error = &ExecError{Name: "ExitError", Value: fmt.Sprintf("process %s with exit code %d", f.Status, code)}
			}
			return true, nil
		case "error", "resync_required":
			return false, fmt.Errorf("command stream reported %s: %s", f.Type, f.Message)
		}
		return false, nil
	}
	for sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if len(line) == 0 {
			if len(data) == 0 {
				continue
			}
			frame := bytes.Join(data, []byte("\n"))
			data = nil
			done, err := handle(frame)
			if err != nil {
				return res, err
			}
			if done {
				res.Stdout, res.Stderr = out.String(), errb.String()
				return res, nil
			}
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		if string(field) == "data" {
			data = append(data, bytes.TrimPrefix(value, []byte(" ")))
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	return res, errors.New("command stream ended without process_exited")
}

func (s *SPACE) get(ctx context.Context, id string) (spaceSandbox, error) {
	var sb spaceSandbox
	status, err := s.api(ctx, http.MethodGet, "/v1/sandboxes/"+id, nil, &sb, false)
	if status == http.StatusNotFound {
		return sb, ErrNotFound
	}
	if err != nil {
		return sb, err
	}
	switch sb.State {
	case "deleted", "deleting", "failed":
		return sb, ErrNotFound
	}
	return sb, nil
}

func (s *SPACE) waitOp(ctx context.Context, opID string) error {
	for {
		var rec spaceOpRecord
		if _, err := s.api(ctx, http.MethodGet, "/v1/operations/"+opID, nil, &rec, false); err != nil {
			return err
		}
		switch rec.State {
		case "succeeded":
			return nil
		case "failed", "canceled", "expired":
			return fmt.Errorf("operation %s %s: %s", opID, rec.State, rec.ProgressMessage)
		}
		if err := sleep(ctx, s.pollEvery); err != nil {
			return err
		}
	}
}

func (s *SPACE) waitRunning(ctx context.Context, id string) error {
	for {
		sb, err := s.get(ctx, id)
		if err != nil {
			return err
		}
		switch sb.State {
		case "running":
			return nil
		case "paused", "suspended", "stopped":
			return fmt.Errorf("sandbox %s is %s", id, sb.State)
		}
		if err := sleep(ctx, s.pollEvery); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// api performs a control-plane call. Mutations carry a fresh Idempotency-Key
// so a retried HTTP request cannot create a second sandbox or command.
func (s *SPACE) api(ctx context.Context, method, path string, in, out any, mutation bool) (int, error) {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.apiURL+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutation {
		req.Header.Set("Idempotency-Key", newKey())
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

func newKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var _ Sandbox = (*SPACE)(nil)
