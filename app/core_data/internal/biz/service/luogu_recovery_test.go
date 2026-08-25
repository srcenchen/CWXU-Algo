package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestFinishLuoGuRecoveryFailureAndIncompleteKeepActive(t *testing.T) {
	db := luoguRecoveryDB(t)
	ctx := context.Background()
	observedAt := luoguRecoveryObservedAt(t, db)
	if changed, err := finishLuoGuRecovery(ctx, db, 1, 11, observedAt, false, errors.New("fetch failed")); err == nil || changed {
		t.Fatalf("fetch failure changed=%v err=%v", changed, err)
	}
	assertLuoGuRecoveryActive(t, db, true)
	if changed, err := finishLuoGuRecovery(ctx, db, 1, 11, observedAt, false, nil); err != nil || changed {
		t.Fatalf("incomplete changed=%v err=%v", changed, err)
	}
	assertLuoGuRecoveryActive(t, db, true)
}

func TestFinishLuoGuRecoveryCompleteSuccessClosesOverride(t *testing.T) {
	db := luoguRecoveryDB(t)
	changed, err := finishLuoGuRecovery(context.Background(), db, 1, 11, luoguRecoveryObservedAt(t, db), true, nil)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	assertLuoGuRecoveryActive(t, db, false)
}

func luoguRecoveryObservedAt(t *testing.T, db *gorm.DB) time.Time {
	t.Helper()
	var snapshot model.LuoGuPublicSnapshot
	if err := db.First(&snapshot, "user_id = 1").Error; err != nil {
		t.Fatal(err)
	}
	return snapshot.ObservedAt
}

func luoguRecoveryDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.LuoGuPublicSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.LuoGuPublicSnapshot{UserID: 1, Platform: "LuoGu", RemoteUID: 11, Active: true, RecoveryRequired: true, ObservedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func assertLuoGuRecoveryActive(t *testing.T, db *gorm.DB, want bool) {
	t.Helper()
	var got model.LuoGuPublicSnapshot
	if err := db.First(&got, "user_id = 1").Error; err != nil {
		t.Fatal(err)
	}
	if got.Active != want || got.RecoveryRequired != want {
		t.Fatalf("active=%v recovery=%v want=%v", got.Active, got.RecoveryRequired, want)
	}
}
