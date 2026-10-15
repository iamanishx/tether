package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTripAndNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	type rec struct{ N int }
	for i := 1; i <= 3; i++ {
		if err := s.Save("a", rec{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	var got rec
	if err := s.Load("a", &got); err != nil || got.N != 3 {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if err := s.Load("missing", &got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "tasks"))
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	ids, _ := s.IDs()
	if len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestSingleOwnerLock(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("second Open on the same dir should fail")
	}
	s1.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	s2.Close()
}

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{"abc": true, "a-b_c.1": true, "t_0123": true, "": false, "../a": false, "a/b": false, ".x": false, "a..b": false} {
		if ValidID(id) != want {
			t.Errorf("ValidID(%q) != %v", id, want)
		}
	}
}
