package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cwxu-algo/internal/opsroot"
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
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(t.TempDir(), "ops.data.json"))
	t.Setenv("GOALGO_ROOT", "/tmp/gtest-root")
	root, err := resolveInstallRoot("")
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != "/tmp/gtest-root" {
		t.Fatalf("got %s", root.Path)
	}
}

func TestResolveInstallRootReadsRootFromEnvFile(t *testing.T) {
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(t.TempDir(), "ops.data.json"))
	t.Setenv("GOALGO_ROOT", "")
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	if err := os.WriteFile(envFile, []byte("GOALGO_ROOT=/custom/data/root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := resolveInstallRootFrom("", envFile)
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != "/custom/data/root" {
		t.Fatalf("got %s, want /custom/data/root", root.Path)
	}
}

func TestResolveInstallRootEnvFileMissingFallsBackToDefault(t *testing.T) {
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(t.TempDir(), "ops.data.json"))
	t.Setenv("GOALGO_ROOT", "")
	root, err := resolveInstallRootFrom("", filepath.Join(t.TempDir(), "missing.env"))
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != "/opt/goalgo" {
		t.Fatalf("got %s, want /opt/goalgo", root.Path)
	}
}

func TestResolveInstallRootExplicitWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(t.TempDir(), "ops.data.json"))
	t.Setenv("GOALGO_ROOT", "/tmp/gtest-other")
	root, err := resolveInstallRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if root.Path != dir {
		t.Fatalf("got %s, want %s", root.Path, dir)
	}
}

func TestRegisteredRootRejectsExplicitMismatch(t *testing.T) {
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(t.TempDir(), "ops.data.json"))
	registered := filepath.Join(t.TempDir(), "registered")
	if err := persistInstallRoot(mustRoot(t, registered)); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveRegisteredRoot(filepath.Join(t.TempDir(), "other")); err == nil || !strings.Contains(err.Error(), registered) {
		t.Fatalf("expected registered path rejection, got %v", err)
	}
	root, err := resolveRegisteredRoot("")
	if err != nil || root.Path != registered {
		t.Fatalf("root=%v err=%v", root, err)
	}
}

func TestInstallRegistrationRejectsSameAndDifferentRoots(t *testing.T) {
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(t.TempDir(), "ops.data.json"))
	registered := mustRoot(t, filepath.Join(t.TempDir(), "registered"))
	if err := persistInstallRoot(registered); err != nil {
		t.Fatal(err)
	}
	if err := ensureInstallAvailable(registered); err == nil || !strings.Contains(err.Error(), "upgrade/restart") {
		t.Fatalf("same root error=%v", err)
	}
	if err := ensureInstallAvailable(mustRoot(t, filepath.Join(t.TempDir(), "other"))); err == nil || !strings.Contains(err.Error(), registered.Path) {
		t.Fatalf("different root error=%v", err)
	}
}

func mustRoot(t *testing.T, path string) *opsroot.Root {
	t.Helper()
	root, err := opsroot.Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	return root
}
