package dal

import (
	"context"
	"testing"
	"time"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestSyncProblemPipelineOverrides 资格镜像：个人题面爬取/AI 开关跟随资格来源。
// - 无任何资格 → 个人开关回落 null（跟随组织默认，公共域=关）
// - 开通 Pro（套餐开启能力）→ 自动开 true
// - 加入开启对应功能的非公共域组织 → 自动开 true
// - 到期/退出组织后 → 回落 null
func TestSyncProblemPipelineOverrides(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Org{}, &model.OrgMember{}, &model.SubscriptionPlan{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	d := &ProfileDal{db: db}
	ctx := context.Background()
	now := time.Now()

	// Pro 套餐：开启题面爬取 + AI 分析
	if err := db.Create(&model.SubscriptionPlan{
		Plan: "pro", Days: 30, SyncIntervalMin: 60,
		EnableFetchProblem: true, EnableAiAnalyze: true, Enabled: true,
	}).Error; err != nil {
		t.Fatalf("seed plan: %v", err)
	}

	// 用户 A：无订阅、仅公共域（无资格）
	uA := model.User{Username: "user-a", Name: "A"}
	if err := db.Create(&uA).Error; err != nil {
		t.Fatalf("seed user A: %v", err)
	}
	// 公共域（系统组织）
	pub := model.Org{Name: "公共域", Slug: "public", IsSystem: true, Status: "active"}
	if err := db.Create(&pub).Error; err != nil {
		t.Fatalf("seed public org: %v", err)
	}
	if err := db.Create(&model.OrgMember{OrgID: pub.ID, UserID: uA.ID}).Error; err != nil {
		t.Fatalf("seed public membership: %v", err)
	}

	if err := d.SyncProblemPipelineOverrides(ctx, int64(uA.ID)); err != nil {
		t.Fatalf("sync no-entitlement: %v", err)
	}
	var a model.User
	db.First(&a, uA.ID)
	if a.ProblemFetchEnabled != nil || a.ProblemAIEnabled != nil {
		t.Fatalf("无资格应回落 null, got fetch=%v ai=%v", a.ProblemFetchEnabled, a.ProblemAIEnabled)
	}

	// 开通 Pro：自动开
	exp := now.Add(30 * 24 * time.Hour)
	if err := db.Model(&model.User{}).Where("id = ?", uA.ID).Updates(map[string]interface{}{
		"sub_tier": "pro", "sub_expire_at": exp, "sub_source": "manager",
	}).Error; err != nil {
		t.Fatalf("grant pro: %v", err)
	}
	if err := d.SyncProblemPipelineOverrides(ctx, int64(uA.ID)); err != nil {
		t.Fatalf("sync pro: %v", err)
	}
	db.First(&a, uA.ID)
	if a.ProblemFetchEnabled == nil || !*a.ProblemFetchEnabled || a.ProblemAIEnabled == nil || !*a.ProblemAIEnabled {
		t.Fatalf("Pro 应自动开 true, got fetch=%v ai=%v", a.ProblemFetchEnabled, a.ProblemAIEnabled)
	}

	// 订阅过期：回落 null
	if err := db.Model(&model.User{}).Where("id = ?", uA.ID).Update("sub_expire_at", now.Add(-time.Hour)).Error; err != nil {
		t.Fatalf("expire sub: %v", err)
	}
	if err := d.SyncProblemPipelineOverrides(ctx, int64(uA.ID)); err != nil {
		t.Fatalf("sync expired: %v", err)
	}
	db.First(&a, uA.ID)
	if a.ProblemFetchEnabled != nil || a.ProblemAIEnabled != nil {
		t.Fatalf("过期应回落 null, got fetch=%v ai=%v", a.ProblemFetchEnabled, a.ProblemAIEnabled)
	}

	// 加入非公共域组织（仅开启爬取、不开启 AI）：fetch 开、ai 回落
	org := model.Org{Name: "校队", Slug: "team-org", Status: "active", InviteCode: "TEAMCODE", EnableFetchProblem: true}
	if err := db.Create(&org).Error; err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// EnableAiAnalyze 显式写 false（GORM 零值不写，用 map 强制）
	if err := db.Model(&model.Org{}).Where("id = ?", org.ID).
		Updates(map[string]interface{}{"enable_ai_analyze": false}).Error; err != nil {
		t.Fatalf("set org ai off: %v", err)
	}
	if err := db.Create(&model.OrgMember{OrgID: org.ID, UserID: uA.ID}).Error; err != nil {
		t.Fatalf("seed org membership: %v", err)
	}
	if err := d.SyncProblemPipelineOverrides(ctx, int64(uA.ID)); err != nil {
		t.Fatalf("sync org: %v", err)
	}
	db.First(&a, uA.ID)
	if a.ProblemFetchEnabled == nil || !*a.ProblemFetchEnabled {
		t.Fatalf("组织开爬取应自动开 fetch, got %v", a.ProblemFetchEnabled)
	}
	if a.ProblemAIEnabled != nil {
		t.Fatalf("组织关 AI 且无订阅应回落 null, got %v", a.ProblemAIEnabled)
	}

	// 退出组织：回落 null
	if err := db.Where("org_id = ? AND user_id = ?", org.ID, uA.ID).Delete(&model.OrgMember{}).Error; err != nil {
		t.Fatalf("leave org: %v", err)
	}
	if err := d.SyncProblemPipelineOverrides(ctx, int64(uA.ID)); err != nil {
		t.Fatalf("sync leave: %v", err)
	}
	db.First(&a, uA.ID)
	if a.ProblemFetchEnabled != nil || a.ProblemAIEnabled != nil {
		t.Fatalf("退出组织应回落 null, got fetch=%v ai=%v", a.ProblemFetchEnabled, a.ProblemAIEnabled)
	}
}

// TestSyncProblemPipelineOverridesBatch 批量版本：
// - 组织开关从开变关后，成员残留的 true 覆盖被清掉（回落）
// - Pro 订阅过期后批量回落
func TestSyncProblemPipelineOverridesBatch(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Org{}, &model.OrgMember{}, &model.SubscriptionPlan{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	d := &ProfileDal{db: db}
	ctx := context.Background()

	if err := db.Create(&model.SubscriptionPlan{
		Plan: "pro", Days: 30, SyncIntervalMin: 60,
		EnableFetchProblem: true, EnableAiAnalyze: true, Enabled: true,
	}).Error; err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	pub := model.Org{Name: "公共域", Slug: "public", IsSystem: true, Status: "active", InviteCode: "PUB1"}
	if err := db.Create(&pub).Error; err != nil {
		t.Fatalf("seed public org: %v", err)
	}
	org := model.Org{Name: "校队", Slug: "team-org", Status: "active", InviteCode: "TEAM1", EnableFetchProblem: true, EnableAiAnalyze: true}
	if err := db.Create(&org).Error; err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// 两个成员：A 仅组织资格；B 组织 + Pro
	uA := model.User{Username: "user-a", Name: "A", Email: "a@test.dev"}
	uB := model.User{Username: "user-b", Name: "B", Email: "b@test.dev"}
	if err := db.Create(&uA).Error; err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if err := db.Create(&uB).Error; err != nil {
		t.Fatalf("seed B: %v", err)
	}
	for _, u := range []model.User{uA, uB} {
		if err := db.Create(&model.OrgMember{OrgID: org.ID, UserID: u.ID}).Error; err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	if err := db.Create(&model.OrgMember{OrgID: pub.ID, UserID: uA.ID}).Error; err != nil {
		t.Fatalf("seed pub membership: %v", err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", uB.ID).Updates(map[string]interface{}{
		"sub_tier": "pro", "sub_expire_at": time.Now().Add(30 * 24 * time.Hour),
	}).Error; err != nil {
		t.Fatalf("grant B pro: %v", err)
	}

	// 加入组织时自动开（模拟单用户同步）
	for _, uid := range []int64{int64(uA.ID), int64(uB.ID)} {
		if err := d.SyncProblemPipelineOverrides(ctx, uid); err != nil {
			t.Fatalf("sync join uid=%d: %v", uid, err)
		}
	}
	var a, b model.User
	db.First(&a, uA.ID)
	db.First(&b, uB.ID)
	if a.ProblemFetchEnabled == nil || !*a.ProblemFetchEnabled || b.ProblemFetchEnabled == nil || !*b.ProblemFetchEnabled {
		t.Fatalf("组织资格应开 true, A=%v B=%v", a.ProblemFetchEnabled, b.ProblemFetchEnabled)
	}

	// 组织关闭爬取：成员批量重查 → 残留 true 清掉
	if err := db.Model(&model.Org{}).Where("id = ?", org.ID).
		Updates(map[string]interface{}{"enable_fetch_problem": false}).Error; err != nil {
		t.Fatalf("org fetch off: %v", err)
	}
	if _, err := d.SyncProblemPipelineOverridesBatch(ctx, []int64{int64(uA.ID), int64(uB.ID)}); err != nil {
		t.Fatalf("batch resync: %v", err)
	}
	db.First(&a, uA.ID)
	db.First(&b, uB.ID)
	// A 无其他资格 → fetch 回落 null；B 有 Pro → fetch 仍 true（订阅资格还在）
	if a.ProblemFetchEnabled != nil {
		t.Fatalf("A 组织关爬取应回落 null, got %v", a.ProblemFetchEnabled)
	}
	if b.ProblemFetchEnabled == nil || !*b.ProblemFetchEnabled {
		t.Fatalf("B 有 Pro 应保持 true, got %v", b.ProblemFetchEnabled)
	}
	// AI 组织还开着 → 两人 ai 仍 true
	if a.ProblemAIEnabled == nil || !*a.ProblemAIEnabled {
		t.Fatalf("A 组织 AI 仍开应 true, got %v", a.ProblemAIEnabled)
	}

	// 全量批量（定时任务）：B 订阅过期 → 回落；A 无变化
	if err := db.Model(&model.User{}).Where("id = ?", uB.ID).
		Update("sub_expire_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatalf("expire B: %v", err)
	}
	if _, err := d.SyncProblemPipelineOverridesBatch(ctx, nil); err != nil {
		t.Fatalf("batch full: %v", err)
	}
	db.First(&b, uB.ID)
	// B 过期 + 组织 AI 还开 → ai 仍 true；fetch 组织关了 → 回落 null
	if b.ProblemFetchEnabled != nil {
		t.Fatalf("B 过期且组织关爬取应回落 null, got %v", b.ProblemFetchEnabled)
	}
	if b.ProblemAIEnabled == nil || !*b.ProblemAIEnabled {
		t.Fatalf("B 过期但组织 AI 开应 true, got %v", b.ProblemAIEnabled)
	}
}
