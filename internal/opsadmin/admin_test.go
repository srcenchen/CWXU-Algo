package opsadmin

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRejectsSymlinkAndMode(t *testing.T) {
	dir := t.TempDir()
	sym := filepath.Join(dir, "sym.env")
	target := filepath.Join(dir, "real.env")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, sym); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAdminConfigFile(sym); err == nil {
		t.Fatal("expected symlink rejection")
	}
	loose := filepath.Join(dir, "loose.env")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAdminConfigFile(loose); err == nil {
		t.Fatal("expected 0600 requirement")
	}
}
