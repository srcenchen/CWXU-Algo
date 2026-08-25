package service

import (
	"context"
	"errors"
	"testing"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestQOJRepairMissingStateForcesFullFetch(t *testing.T) {
	db := qojRepairDB(t)
	needAll, err := forceRepairFetch(context.Background(), db, 7, spider.QOJ, false)
	if err != nil || !needAll {
		t.Fatalf("needAll=%v err=%v", needAll, err)
	}
}

func TestQOJRepairCompleteSuccessMarksCurrentVersion(t *testing.T) {
	db := qojRepairDB(t)
	if err := finishRepair(context.Background(), db, 7, spider.QOJ, true, true, nil); err != nil {
		t.Fatal(err)
	}
	needAll, err := forceRepairFetch(context.Background(), db, 7, spider.QOJ, false)
	if err != nil || needAll {
		t.Fatalf("needAll=%v err=%v", needAll, err)
	}
}

func TestQOJRepairEmptyCompleteFetchMarksCurrentVersion(t *testing.T) {
	db := qojRepairDB(t)
	if err := finishRepair(context.Background(), db, 7, spider.QOJ, true, true, nil); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.SpiderRepairState{}).Where("user_id = ? AND platform = ?", 7, spider.QOJ).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("repair state count=%d want=1", count)
	}
}

func TestQOJRepairFailureOrTruncationStaysIncomplete(t *testing.T) {
	for _, tt := range []struct {
		name     string
		complete bool
		fetchErr error
	}{
		{name: "fetch failure", complete: true, fetchErr: errors.New("fetch failed")},
		{name: "truncated", complete: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := qojRepairDB(t)
			_ = finishRepair(context.Background(), db, 7, spider.QOJ, true, tt.complete, tt.fetchErr)
			needAll, err := forceRepairFetch(context.Background(), db, 7, spider.QOJ, false)
			if err != nil || !needAll {
				t.Fatalf("needAll=%v err=%v", needAll, err)
			}
		})
	}
}

func qojRepairDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	return db
}
