package service

import (
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
)

// noNetworkBackfill 关掉后台补窗（单测不打外网），并清空 memo 防止用例互相污染。
func noNetworkBackfill(t *testing.T) {
	t.Helper()
	prev := ncBackfillEnabled
	ncBackfillEnabled = false
	contestWindowMemoMu.Lock()
	contestWindowMemo = map[string]contestWindowMemoEntry{}
	contestWindowMemoMu.Unlock()
	t.Cleanup(func() {
		ncBackfillEnabled = prev
		contestWindowMemoMu.Lock()
		contestWindowMemo = map[string]contestWindowMemoEntry{}
		contestWindowMemoMu.Unlock()
	})
}

// 缺日历的牛客场次：展示路径必须立刻用 hint 兜底返回，不得同步去抓比赛页。
func TestBatchContestDisplayTimes_NoSyncFetchForMissingCalendar(t *testing.T) {
	noNetworkBackfill(t)
	db := testInferDB(t)
	hint := time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC)
	logs := []model.ContestLog{
		{Platform: spider.NowCoder, ContestId: "nc-no-cal", ContestName: "无日历场", Time: hint},
	}

	done := make(chan map[string][2]int64, 1)
	go func() { done <- BatchContestDisplayTimes(db, logs) }()

	var out map[string][2]int64
	select {
	case out = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("展示时间解析阻塞：读路径不应有外网 I/O")
	}

	win, ok := out[spider.NowCoder+"\x00"+"nc-no-cal"]
	if !ok {
		t.Fatal("缺日历时应有兜底窗")
	}
	if win[0] != hint.Unix() {
		t.Fatalf("start=%d want %d", win[0], hint.Unix())
	}
	if got := time.Duration(win[1]-win[0]) * time.Second; got != 3*time.Hour {
		t.Fatalf("兜底赛长 %v，want 3h", got)
	}
}

// 有日历时批量解析用真实赛长（4h，不被默认 3h 截断）。
func TestBatchContestDisplayTimes_UsesCalendar(t *testing.T) {
	noNetworkBackfill(t)
	db := testInferDB(t)
	startSec := int64(1784523600)
	endSec := startSec + 4*3600
	if err := UpsertNowCoderContestCalendar(db, "nc-4h", "四小时赛", "", startSec, endSec); err != nil {
		t.Fatal(err)
	}
	out := BatchContestDisplayTimes(db, []model.ContestLog{
		{Platform: spider.NowCoder, ContestId: "nc-4h", Time: time.Unix(startSec, 0)},
	})
	win := out[spider.NowCoder+"\x00"+"nc-4h"]
	if win[0] != startSec || win[1] != endSec {
		t.Fatalf("window %d–%d want %d–%d", win[0], win[1], startSec, endSec)
	}
}

// memo 命中后仍能被日历写入作废：补窗成功的下一次请求要拿到真实赛长。
func TestResolveContestDisplayWindowCached_InvalidatedByCalendarUpsert(t *testing.T) {
	noNetworkBackfill(t)
	db := testInferDB(t)
	hint := time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC)

	s, e, ok := ResolveContestDisplayWindowCached(db, spider.NowCoder, "nc-memo", hint)
	if !ok || e.Sub(s) != 3*time.Hour {
		t.Fatalf("兜底窗 %v–%v ok=%v", s, e, ok)
	}
	// memo 命中（同样是兜底值）
	if s2, e2, _ := ResolveContestDisplayWindowCached(db, spider.NowCoder, "nc-memo", hint); !s2.Equal(s) || !e2.Equal(e) {
		t.Fatalf("memo 命中应返回同一窗，got %v–%v", s2, e2)
	}

	realStart := hint.Unix()
	realEnd := realStart + 5*3600
	if err := UpsertNowCoderContestCalendar(db, "nc-memo", "五小时赛", "", realStart, realEnd); err != nil {
		t.Fatal(err)
	}
	s3, e3, ok3 := ResolveContestDisplayWindowCached(db, spider.NowCoder, "nc-memo", hint)
	if !ok3 || s3.Unix() != realStart || e3.Unix() != realEnd {
		t.Fatalf("日历写入后应作废 memo，got %v–%v ok=%v", s3, e3, ok3)
	}
}

// 同一场在节流窗口内只排队一次，避免每个请求都打牛客。
func TestNcBackfillClaim_Throttles(t *testing.T) {
	ncBackfillMu.Lock()
	ncBackfillAttempt = map[string]time.Time{}
	ncBackfillInflight = map[string]struct{}{}
	ncBackfillMu.Unlock()

	if !ncBackfillClaim("nc-throttle") {
		t.Fatal("首次应取得资格")
	}
	if ncBackfillClaim("nc-throttle") {
		t.Fatal("进行中不应重复排队")
	}
	ncBackfillRelease("nc-throttle")
	if ncBackfillClaim("nc-throttle") {
		t.Fatal("负缓存期内不应重复排队")
	}
}
