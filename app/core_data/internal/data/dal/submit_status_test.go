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

func TestShouldRewriteFinalStatus(t *testing.T) {
	if !shouldRewriteFinalStatus("WRONG_ANSWER", "WA") {
		t.Fatal("long→short should rewrite")
	}
	if shouldRewriteFinalStatus("WA", "OK") {
		t.Fatal("must not rewrite final→other final")
	}
	if shouldRewriteFinalStatus("OK", "OK") {
		t.Fatal("same status no rewrite")
	}
}

func TestShouldRewriteFetchedStatus(t *testing.T) {
	if !shouldRewriteFetchedStatus("QOJ", "WA", "AC") {
		t.Fatal("QOJ full-score correction should rewrite WA to AC")
	}
	if shouldRewriteFetchedStatus("LuoGu", "WA", "AC") {
		t.Fatal("other platforms must not rewrite final WA to AC")
	}
	if shouldRewriteFetchedStatus("QOJ", "AC", "WA") {
		t.Fatal("QOJ AC must not be downgraded")
	}
}

func TestRefreshQOJFullScoreCorrectionIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SubmitLog{}, &model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	old := model.SubmitLog{UserID: 7, Platform: "QOJ", SubmitID: "42", Problem: "#1. Test", Status: "WA", Time: at}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	fetched := old
	fetched.Status = "AC"
	fetched.IsAC = true
	want := map[string]model.SubmitLog{"QOJ\x0042": fetched}

	for i := 0; i < 2; i++ {
		var existing model.SubmitLog
		if err := db.First(&existing, "platform = ? AND submit_id = ?", "QOJ", "42").Error; err != nil {
			t.Fatal(err)
		}
		updated, err := refreshExistingVerdicts(context.Background(), db, []model.SubmitLog{existing}, want)
		if err != nil {
			t.Fatal(err)
		}
		if updated != int64(1-i) {
			t.Fatalf("round %d updated=%d", i+1, updated)
		}
	}

	var got model.SubmitLog
	if err := db.First(&got, "platform = ? AND submit_id = ?", "QOJ", "42").Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "AC" || !got.IsAC {
		t.Fatalf("submit=%+v", got)
	}
	var daily model.DailyUserStat
	if err := db.First(&daily, "user_id = ? AND platform = ?", 7, "QOJ").Error; err != nil {
		t.Fatal(err)
	}
	if daily.SubmitCnt != 0 || daily.AcCnt != 1 {
		t.Fatalf("daily submit=%d ac=%d", daily.SubmitCnt, daily.AcCnt)
	}
	var lifetime, days int64
	if err := db.Model(&model.UserACProblem{}).Where("user_id = ?", 7).Count(&lifetime).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.UserACProblemDay{}).Where("user_id = ?", 7).Count(&days).Error; err != nil {
		t.Fatal(err)
	}
	if lifetime != 1 || days != 1 {
		t.Fatalf("lifetime=%d days=%d", lifetime, days)
	}
}

func TestRefreshQOJFullScoreCorrectionRollsBackOnAggregateFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SubmitLog{}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	old := model.SubmitLog{UserID: 7, Platform: "QOJ", SubmitID: "43", Problem: "#2. Test", Status: "WA", Time: at}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	fetched := old
	fetched.Status = "AC"
	fetched.IsAC = true
	want := map[string]model.SubmitLog{"QOJ\x0043": fetched}

	if _, err := refreshExistingVerdicts(context.Background(), db, []model.SubmitLog{old}, want); err == nil {
		t.Fatal("missing aggregate tables must fail the correction")
	}
	var got model.SubmitLog
	if err := db.First(&got, "platform = ? AND submit_id = ?", "QOJ", "43").Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "WA" || got.IsAC {
		t.Fatalf("correction was not rolled back: %+v", got)
	}
}

func TestRefreshQOJFullScoreCorrectionKeepsDistinctProblems(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SubmitLog{}, &model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	old := []model.SubmitLog{
		{UserID: 7, Platform: "QOJ", SubmitID: "51", Problem: "#1. First", ExternalID: "1", Status: "WA", Time: at},
		{UserID: 7, Platform: "QOJ", SubmitID: "52", Problem: "#2. Second", ExternalID: "2", Status: "WA", Time: at},
	}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	fetched := append([]model.SubmitLog(nil), old...)
	for i := range fetched {
		fetched[i].Status = "AC"
		fetched[i].IsAC = true
	}
	updated, err := RefreshPendingSubmitVerdicts(context.Background(), db, fetched)
	if err != nil {
		t.Fatal(err)
	}
	if updated != 2 {
		t.Fatalf("updated=%d", updated)
	}
	var lifetime, days int64
	if err := db.Model(&model.UserACProblem{}).Where("user_id = ?", 7).Count(&lifetime).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.UserACProblemDay{}).Where("user_id = ?", 7).Count(&days).Error; err != nil {
		t.Fatal(err)
	}
	if lifetime != 2 || days != 2 {
		t.Fatalf("lifetime=%d days=%d", lifetime, days)
	}
}

func TestIsPendingSubmitStatus(t *testing.T) {
	if !model.IsPendingSubmitStatus("") || !model.IsPendingSubmitStatus("TESTING") {
		t.Fatal("empty/TESTING pending")
	}
	if !model.IsPendingSubmitStatus("Judging") || !model.IsPendingSubmitStatus("JUDGING") {
		t.Fatal("Judging should be pending")
	}
	if !model.IsPendingSubmitStatus("正在评测") || !model.IsPendingSubmitStatus("评测中") {
		t.Fatal("Chinese judging should be pending")
	}
	if model.IsPendingSubmitStatus("OK") || model.IsPendingSubmitStatus("WA") {
		t.Fatal("final not pending")
	}
}
