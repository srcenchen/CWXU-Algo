package sitesettings

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRowToRuntimeReadsPlaintextSecrets(t *testing.T) {
	rt, err := (&Row{
		BackupEnabled: true, BackupTime: "03:15", BackupPrefix: " disaster ",
		UpyunBucket: "bucket", UpyunOperator: "operator", UpyunPassword: "upyun-plain",
		SMTPPassword: "smtp-plain", AgentSecret: "agent-plain",
		AiAnalyzeSecret: "analyze-plain", OjLuoguPassword: "luogu-plain",
		OjQojPassword: "qoj-plain", PayFmSecret: "payfm-plain",
	}).ToRuntimeChecked()
	if err != nil {
		t.Fatal(err)
	}
	if rt.SMTPPassword != "smtp-plain" || rt.AgentSecret != "agent-plain" ||
		rt.AiAnalyzeSecret != "analyze-plain" || rt.OjLuoguPassword != "luogu-plain" ||
		rt.OjQojPassword != "qoj-plain" || rt.PayFmSecret != "payfm-plain" {
		t.Fatalf("plaintext secrets were not preserved: %#v", rt)
	}
	if !rt.BackupEnabled || rt.BackupTime != "03:15" || rt.BackupPrefix != "disaster" ||
		rt.UpyunBucket != "bucket" || rt.UpyunOperator != "operator" || rt.UpyunPassword != "upyun-plain" {
		t.Fatalf("backup runtime fields were not copied: %#v", rt)
	}
}

func TestRowToRuntimeConcurrencyDefaults(t *testing.T) {
	rt, err := (&Row{}).ToRuntimeChecked()
	if err != nil {
		t.Fatal(err)
	}
	if rt.SpiderConcurrency != 4 || rt.ProblemFetchConcurrency != 4 || rt.ProblemAnalyzeConcurrency != 4 {
		t.Fatalf("concurrency defaults = %d/%d/%d, want 4/4/4", rt.SpiderConcurrency, rt.ProblemFetchConcurrency, rt.ProblemAnalyzeConcurrency)
	}

	rt, err = (&Row{SpiderConcurrency: 12, ProblemFetchConcurrency: 9, ProblemAnalyzeConcurrency: 7}).ToRuntimeChecked()
	if err != nil {
		t.Fatal(err)
	}
	if rt.SpiderConcurrency != 12 || rt.ProblemFetchConcurrency != 9 || rt.ProblemAnalyzeConcurrency != 7 {
		t.Fatalf("concurrency mapping = %d/%d/%d, want 12/9/7", rt.SpiderConcurrency, rt.ProblemFetchConcurrency, rt.ProblemAnalyzeConcurrency)
	}
}

func TestRuntimeJSONBackwardCompatibleConcurrency(t *testing.T) {
	var old Runtime
	if err := json.Unmarshal([]byte(`{"siteTitle":"GoAlgo","configVersion":1}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.SpiderConcurrency != 0 || old.ProblemFetchConcurrency != 0 || old.ProblemAnalyzeConcurrency != 0 {
		t.Fatalf("old JSON must preserve unset concurrency, got %#v", old)
	}

	b, err := json.Marshal(Runtime{SpiderConcurrency: 8, ProblemFetchConcurrency: 10, ProblemAnalyzeConcurrency: 6})
	if err != nil {
		t.Fatal(err)
	}
	var current Runtime
	if err := json.Unmarshal(b, &current); err != nil {
		t.Fatal(err)
	}
	if current.SpiderConcurrency != 8 || current.ProblemFetchConcurrency != 10 || current.ProblemAnalyzeConcurrency != 6 {
		t.Fatalf("JSON round trip = %#v", current)
	}
}

func TestBackupTimeDefaultsAndValidatesStrictHHMM(t *testing.T) {
	for input, want := range map[string]string{"": "02:00", "3:15": "02:00", "24:00": "02:00", "03:15": "03:15"} {
		if got := NormalizeBackupTime(input); got != want {
			t.Errorf("NormalizeBackupTime(%q) = %q, want %q", input, got, want)
		}
	}
	if err := ValidateBackupTime("03:15"); err != nil {
		t.Fatalf("valid time rejected: %v", err)
	}
	if err := ValidateBackupTime("3:15"); err == nil {
		t.Fatal("non-HH:mm time accepted")
	}
}

func TestRowToRuntimeRejectsUnmigratedCiphertext(t *testing.T) {
	_, err := (&Row{SMTPPassword: "enc:v1:broken"}).ToRuntimeChecked()
	if err == nil || !strings.Contains(err.Error(), "smtp_password") {
		t.Fatalf("expected diagnostic smtp_password error, got %v", err)
	}
}

func TestRuntimeWorthCaching(t *testing.T) {
	if (&Runtime{SiteTitle: "GoAlgo"}).worthCaching() {
		t.Fatal("title-only should not be cached")
	}
	if !(&Runtime{SMTPHost: "smtp.example.com"}).worthCaching() {
		t.Fatal("SMTP host should be cacheable")
	}
	if !(&Runtime{AiAnalyzeEndpoint: "http://x"}).worthCaching() {
		t.Fatal("AI endpoint should be cacheable")
	}
	if !(&Runtime{AgentModel: "gpt"}).worthCaching() {
		t.Fatal("agent model should be cacheable")
	}
	if !(&Runtime{AgentEndpoint: "https://api.openai.com/v1"}).worthCaching() {
		t.Fatal("agent endpoint should be cacheable")
	}
	if !(&Runtime{BackupEnabled: true}).worthCaching() {
		t.Fatal("enabled backup switch should be cacheable")
	}
	if !(&Runtime{ConfigVersion: 1, BackupEnabled: false}).worthCaching() {
		t.Fatal("versioned disabled backup switch should be cacheable")
	}
}

func TestValidateEndpoint(t *testing.T) {
	if err := ValidateEndpoint("https://api.openai.com/v1"); err != nil {
		t.Fatalf("valid endpoint rejected: %v", err)
	}
	if err := ValidateEndpoint("http://host:8001/api"); err != nil {
		t.Fatalf("http endpoint rejected: %v", err)
	}
	if err := ValidateEndpoint(""); err != nil {
		t.Fatalf("empty endpoint should pass: %v", err)
	}
	if err := ValidateEndpoint("ftp://x/v1"); err == nil {
		t.Fatal("ftp scheme should be rejected")
	}
	if err := ValidateEndpoint("https://u:p@host/v1"); err == nil {
		t.Fatal("userinfo should be rejected")
	}
	if err := ValidateEndpoint("https://host/v1?k=v"); err == nil {
		t.Fatal("query should be rejected")
	}
	if err := ValidateEndpoint("not-a-url"); err == nil {
		t.Fatal("invalid url should be rejected")
	}
}

func TestHasSMTP(t *testing.T) {
	if (*Runtime)(nil).HasSMTP() {
		t.Fatal("nil should not have SMTP")
	}
	if (&Runtime{}).HasSMTP() {
		t.Fatal("empty should not have SMTP")
	}
	if !(&Runtime{SMTPHost: "smtp.example.com"}).HasSMTP() {
		t.Fatal("host set should have SMTP")
	}
}

func TestLoadNilDBNoPanic(t *testing.T) {
	// Redis miss + db=nil（core_data 路径）：返回空 Runtime，不得 panic
	rt := Load(context.Background(), nil, nil)
	if rt == nil {
		t.Fatal("expected non-nil runtime")
	}
	if rt.HasSMTP() {
		t.Fatal("expected no SMTP without redis/db")
	}
	if rt.SiteTitle != "GoAlgo" {
		t.Fatalf("site title = %q", rt.SiteTitle)
	}
}

func TestPublishRedisNilSafe(t *testing.T) {
	if err := PublishRedis(context.Background(), nil, &Runtime{SMTPHost: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := PublishRedis(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
}
