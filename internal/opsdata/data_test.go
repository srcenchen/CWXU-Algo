package opsdata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(dir, "ops.data.json"))
	data := &Data{
		Version: schemaVersion,
		Root:    filepath.Join(dir, "install", "..", "goalgo"),
		Deploy:  Deploy{LastDigests: map[string]string{"frontend": "sha256:aaaa"}, UpdatedAt: "now"},
	}
	if err := data.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Deploy.LastDigests["frontend"] != "sha256:aaaa" {
		t.Fatalf("deploy digest lost: %#v", got.Deploy)
	}
	if got.Root != filepath.Join(dir, "goalgo") {
		t.Fatalf("root must be canonical, got %q", got.Root)
	}
	info, err := os.Stat(filepath.Join(dir, "ops.data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("data file must be 0600, got %v", info.Mode().Perm())
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(dir, "ops.data.json"))
	data, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if data.Version != schemaVersion {
		t.Fatalf("expected default version")
	}
}

func TestPathDefaultsToSystemRegistration(t *testing.T) {
	t.Setenv("GOALGO_OPS_DATA_FILE", "")
	if got := Path(); got != "/var/lib/goalgo-ops/ops.data.json" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadMigratesLegacyOnlyWhenSystemFileMissing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("GOALGO_OPS_DATA_FILE", filepath.Join(dir, "system", "ops.data.json"))
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"version":1,"root":"/tmp/legacy","webhook":{"key":"retired"}}`)
	if err := os.WriteFile(filepath.Join(dir, "home", ".ops.data.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if data.Root != "/tmp/legacy" {
		t.Fatalf("legacy root not migrated: %#v", data)
	}
	content, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "webhook") || strings.Contains(string(content), "calls") {
		t.Fatalf("retired state persisted: %s", content)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(content, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["root"]; !ok {
		t.Fatal("migrated registration has no root")
	}
	if _, err := os.Stat(filepath.Join(dir, "home", ".ops.data.json")); !os.IsNotExist(err) {
		t.Fatalf("legacy registration must be removed after migration, stat=%v", err)
	}
}
