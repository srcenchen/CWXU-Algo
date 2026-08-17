package service

import (
	"context"
	"strings"
	"testing"

	site "cwxu-algo/api/user/v1/site"
	"cwxu-algo/app/user/internal/data/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestAtomicSiteConfigUpdateRejectsStaleVersionByRowsAffected(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SiteConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SiteConfig{ID: 1, SiteTitle: "before", ConfigVersion: 2}).Error; err != nil {
		t.Fatal(err)
	}
	updated, err := updateSiteConfigAtomic(context.Background(), db, 1, map[string]interface{}{"site_title": "stale"})
	if err != nil || updated {
		t.Fatalf("stale update = %v, %v", updated, err)
	}
	updated, err = updateSiteConfigAtomic(context.Background(), db, 2, map[string]interface{}{"site_title": "fresh"})
	if err != nil || !updated {
		t.Fatalf("fresh update = %v, %v", updated, err)
	}
	var row model.SiteConfig
	if err := db.First(&row, 1).Error; err != nil {
		t.Fatal(err)
	}
	if row.SiteTitle != "fresh" || row.ConfigVersion != 3 {
		t.Fatalf("atomic update row = %#v", row)
	}
}

func TestAtomicSiteConfigUpdateWithoutExpectedVersionIncrementsDatabaseValue(t *testing.T) {
	db, _ := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	_ = db.AutoMigrate(&model.SiteConfig{})
	_ = db.Create(&model.SiteConfig{ID: 1, ConfigVersion: 7}).Error
	updated, err := updateSiteConfigAtomic(context.Background(), db, 0, map[string]interface{}{"site_title": "legacy-client"})
	if err != nil || !updated {
		t.Fatalf("legacy update = %v, %v", updated, err)
	}
	var row model.SiteConfig
	_ = db.First(&row, 1).Error
	if row.ConfigVersion != 8 {
		t.Fatalf("version = %d, want database increment to 8", row.ConfigVersion)
	}
}

func TestReadSiteSecretRejectsUnmigratedCiphertext(t *testing.T) {
	_, err := readSiteSecret("oj_luogu_password", "enc:v1:broken")
	if err == nil || !strings.Contains(err.Error(), "oj_luogu_password") {
		t.Fatalf("expected diagnostic field error, got %v", err)
	}
}

func TestReadSiteSecretAcceptsPlaintext(t *testing.T) {
	got, err := readSiteSecret("agent_secret", "plain")
	if err != nil || got != "plain" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func fakeEncrypt(v string) (string, error) { return "enc:" + v, nil }

func TestSectionProjectionAIOnly(t *testing.T) {
	updates, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{
		AgentEndpoint:     "https://api.openai.com/v1",
		AgentModel:        "gpt-4.1-mini",
		AiAnalyzeEndpoint: "https://x.com/v1",
		SmtpHost:          "smtp.example.com", // 非本分区，应被忽略
	}, fakeEncrypt, "ai")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updates["smtp_host"]; ok {
		t.Fatal("ai section must not touch smtp_host")
	}
	if updates["agent_endpoint"] != "https://api.openai.com/v1" {
		t.Fatalf("agent_endpoint = %v", updates["agent_endpoint"])
	}
	if updates["ai_analyze_endpoint"] != "https://x.com/v1" {
		t.Fatalf("ai_analyze_endpoint = %v", updates["ai_analyze_endpoint"])
	}
}

func TestSectionProjectionEmailOnly(t *testing.T) {
	updates, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{
		SmtpHost:          "smtp.example.com",
		AgentModel:        "gpt-4.1-mini",
		AiAnalyzeEndpoint: "https://x.com/v1",
	}, fakeEncrypt, "email")
	if err != nil {
		t.Fatal(err)
	}
	if updates["smtp_host"] != "smtp.example.com" {
		t.Fatalf("smtp_host = %v", updates["smtp_host"])
	}
	if _, ok := updates["agent_model"]; ok {
		t.Fatal("email section must not touch agent_model")
	}
}

func TestSectionProjectionBackupOnly(t *testing.T) {
	updates, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{
		BackupEnabled: true,
		BackupTime:    "03:15",
		BackupPrefix:  " disaster ",
		SmtpHost:      "smtp.example.com",
	}, fakeEncrypt, "backup")
	if err != nil {
		t.Fatal(err)
	}
	if updates["backup_enabled"] != true {
		t.Fatalf("backup_enabled = %v", updates["backup_enabled"])
	}
	if updates["backup_time"] != "03:15" || updates["backup_prefix"] != "disaster" {
		t.Fatalf("backup schedule updates = %#v", updates)
	}
	if _, ok := updates["smtp_host"]; ok {
		t.Fatal("backup section must not touch smtp_host")
	}
}

func TestSectionProjectionBackupRejectsInvalidTime(t *testing.T) {
	_, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{BackupTime: "3:15"}, fakeEncrypt, "backup")
	if err == nil || !strings.Contains(err.Error(), "HH:mm") {
		t.Fatalf("invalid backup time error = %v", err)
	}
}

func TestSectionProjectionBackupRequiresPrefix(t *testing.T) {
	for _, prefix := range []string{"", "  ", "/"} {
		_, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{BackupEnabled: true, BackupTime: "02:00", BackupPrefix: prefix}, fakeEncrypt, "backup")
		if err == nil || !strings.Contains(err.Error(), "存储目录") {
			t.Fatalf("empty prefix %q error = %v", prefix, err)
		}
	}
	u, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{BackupEnabled: true, BackupTime: "02:00", BackupPrefix: "/goalgo/backup/"}, fakeEncrypt, "backup")
	if err != nil {
		t.Fatal(err)
	}
	if u["backup_prefix"] != "goalgo/backup" {
		t.Fatalf("backup_prefix = %v, want normalized goalgo/backup", u["backup_prefix"])
	}
}

func TestRowToRuntimeIncludesBackupEnabled(t *testing.T) {
	rt, err := rowToRuntime(&model.SiteConfig{BackupEnabled: true, BackupTime: "04:30", BackupPrefix: "bak", UpyunBucket: "b", UpyunOperator: "o", UpyunPassword: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.BackupEnabled || rt.BackupTime != "04:30" || rt.BackupPrefix != "bak" || rt.UpyunBucket != "b" || rt.UpyunOperator != "o" || rt.UpyunPassword != "p" {
		t.Fatalf("runtime backup settings were not propagated: %#v", rt)
	}
}

func TestSecretClearAndConflict(t *testing.T) {
	// 空 + clear=false → 不修改
	u, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{}, fakeEncrypt, "ai")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u["agent_secret"]; ok {
		t.Fatal("empty secret must not be written")
	}
	// clear=true → 清空
	u, err = buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{ClearAgentSecret: true}, fakeEncrypt, "ai")
	if err != nil {
		t.Fatal(err)
	}
	if u["agent_secret"] != "" {
		t.Fatalf("agent_secret = %v, want empty", u["agent_secret"])
	}
	// 新密钥 + clear=true → 冲突
	if _, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{AgentSecret: "new", ClearAgentSecret: true}, fakeEncrypt, "ai"); err == nil {
		t.Fatal("secret+clear should be rejected")
	}
}

func TestNewSiteSecretsAreStoredAsPlaintext(t *testing.T) {
	u, err := buildSectionUpdates("all", &model.SiteConfig{}, &site.UpdateConfigReq{
		SmtpPassword: "smtp", AgentSecret: "agent", AiAnalyzeSecret: "analyze",
		UpyunPassword: "upyun", OjLuoguPassword: "luogu", OjQojPassword: "qoj",
		PayfmSecret: "payfm", BackupEnabled: true, BackupTime: "02:00", BackupPrefix: "goalgo/backup",
	})
	if err != nil {
		t.Fatal(err)
	}
	for column, want := range map[string]string{
		"smtp_password": "smtp", "agent_secret": "agent", "ai_analyze_secret": "analyze",
		"upyun_password": "upyun", "oj_luogu_password": "luogu", "oj_qoj_password": "qoj",
		"payfm_secret": "payfm",
	} {
		if got := u[column]; got != want {
			t.Errorf("%s = %v, want plaintext %q", column, got, want)
		}
	}
}

func TestSectionUnknown(t *testing.T) {
	if _, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{}, fakeEncrypt, "bogus"); err == nil {
		t.Fatal("unknown section should be rejected")
	}
}

func TestSectionDefaultAll(t *testing.T) {
	updates, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{
		SmtpHost:      "smtp.example.com",
		SiteTitle:     "GoAlgo",
		BackupEnabled: true,
		BackupTime:    "02:00",
		BackupPrefix:  "goalgo/backup",
	}, fakeEncrypt, "")
	if err != nil {
		t.Fatal(err)
	}
	if updates["smtp_host"] != "smtp.example.com" {
		t.Fatal("default all should include smtp")
	}
	if updates["site_title"] != "GoAlgo" {
		t.Fatal("default all should include title")
	}
}

func TestEndpointValidationInAI(t *testing.T) {
	if _, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{AgentEndpoint: "ftp://x/v1"}, fakeEncrypt, "ai"); err == nil {
		t.Fatal("bad agent endpoint should be rejected")
	}
	if _, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{AiAnalyzeEndpoint: "https://x.com/v1?k=v"}, fakeEncrypt, "ai"); err == nil {
		t.Fatal("ai endpoint with query should be rejected")
	}
}

func TestSectionProjectionOpsOnly(t *testing.T) {
	updates, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{
		SpiderConcurrency:         9,
		ProblemAnalyzeConcurrency: 6,
		SmtpHost:                  "smtp.example.com",
	}, fakeEncrypt, "ops")
	if err != nil {
		t.Fatal(err)
	}
	if updates["spider_concurrency"] != 9 || updates["problem_analyze_concurrency"] != 6 {
		t.Fatalf("ops updates = %#v", updates)
	}
	if _, ok := updates["smtp_host"]; ok {
		t.Fatal("ops section must not touch smtp_host")
	}
}

func TestSectionProjectionOpsRejectsConcurrencyOutsideRange(t *testing.T) {
	for _, value := range []int32{0, 33} {
		_, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{
			SpiderConcurrency: value, ProblemAnalyzeConcurrency: 4,
		}, fakeEncrypt, "ops")
		if err == nil || !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "32") {
			t.Fatalf("spider concurrency %d error = %v", value, err)
		}
	}
}

func TestRowToRuntimeIncludesConcurrency(t *testing.T) {
	rt, err := rowToRuntime(&model.SiteConfig{SpiderConcurrency: 11, ProblemAnalyzeConcurrency: 5})
	if err != nil {
		t.Fatal(err)
	}
	if rt.SpiderConcurrency != 11 || rt.ProblemAnalyzeConcurrency != 5 {
		t.Fatalf("runtime concurrency = %#v", rt)
	}
}
