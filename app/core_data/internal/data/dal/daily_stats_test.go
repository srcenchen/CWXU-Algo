package dal

import (
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
)

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
