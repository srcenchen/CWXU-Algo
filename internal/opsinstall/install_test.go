package opsinstall

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"cwxu-algo/internal/opsroot"
)

func TestAppEnvIncludesEscapedBackupPostgresURI(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join(".env"), []byte("POSTGRES_USER=user+ops@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.Join("secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("secrets", "postgres_password"), []byte("p@ss:/?#[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New(root).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	value := ""
	for _, line := range splitLines(string(mustReadFile(t, root.Join("secrets", "app.env")))) {
		if strings.HasPrefix(line, "CWXU_BACKUP_PG_DSN=") {
			value = strings.TrimPrefix(line, "CWXU_BACKUP_PG_DSN=")
		}
	}
	u, err := url.Parse(value)
	if err != nil || u.Host != "postgres:5432" || u.User.Username() != "user+ops@example.com" {
		t.Fatalf("backup DSN invalid: %q, %v", value, err)
	}
	password, ok := u.User.Password()
	if !ok || password != "p@ss:/?#[]" {
		t.Fatalf("backup DSN password was not URI escaped: %q", value)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

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
	for _, name := range []string{"postgres_password", "redis_password", "rabbitmq_password"} {
		data, err := os.ReadFile(root.Join("secrets", name))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 65 {
			t.Fatalf("%s must be 64 hex + newline, got %d bytes", name, len(data))
		}
	}
	if _, err := os.Stat(root.Join("secrets", "config_encryption_key")); !os.IsNotExist(err) {
		t.Fatalf("config_encryption_key must not be generated, stat error = %v", err)
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

func TestAppEnvOmitsConfigEncryptionKey(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := New(root).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(root.Join("secrets", "app.env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "CONFIG_ENCRYPTION_KEY") {
		t.Fatalf("app.env contains retired config encryption key: %s", content)
	}
}

func TestAdminCreatedMarker(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
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
	if err := inst.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !AdminCreated(root) {
		t.Fatal("repeat installer call must preserve adminCreated")
	}
}

func TestAppEnvUsesConfiguredDatabaseAndRabbitUsers(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\nPOSTGRES_USER=custom_pg\nRABBITMQ_USER=custom_mq\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New(root).Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(root.Join("secrets", "app.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"POSTGRES_USER=custom_pg", "RABBITMQ_USER=custom_mq", "user=custom_pg", "amqp://custom_mq:"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("app.env missing %q: %s", want, content)
		}
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

func TestRefreshManagedWritesMissingTemplatesAndSnapshots(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("compose.yaml"), []byte("old-compose\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := RefreshManaged(root)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("missing templates must be reported as changed")
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
	} {
		if _, err := os.Stat(root.Join(relative)); err != nil {
			t.Errorf("missing %s: %v", relative, err)
		}
	}
	got, err := os.ReadFile(root.Join("compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "old-compose\n" {
		t.Fatal("compose.yaml was not refreshed from embedded template")
	}
	snapshot, err := os.ReadFile(root.Join(templatesBackupDir, "compose.yaml"))
	if err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if string(snapshot) != "old-compose\n" {
		t.Fatalf("snapshot content = %q, want old-compose", snapshot)
	}
	// 内容一致时跳过，不再产生变化。
	changed, err = RefreshManaged(root)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("identical templates must not be reported as changed")
	}
}

func TestRestoreManagedRestoresSnapshotAndCleansUp(t *testing.T) {
	root, err := opsroot.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root.Join("config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("config", "gateway.yaml"), []byte("old-gateway\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := RefreshManaged(root)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected refresh to change files")
	}
	if _, err := os.Stat(root.Join(templatesBackupDir, "config", "gateway.yaml")); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	restored, err := RestoreManaged(root)
	if err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("expected restore to change files")
	}
	got, err := os.ReadFile(root.Join("config", "gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old-gateway\n" {
		t.Fatalf("gateway.yaml after restore = %q, want old-gateway", got)
	}
	if _, err := os.Stat(root.Join(templatesBackupDir, "config", "gateway.yaml")); !os.IsNotExist(err) {
		t.Fatalf("snapshot should be removed after restore, stat error = %v", err)
	}
	// 无快照时静默无操作。
	restored, err = RestoreManaged(root)
	if err != nil || restored {
		t.Fatalf("restore without snapshot: restored=%v err=%v", restored, err)
	}
}
