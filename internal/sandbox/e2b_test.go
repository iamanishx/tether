package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseStream(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"number_of_executions","execution_count":1}`,
		`{"type":"stdout","text":"hello\n"}`,
		`{"type":"stderr","text":"warn\n"}`,
		`{"type":"result","text":"42","is_main_result":true,"png":"AAAA"}`,
		`{"type":"end_of_execution"}`,
	}, "\n") + "\n"
	r, err := parseStream(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if r.Stdout != "hello\n" || r.Stderr != "warn\n" || len(r.Results) != 1 || r.Results[0] != "42" || r.Error != nil {
		t.Fatalf("got %+v", r)
	}
}

func TestParseStreamError(t *testing.T) {
	in := `{"type":"error","name":"ZeroDivisionError","value":"division by zero","traceback":"tb"}` + "\n" + `{"type":"end_of_execution"}`
	r, err := parseStream(strings.NewReader(in))
	if err != nil || r.Error == nil || r.Error.Name != "ZeroDivisionError" {
		t.Fatalf("got %+v err=%v", r, err)
	}
}

func TestParseStreamTruncatedIsAnError(t *testing.T) {
	if _, err := parseStream(strings.NewReader(`{"type":"stdout","text":"x"}` + "\n")); err == nil {
		t.Fatal("stream without end_of_execution must be an error (outcome unknown)")
	}
	if _, err := parseStream(strings.NewReader(`{"type":"unexpected_end_of_execution"}` + "\n")); err == nil {
		t.Fatal("unexpected_end_of_execution must be an error")
	}
}

func TestExecURL(t *testing.T) {
	cloud := NewE2B("k", "t")
	if got := cloud.execURL(Handle{ID: "abc", Domain: "e2b.app"}); got != "https://sandbox.e2b.app/execute" {
		t.Fatal(got)
	}
	if got := cloud.execURL(Handle{ID: "abc", Domain: "example.dev"}); got != "https://49999-abc.example.dev/execute" {
		t.Fatal(got)
	}
}

// TestE2BSelfHostedEmbed drives the adapter against a fake E2B Embed: the REST
// API on one origin and the header-routed client-proxy on another, both plain HTTP.
func TestE2BSelfHostedEmbed(t *testing.T) {
	var apiCalls, execCalls []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls = append(apiCalls, r.Method+" "+r.URL.Path)
		if r.Header.Get("X-API-Key") != "embed-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v2/sandboxes":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"sandboxID": "sb1", "envdAccessToken": "envd-tok", "domain": nil})
		case r.Method == "POST" && r.URL.Path == "/v2/sandboxes/sb1/connect":
			json.NewEncoder(w).Encode(map[string]any{"sandboxID": "sb1", "envdAccessToken": "envd-tok"})
		case r.Method == "DELETE" && r.URL.Path == "/sandboxes/sb1":
			w.WriteHeader(204)
		default:
			http.Error(w, "unexpected", 500)
		}
	}))
	defer api.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		execCalls = append(execCalls, r.URL.Path)
		if r.Header.Get("E2b-Sandbox-Id") != "sb1" || r.Header.Get("E2b-Sandbox-Port") != "49999" || r.Header.Get("X-Access-Token") != "envd-tok" {
			http.Error(w, "bad routing headers", 400)
			return
		}
		w.Write([]byte(`{"type":"stdout","text":"hi\n"}` + "\n" + `{"type":"end_of_execution"}` + "\n"))
	}))
	defer proxy.Close()

	e, err := NewE2BWithOptions("embed-key", "base", E2BOptions{APIURL: api.URL, SandboxURL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, err := e.Create(ctx, CreateOpts{TaskID: "t", TimeoutSec: 60})
	if err != nil || h.ID != "sb1" {
		t.Fatalf("create: %+v %v", h, err)
	}
	// Embed returns no domain; the explicit sandbox URL must still be used.
	if got := e.execURL(h); got != proxy.URL+"/execute" {
		t.Fatalf("execURL=%s", got)
	}
	res, err := e.Exec(ctx, h, ExecRequest{Code: "print('hi')"})
	if err != nil || res.Stdout != "hi\n" {
		t.Fatalf("exec: %+v %v", res, err)
	}
	if _, err := e.Connect(ctx, "sb1", 60); err != nil {
		t.Fatal(err)
	}
	if err := e.Kill(ctx, "sb1"); err != nil {
		t.Fatal(err)
	}
	if len(execCalls) != 1 || execCalls[0] != "/execute" {
		t.Fatalf("exec calls=%v", execCalls)
	}
	if strings.Join(apiCalls, ",") != "POST /v2/sandboxes,POST /v2/sandboxes/sb1/connect,DELETE /sandboxes/sb1" {
		t.Fatalf("api calls=%v", apiCalls)
	}
}

func TestE2BRejectsBadURLs(t *testing.T) {
	for _, o := range []E2BOptions{{APIURL: "ftp://x"}, {APIURL: "http://x/api"}, {SandboxURL: "http://u:p@x"}} {
		if _, err := NewE2BWithOptions("k", "t", o); err == nil {
			t.Errorf("options %+v accepted", o)
		}
	}
}
