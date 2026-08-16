package opslock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock", "goalgo-ops.lock")
	lock, err := Acquire(path, 0)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := Acquire(path, 0); err == nil {
		t.Fatal("expected second non-blocking acquire to fail")
	}
	lock.Release()
	lock, err = Acquire(path, 0)
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	lock.Release()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file should remain: %v", err)
	}
}
