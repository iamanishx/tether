package sandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func envelope(flags byte, s string) []byte {
	b := make([]byte, len(s)+5)
	b[0] = flags
	binary.BigEndian.PutUint32(b[1:5], uint32(len(s)))
	copy(b[5:], s)
	return b
}

func wire(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func TestE2BCommandsWire(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != "POST" || r.URL.Path != processStart || r.Header.Get("Content-Type") != "application/connect+json" || r.Header.Get("Connect-Protocol-Version") != "1" {
			t.Errorf("bad Connect request: %s %s %v", r.Method, r.URL.Path, r.Header)
		}
		if r.Header.Get("E2b-Sandbox-Port") != "49983" || r.Header.Get("E2b-Sandbox-Id") != "sb" || r.Header.Get("X-Access-Token") != "envd-token" || r.Header.Get("E2B-Traffic-Access-Token") != "traffic-token" {
			t.Error("routing/scoped token missing")
		}
		if r.Header.Get("X-API-Key") != "" || r.Header.Get("Connect-Timeout-Ms") == "" {
			t.Error("master key leaked or deadline missing")
		}
		b, _ := io.ReadAll(r.Body)
		if len(b) < 5 || b[0] != 0 || int(binary.BigEndian.Uint32(b[1:5])) != len(b)-5 {
			t.Fatal("request not framed")
		}
		var in struct {
			Process struct {
				Cmd  string   `json:"cmd"`
				Args []string `json:"args"`
			} `json:"process"`
			Stdin bool `json:"stdin"`
		}
		if err := json.Unmarshal(b[5:], &in); err != nil || in.Process.Cmd != "python3" || len(in.Process.Args) != 2 || in.Process.Args[1] != `print("hi")` || in.Stdin {
			t.Errorf("payload %+v %v", in, err)
		}
		w.Header().Set("Content-Type", "application/connect+json")
		w.Write(wire(envelope(0, `{"event":{"start":{"pid":42}}}`), envelope(0, `{"event":{"data":{"stdout":"aGkK"}}}`), envelope(0, `{"event":{"data":{"stderr":"d2Fybgo="}}}`), envelope(0, `{"event":{"end":{"exited":true}}}`), envelope(2, `{}`)))
	}))
	defer server.Close()
	e, err := NewE2BWithOptions("master", "base", E2BOptions{SandboxURL: server.URL, ExecMode: "commands"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := e.Exec(ctx, Handle{ID: "sb", EnvdToken: "envd-token", TrafficToken: "traffic-token"}, ExecRequest{Code: `print("hi")`})
	if err != nil || !called || res.Stdout != "hi\n" || res.Stderr != "warn\n" || res.ProcessID != 42 || res.ExitCode != 0 || res.Error != nil {
		t.Fatalf("res=%+v err=%v called=%t", res, err, called)
	}
}

func TestCommandsAreArgvNotInterpolated(t *testing.T) {
	code := "print('quotes; $(bad)')\n"
	for _, c := range []struct {
		lang, cmd string
		args      []string
	}{
		{"python", "python3", []string{"-c", code}},
		{"", "python3", []string{"-c", code}},
		{"javascript", "node", []string{"-e", code}},
		{"bash", "/bin/bash", []string{"-lc", code}},
		{"sh", "/bin/bash", []string{"-lc", code}},
	} {
		program, args, err := commandArgs(ExecRequest{Language: c.lang, Code: code})
		if err != nil || program != c.cmd || strings.Join(args, "\x00") != strings.Join(c.args, "\x00") {
			t.Errorf("%s: %s %v %v", c.lang, program, args, err)
		}
	}
	if _, _, err := commandArgs(ExecRequest{Language: "r"}); err == nil {
		t.Fatal("unsupported language accepted")
	}
	if _, err := NewE2BWithOptions("k", "t", E2BOptions{ExecMode: "typo"}); err == nil {
		t.Fatal("invalid execution mode accepted")
	}
}

func TestProcessStreamExitAndFailure(t *testing.T) {
	start := envelope(0, `{"event":{"start":{"pid":7}}}`)
	data := envelope(0, `{"event":{"data":{"stdout":"cGFydGlhbA=="}}}`)
	tests := []struct {
		name string
		body []byte
		err  bool
	}{
		{"nonzero exit is confirmed code failure", wire(start, data, envelope(0, `{"event":{"end":{"exited":true,"exitCode":9}}}`), envelope(2, `{}`)), false},
		{"no process exit", wire(start, data, envelope(2, `{}`)), true},
		{"no RPC trailer", wire(start, data, envelope(0, `{"event":{"end":{"exited":true}}}`)), true},
		{"RPC failure", wire(start, data, envelope(2, `{"error":{"code":"unavailable","message":"lost"}}`)), true},
		{"RPC failure after exit stays conservative", wire(start, data, envelope(0, `{"event":{"end":{"exited":true}}}`), envelope(2, `{"error":{"code":"internal","message":"lost"}}`)), true},
		{"invalid JSON", wire(start, data, envelope(0, `{broken`)), true},
		{"invalid base64", wire(start, data, envelope(0, `{"event":{"data":{"stdout":"!!!"}}}`)), true},
		{"compressed envelope not requested", wire(start, data, envelope(1, `{}`)), true},
		{"false exited", wire(start, data, envelope(0, `{"event":{"end":{"exited":false}}}`), envelope(2, `{}`)), true},
		{"truncated body", wire(start, data, envelope(0, `{"event":{}}`)[:6]), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := parseProcessStream(bytes.NewReader(tc.body))
			if (err != nil) != tc.err || res.ProcessID != 7 || res.Stdout != "partial" {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if !tc.err && (res.ExitCode != 9 || res.Error == nil || res.Error.Name != "ExitError") {
				t.Fatalf("nonzero exit not reported: %+v", res)
			}
		})
	}
}

func TestProcessStreamBounds(t *testing.T) {
	var header [5]byte
	binary.BigEndian.PutUint32(header[1:], maxFrameBytes+1)
	if _, err := parseProcessStream(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversized frame accepted")
	}
	chunk, _ := json.Marshal(map[string]any{"event": map[string]any{"data": map[string]string{"stdout": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 512<<10))}}})
	var b bytes.Buffer
	for i := 0; i < 10; i++ {
		b.Write(envelope(0, string(chunk)))
	}
	b.Write(envelope(0, `{"event":{"end":{"exited":true}}}`))
	b.Write(envelope(2, `{}`))
	res, err := parseProcessStream(&b)
	if err != nil || !res.OutputTruncated || len(res.Stdout) != maxOutput {
		t.Fatalf("len=%d truncated=%t err=%v", len(res.Stdout), res.OutputTruncated, err)
	}
}

func TestProcessStreamHTTPFailure(t *testing.T) {
	for _, status := range []int{401, 403, 500} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		e, _ := NewE2BWithOptions("k", "base", E2BOptions{SandboxURL: s.URL, ExecMode: "commands"})
		if _, err := e.Exec(context.Background(), Handle{ID: "s"}, ExecRequest{Language: "bash", Code: "true"}); err == nil {
			t.Errorf("HTTP %d accepted", status)
		}
		s.Close()
	}
}
