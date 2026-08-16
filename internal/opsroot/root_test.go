package opsroot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDefaultAndJoin(t *testing.T) {
	if os.Getenv("GOALGO_ROOT") != "" {
		t.Setenv("GOALGO_ROOT", "")
	}
	root, err := Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if root.Path != "/opt/goalgo" {
		t.Fatalf("expected /opt/goalgo, got %s", root.Path)
	}
	if root.Join("config", "app.yaml") != "/opt/goalgo/config/app.yaml" {
		t.Fatalf("unexpected join: %s", root.Join("config", "app.yaml"))
	}
}

func TestResolveRejectsSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	link := filepath.Join(base, "link")
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(link); err == nil {
		t.Fatal("expected error for symlink root")
	}
}

func TestEnsureLayoutAndRequireFiles(t *testing.T) {
	root, err := Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := root.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(root.Join("secrets")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("secrets must be 0700: mode=%v err=%v", info.Mode().Perm(), err)
	}
	if err := root.RequireFiles(); err == nil {
		t.Fatal("expected missing required file error")
	}
	for _, name := range []string{"compose.yaml", ".env", "release.env"} {
		if err := os.WriteFile(root.Join(name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := root.RequireFiles(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
