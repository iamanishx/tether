package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iamanishx/tether/internal/sandbox"
	"github.com/iamanishx/tether/internal/store"
	"github.com/iamanishx/tether/internal/task"
)

func setup(t *testing.T) (*httptest.Server, *sandbox.Fake) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fk := sandbox.NewFake()
	srv := httptest.NewServer(New(task.NewEngine(st, fk, task.Config{}), "s3cret").Handler())
	t.Cleanup(srv.Close)
	return srv, fk
}

func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rd bytes.Buffer
	if body != nil {
		json.NewEncoder(&rd).Encode(body)
	}
	req, _ := http.NewRequest(method, srv.URL+path, &rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestAuthRequired(t *testing.T) {
	srv, _ := setup(t)
	if r, _ := call(t, srv, "GET", "/v1/tasks", "", nil); r.StatusCode != 401 {
		t.Fatalf("no token: %d", r.StatusCode)
	}
	if r, _ := call(t, srv, "GET", "/v1/tasks", "wrong", nil); r.StatusCode != 401 {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	if r, _ := call(t, srv, "GET", "/healthz", "", nil); r.StatusCode != 200 {
		t.Fatalf("healthz: %d", r.StatusCode)
	}
}

func TestEndToEndFlow(t *testing.T) {
	srv, fk := setup(t)
	const tok = "s3cret"

	if r, _ := call(t, srv, "POST", "/v1/tasks", tok, map[string]any{"id": "j1", "title": "report"}); r.StatusCode != 201 {
		t.Fatalf("create: %d", r.StatusCode)
	}
	act := map[string]any{"action_id": "a1", "code": "print(1)"}
	r, out := call(t, srv, "POST", "/v1/tasks/j1/actions", tok, act)
	if r.StatusCode != 200 || out["status"] != "succeeded" {
		t.Fatalf("submit: %d %v", r.StatusCode, out)
	}
	r, _ = call(t, srv, "POST", "/v1/tasks/j1/actions", tok, act)
	if r.Header.Get("Idempotent-Replay") != "true" || fk.ExecCount() != 1 {
		t.Fatalf("replay header=%q execs=%d", r.Header.Get("Idempotent-Replay"), fk.ExecCount())
	}

	r, out = call(t, srv, "POST", "/v1/tasks/j1/actions", tok, map[string]any{"action_id": "p", "code": "publish()", "requires_approval": true})
	if r.StatusCode != 202 || out["status"] != "pending_approval" {
		t.Fatalf("pending: %d %v", r.StatusCode, out)
	}
	if r, _ = call(t, srv, "POST", "/v1/tasks/j1/complete", tok, map[string]any{"state": "succeeded", "evidence": "x"}); r.StatusCode != 409 {
		t.Fatalf("complete with pending: %d", r.StatusCode)
	}
	if r, out = call(t, srv, "POST", "/v1/tasks/j1/actions/p/approve", tok, map[string]any{"note": "ok"}); r.StatusCode != 200 || out["status"] != "succeeded" {
		t.Fatalf("approve: %d %v", r.StatusCode, out)
	}
	if r, out = call(t, srv, "POST", "/v1/tasks/j1/complete", tok, map[string]any{"state": "succeeded", "evidence": "published"}); r.StatusCode != 200 || out["state"] != "succeeded" {
		t.Fatalf("complete: %d %v", r.StatusCode, out)
	}
	if r, _ = call(t, srv, "GET", "/v1/tasks/nope", tok, nil); r.StatusCode != 404 {
		t.Fatalf("missing task: %d", r.StatusCode)
	}
	if r, _ = call(t, srv, "POST", "/v1/tasks", tok, map[string]any{"bogus": 1}); r.StatusCode != 400 {
		t.Fatalf("unknown field: %d", r.StatusCode)
	}
}
