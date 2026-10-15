package sandbox

import (
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
	if got := execURL(Handle{ID: "abc", Domain: "e2b.app"}); got != "https://sandbox.e2b.app/execute" {
		t.Fatal(got)
	}
	if got := execURL(Handle{ID: "abc", Domain: "example.dev"}); got != "https://49999-abc.example.dev/execute" {
		t.Fatal(got)
	}
}
