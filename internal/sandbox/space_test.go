package sandbox

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSpace is a minimal SPACE control plane + data plane.
type fakeSpace struct {
	mu        sync.Mutex
	state     map[string]string // sandbox id -> state
	ops       map[string]string // op id -> state
	keys      map[string]bool   // Idempotency-Keys seen
	wakeSteps int               // extra polls before a woken sandbox is running
	stream    string            // SSE body served for any command
	commands  [][]string
	api, data *httptest.Server
}

func newFakeSpace(t *testing.T) *fakeSpace {
	f := &fakeSpace{state: map[string]string{}, ops: map[string]string{}, keys: map[string]bool{}}
	f.data = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer attach-tok" {
			http.Error(w, "bad attach token", 401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, f.stream)
	}))
	f.api = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.api.Close)
	t.Cleanup(f.data.Close)
	return f
}

func (f *fakeSpace) op(w http.ResponseWriter, sb string) {
	id := fmt.Sprintf("op_%d", len(f.ops))
	f.ops[id] = "succeeded"
	w.WriteHeader(202)
	json.NewEncoder(w).Encode(map[string]string{"operation_id": id, "sandbox_id": sb})
}

func (f *fakeSpace) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer mgmt-tok" {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != http.MethodGet {
		k := r.Header.Get("Idempotency-Key")
		if k == "" || f.keys[k] {
			http.Error(w, "missing or reused Idempotency-Key", 400)
			return
		}
		f.keys[k] = true
	}
	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == "/v1/sandboxes":
		id := fmt.Sprintf("sbx_%d", len(f.state)+1)
		f.state[id] = "running"
		f.op(w, id)
	case strings.HasPrefix(p, "/v1/operations/"):
		st, ok := f.ops[strings.TrimPrefix(p, "/v1/operations/")]
		if !ok {
			http.Error(w, "no op", 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"state": st, "progress_message": ""})
	case r.Method == "GET" && strings.HasPrefix(p, "/v1/sandboxes/"):
		id := strings.TrimPrefix(p, "/v1/sandboxes/")
		st, ok := f.state[id]
		if !ok {
			http.Error(w, "no sandbox", 404)
			return
		}
		if st == "resuming" {
			if f.wakeSteps--; f.wakeSteps <= 0 {
				f.state[id] = "running"
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "state": st, "failure_code": nil})
	case r.Method == "POST" && strings.HasSuffix(p, ":wake"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/sandboxes/"), ":wake")
		if _, ok := f.state[id]; !ok {
			http.Error(w, "no sandbox", 404)
			return
		}
		f.state[id] = "resuming"
		f.op(w, id)
	case r.Method == "POST" && strings.HasSuffix(p, ":suspend"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/sandboxes/"), ":suspend")
		f.state[id] = "suspended"
		f.op(w, id)
	case r.Method == "DELETE" && strings.HasPrefix(p, "/v1/sandboxes/"):
		id := strings.TrimPrefix(p, "/v1/sandboxes/")
		if _, ok := f.state[id]; !ok {
			http.Error(w, "no sandbox", 404)
			return
		}
		delete(f.state, id)
		f.op(w, id)
	case r.Method == "POST" && strings.HasSuffix(p, "/commands"):
		var body struct {
			Command []string `json:"command"`
			Timeout string   `json:"timeout"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.commands = append(f.commands, body.Command)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{
			"session_id": "proc_1", "sandbox_id": "x", "state": "running",
			"attach": map[string]any{"url": f.data.URL, "events_url": "/v1/sessions/proc_1/events", "token": "attach-tok"},
		})
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, 500)
	}
}

func sse(frames ...string) string {
	var b strings.Builder
	for _, fr := range frames {
		b.WriteString("data: " + fr + "\n\n")
	}
	return b.String()
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func newSpaceClient(t *testing.T, f *fakeSpace) *SPACE {
	s, err := NewSPACE(f.api.URL, f.data.URL, "mgmt-tok", "seed:latest")
	if err != nil {
		t.Fatal(err)
	}
	s.pollEvery = time.Millisecond
	return s
}

func TestSpaceCreateExecKill(t *testing.T) {
	f := newFakeSpace(t)
	f.stream = sse(
		`{"type":"attached","session_id":"proc_1"}`,
		`{"type":"process_started","process_id":"proc_1","pid":42}`,
		`{"type":"stdout","seq":1,"data_base64":"`+b64("hello\n")+`"}`,
		`{"type":"stderr","seq":2,"data_base64":"`+b64("warn")+`"}`,
		`{"type":"process_exited","process_id":"proc_1","exit_code":0,"status":"exited"}`,
	)
	s := newSpaceClient(t, f)
	ctx := context.Background()

	h, err := s.Create(ctx, CreateOpts{TaskID: "t1", TimeoutSec: 60})
	if err != nil || h.ID != "sbx_1" {
		t.Fatalf("create: %+v %v", h, err)
	}
	res, err := s.Exec(ctx, h, ExecRequest{Code: "print('hello')", Language: "python"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hello\n" || res.Stderr != "warn" || res.Error != nil || res.ExitCode != 0 {
		t.Fatalf("got %+v", res)
	}
	if got := f.commands[0]; len(got) != 3 || got[0] != "python3" || got[1] != "-c" {
		t.Fatalf("argv=%v", got)
	}
	if err := s.Kill(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Kill(ctx, h.ID); err != ErrNotFound {
		t.Fatalf("second kill err=%v, want ErrNotFound", err)
	}
}

func TestSpaceNonZeroExitIsCodeError(t *testing.T) {
	f := newFakeSpace(t)
	f.stream = sse(`{"type":"process_exited","process_id":"p","exit_code":3,"status":"exited"}`)
	s := newSpaceClient(t, f)
	h, _ := s.Create(context.Background(), CreateOpts{})
	res, err := s.Exec(context.Background(), h, ExecRequest{Code: "exit 3", Language: "bash"})
	if err != nil {
		t.Fatalf("exec err=%v, want nil (it is a code error, not an infra error)", err)
	}
	if res.Error == nil || res.ExitCode != 3 {
		t.Fatalf("got %+v", res)
	}
	if f.commands[0][0] != "sh" {
		t.Fatalf("argv=%v", f.commands[0])
	}
}

func TestSpaceTruncatedStreamIsUnknownOutcome(t *testing.T) {
	f := newFakeSpace(t)
	f.stream = sse(`{"type":"stdout","seq":1,"data_base64":"` + b64("partial") + `"}`)
	s := newSpaceClient(t, f)
	h, _ := s.Create(context.Background(), CreateOpts{})
	if _, err := s.Exec(context.Background(), h, ExecRequest{Code: "x"}); err == nil {
		t.Fatal("stream without process_exited must be an error")
	}
	f.stream = sse(`{"type":"resync_required","message":"node moved"}`)
	if _, err := s.Exec(context.Background(), h, ExecRequest{Code: "x"}); err == nil {
		t.Fatal("resync_required must be an error")
	}
}

func TestSpaceConnectWakesSuspended(t *testing.T) {
	f := newFakeSpace(t)
	s := newSpaceClient(t, f)
	ctx := context.Background()
	h, _ := s.Create(ctx, CreateOpts{})
	if err := s.Pause(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if f.state[h.ID] != "suspended" {
		t.Fatalf("state=%s", f.state[h.ID])
	}
	f.wakeSteps = 3
	if _, err := s.Connect(ctx, h.ID, 60); err != nil {
		t.Fatal(err)
	}
	if f.state[h.ID] != "running" {
		t.Fatalf("state after connect=%s", f.state[h.ID])
	}
	if _, err := s.Connect(ctx, "sbx_nope", 60); err != ErrNotFound {
		t.Fatalf("connect missing: %v", err)
	}
}

func TestSpaceRejectsUnknownLanguageAndBadOrigin(t *testing.T) {
	if _, err := argv("ruby", "puts 1"); err == nil {
		t.Fatal("ruby should be rejected")
	}
	for _, u := range []string{"ftp://x", "https://user:pw@x", "https://x/v1", "https://x?y=1", "x"} {
		if _, err := NewSPACE(u, "https://ok", "t", ""); err == nil {
			t.Errorf("origin %q accepted", u)
		}
	}
}

func TestSpaceRefusesForeignEventsURL(t *testing.T) {
	f := newFakeSpace(t)
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("attachment token leaked to foreign host: %s", r.Header.Get("Authorization"))
	}))
	defer evil.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/commands") {
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"session_id": "proc_1", "attach": map[string]any{
				"url": evil.URL, "events_url": evil.URL + "/v1/sessions/proc_1/events", "token": "attach-tok"}})
			return
		}
		f.handle(w, r)
	}))
	defer api.Close()
	s, _ := NewSPACE(api.URL, f.data.URL, "mgmt-tok", "")
	if _, err := s.Exec(context.Background(), Handle{ID: "sbx_1"}, ExecRequest{Code: "x"}); err == nil || !strings.Contains(err.Error(), "unexpected events url") {
		t.Fatalf("err=%v", err)
	}
}
