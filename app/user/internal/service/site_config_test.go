package service

import (
	"testing"

	site "cwxu-algo/api/user/v1/site"
	"cwxu-algo/app/user/internal/data/model"
)

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

func TestSectionUnknown(t *testing.T) {
	if _, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{}, fakeEncrypt, "bogus"); err == nil {
		t.Fatal("unknown section should be rejected")
	}
}

func TestSectionDefaultAll(t *testing.T) {
	updates, err := buildSectionUpdatesWith(&model.SiteConfig{}, &site.UpdateConfigReq{
		SmtpHost:  "smtp.example.com",
		SiteTitle: "GoAlgo",
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
