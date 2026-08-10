package service

import (
	"context"
	"testing"

	"cwxu-algo/app/user/internal/data"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestResolveAiAnalyzeStatus(t *testing.T) {
	cases := []struct {
		name         string
		proActive    bool
		proQuota     int
		orgUnlimited bool
		wantSource   string
		wantUnlimit  bool
		wantQuota    int
	}{
		{"无订阅无组织", false, 0, false, "none", false, 0},
		{"组织开通（无限）", false, 0, true, "org", true, 0},
		{"Pro 订阅", true, 400, false, "pro", false, 400},
		{"Pro + 组织开通（无限优先）", true, 400, true, "pro_org", true, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source, unlimited, quota := resolveAiAnalyzeStatus(c.proActive, c.proQuota, c.orgUnlimited)
			if source != c.wantSource || unlimited != c.wantUnlimit || quota != c.wantQuota {
				t.Fatalf("got (%s, %v, %d), want (%s, %v, %d)",
					source, unlimited, quota, c.wantSource, c.wantUnlimit, c.wantQuota)
			}
		})
	}
}

// TestOrgAiAnalyzeUnlimited 组织开通题面 AI 分析判定：
// 仅「非公共域 + active + enable_ai_analyze=true」的组织成员算组织开通（无限）。
func TestOrgAiAnalyzeUnlimited(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:org_ai_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.Org{}, &model.OrgMember{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := &SubscriptionService{data: &data.Data{DB: db}}

	// 公共域（is_system）即使开 AI 也不计
	if err := db.Create(&model.Org{Name: "公共域", Slug: model.PublicOrgSlug, IsSystem: true, EnableAiAnalyze: true}).Error; err != nil {
		t.Fatalf("seed public org: %v", err)
	}
	// 普通组织：开 AI 的组织（id=2）、关 AI 的组织（id=3）、suspended 组织（id=4）
	if err := db.Create([]*model.Org{
		{Name: "校队A", Slug: "team-a", InviteCode: "INV-A", EnableAiAnalyze: true},
		{Name: "校队B", Slug: "team-b", InviteCode: "INV-B"},
		{Name: "停用队", Slug: "team-c", InviteCode: "INV-C", Status: model.OrgStatusSuspended},
	}).Error; err != nil {
		t.Fatalf("seed orgs: %v", err)
	}
	// 注意：EnableAiAnalyze 带 default:true 标签，Create struct 时 false 会被 GORM 替换成 true，
	// 关 AI 的组织须用 Updates 显式置 false（与 UpsertPlan 同源的零值写入问题）
	if err := db.Model(&model.Org{}).Where("slug IN ?", []string{"team-b", "team-c"}).
		Update("enable_ai_analyze", false).Error; err != nil {
		t.Fatalf("disable ai analyze: %v", err)
	}
	members := []*model.OrgMember{
		{OrgID: 1, UserID: 10}, // 公共域
		{OrgID: 2, UserID: 11}, // 开 AI 组织
		{OrgID: 3, UserID: 12}, // 关 AI 组织
		{OrgID: 4, UserID: 13}, // 停用组织
		{OrgID: 2, UserID: 14}, // 开 AI + 关 AI 各一个
		{OrgID: 3, UserID: 14},
		{OrgID: 3, UserID: 15}, // 仅关 AI 组织
	}
	if err := db.Create(&members).Error; err != nil {
		t.Fatalf("seed members: %v", err)
	}

	ctx := context.Background()
	cases := []struct {
		uid  int64
		want bool
	}{
		{10, false},  // 公共域不计
		{11, true},   // 开 AI 组织
		{12, false},  // 关 AI 组织
		{13, false},  // 停用组织不计
		{14, true},   // 任一组织开 AI 即算
		{15, false},  // 仅关 AI 组织
		{99, false},  // 无组织
		{0, false},   // 非法 uid
	}
	for _, c := range cases {
		if got := s.orgAiAnalyzeUnlimited(ctx, c.uid); got != c.want {
			t.Fatalf("orgAiAnalyzeUnlimited(%d) = %v, want %v", c.uid, got, c.want)
		}
	}
}
