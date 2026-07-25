package service

import (
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testInferDB(t *testing.T) *gorm.DB {
	t.Helper()
	// 每测独立内存库，避免 submit_id 唯一约束互相污染
	dsn := "file:contest_infer_" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SubmitLog{},
		&model.ContestLog{},
		&model.ContestProblem{},
		&model.ContestUserProblem{},
		&model.ContestCalendar{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestResolveContestWindow_DefaultDuration(t *testing.T) {
	db := testInferDB(t)
	startHint := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start, end := ResolveContestWindow(db, spider.LeetCode, "weekly-contest-400", startHint)
	if !start.Equal(startHint) {
		t.Fatalf("start=%v want %v", start, startHint)
	}
	// 90min + 15min buffer
	wantEnd := startHint.Add(90*time.Minute + contestInferEndBuffer)
	if !end.Equal(wantEnd) {
		t.Fatalf("end=%v want %v", end, wantEnd)
	}
}

func TestResolveContestWindow_FromCalendar(t *testing.T) {
	db := testInferDB(t)
	_ = db.Create(&model.ContestCalendar{
		Platform:   spider.NowCoder,
		ExternalID: "12345",
		Name:       "test",
		URL:        "https://ac.nowcoder.com/acm/contest/12345",
		StartTime:  1700000000,
		EndTime:    1700007200,
		Source:     "cpolar",
	}).Error
	start, end := ResolveContestWindow(db, spider.NowCoder, "12345", time.Time{})
	if start.Unix() != 1700000000 {
		t.Fatalf("start unix=%d", start.Unix())
	}
	// end + buffer
	if end.Unix() != 1700007200+int64(contestInferEndBuffer.Seconds()) {
		t.Fatalf("end unix=%d", end.Unix())
	}
}

func TestUpsertNowCoderContestCalendar_FourHour(t *testing.T) {
	db := testInferDB(t)
	// 模拟官方 4h（非固定默认）
	startSec := int64(1784523600) // 2026-07-20 13:00 CST
	endSec := int64(1784538000)   // 2026-07-20 17:00 CST
	if err := UpsertNowCoderContestCalendar(db, "137658", "河南萌新", "", startSec, endSec); err != nil {
		t.Fatal(err)
	}
	s, e, ok := ResolveContestDisplayWindow(db, spider.NowCoder, "137658", time.Time{})
	if !ok {
		t.Fatal("expected window")
	}
	if s.Unix() != startSec || e.Unix() != endSec {
		t.Fatalf("window %v–%v want %d–%d", s, e, startSec, endSec)
	}
	if e.Sub(s) != 4*time.Hour {
		t.Fatalf("dur=%v want 4h", e.Sub(s))
	}
	// Ensure 不应在官方源下再刷新为默认
	s2, e2, ok2 := EnsureNowCoderContestCalendar(db, "137658", "", "", time.Time{}, time.Time{})
	if !ok2 || s2.Unix() != startSec || e2.Unix() != endSec {
		t.Fatalf("ensure got %v–%v ok=%v", s2, e2, ok2)
	}
}

// 长赛（>12h）须被日历/展示接受，禁止再被 calendarWindowPlausible 否决。
func TestResolveContestDisplayWindow_LongContest24h(t *testing.T) {
	db := testInferDB(t)
	startSec := int64(1700000000)
	endSec := startSec + 24*3600
	if err := UpsertNowCoderContestCalendar(db, "nc-24h", "长赛", "", startSec, endSec); err != nil {
		t.Fatal(err)
	}
	s, e, ok := ResolveContestDisplayWindow(db, spider.NowCoder, "nc-24h", time.Time{})
	if !ok {
		t.Fatal("24h calendar must be accepted")
	}
	if e.Sub(s) != 24*time.Hour {
		t.Fatalf("dur=%v want 24h", e.Sub(s))
	}
	start, end := ResolveContestWindow(db, spider.NowCoder, "nc-24h", time.Time{})
	if end.Sub(start) < 24*time.Hour {
		t.Fatalf("infer window too short: %v", end.Sub(start))
	}
}

func TestCalendarWindowValid(t *testing.T) {
	if calendarWindowValid(time.Time{}, time.Now()) {
		t.Fatal("zero start")
	}
	s := time.Unix(100, 0)
	if calendarWindowValid(s, s) {
		t.Fatal("end==start")
	}
	if !calendarWindowValid(s, s.Add(24*time.Hour)) {
		t.Fatal("24h must be valid")
	}
}

func TestEnsureNowCoderContestCalendar_HistoryVariableDuration(t *testing.T) {
	db := testInferDB(t)
	// 模拟爬虫带回的 2h 场（非 3h）
	start := time.Unix(1700000000, 0)
	end := start.Add(2 * time.Hour)
	s, e, ok := EnsureNowCoderContestCalendar(db, "nc-2h", "两小时赛", "", start, end)
	if !ok {
		t.Fatal("expected ok from history end")
	}
	if e.Sub(s) != 2*time.Hour {
		t.Fatalf("dur=%v want 2h", e.Sub(s))
	}
	// 再来一场 5h
	start5 := time.Unix(1700100000, 0)
	end5 := start5.Add(5 * time.Hour)
	s5, e5, ok5 := EnsureNowCoderContestCalendar(db, "nc-5h", "五小时赛", "", start5, end5)
	if !ok5 || e5.Sub(s5) != 5*time.Hour {
		t.Fatalf("5h got %v ok=%v", e5.Sub(s5), ok5)
	}
	// 批量：history 全量写入
	logs := []model.ContestLog{
		{Platform: spider.NowCoder, ContestId: "nc-batch-a", Time: start, EndTime: end, ContestName: "A"},
		{Platform: spider.NowCoder, ContestId: "nc-batch-b", Time: start5, EndTime: end5, ContestName: "B"},
	}
	ensureNowCoderCalendarsFromContestLogs(db, logs, 0)
	for _, id := range []string{"nc-batch-a", "nc-batch-b"} {
		if !nowCoderHasOfficialCalendar(db, id) {
			t.Fatalf("missing official calendar %s", id)
		}
	}
}

func TestInferContestUserProblems_NowCoderByProblemSetAndWindow(t *testing.T) {
	db := testInferDB(t)
	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	// 题目目录
	_ = db.Create(&model.ContestProblem{
		Platform: spider.NowCoder, ContestID: "999", Label: "A", ExternalID: "316899", Title: "A题",
	}).Error
	_ = db.Create(&model.ContestProblem{
		Platform: spider.NowCoder, ContestID: "999", Label: "B", ExternalID: "316900", Title: "B题",
	}).Error
	// 窗内：A WA + AC，B 仅 WA；窗外同题不计入
	logs := []model.SubmitLog{
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s1", Problem: "316899 题A", Status: "答案错误", Time: start.Add(10 * time.Minute)},
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s2", Problem: "316899 题A", Status: "答案正确", Time: start.Add(20 * time.Minute)},
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s3", Problem: "316900 题B", Status: "答案错误", Time: start.Add(30 * time.Minute)},
		// 窗外
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s4", Problem: "316899 题A", Status: "答案正确", Time: start.Add(5 * time.Hour)},
		// 他场题号
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s5", Problem: "111111 其他", Status: "答案正确", Time: start.Add(15 * time.Minute)},
	}
	for i := range logs {
		logs[i].FillIsAC()
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}

	n, err := InferContestUserProblems(db, spider.NowCoder, "999", []int64{1}, start)
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("upsert n=%d want >=2", n)
	}
	var cells []model.ContestUserProblem
	_ = db.Where("platform = ? AND contest_id = ? AND user_id = ?", spider.NowCoder, "999", 1).Find(&cells).Error
	byExt := map[string]model.ContestUserProblem{}
	for _, c := range cells {
		byExt[c.ExternalID] = c
	}
	a := byExt["316899"]
	if a.Status != model.ContestCellAC || a.Attempts != 1 {
		t.Fatalf("A cell=%+v want AC attempts=1", a)
	}
	if a.RelativeSec == nil || *a.RelativeSec != 20*60 {
		t.Fatalf("A relativeSec=%v want 1200", a.RelativeSec)
	}
	b := byExt["316900"]
	if b.Status != model.ContestCellTried || b.Attempts != 1 {
		t.Fatalf("B cell=%+v want TRIED attempts=1", b)
	}
	if _, ok := byExt["111111"]; ok {
		t.Fatal("other problem should not be inferred")
	}
}

func TestInferContestUserProblems_LeetCodeLcProbInWindow(t *testing.T) {
	db := testInferDB(t)
	start := time.Date(2026, 3, 1, 2, 30, 0, 0, time.UTC)
	_ = db.Create(&model.ContestProblem{
		Platform: spider.LeetCode, ContestID: "weekly-contest-400", Label: "A",
		ExternalID: "minimum-number-of-chairs-in-a-waiting-room", Title: "椅子",
	}).Error
	// lc-prob 在窗内；lc-cal 应忽略
	_ = db.Create(&model.SubmitLog{
		Platform: spider.LeetCode, UserID: 7, SubmitID: "lc-prob-100",
		Problem: "minimum-number-of-chairs-in-a-waiting-room 椅子", ExternalID: "minimum-number-of-chairs-in-a-waiting-room",
		Status: "AC", Time: start.Add(12 * time.Minute),
	}).Error
	_ = db.Create(&model.SubmitLog{
		Platform: spider.LeetCode, UserID: 7, SubmitID: "lc-cal-7-20260301-0",
		Problem: "leetcode-submit", Status: "SUBMIT", Time: start.Add(5 * time.Minute),
	}).Error

	n, err := InferContestUserProblems(db, spider.LeetCode, "weekly-contest-400", []int64{7}, start)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n=%d want 1", n)
	}
	var cell model.ContestUserProblem
	if err := db.Where("user_id = 7").First(&cell).Error; err != nil {
		t.Fatal(err)
	}
	if cell.Status != model.ContestCellAC || cell.ExternalID != "minimum-number-of-chairs-in-a-waiting-room" {
		t.Fatalf("cell=%+v", cell)
	}
}

func TestInferContestUserProblems_NoProblemSetRequiresContestField(t *testing.T) {
	db := testInferDB(t)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 无 contest_problems：仅 contest 字段可归属
	_ = db.Create(&model.SubmitLog{
		Platform: spider.CodeForces, UserID: 2, SubmitID: "1",
		Contest: "2000", Problem: "A-Hello", Status: "OK", Time: start.Add(time.Minute),
	}).Error
	_ = db.Create(&model.SubmitLog{
		Platform: spider.CodeForces, UserID: 2, SubmitID: "2",
		Contest: "", Problem: "A-Hello", Status: "OK", Time: start.Add(2 * time.Minute),
	}).Error

	n, err := InferContestUserProblems(db, spider.CodeForces, "2000", []int64{2}, start)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("n=%d want 1 (only contest-tagged)", n)
	}
}

func TestResolveContestDisplayWindow_AtCoderHintIsEnd(t *testing.T) {
	db := testInferDB(t)
	endHint := time.Date(2026, 6, 13, 13, 40, 0, 0, time.UTC) // abc462 EndTime
	start, end, ok := ResolveContestDisplayWindow(db, spider.AtCoder, "abc462", endHint)
	if !ok {
		t.Fatal("expected ok")
	}
	wantStart := endHint.Add(-100 * time.Minute)
	if !start.Equal(wantStart) {
		t.Fatalf("start=%v want %v (hint is end, not start)", start, wantStart)
	}
	if !end.Equal(endHint) {
		t.Fatalf("end=%v want %v", end, endHint)
	}
}

func TestResolveCellSubmitWindow_IgnoresSinglePoisonRelativeSec(t *testing.T) {
	db := testInferDB(t)
	endHint := time.Date(2026, 6, 13, 13, 40, 0, 0, time.UTC)
	// 赛后练习 AC + 错误 relative_sec（线上 user2 D 题脏数据形态）
	practice := endHint.Add(40 * time.Minute)
	badRel := 3403
	_ = db.Create(&model.ContestUserProblem{
		Platform: spider.AtCoder, ContestID: "abc462", UserID: 2,
		Label: "D", ExternalID: "abc462_d", Status: model.ContestCellAC,
		FirstACAt: &practice, RelativeSec: &badRel,
	}).Error
	// 正常赛时 AC（无 relative）
	okAC := endHint.Add(-98 * time.Minute)
	_ = db.Create(&model.ContestUserProblem{
		Platform: spider.AtCoder, ContestID: "abc462", UserID: 98,
		Label: "A", ExternalID: "abc462_a", Status: model.ContestCellAC,
		FirstACAt: &okAC,
	}).Error

	start, end := resolveCellSubmitWindow(db, spider.AtCoder, "abc462", endHint)
	wantStart := endHint.Add(-100 * time.Minute)
	if start.After(wantStart.Add(time.Minute)) {
		t.Fatalf("start=%v should cover real open %v (poison relative ignored)", start, wantStart)
	}
	if end.Before(endHint) {
		t.Fatalf("end=%v want >= %v", end, endHint)
	}
	// 窗必须包含赛时 AC，排除练习点
	if !okAC.After(start) || !okAC.Before(end) {
		t.Fatalf("okAC %v not in [%v,%v]", okAC, start, end)
	}
	if practice.Before(end) && practice.After(start) {
		// 练习在 end+40min，应在窗外
		t.Fatalf("practice %v should be outside window end %v", practice, end)
	}
}

func TestListContestCellSubmits_AtCoderByProblemWhenExternalEmpty(t *testing.T) {
	db := testInferDB(t)
	endHint := time.Date(2026, 6, 13, 13, 40, 0, 0, time.UTC)
	// 历史行：只有 problem=abc462_a，无 external_id
	logs := []model.SubmitLog{
		{Platform: spider.AtCoder, UserID: 98, SubmitID: "1", Contest: "abc462",
			Problem: "abc462_a", Status: "AC",
			Time: endHint.Add(-98 * time.Minute)},
		{Platform: spider.AtCoder, UserID: 98, SubmitID: "2", Contest: "abc462",
			Problem: "abc462_b", Status: "AC",
			Time: endHint.Add(-90 * time.Minute)},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	list, _, _, err := ListContestCellSubmits(
		db, spider.AtCoder, "abc462", 98, "A", "abc462_a", endHint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].SubmitID != "1" {
		t.Fatalf("list=%+v want one A submit", list)
	}
}

// AtCoder contest_logs.time 是结束时间；按 external_id 反查应落在 end−赛长 窗内。
func TestListContestCellSubmits_AtCoderByExternalIDEndHint(t *testing.T) {
	db := testInferDB(t)
	// ABC 开赛 12:00 UTC，结束 13:40（100min）——与线上 history EndTime 一致
	endHint := time.Date(2026, 7, 18, 13, 40, 0, 0, time.UTC)
	logs := []model.SubmitLog{
		{Platform: spider.AtCoder, UserID: 13, SubmitID: "1", Contest: "abc467",
			Problem: "abc467_a", ExternalID: "abc467_a", Status: "WA",
			Time: time.Date(2026, 7, 18, 12, 3, 52, 0, time.UTC)},
		{Platform: spider.AtCoder, UserID: 13, SubmitID: "2", Contest: "abc467",
			Problem: "abc467_a", ExternalID: "abc467_a", Status: "WA",
			Time: time.Date(2026, 7, 18, 12, 4, 37, 0, time.UTC)},
		{Platform: spider.AtCoder, UserID: 13, SubmitID: "3", Contest: "abc467",
			Problem: "abc467_a", ExternalID: "abc467_a", Status: "AC",
			Time: time.Date(2026, 7, 18, 12, 13, 3, 0, time.UTC)},
		// 赛后补题：应计入并标 phase=upsolve
		{Platform: spider.AtCoder, UserID: 13, SubmitID: "4", Contest: "abc467",
			Problem: "abc467_a", ExternalID: "abc467_a", Status: "AC",
			Time: time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)},
		// 他题
		{Platform: spider.AtCoder, UserID: 13, SubmitID: "5", Contest: "abc467",
			Problem: "abc467_b", ExternalID: "abc467_b", Status: "AC",
			Time: time.Date(2026, 7, 18, 12, 16, 57, 0, time.UTC)},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}

	list, start, end, err := ListContestCellSubmits(
		db, spider.AtCoder, "abc467", 13, "A", "abc467_a", endHint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if start.After(time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("start=%v should cover 12:00", start)
	}
	if end.Before(endHint) {
		t.Fatalf("end=%v want >= %v", end, endHint)
	}
	// 3 赛时 + 1 赛后；排除 B 题
	if len(list) != 4 {
		t.Fatalf("list=%d want 4 (3 contest + 1 upsolve), got %+v", len(list), list)
	}
	// 逆序：最新（补题）在前
	if list[0].SubmitID != "4" || list[0].Phase != CellSubmitPhaseUpsolve {
		t.Fatalf("newest should be upsolve submit4, got %+v", list[0])
	}
	if list[0].RelativeSec != nil {
		t.Fatalf("upsolve should not have relativeSec, got %v", *list[0].RelativeSec)
	}
	// 赛时 AC 次新
	if list[1].Status != "AC" || list[1].SubmitID != "3" || list[1].Phase != CellSubmitPhaseContest {
		t.Fatalf("newest in-contest=%+v", list[1])
	}
	if list[1].RelativeSec == nil {
		t.Fatal("contest submit should have relativeSec")
	}
	if list[3].SubmitID != "1" || list[3].Phase != CellSubmitPhaseContest {
		t.Fatalf("oldest in-contest=%+v", list[3])
	}
}

// 官方结束 22:00、提交 22:01:35 必须是赛后（不能被 15min 缓冲误判为赛时）。
// AtCoder hint=结束时间；不用日历，避免 Unix→本地时区在 SQLite 比较失真。
func TestListContestCellSubmits_PhaseUsesOfficialEndNotBuffer(t *testing.T) {
	db := testInferDB(t)
	endHint := time.Date(2026, 7, 20, 22, 0, 0, 0, time.UTC) // 官方结束
	logs := []model.SubmitLog{
		// 赛时内
		{Platform: spider.AtCoder, UserID: 7, SubmitID: "in", Contest: "abc-end",
			Problem: "abc-end_a", ExternalID: "abc-end_a", Status: "WA",
			Time: time.Date(2026, 7, 20, 21, 59, 50, 0, time.UTC)},
		// 结束 1 分 35 秒后——用户吐槽场景（落在旧 15min 缓冲内）
		{Platform: spider.AtCoder, UserID: 7, SubmitID: "late", Contest: "abc-end",
			Problem: "abc-end_a", ExternalID: "abc-end_a", Status: "AC",
			Time: time.Date(2026, 7, 20, 22, 1, 35, 0, time.UTC)},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	list, _, end, err := ListContestCellSubmits(
		db, spider.AtCoder, "abc-end", 7, "A", "abc-end_a", endHint,
	)
	if err != nil {
		t.Fatal(err)
	}
	// 返回的 end 应为官方 22:00，而非 +15min
	if !end.Equal(endHint) {
		t.Fatalf("end=%v want official %v (no 15m buffer)", end, endHint)
	}
	if len(list) != 2 {
		t.Fatalf("list=%d want 2, %+v", len(list), list)
	}
	// 逆序：late 在前且必须 upsolve
	if list[0].SubmitID != "late" || list[0].Phase != CellSubmitPhaseUpsolve {
		t.Fatalf("22:01:35 must be upsolve, got %+v", list[0])
	}
	if list[1].SubmitID != "in" || list[1].Phase != CellSubmitPhaseContest {
		t.Fatalf("21:59:50 must be contest, got %+v", list[1])
	}
}

// 赛时 WA + 赛后 AC：两条均返回且 phase 正确。
func TestListContestCellSubmits_ContestWAThenUpsolveAC(t *testing.T) {
	db := testInferDB(t)
	endHint := time.Date(2026, 7, 18, 13, 40, 0, 0, time.UTC)
	logs := []model.SubmitLog{
		{Platform: spider.AtCoder, UserID: 42, SubmitID: "wa1", Contest: "abc467",
			Problem: "abc467_c", ExternalID: "abc467_c", Status: "WA",
			Time: time.Date(2026, 7, 18, 12, 30, 0, 0, time.UTC)},
		{Platform: spider.AtCoder, UserID: 42, SubmitID: "ac2", Contest: "abc467",
			Problem: "abc467_c", ExternalID: "abc467_c", Status: "AC",
			Time: time.Date(2026, 7, 18, 20, 0, 0, 0, time.UTC)},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	list, _, _, err := ListContestCellSubmits(
		db, spider.AtCoder, "abc467", 42, "C", "abc467_c", endHint,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list=%d want 2, %+v", len(list), list)
	}
	if list[0].Phase != CellSubmitPhaseUpsolve || list[0].SubmitID != "ac2" {
		t.Fatalf("upsolve first: %+v", list[0])
	}
	if list[1].Phase != CellSubmitPhaseContest || list[1].SubmitID != "wa1" {
		t.Fatalf("contest second: %+v", list[1])
	}
}

// CF：external_id=2230A + 结算时间 hint，应扫到赛时 WA/OK。
func TestListContestCellSubmits_CodeforcesByExternalID(t *testing.T) {
	db := testInferDB(t)
	ratingUpdate := time.Date(2026, 5, 18, 16, 35, 0, 0, time.UTC)
	logs := []model.SubmitLog{
		{Platform: spider.CodeForces, UserID: 13, SubmitID: "a1", Contest: "2230",
			Problem: "A-Optimal Purchase", ExternalID: "2230A", Status: "WA",
			Time: time.Date(2026, 5, 18, 14, 43, 47, 0, time.UTC)},
		{Platform: spider.CodeForces, UserID: 13, SubmitID: "a2", Contest: "2230",
			Problem: "A-Optimal Purchase", ExternalID: "2230A", Status: "OK",
			Time: time.Date(2026, 5, 18, 14, 47, 33, 0, time.UTC)},
		{Platform: spider.CodeForces, UserID: 13, SubmitID: "b1", Contest: "2230",
			Problem: "B-Digit String", ExternalID: "2230B", Status: "OK",
			Time: time.Date(2026, 5, 18, 16, 18, 43, 0, time.UTC)},
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	// 用格子 relative 反推开赛更准
	rel := 753
	first := time.Date(2026, 5, 18, 14, 47, 33, 0, time.UTC)
	_ = db.Create(&model.ContestUserProblem{
		Platform: spider.CodeForces, ContestID: "2230", UserID: 13,
		Label: "A", ExternalID: "2230A", Status: model.ContestCellAC,
		Attempts: 1, FirstACAt: &first, RelativeSec: &rel,
	}).Error

	list, _, _, err := ListContestCellSubmits(
		db, spider.CodeForces, "2230", 13, "A", "2230A", ratingUpdate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list=%d want 2, %+v", len(list), list)
	}
}

func TestInferContestUpsolves_PostContestAC(t *testing.T) {
	db := testInferDB(t)
	// 与现有 Infer 单测一致：仅用 hint 开赛，避免日历 Unix→本地时区在 SQLite 下比较失真
	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	_ = db.Create(&model.ContestProblem{
		Platform: spider.NowCoder, ContestID: "999", Label: "A", ExternalID: "316899", Title: "A",
	}).Error
	_ = db.Create(&model.ContestProblem{
		Platform: spider.NowCoder, ContestID: "999", Label: "B", ExternalID: "316900", Title: "B",
	}).Error
	// 赛时：A 已 AC；B 仅 WA
	// 赛后：B AC；A 再交不影响
	logs := []model.SubmitLog{
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s1", Problem: "316899 题A", Status: "答案正确", Time: start.Add(20 * time.Minute)},
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s2", Problem: "316900 题B", Status: "答案错误", Time: start.Add(30 * time.Minute)},
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s3", Problem: "316900 题B", Status: "答案正确", Time: start.Add(5 * time.Hour)},
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "s4", Problem: "316899 题A", Status: "答案正确", Time: start.Add(6 * time.Hour)},
	}
	for i := range logs {
		logs[i].FillIsAC()
	}
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := InferContestUserProblems(db, spider.NowCoder, "999", []int64{1}, start); err != nil {
		t.Fatal(err)
	}
	n, err := InferContestUpsolves(db, spider.NowCoder, "999", []int64{1}, start)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("upsolve n=%d want >=1", n)
	}
	var cells []model.ContestUserProblem
	_ = db.Where("user_id = 1").Find(&cells).Error
	byExt := map[string]model.ContestUserProblem{}
	for _, c := range cells {
		byExt[c.ExternalID] = c
	}
	if byExt["316899"].Status != model.ContestCellAC {
		t.Fatalf("A should stay AC, got %+v", byExt["316899"])
	}
	b := byExt["316900"]
	if b.Status != model.ContestCellUpsolve {
		t.Fatalf("B should be UPSOLVE, got %+v", b)
	}
	if b.RelativeSec != nil {
		t.Fatalf("upsolve relativeSec should be nil, got %v", *b.RelativeSec)
	}
	if b.Attempts != 1 {
		t.Fatalf("B attempts should keep contest TRIED attempts=1, got %d", b.Attempts)
	}
}

func TestListContestPracticeCells_DerivesPassedAndTriedFromSubmitLogs(t *testing.T) {
	db := testInferDB(t)
	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	for _, p := range []model.ContestProblem{
		{Platform: spider.NowCoder, ContestID: "999", Label: "A", ExternalID: "316899", Title: "A"},
		{Platform: spider.NowCoder, ContestID: "999", Label: "B", ExternalID: "316900", Title: "B"},
	} {
		_ = db.Create(&p).Error
	}
	logs := []model.SubmitLog{
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "p0", Problem: "316900 B", Status: "答案正确", Time: start.Add(time.Hour)},
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "p1", Problem: "316899 A", Status: "答案错误", Time: start.Add(5 * time.Hour)},
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "p2", Problem: "316899 A", Status: "答案正确", Time: start.Add(6 * time.Hour)},
		{Platform: spider.NowCoder, UserID: 1, SubmitID: "p2b", Problem: "316900 B", Status: "答案错误", Time: start.Add(6 * time.Hour)},
		{Platform: spider.NowCoder, UserID: 2, SubmitID: "p3", Problem: "316900 B", Status: "答案错误", Time: start.Add(5 * time.Hour)},
	}
	model.FillIsACBatch(logs)
	if err := db.Create(&logs).Error; err != nil {
		t.Fatal(err)
	}
	cells, err := ListContestPracticeCells(db, spider.NowCoder, "999", nil, start)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]model.ContestUserProblem{}
	for _, cell := range cells {
		if cell.ExternalID == "316900" && cell.UserID == 1 {
			t.Fatalf("contest-time AC must not become practice: %+v", cell)
		}
		got[cell.UserID] = cell
	}
	if got[1].Status != model.ContestCellUpsolve || got[1].Attempts != 1 {
		t.Fatalf("user1=%+v want UPSOLVE attempts=1", got[1])
	}
	if got[2].Status != model.ContestCellUpsolveTried || got[2].Attempts != 1 {
		t.Fatalf("user2=%+v want UPSOLVE_TRIED attempts=1", got[2])
	}
}

func TestMergeContestCell_DoesNotDowngradeUpsolveToTried(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 18, 0, 0, 0, time.UTC)
	prev := model.ContestUserProblem{
		Platform: spider.NowCoder, ContestID: "1", UserID: 9,
		Label: "C", ExternalID: "extC", Status: model.ContestCellUpsolve,
		Attempts: 2, FirstACAt: &t0,
	}
	next := model.ContestUserProblem{
		Platform: spider.NowCoder, ContestID: "1", UserID: 9,
		Label: "C", ExternalID: "extC", Status: model.ContestCellTried, Attempts: 1,
	}
	out, write := mergeContestCellIncoming(&prev, next)
	if !write {
		t.Fatal("expected write=true when merging attempts into UPSOLVE")
	}
	if out.Status != model.ContestCellUpsolve {
		t.Fatalf("status=%s want UPSOLVE", out.Status)
	}
	if out.Attempts != 2 {
		t.Fatalf("attempts=%d want 2 (keep higher)", out.Attempts)
	}
	// 新 TRIED attempts 更高时应抬升
	next.Attempts = 5
	out2, _ := mergeContestCellIncoming(&prev, next)
	if out2.Attempts != 5 {
		t.Fatalf("attempts=%d want 5", out2.Attempts)
	}
}

// 脏 AC（赛后误标）可被 UPSOLVE 纠正；真·赛时 AC（更早 first_ac / 有 relative）保持。
func TestMergeContestCell_UpsolveOverridesDirtyAC(t *testing.T) {
	tPost := time.Date(2026, 7, 21, 2, 3, 31, 0, time.UTC)
	tContest := time.Date(2026, 7, 19, 20, 30, 0, 0, time.UTC)
	dirty := model.ContestUserProblem{
		Platform: spider.AtCoder, ContestID: "arc225", UserID: 38,
		Label: "E", ExternalID: "arc225_e", Status: model.ContestCellAC,
		Attempts: 1, FirstACAt: &tPost, RelativeSec: nil,
	}
	up := model.ContestUserProblem{
		Platform: spider.AtCoder, ContestID: "arc225", UserID: 38,
		Label: "E", ExternalID: "arc225_e", Status: model.ContestCellUpsolve,
		Attempts: 1, FirstACAt: &tPost,
	}
	out, write := mergeContestCellIncoming(&dirty, up)
	if !write {
		t.Fatal("expected write when UPSOLVE corrects dirty AC")
	}
	if out.Status != model.ContestCellUpsolve {
		t.Fatalf("status=%s want UPSOLVE", out.Status)
	}
	if out.RelativeSec != nil {
		t.Fatalf("upsolve relativeSec must be nil, got %v", *out.RelativeSec)
	}

	// 真赛时 AC + 赛后再交：first_ac 更早 → 保持 AC
	rel := 1800
	real := model.ContestUserProblem{
		Platform: spider.AtCoder, ContestID: "arc225", UserID: 1,
		Label: "A", ExternalID: "arc225_a", Status: model.ContestCellAC,
		Attempts: 0, FirstACAt: &tContest, RelativeSec: &rel,
	}
	upA := model.ContestUserProblem{
		Platform: spider.AtCoder, ContestID: "arc225", UserID: 1,
		Label: "A", ExternalID: "arc225_a", Status: model.ContestCellUpsolve,
		FirstACAt: &tPost,
	}
	outKeep, writeKeep := mergeContestCellIncoming(&real, upA)
	if writeKeep {
		t.Fatal("real contest AC must not be overwritten by later upsolve")
	}
	if outKeep.Status != model.ContestCellAC {
		t.Fatalf("status=%s want AC", outKeep.Status)
	}

	// 无 relative 但 first_ac 更早（赛时）→ 仍保持
	realNoRel := real
	realNoRel.RelativeSec = nil
	outKeep2, writeKeep2 := mergeContestCellIncoming(&realNoRel, upA)
	if writeKeep2 {
		t.Fatal("earlier first_ac AC must stay")
	}
	if outKeep2.Status != model.ContestCellAC {
		t.Fatalf("status=%s want AC", outKeep2.Status)
	}
}
