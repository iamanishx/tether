// Package store persists task records as one JSON file per task.
//
// Every write goes to a temp file, is fsynced, renamed over the target, and the
// directory is fsynced, so a crash leaves either the old or the new record,
// never a torn one. A flock on the data directory enforces a single owning
// process.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var ErrNotFound = errors.New("not found")

var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidID reports whether id is safe to use as a file name.
func ValidID(id string) bool { return validID.MatchString(id) && !strings.Contains(id, "..") }

type Store struct {
	dir  string
	lock *os.File
}

// Open creates dir if needed and takes an exclusive lock on it.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("data dir %s is owned by another process: %w", dir, err)
	}
	return &Store{dir: dir, lock: lock}, nil
}

func (s *Store) Close() error { return s.lock.Close() }

func (s *Store) path(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid id %q", id)
	}
	return filepath.Join(s.dir, "tasks", id+".json"), nil
}

// Save atomically and durably writes v as the record for id.
func (s *Store) Save(id string, v any) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { tmp.Close(); os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return err
	}
	return syncDir(filepath.Dir(p))
}

// Load reads the record for id into v.
func (s *Store) Load(id string, v any) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// IDs lists all stored record ids.
func (s *Store) IDs() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "tasks"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			ids = append(ids, strings.TrimSuffix(n, ".json"))
		}
	}
	return ids, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
