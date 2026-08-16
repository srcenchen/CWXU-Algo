package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDotenvValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("GOALGO_ROOT=/root/goalgo/algo\nTZ=Asia/Shanghai\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := dotenvValue(path, "GOALGO_ROOT")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/root/goalgo/algo" {
		t.Fatalf("got %q", got)
	}
	if _, err := dotenvValue(path, "NOPE"); err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestResolveInstallRootPrefersEnvVar(t *testing.T) {
	t.Setenv("GOALGO_ROOT", "/tmp/gtest-root")
	root, err := resolveInstallRoot("")
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != "/tmp/gtest-root" {
		t.Fatalf("got %s", root.Path)
	}
}

func TestResolveInstallRootExplicitWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOALGO_ROOT", "/tmp/gtest-other")
	root, err := resolveInstallRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != dir {
		t.Fatalf("got %s, want %s", root.Path, dir)
	}
}
