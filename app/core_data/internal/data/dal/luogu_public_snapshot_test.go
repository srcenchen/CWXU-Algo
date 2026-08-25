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

func luoguSnapshotDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.LuoGuPublicSnapshot{}, &model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestApplyLuoGuPublicPeriodOverrideDoesNotDoubleCountRecoveredTodayRows(t *testing.T) {
	db := luoguSnapshotDB(t)
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	snapshot := model.LuoGuPublicSnapshot{
		UserID: 1, Platform: "LuoGu", RemoteUID: 11, TotalSolved: 30, TotalSubmit: 80,
		TodaySolved: 5, TodaySubmit: 8, RealTodaySolvedBaseline: 2, RealTodayACBaseline: 3,
		RealTodaySubmitBaseline: 4, Active: true, RecoveryRequired: true, ObservedAt: now,
	}
	if err := db.Create(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.DailyUserStat{UserID: 1, Platform: "LuoGu", Day: today, SubmitCnt: 7, AcCnt: 5}).Error; err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a", "b", "c", "d"} {
		if err := db.Create(&model.UserACProblemDay{UserID: 1, Platform: "LuoGu", Day: today, ProblemKey: key}).Error; err != nil {
			t.Fatal(err)
		}
	}

	personalSubmit, personalAC, err := ApplyLuoGuPublicPeriodOverride(db, 1, nil,
		PeriodSubmitCount{Today: 7}, PeriodAcCount{Today: 4})
	if err != nil {
		t.Fatal(err)
	}
	if personalSubmit.Today != 12 || personalAC.Today != 7 {
		t.Fatalf("personal submit=%+v ac=%+v", personalSubmit, personalAC)
	}

	orgSubmit, orgAC, err := ApplyLuoGuPublicPeriodOverride(db, -1, []int64{1},
		PeriodSubmitCount{Today: 7}, PeriodAcCount{Today: 5})
	if err != nil {
		t.Fatal(err)
	}
	if orgSubmit.Today != 12 || orgAC.Today != 8 {
		t.Fatalf("org submit=%+v ac=%+v", orgSubmit, orgAC)
	}
}

func TestApplyLuoGuPublicPeriodOverrideIgnoresYesterdayTodayContribution(t *testing.T) {
	db := luoguSnapshotDB(t)
	yesterday := time.Now().AddDate(0, 0, -1)
	if err := db.Create(&model.LuoGuPublicSnapshot{
		UserID: 1, Platform: "LuoGu", RemoteUID: 11, TotalSolved: 30, TotalSubmit: 80,
		TodaySolved: 5, TodaySubmit: 8, Active: true, RecoveryRequired: true, ObservedAt: yesterday,
	}).Error; err != nil {
		t.Fatal(err)
	}
	submit, ac, err := ApplyLuoGuPublicPeriodOverride(db, 1, nil, PeriodSubmitCount{Today: 2}, PeriodAcCount{Today: 1})
	if err != nil {
		t.Fatal(err)
	}
	if submit.Today != 2 || ac.Today != 1 {
		t.Fatalf("submit=%+v ac=%+v", submit, ac)
	}
}

func TestUpsertLuoGuPublicSnapshotFirstSameDayCrossDayAndDecrease(t *testing.T) {
	db := luoguSnapshotDB(t)
	ctx := context.Background()
	day1 := time.Date(2026, 8, 24, 10, 0, 0, 0, time.Local)

	changed, err := UpsertLuoGuPublicSnapshot(ctx, db, 7, 100544, 120, 400, 100, 350, 0, 0, 0, day1)
	if err != nil || !changed {
		t.Fatalf("first changed=%v err=%v", changed, err)
	}
	assertLuoGuSnapshot(t, db, 7, 120, 400, 20, 50)

	changed, err = UpsertLuoGuPublicSnapshot(ctx, db, 7, 100544, 125, 410, 100, 350, 0, 0, 0, day1.Add(time.Hour))
	if err != nil || !changed {
		t.Fatalf("same-day changed=%v err=%v", changed, err)
	}
	assertLuoGuSnapshot(t, db, 7, 125, 410, 25, 60)

	changed, err = UpsertLuoGuPublicSnapshot(ctx, db, 7, 100544, 125, 410, 100, 350, 0, 0, 0, day1.Add(2*time.Hour))
	if err != nil || changed {
		t.Fatalf("idempotent changed=%v err=%v", changed, err)
	}

	day2 := day1.AddDate(0, 0, 1)
	changed, err = UpsertLuoGuPublicSnapshot(ctx, db, 7, 100544, 128, 416, 100, 350, 0, 0, 0, day2)
	if err != nil || !changed {
		t.Fatalf("cross-day changed=%v err=%v", changed, err)
	}
	assertLuoGuSnapshot(t, db, 7, 128, 416, 3, 6)

	changed, err = UpsertLuoGuPublicSnapshot(ctx, db, 7, 100544, 110, 390, 100, 350, 0, 0, 0, day2.Add(time.Hour))
	if err != nil || changed {
		t.Fatalf("decrease changed=%v err=%v", changed, err)
	}
	assertLuoGuSnapshot(t, db, 7, 128, 416, 3, 6)
}

func assertLuoGuSnapshot(t *testing.T, db *gorm.DB, userID, solved, submit, todaySolved, todaySubmit int64) {
	t.Helper()
	var got model.LuoGuPublicSnapshot
	if err := db.First(&got, "user_id = ?", userID).Error; err != nil {
		t.Fatal(err)
	}
	if !got.Active || !got.RecoveryRequired || got.TotalSolved != solved || got.TotalSubmit != submit || got.TodaySolved != todaySolved || got.TodaySubmit != todaySubmit {
		t.Fatalf("snapshot=%+v", got)
	}
}

func TestApplyLuoGuPublicPeriodOverridePersonalAndScoped(t *testing.T) {
	db := luoguSnapshotDB(t)
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rows := []model.LuoGuPublicSnapshot{
		{UserID: 1, Platform: "LuoGu", RemoteUID: 11, TotalSolved: 30, TotalSubmit: 80, TodaySolved: 4, TodaySubmit: 9, RealTodaySolvedBaseline: 2, RealTodayACBaseline: 2, RealTodaySubmitBaseline: 3, Active: true, RecoveryRequired: true, ObservedAt: now},
		{UserID: 2, Platform: "LuoGu", RemoteUID: 22, TotalSolved: 20, TotalSubmit: 50, TodaySolved: 2, TodaySubmit: 5, RealTodayACBaseline: 3, RealTodaySubmitBaseline: 4, Active: true, RecoveryRequired: true, ObservedAt: now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.DailyUserStat{
		{UserID: 1, Platform: "LuoGu", Day: today, SubmitCnt: 3, AcCnt: 2},
		{UserID: 2, Platform: "LuoGu", Day: today, SubmitCnt: 4, AcCnt: 3},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.UserACProblem{
		{UserID: 1, Platform: "LuoGu", ProblemKey: "a", FirstACAt: now},
		{UserID: 1, Platform: "LuoGu", ProblemKey: "b", FirstACAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}

	submit, ac, err := ApplyLuoGuPublicPeriodOverride(db, 1, nil, PeriodSubmitCount{Total: 10, Today: 3}, PeriodAcCount{Total: 7, Today: 2, TotalRaw: 8})
	if err != nil {
		t.Fatal(err)
	}
	if submit.Total != 87 || submit.Today != 12 || ac.Total != 35 || ac.Today != 6 || ac.TotalRaw != 35 {
		t.Fatalf("personal submit=%+v ac=%+v", submit, ac)
	}

	submit, ac, err = ApplyLuoGuPublicPeriodOverride(db, -1, []int64{1, 2}, PeriodSubmitCount{Total: 10, Today: 7}, PeriodAcCount{Total: 7, Today: 5, TotalRaw: 100})
	if err != nil {
		t.Fatal(err)
	}
	if submit.Total != 133 || submit.Today != 21 || ac.Total != 52 || ac.Today != 11 || ac.TotalRaw != 100 {
		t.Fatalf("scoped submit=%+v ac=%+v", submit, ac)
	}
}

func TestApplyLuoGuPublicPlatformACOverride(t *testing.T) {
	db := luoguSnapshotDB(t)
	if err := db.Create(&model.LuoGuPublicSnapshot{UserID: 1, Platform: "LuoGu", RemoteUID: 11, TotalSolved: 42, Active: true, RecoveryRequired: true, ObservedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	got, err := ApplyLuoGuPublicPlatformACOverride(db, 1, []PlatformACCount{{Name: "LuoGu", Count: 3}, {Name: "AtCoder", Count: 9}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "LuoGu" || got[0].Count != 42 {
		t.Fatalf("platforms=%+v", got)
	}
}

func TestCloseLuoGuPublicSnapshotOnlyAfterCompleteSuccess(t *testing.T) {
	db := luoguSnapshotDB(t)
	s := model.LuoGuPublicSnapshot{UserID: 1, Platform: "LuoGu", RemoteUID: 11, Active: true, RecoveryRequired: true, ObservedAt: time.Now()}
	if err := db.Create(&s).Error; err != nil {
		t.Fatal(err)
	}
	if changed, err := CloseLuoGuPublicSnapshot(context.Background(), db, 1, 11, s.ObservedAt, false); err != nil || changed {
		t.Fatalf("incomplete changed=%v err=%v", changed, err)
	}
	assertSnapshotActive(t, db, 1, true)
	if changed, err := CloseLuoGuPublicSnapshot(context.Background(), db, 1, 11, s.ObservedAt, true); err != nil || !changed {
		t.Fatalf("complete changed=%v err=%v", changed, err)
	}
	assertSnapshotActive(t, db, 1, false)
}

func TestCloseLuoGuPublicSnapshotUIDOrVersionMismatchKeepsActive(t *testing.T) {
	tests := []struct {
		name       string
		remoteUID  int64
		observedAt time.Time
	}{
		{name: "remote uid mismatch", remoteUID: 12},
		{name: "snapshot version mismatch", remoteUID: 11, observedAt: time.Now().Add(time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := luoguSnapshotDB(t)
			observedAt := time.Now().Truncate(time.Millisecond)
			if tt.observedAt.IsZero() {
				tt.observedAt = observedAt
			}
			s := model.LuoGuPublicSnapshot{UserID: 1, Platform: "LuoGu", RemoteUID: 11, Active: true, RecoveryRequired: true, ObservedAt: observedAt}
			if err := db.Create(&s).Error; err != nil {
				t.Fatal(err)
			}
			changed, err := CloseLuoGuPublicSnapshot(context.Background(), db, 1, tt.remoteUID, tt.observedAt, true)
			if err != nil || changed {
				t.Fatalf("changed=%v err=%v", changed, err)
			}
			assertSnapshotActive(t, db, 1, true)
		})
	}
}

func assertSnapshotActive(t *testing.T, db *gorm.DB, userID int64, want bool) {
	t.Helper()
	var got model.LuoGuPublicSnapshot
	if err := db.First(&got, "user_id = ?", userID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Active != want || got.RecoveryRequired != want {
		t.Fatalf("active=%v recovery=%v want=%v", got.Active, got.RecoveryRequired, want)
	}
}
