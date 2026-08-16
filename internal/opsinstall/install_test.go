package opsinstall

import (
	"context"
	"os"
	"strings"
	"testing"

	"cwxu-algo/internal/opsroot"
)

func TestInstallProvisionsFullLayout(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := New(root)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}
	for _, relative := range []string{
		"compose.yaml",
		"config/gateway.yaml",
		"config/user.yaml",
		"config/core-data.yaml",
		"config/agent.yaml",
		"config/postgres-init.sh",
		"config/nginx.conf",
		"config/rabbitmq-entrypoint.sh",
		".env",
		"state/install.json",
	} {
		if _, err := os.Stat(root.Join(relative)); err != nil {
			t.Errorf("missing %s: %v", relative, err)
		}
	}
}

func TestInstallGeneratesSecretsAndCertificates(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := New(root)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Backup key must be exactly 32 raw bytes (not hex text).
	info, err := os.Stat(root.Join("secrets", "backup_encryption_key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 32 {
		t.Fatalf("backup key must be 32 raw bytes, got %d", info.Size())
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("backup key must be 0600, got %v", info.Mode().Perm())
	}
	// Text passwords must be 64 hex chars plus newline.
	for _, name := range []string{"postgres_password", "redis_password", "rabbitmq_password", "config_encryption_key"} {
		data, err := os.ReadFile(root.Join("secrets", name))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 65 {
			t.Fatalf("%s must be 64 hex + newline, got %d bytes", name, len(data))
		}
	}
	// RSA key pair must exist.
	for _, name := range []string{"jwt_private_key.pem", "jwt_public_key.pem"} {
		if _, err := os.Stat(root.Join("secrets", name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

func TestInstallPreservesExistingSecrets(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := New(root)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(root.Join("secrets", "postgres_password"))
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(root.Join("secrets", "postgres_password"))
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(after) {
		t.Fatal("re-install must not regenerate existing secrets")
	}
}

func TestAppEnvHasNoEmbeddedNewlinesInDSNs(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := New(root)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(root.Join("secrets", "app.env"))
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Fatal("app.env must not be empty")
	}
	// Every assignment must fit on exactly one line.
	for _, line := range splitLines(string(content)) {
		if line == "" {
			t.Fatal("app.env must not contain blank lines")
		}
	}
}

func TestAppEnvIncludesJWTSecret(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := New(root)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(root.Join("secrets", "app.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range splitLines(string(content)) {
		if strings.HasPrefix(line, "CWXU_JWT_SECRET=") {
			value := strings.TrimPrefix(line, "CWXU_JWT_SECRET=")
			if len(value) < 32 {
				t.Fatalf("CWXU_JWT_SECRET must be >= 32 chars, got %d", len(value))
			}
			return
		}
	}
	t.Fatal("app.env must define CWXU_JWT_SECRET")
}

func TestAdminCreatedMarker(t *testing.T) {	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := New(root)
	if err := inst.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if AdminCreated(root) {
		t.Fatal("fresh install must not be admin-created")
	}
	if err := MarkAdminCreated(root); err != nil {
		t.Fatal(err)
	}
	if !AdminCreated(root) {
		t.Fatal("marker must be readable after set")
	}
}

func splitLines(content string) []string {
	var lines []string
	current := ""
	for _, r := range content {
		if r == '\n' {
			lines = append(lines, current)
			current = ""
			continue
		}
		current += string(r)
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}
