package dal

import (
	"context"
	"testing"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestUpsertPlanZeroValues 回归：带 default 标签的字段传零值（0/false）必须真实写入，
// 不被 GORM 零值剔除 + DB DEFAULT 覆盖（manual_refresh_daily=0 曾变回 2、enabled=false 无法下架）。
func TestUpsertPlanZeroValues(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.SubscriptionPlan{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	d := &SubscriptionDal{db: db}
	ctx := context.Background()

	// 首次 upsert：全 0 / false（除 plan、sync、days）
	p := model.SubscriptionPlan{
		Plan: "free", PriceCents: 0, ManualRefreshDaily: 0, SyncIntervalMin: 180,
		AiAnalyzeMonth: 0, EnableFetchProblem: false, EnableAiAnalyze: false,
		EnableAiDaily: false, EnableRegularDaily: false, Days: 30, Enabled: false,
	}
	if err := d.UpsertPlan(ctx, p); err != nil {
		t.Fatalf("upsert zero: %v", err)
	}
	got, err := d.PlanByTier(ctx, "free")
	if err != nil {
		t.Fatalf("plan by tier: %v", err)
	}
	if got.ManualRefreshDaily != 0 || got.EnableRegularDaily || got.Enabled {
		t.Fatalf("zero values not persisted: manualRefresh=%d regular=%v enabled=%v",
			got.ManualRefreshDaily, got.EnableRegularDaily, got.Enabled)
	}

	// 覆盖更新：0 与 非零混合，0 值仍须真实写入（不能被旧值/默认值覆盖）
	p2 := model.SubscriptionPlan{
		Plan: "free", PriceCents: 0, ManualRefreshDaily: 0, SyncIntervalMin: 60,
		AiAnalyzeMonth: 0, EnableFetchProblem: true, EnableAiAnalyze: false,
		EnableAiDaily: true, EnableRegularDaily: true, Days: 30, Enabled: true,
	}
	if err := d.UpsertPlan(ctx, p2); err != nil {
		t.Fatalf("upsert mixed: %v", err)
	}
	got2, err := d.PlanByTier(ctx, "free")
	if err != nil {
		t.Fatalf("plan by tier 2: %v", err)
	}
	if got2.ManualRefreshDaily != 0 || got2.SyncIntervalMin != 60 || got2.EnableAiAnalyze {
		t.Fatalf("mixed update wrong: manualRefresh=%d sync=%d ai=%v",
			got2.ManualRefreshDaily, got2.SyncIntervalMin, got2.EnableAiAnalyze)
	}
}
