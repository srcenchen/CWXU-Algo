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

func userACCleanupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.DailyUserStat{}, &model.UserACProblem{}, &model.UserACProblemDay{},
		&model.UserProblemStatus{}, &model.UserTagAC{}, &model.UserTagACSnapshot{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDeletePlatformUserACClearsCrossPlatformUserTagAggregate(t *testing.T) {
	db := userACCleanupTestDB(t)
	now := time.Now()
	rows := []model.UserACProblem{
		{UserID: 81, Platform: "LuoGu", ProblemKey: "p:1", FirstACAt: now},
		{UserID: 81, Platform: "CodeForces", ProblemKey: "p:2", FirstACAt: now},
		{UserID: 82, Platform: "LuoGu", ProblemKey: "p:3", FirstACAt: now},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.UserTagAC{
		{UserID: 81, Tag: "old", Count: 2, Weight: 1},
		{UserID: 82, Tag: "keep", Count: 1, Weight: 1},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.UserTagACSnapshot{
		{UserID: 81, ScoreVersion: 1, ModelVersion: 1, RowCount: 1, PublishedAt: now},
		{UserID: 82, ScoreVersion: 1, ModelVersion: 1, RowCount: 1, PublishedAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := DeletePlatformUserAC(context.Background(), db, 81, "LuoGu"); err != nil {
		t.Fatal(err)
	}
	var removed, kept int64
	if err := db.Model(&model.UserTagAC{}).Where("user_id = ?", 81).Count(&removed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.UserTagAC{}).Where("user_id = ?", 82).Count(&kept).Error; err != nil {
		t.Fatal(err)
	}
	if removed != 0 || kept != 1 {
		t.Fatalf("tag rows removed-user=%d other-user=%d", removed, kept)
	}
	if err := db.Model(&model.UserTagACSnapshot{}).Where("user_id = ?", 81).Count(&removed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.UserTagACSnapshot{}).Where("user_id = ?", 82).Count(&kept).Error; err != nil {
		t.Fatal(err)
	}
	if removed != 0 || kept != 1 {
		t.Fatalf("snapshot rows removed-user=%d other-user=%d", removed, kept)
	}
}

func TestDeleteUserPreaggDeletesStatusAndTagRows(t *testing.T) {
	db := userACCleanupTestDB(t)
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for _, uid := range []int64{91, 92} {
		if err := db.Create(&model.DailyUserStat{UserID: uid, Day: day}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.UserACProblem{UserID: uid, Platform: "LuoGu", ProblemKey: "p:1", FirstACAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.UserACProblemDay{UserID: uid, Day: day, Platform: "LuoGu", ProblemKey: "p:1"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.UserProblemStatus{UserID: uid, ProblemID: 1, Status: model.UserProblemStatusAC, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.UserTagAC{UserID: uid, Tag: "dp", Count: 1, Weight: 1}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.UserTagACSnapshot{UserID: uid, ScoreVersion: 1, ModelVersion: 1, RowCount: 1, PublishedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := DeleteUserPreagg(context.Background(), db, 91); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"daily_user_stats", "user_ac_problems", "user_ac_problem_days", "user_problem_status", "user_tag_ac", "user_tag_ac_snapshots"} {
		var removed, kept int64
		if err := db.Table(table).Where("user_id = ?", 91).Count(&removed).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Table(table).Where("user_id = ?", 92).Count(&kept).Error; err != nil {
			t.Fatal(err)
		}
		if removed != 0 || kept != 1 {
			t.Fatalf("table=%s removed-user=%d other-user=%d", table, removed, kept)
		}
	}
}
