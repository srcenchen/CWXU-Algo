package main

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	"cwxu-algo/internal/opsroot"
)

func TestValidateConfigExportRequiresEveryCriticalEntry(t *testing.T) {
	root := mustRoot(t, t.TempDir())
	for _, relative := range []string{".env", "release.env"} {
		if err := os.WriteFile(root.Join(relative), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(root.Join("config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateConfigExport(root); err == nil {
		t.Fatal("missing secrets must fail export")
	}
}

func TestValidateStagedConfigRejectsInvalidContentsBeforeCommit(t *testing.T) {
	root := mustRoot(t, t.TempDir())
	if err := seedValidStagedConfig(root.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("config", "user.yaml"), []byte("server: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateStagedConfig(root); err == nil {
		t.Fatal("invalid YAML accepted")
	}
	if err := os.WriteFile(root.Join("config", "user.yaml"), []byte("server: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root.Join("secrets", "backup_encryption_key"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateStagedConfig(root); err == nil {
		t.Fatal("empty critical secret accepted")
	}
}

func TestValidateStagedConfigParsesReleaseAndComposeOffline(t *testing.T) {
	root := mustRoot(t, t.TempDir())
	if err := seedValidStagedConfig(root.Path); err != nil {
		t.Fatal(err)
	}
	if err := validateStagedConfig(root); err != nil {
		t.Fatalf("valid staged config rejected: %v", err)
	}
	if err := os.WriteFile(root.Join("release.env"), []byte("BROKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateStagedConfig(root); err == nil {
		t.Fatal("invalid release accepted")
	}
}

func seedValidStagedConfig(path string) error {
	root, err := mustResolveRoot(path)
	if err != nil {
		return err
	}
	for _, dir := range []string{"config", "secrets"} {
		if err := os.MkdirAll(root.Join(dir), 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(root.Join(".env"), []byte("GOALGO_ROOT="+root.Path+"\n"), 0o600); err != nil {
		return err
	}
	if err := testRelease('a').WriteFile(root.Join("release.env")); err != nil {
		return err
	}
	if err := os.WriteFile(root.Join("compose.yaml"), []byte("services:\n  user:\n    image: test\n"), 0o600); err != nil {
		return err
	}
	for _, file := range []string{"gateway.yaml", "user.yaml", "core-data.yaml", "agent.yaml"} {
		if err := os.WriteFile(root.Join("config", file), []byte("server: {}\n"), 0o600); err != nil {
			return err
		}
	}
	for _, file := range []string{"app.env", "jwt_private_key.pem", "jwt_public_key.pem", "postgres_password", "redis_password", "rabbitmq_password"} {
		value := []byte("secret")
		if file == "app.env" {
			value = []byte("CWXU_BACKUP_PG_DSN=postgres://u:p@postgres:5432/postgres\n")
		}
		if err := os.WriteFile(root.Join("secrets", file), value, 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(root.Join("secrets", "backup_encryption_key"), make([]byte, 32), 0o600)
}

func mustResolveRoot(path string) (*opsroot.Root, error) { return opsroot.Resolve(path) }

func TestExtractConfigBundleStagesBeforeReplacingTarget(t *testing.T) {
	root := mustRoot(t, t.TempDir())
	if err := os.WriteFile(root.Join(".env"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(t.TempDir(), "bad.tar")
	file, err := os.Create(bundle)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(file)
	data := []byte("new")
	if err := w.WriteHeader(&tar.Header{Name: ".env", Mode: 0o600, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: 0}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := importConfigBundle(root, bundle); err == nil {
		t.Fatal("invalid archive accepted")
	}
	content, err := os.ReadFile(root.Join(".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "old" {
		t.Fatalf("target changed before full validation: %q", content)
	}
}
