package sandbox

// Plain-template execution uses envd's Connect server-streaming JSON protocol,
// not the code-interpreter /execute endpoint. Wire contract:
// https://github.com/e2b-dev/E2B/blob/main/spec/envd/process/process.proto
// https://connectrpc.com/docs/protocol/#streaming-rpcs
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	envdPort      = "49983"
	processStart  = "/process.Process/Start"
	maxFrameBytes = 1 << 20
	maxOutput     = 4 << 20
)

func (e *E2B) commandURL(h Handle) string {
	if e.sandboxURL != "" {
		return e.sandboxURL + processStart
	}
	if h.Domain == defaultDomain {
		return "https://sandbox." + h.Domain + processStart
	}
	return fmt.Sprintf("https://%s-%s.%s%s", envdPort, h.ID, h.Domain, processStart)
}

func commandArgs(r ExecRequest) (string, []string, error) {
	switch r.Language {
	case "", "python":
		return "python3", []string{"-c", r.Code}, nil
	case "bash", "sh":
		return "/bin/bash", []string{"-lc", r.Code}, nil
	case "javascript":
		return "node", []string{"-e", r.Code}, nil
	default:
		return "", nil, fmt.Errorf("plain E2B templates support python, bash/sh and javascript; got %q", r.Language)
	}
}

func (e *E2B) execCommand(ctx context.Context, h Handle, r ExecRequest) (ExecResult, error) {
	program, args, err := commandArgs(r)
	if err != nil {
		return ExecResult{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"process": map[string]any{"cmd": program, "args": args},
		"stdin":   false,
	})
	if err != nil {
		return ExecResult{}, err
	}
	body := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(body[1:5], uint32(len(payload)))
	copy(body[5:], payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.commandURL(h), bytes.NewReader(body))
	if err != nil {
		return ExecResult{}, err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Keepalive-Ping-Interval", "10")
	req.Header.Set("E2b-Sandbox-Id", h.ID)
	req.Header.Set("E2b-Sandbox-Port", envdPort)
	if h.EnvdToken != "" {
		req.Header.Set("X-Access-Token", h.EnvdToken)
	}
	if h.TrafficToken != "" {
		req.Header.Set("E2B-Traffic-Access-Token", h.TrafficToken)
	}
	if deadline, ok := ctx.Deadline(); ok {
		ms := time.Until(deadline).Milliseconds()
		if ms <= 0 {
			return ExecResult{}, context.DeadlineExceeded
		}
		req.Header.Set("Connect-Timeout-Ms", strconv.FormatInt(ms, 10))
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return ExecResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes))
		return ExecResult{}, fmt.Errorf("envd process start: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/connect+json") {
		return ExecResult{}, errors.New("envd returned a non-Connect response")
	}
	return parseProcessStream(resp.Body)
}

func parseProcessStream(r io.Reader) (res ExecResult, err error) {
	var stdout, stderr strings.Builder
	var exited bool
	defer func() {
		res.Stdout, res.Stderr = stdout.String(), stderr.String()
	}()
	appendOutput := func(dst *strings.Builder, encoded string) error {
		b, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("invalid envd output encoding: %w", err)
		}
		remaining := maxOutput - stdout.Len() - stderr.Len()
		if remaining < len(b) {
			res.OutputTruncated = true
			b = b[:remaining]
		}
		_, _ = dst.Write(b)
		return nil
	}
	for {
		var header [5]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return res, fmt.Errorf("envd stream ended without RPC completion: %w", err)
		}
		size := binary.BigEndian.Uint32(header[1:])
		if size > maxFrameBytes {
			return res, fmt.Errorf("envd frame exceeds %d bytes", maxFrameBytes)
		}
		body := make([]byte, int(size))
		if _, err := io.ReadFull(r, body); err != nil {
			return res, fmt.Errorf("truncated envd frame: %w", err)
		}
		switch header[0] {
		case 2: // Connect EndStreamResponse envelope, not a process event.
			var trailer struct {
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &trailer); err != nil {
				return res, fmt.Errorf("invalid envd RPC trailer: %w", err)
			}
			if trailer.Error != nil {
				return res, fmt.Errorf("envd RPC %s: %s", trailer.Error.Code, trailer.Error.Message)
			}
			if !exited {
				return res, errors.New("envd RPC completed without a confirmed process exit")
			}
			return res, nil
		case 0:
			var event struct {
				Event struct {
					Start *struct {
						PID uint32 `json:"pid"`
					} `json:"start"`
					Data *struct {
						Stdout string `json:"stdout"`
						Stderr string `json:"stderr"`
					} `json:"data"`
					End *struct {
						ExitCode int    `json:"exitCode"`
						Exited   bool   `json:"exited"`
						Error    string `json:"error"`
					} `json:"end"`
				} `json:"event"`
			}
			if err := json.Unmarshal(body, &event); err != nil {
				return res, fmt.Errorf("invalid envd event: %w", err)
			}
			if start := event.Event.Start; start != nil {
				res.ProcessID = start.PID
			}
			if data := event.Event.Data; data != nil {
				if err := appendOutput(&stdout, data.Stdout); err != nil {
					return res, err
				}
				if err := appendOutput(&stderr, data.Stderr); err != nil {
					return res, err
				}
			}
			if end := event.Event.End; end != nil {
				if !end.Exited || exited {
					return res, errors.New("invalid or duplicate envd process exit")
				}
				exited = true
				res.ExitCode = end.ExitCode
				if end.ExitCode != 0 || end.Error != "" {
					res.Error = &ExecError{Name: "ExitError", Value: fmt.Sprintf("exit code %d: %s", end.ExitCode, end.Error)}
				}
			}
		default:
			return res, fmt.Errorf("unsupported envd envelope flags %d", header[0])
		}
	}
}
