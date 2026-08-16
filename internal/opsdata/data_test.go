package opsdata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	data := &Data{
		Version: schemaVersion,
		Deploy:  Deploy{LastDigests: map[string]string{"frontend": "sha256:aaaa"}, UpdatedAt: "now"},
		Webhook: Webhook{Key: "secret", Port: 8787, Bind: "0.0.0.0", EnabledActions: []string{"upgrade"}},
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
	if got.Webhook.Key != "secret" || got.Webhook.Port != 8787 {
		t.Fatalf("webhook state lost: %#v", got.Webhook)
	}
	info, err := os.Stat(filepath.Join(dir, ".ops.data.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("data file must be 0600, got %v", info.Mode().Perm())
	}
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	data, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if data.Version != schemaVersion {
		t.Fatalf("expected default version")
	}
}
