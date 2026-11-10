package filelock

import (
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestExclusiveExcludesOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	a, b := open(t, path), open(t, path)
	if err := TryExclusive(a); err != nil {
		t.Fatalf("first exclusive: %v", err)
	}
	if err := TryExclusive(b); !Busy(err) {
		t.Fatalf("second exclusive: got %v, want busy", err)
	}
	if err := TryShared(b); !Busy(err) {
		t.Fatalf("shared under exclusive: got %v, want busy", err)
	}
	if err := Unlock(a); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := TryExclusive(b); err != nil {
		t.Fatalf("exclusive after unlock: %v", err)
	}
}

func TestSharedAdmitsShared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.lock")
	a, b, c := open(t, path), open(t, path), open(t, path)
	if err := TryShared(a); err != nil {
		t.Fatalf("first shared: %v", err)
	}
	if err := TryShared(b); err != nil {
		t.Fatalf("second shared: %v", err)
	}
	if err := TryExclusive(c); !Busy(err) {
		t.Fatalf("exclusive under shared: got %v, want busy", err)
	}
}

func TestCloseReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.lock")
	a := open(t, path)
	if err := TryExclusive(a); err != nil {
		t.Fatal(err)
	}
	a.Close()
	if err := TryExclusive(open(t, path)); err != nil {
		t.Fatalf("exclusive after close: %v", err)
	}
}
