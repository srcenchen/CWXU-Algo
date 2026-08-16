package main

import (
	"os"
	"path/filepath"
	"testing"

	"cwxu-algo/app/common/conf"
	"github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
)

func TestHistoricalUserYAMLStillLoadsMigrationFields(t *testing.T) {
	dir := t.TempDir()
	yaml := `server:
  config_encryption_key: yaml-legacy-key-yaml-legacy-key-1234
smtp:
  host: smtp.old.example
  port: 465
  username: old-user
  password: old-password
  from: old@example.com
agent:
  endpoint: https://agent.old/v1
  model: old-agent
  secret: old-agent-secret
ai_analyze:
  endpoint: https://analyze.old/v1
  model: old-analyze
  secret: old-analyze-secret
`
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c := config.New(config.WithSource(file.NewSource(dir)))
	defer c.Close()
	if err := c.Load(); err != nil {
		t.Fatal(err)
	}
	var bc conf.Bootstrap
	if err := c.Scan(&bc); err != nil {
		t.Fatal(err)
	}
	if bc.Server.GetConfigEncryptionKey() == "" || bc.Smtp.GetPassword() != "old-password" || bc.AiAnalyze.GetSecret() != "old-analyze-secret" || bc.Agent.GetSecret() != "old-agent-secret" {
		t.Fatalf("historical migration fields not loaded: %#v", &bc)
	}
}
