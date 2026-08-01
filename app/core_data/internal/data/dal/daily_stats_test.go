package dal

import (
	"context"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testDALDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.SubmitLog{}, &model.UserACProblem{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestAggregateSubmitDeltas(t *testing.T) {
	day := time.Date(2026, 7, 16, 10, 0, 0, 0, time.Local)
	logs := []model.SubmitLog{
		{UserID: 1, Time: day, Platform: "CF", SubmitID: "a", IsAC: true},
		{UserID: 1, Time: day, Platform: "CF", SubmitID: "b", IsAC: false},
		{UserID: 1, Time: day, Platform: "LeetCode", SubmitID: "lc-ac-1", IsAC: true},
		{UserID: 1, Time: day, Platform: "LeetCode", SubmitID: "lc-prob-99", IsAC: true},
		{UserID: 1, Time: day, Platform: "LeetCode", SubmitID: "lc-cal-1-20260716-0", IsAC: false},
	}
	d := AggregateSubmitDeltas(logs)
	if len(d) != 2 {
		t.Fatalf("want 2 deltas (CF+LeetCode) got %d", len(d))
	}
	byPlat := map[string]DailyDelta{}
	for _, x := range d {
		byPlat[x.Platform] = x
	}
	cf, ok := byPlat["CF"]
	if !ok {
		t.Fatal("missing CF delta")
	}
	if cf.SubmitCnt != 2 || cf.AcCnt != 1 {
		t.Fatalf("CF submit=%d ac=%d want 2/1", cf.SubmitCnt, cf.AcCnt)
	}
	lc, ok := byPlat["LeetCode"]
	if !ok {
		t.Fatal("missing LeetCode delta")
	}
	if lc.SubmitCnt != 1 { // 仅 lc-cal；排除 lc-ac / lc-prob
		t.Fatalf("LC submit_cnt=%d want 1", lc.SubmitCnt)
	}
	// 日 AC 只计 lc-prob；lc-ac 仅服务生涯 total，不进日统计
	if lc.AcCnt != 1 {
		t.Fatalf("LC ac_cnt=%d want 1 (lc-prob only, not lc-ac)", lc.AcCnt)
	}
}

func TestDedupeSubmitLogsBySubmitID(t *testing.T) {
	logs := []model.SubmitLog{
		{SubmitID: "a", Problem: "1"},
		{SubmitID: "a", Problem: "2"},
		{SubmitID: "b", Problem: "3"},
		{SubmitID: "", Problem: "skip"},
	}
	out := dedupeSubmitLogsBySubmitID(logs)
	if len(out) != 2 {
		t.Fatalf("len=%d want 2", len(out))
	}
	if out[0].Problem != "1" || out[1].SubmitID != "b" {
		t.Fatalf("unexpected %+v", out)
	}
}

// 隔日重刷 329 应放行；同日二次 / 库内已有同日同 slug 应丢。
func TestFilterLeetCodeProbAlreadyHaveSlug_SameDayOnly(t *testing.T) {
	db := testDALDB(t)
	loc := leetCodeStatsLoc
	july12 := time.Date(2026, 7, 12, 20, 5, 29, 0, loc)
	aug1 := time.Date(2026, 8, 1, 6, 40, 32, 0, loc)
	aug1b := time.Date(2026, 8, 1, 9, 0, 0, 0, loc)

	// 库内已有 7/12 的 329
	if err := db.Create(&model.SubmitLog{
		UserID: 2, Platform: "LeetCode", SubmitID: "lc-prob-old",
		ExternalID: "longest-increasing-path-in-a-matrix",
		Problem: "longest-increasing-path-in-a-matrix 329", Status: "AC", IsAC: true, Time: july12,
	}).Error; err != nil {
		t.Fatal(err)
	}
	// 生涯表也有该题（旧逻辑会终身拦截；现应忽略）
	if err := db.Create(&model.UserACProblem{
		UserID: 2, ProblemKey: "e:LeetCode:longest-increasing-path-in-a-matrix",
		Platform: "LeetCode", FirstACAt: july12,
	}).Error; err != nil {
		t.Fatal(err)
	}

	in := []model.SubmitLog{
		{UserID: 2, Platform: "LeetCode", SubmitID: "lc-prob-739335666",
			ExternalID: "longest-increasing-path-in-a-matrix",
			Problem: "longest-increasing-path-in-a-matrix 329", Status: "AC", IsAC: true, Time: aug1},
		// 同批同日同 slug 第二条
		{UserID: 2, Platform: "LeetCode", SubmitID: "lc-prob-dup",
			ExternalID: "longest-increasing-path-in-a-matrix",
			Problem: "longest-increasing-path-in-a-matrix 329", Status: "AC", IsAC: true, Time: aug1b},
		// 无关平台
		{UserID: 2, Platform: "NowCoder", SubmitID: "nc-1", Problem: "x", Time: aug1},
	}
	out, err := filterLeetCodeProbAlreadyHaveSlug(context.Background(), db, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("len=%d want 2 (aug1 first 329 + NowCoder); got %+v", len(out), out)
	}
	if out[0].SubmitID != "lc-prob-739335666" || out[1].SubmitID != "nc-1" {
		t.Fatalf("unexpected %+v", out)
	}

	// 库内已有 8/1 同 slug 后再爬 → 应丢
	if err := db.Create(&model.SubmitLog{
		UserID: 2, Platform: "LeetCode", SubmitID: "lc-prob-739335666",
		ExternalID: "longest-increasing-path-in-a-matrix",
		Problem: "x", Status: "AC", IsAC: true, Time: aug1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	again := []model.SubmitLog{{
		UserID: 2, Platform: "LeetCode", SubmitID: "lc-prob-newid",
		ExternalID: "longest-increasing-path-in-a-matrix",
		Time: aug1b, Status: "AC", IsAC: true,
	}}
	out2, err := filterLeetCodeProbAlreadyHaveSlug(context.Background(), db, again)
	if err != nil {
		t.Fatal(err)
	}
	if len(out2) != 0 {
		t.Fatalf("same-day re-crawl should drop, got %+v", out2)
	}
}
