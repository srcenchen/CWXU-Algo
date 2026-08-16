package data

import (
	"errors"
	"strings"
	"testing"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openSiteSecretMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SiteConfig{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateLegacySiteSecretsAtomicallyWritesPlaintext(t *testing.T) {
	db := openSiteSecretMigrationDB(t)
	row := model.SiteConfig{ID: 1, SMTPPassword: "enc:v1:smtp", AgentSecret: "already-plain", PayFmSecret: "enc:v1:payfm"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	decrypt := func(value string) (string, error) { return strings.TrimPrefix(value, "enc:v1:"), nil }
	if err := migrateLegacySiteSecrets(db, decrypt); err != nil {
		t.Fatal(err)
	}
	var got model.SiteConfig
	if err := db.First(&got, 1).Error; err != nil {
		t.Fatal(err)
	}
	if got.SMTPPassword != "smtp" || got.AgentSecret != "already-plain" || got.PayFmSecret != "payfm" {
		t.Fatalf("unexpected migrated values: %#v", got)
	}
}

func TestMigrateLegacySiteSecretsFailurePreservesEveryValue(t *testing.T) {
	db := openSiteSecretMigrationDB(t)
	row := model.SiteConfig{ID: 1, SMTPPassword: "enc:v1:smtp", AgentSecret: "enc:v1:broken"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	decrypt := func(value string) (string, error) {
		if value == "enc:v1:broken" {
			return "", errors.New("authentication failed")
		}
		return "smtp", nil
	}
	err := migrateLegacySiteSecrets(db, decrypt)
	if err == nil || !strings.Contains(err.Error(), "agent_secret") {
		t.Fatalf("expected diagnostic agent_secret error, got %v", err)
	}
	var got model.SiteConfig
	if err := db.First(&got, 1).Error; err != nil {
		t.Fatal(err)
	}
	if got.SMTPPassword != row.SMTPPassword || got.AgentSecret != row.AgentSecret {
		t.Fatalf("failed migration modified row: %#v", got)
	}
}

func TestMigrateLegacyBootstrapOnlyFillsEmptyDatabaseFields(t *testing.T) {
	db := openSiteSecretMigrationDB(t)
	row := model.SiteConfig{ID: 1, SMTPHost: "db.smtp", SMTPPassword: "", AiAnalyzeEndpoint: "", AgentModel: "db-agent"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	legacy := LegacySiteConfig{
		SMTPHost: "yaml.smtp", SMTPPassword: "yaml-smtp-secret", SMTPPort: 465,
		AiAnalyzeEndpoint: "https://yaml-ai/v1", AiAnalyzeModel: "yaml-ai", AiAnalyzeSecret: "yaml-ai-secret",
		AgentModel: "yaml-agent", AgentSecret: "yaml-agent-secret",
	}
	if err := migrateLegacyBootstrapSiteConfig(db, legacy); err != nil {
		t.Fatal(err)
	}
	var got model.SiteConfig
	if err := db.First(&got, 1).Error; err != nil {
		t.Fatal(err)
	}
	if got.SMTPHost != "db.smtp" || got.SMTPPassword != "yaml-smtp-secret" {
		t.Fatalf("SMTP migration overwrote DB or missed empty field: %#v", got)
	}
	if got.AiAnalyzeEndpoint != "https://yaml-ai/v1" || got.AiAnalyzeSecret != "yaml-ai-secret" {
		t.Fatalf("AI migration missing: %#v", got)
	}
	if got.AgentModel != "db-agent" || got.AgentSecret != "yaml-agent-secret" {
		t.Fatalf("agent migration did not honor per-field emptiness: %#v", got)
	}
}
