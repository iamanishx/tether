package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	defaultDomain   = "e2b.app"
	codeInterpPort  = "49999"
	maxErrBodyBytes = 4 << 10
)

// E2B talks to the E2B control plane (sandbox lifecycle) and to the
// code-interpreter server inside the sandbox (code execution) over plain HTTP.
// It works against E2B Cloud and against a self-hosted E2B Embed / BYOC
// deployment: the same REST API and the same header-routed sandbox proxy.
type E2B struct {
	apiKey     string
	apiURL     string
	sandboxURL string // if set, every sandbox is reached here via header routing
	domain     string
	template   string
	http       *http.Client
}

// E2BOptions overrides the cloud defaults for a self-hosted deployment. The
// names match the official SDKs' E2B_API_URL / E2B_SANDBOX_URL / E2B_DOMAIN.
type E2BOptions struct {
	APIURL     string // e.g. http://embed-host:3000
	SandboxURL string // e.g. http://embed-host:3002 (client-proxy, header routing)
	Domain     string // used to build per-sandbox hosts when SandboxURL is empty
}

// NewE2B returns a client for E2B Cloud. template is e.g. "code-interpreter-v1".
func NewE2B(apiKey, template string) *E2B {
	e, _ := NewE2BWithOptions(apiKey, template, E2BOptions{})
	return e
}

// NewE2BWithOptions returns a client for E2B Cloud or a self-hosted deployment.
func NewE2BWithOptions(apiKey, template string, o E2BOptions) (*E2B, error) {
	domain := o.Domain
	if domain == "" {
		domain = defaultDomain
	}
	apiURL := o.APIURL
	if apiURL == "" {
		apiURL = "https://api." + domain
	}
	for _, u := range []string{apiURL, o.SandboxURL} {
		if u != "" {
			if err := checkOrigin(u); err != nil {
				return nil, fmt.Errorf("E2B: %w", err)
			}
		}
	}
	return &E2B{
		apiKey:     apiKey,
		apiURL:     strings.TrimRight(apiURL, "/"),
		sandboxURL: strings.TrimRight(o.SandboxURL, "/"),
		domain:     domain,
		template:   template,
		http:       &http.Client{Timeout: 0}, // per-call deadlines come from ctx
	}, nil
}

type sandboxResp struct {
	SandboxID          string  `json:"sandboxID"`
	EnvdAccessToken    string  `json:"envdAccessToken"`
	TrafficAccessToken *string `json:"trafficAccessToken"`
	Domain             *string `json:"domain"`
}

func (e *E2B) handle(r sandboxResp) Handle {
	h := Handle{ID: r.SandboxID, Domain: e.domain, EnvdToken: r.EnvdAccessToken}
	if r.Domain != nil && *r.Domain != "" {
		h.Domain = *r.Domain
	}
	if r.TrafficAccessToken != nil {
		h.TrafficToken = *r.TrafficAccessToken
	}
	return h
}

func (e *E2B) Create(ctx context.Context, o CreateOpts) (Handle, error) {
	body := map[string]any{
		"templateID": e.template,
		"timeout":    o.TimeoutSec,
		"autoPause":  true,
		"metadata":   map[string]string{"tether_task_id": o.TaskID},
	}
	var out sandboxResp
	if _, err := e.api(ctx, http.MethodPost, "/v2/sandboxes", body, &out); err != nil {
		return Handle{}, fmt.Errorf("create sandbox: %w", err)
	}
	return e.handle(out), nil
}

func (e *E2B) Connect(ctx context.Context, id string, timeoutSec int) (Handle, error) {
	var out sandboxResp
	status, err := e.api(ctx, http.MethodPost, "/v2/sandboxes/"+id+"/connect", map[string]any{"timeout": timeoutSec}, &out)
	if status == http.StatusNotFound {
		return Handle{}, ErrNotFound
	}
	if err != nil {
		return Handle{}, fmt.Errorf("connect sandbox: %w", err)
	}
	return e.handle(out), nil
}

func (e *E2B) Pause(ctx context.Context, id string) error {
	status, err := e.api(ctx, http.MethodPost, "/sandboxes/"+id+"/pause", nil, nil)
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	return err
}

func (e *E2B) Kill(ctx context.Context, id string) error {
	status, err := e.api(ctx, http.MethodDelete, "/sandboxes/"+id, nil, nil)
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	return err
}

// api performs a control-plane call and returns the HTTP status.
func (e *E2B) api(ctx context.Context, method, path string, in, out any) (int, error) {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.apiURL+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", e.apiKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
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

// execURL mirrors the E2B SDKs: an explicit sandbox URL (self-hosted
// client-proxy, header routing) wins; then the stable host on E2B Cloud; else a
// per-sandbox host under the domain.
func (e *E2B) execURL(h Handle) string {
	if e.sandboxURL != "" {
		return e.sandboxURL + "/execute"
	}
	if h.Domain == defaultDomain {
		return "https://sandbox." + h.Domain + "/execute"
	}
	return fmt.Sprintf("https://%s-%s.%s/execute", codeInterpPort, h.ID, h.Domain)
}

type streamLine struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Name      string `json:"name"`
	Value     string `json:"value"`
	Traceback string `json:"traceback"`
}

// Exec runs code with the in-sandbox code-interpreter server and folds its
// newline-delimited JSON stream into one result.
func (e *E2B) Exec(ctx context.Context, h Handle, r ExecRequest) (ExecResult, error) {
	payload := map[string]any{"code": r.Code}
	if r.Language != "" {
		payload["language"] = r.Language
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.execURL(h), bytes.NewReader(b))
	if err != nil {
		return ExecResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("E2b-Sandbox-Id", h.ID)
	req.Header.Set("E2b-Sandbox-Port", codeInterpPort)
	if h.EnvdToken != "" {
		req.Header.Set("X-Access-Token", h.EnvdToken)
	}
	if h.TrafficToken != "" {
		req.Header.Set("E2B-Traffic-Access-Token", h.TrafficToken)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return ExecResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		eb, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
		return ExecResult{}, fmt.Errorf("execute: %d %s", resp.StatusCode, strings.TrimSpace(string(eb)))
	}
	return parseStream(resp.Body)
}

func parseStream(r io.Reader) (ExecResult, error) {
	var (
		res   ExecResult
		out   strings.Builder
		errb  strings.Builder
		ended bool
	)
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var l streamLine
			if err := json.Unmarshal(line, &l); err != nil {
				return res, fmt.Errorf("bad stream line: %w", err)
			}
			switch l.Type {
			case "stdout":
				out.WriteString(l.Text)
			case "stderr":
				errb.WriteString(l.Text)
			case "result":
				if l.Text != "" {
					res.Results = append(res.Results, l.Text)
				}
			case "error":
				res.Error = &ExecError{Name: l.Name, Value: l.Value, Traceback: l.Traceback}
			case "end_of_execution":
				ended = true
			case "unexpected_end_of_execution":
				return res, errors.New("sandbox kernel ended unexpectedly")
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return res, rerr
		}
	}
	res.Stdout, res.Stderr = out.String(), errb.String()
	if !ended {
		return res, errors.New("execution stream ended without end_of_execution")
	}
	return res, nil
}

// compile-time check
var _ Sandbox = (*E2B)(nil)
