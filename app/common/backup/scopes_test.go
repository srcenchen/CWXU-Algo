package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestHasNoSQLSecretKeyFingerprint(t *testing.T) {
	raw, err := json.Marshal(Manifest{Version: FormatVersion})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "encryptionKeyFp") {
		t.Fatalf("manifest must not contain SQL secret key fingerprint: %s", raw)
	}
}

func TestNormalizeScopes(t *testing.T) {
	all, err := NormalizeScopes(nil)
	if err != nil || len(all) != 1 || all[0] != ScopeAll {
		t.Fatalf("empty → all: %v %v", all, err)
	}
	s, err := NormalizeScopes([]string{"users", "USERS", "problems"})
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 2 {
		t.Fatalf("dedupe: %v", s)
	}
	if _, err := NormalizeScopes([]string{"nope"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestExpandedAndNeedsCore(t *testing.T) {
	ex := ExpandedScopes([]string{ScopeAll})
	if !HasScope(ex, ScopeSubmits) || !HasScope(ex, ScopeFiles) {
		t.Fatalf("all should expand: %v", ex)
	}
	if !NeedsCoreDB(ExpandedScopes([]string{ScopeProblems})) {
		t.Fatal("problems needs core")
	}
	if NeedsCoreDB(ExpandedScopes([]string{ScopeUsers})) {
		t.Fatal("users does not need core")
	}
}

func TestBuildExportOrderBy(t *testing.T) {
	// user_ac_problems: 无 day，不得出现 day
	order := buildExportOrderBy(func(c string) bool {
		return c == "user_id" || c == "problem_key" || c == "platform" || c == "first_ac_at"
	})
	if !strings.Contains(order, "user_id") || !strings.Contains(order, "problem_key") {
		t.Fatalf("user_ac_problems order: %q", order)
	}
	if strings.Contains(order, "day") {
		t.Fatalf("must not order by missing day: %q", order)
	}
	// daily_user_stats: user_id + day + platform
	order2 := buildExportOrderBy(func(c string) bool {
		return c == "user_id" || c == "day" || c == "platform"
	})
	if !strings.Contains(order2, "day") || !strings.Contains(order2, "platform") {
		t.Fatalf("daily_user_stats order: %q", order2)
	}
	// submit_logs: submit_id 优先
	order3 := buildExportOrderBy(func(c string) bool {
		return c == "submit_id" || c == "user_id" || c == "platform" || c == "id"
	})
	if !strings.Contains(order3, "submit_id") {
		t.Fatalf("submit_logs order: %q", order3)
	}
}

func TestPlanQuotaTableName(t *testing.T) {
	// 回归：GORM 把 PlanQuota 收成 plan_quota，备份清单不得写成 plan_quotas
	for _, spec := range AllTableSpecs {
		if strings.Contains(spec.File, "plan_quota") || strings.Contains(spec.Table, "plan_quota") {
			if spec.Table != "plan_quota" {
				t.Fatalf("PlanQuota table must be plan_quota, got %q", spec.Table)
			}
			if spec.File != "plan_quota.ndjson" {
				t.Fatalf("PlanQuota file must be plan_quota.ndjson, got %q", spec.File)
			}
			return
		}
	}
	t.Fatal("plan_quota missing from AllTableSpecs")
}

func TestZipRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	_ = os.MkdirAll(filepath.Join(src, "data"), 0o755)
	if err := os.WriteFile(filepath.Join(src, "manifest.json"), []byte(`{"version":"goalgo-backup-v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "data", "users.ndjson"), []byte("{\"id\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(dir, "b.zip")
	if err := ZipDir(src, zipPath); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := UnzipTo(zipPath, out); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil || len(raw) == 0 {
		t.Fatalf("manifest missing: %v", err)
	}
}
