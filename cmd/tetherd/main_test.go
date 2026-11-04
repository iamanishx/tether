package main

import (
	"testing"
)

func TestBackendExecutionModeConfiguration(t *testing.T) {
	for _, name := range []string{"E2B_API_URL", "E2B_SANDBOX_URL", "E2B_DOMAIN", "E2B_EXEC_MODE"} {
		t.Setenv(name, "")
	}
	t.Setenv("E2B_API_KEY", "test-only")
	if _, err := newBackend("e2b", "", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("E2B_API_URL", "http://localhost:3000")
	t.Setenv("E2B_SANDBOX_URL", "http://localhost:3002")
	for _, mode := range []string{"", "commands", "interpreter"} {
		if _, err := newBackend("e2b", "base", mode); err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
	if _, err := newBackend("e2b", "base", "bad"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	t.Setenv("E2B_API_KEY", "")
	if _, err := newBackend("e2b", "base", "commands"); err == nil {
		t.Fatal("missing API key accepted")
	}
}
