package service

import (
	"context"
	"strings"
	"testing"

	"cwxu-algo/app/common/event"
	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func refetchTestUseCase(t *testing.T, status string) (*ProblemUseCase, model.Problem) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Problem{}); err != nil {
		t.Fatal(err)
	}
	p := model.Problem{
		Platform: "CodeForces", ExternalID: "gym104369D",
		Title: "New Houses", URL: "https://codeforces.com/gym/104369/problem/D",
		Status: status, ErrorMsg: "permanent failure",
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	return &ProblemUseCase{data: &coredata.Data{DB: db}}, p
}

func TestForceEnqueueRefetchRejectsPermanentFailure(t *testing.T) {
	uc, p := refetchTestUseCase(t, model.ProblemStatusFailedPerm)
	err := uc.ForceEnqueueRefetch(p.ID, 1)
	if err == nil || !strings.Contains(err.Error(), "永久失败") {
		t.Fatalf("err=%v, want permanent failure rejection", err)
	}
}

func TestProcessFetchKeepsPermanentFailureForForcedMessage(t *testing.T) {
	uc, p := refetchTestUseCase(t, model.ProblemStatusFailedPerm)
	if err := uc.ProcessFetch(context.Background(), event.ProblemFetchEvent{
		ProblemID: p.ID, Force: true, ForceRefetch: true, BypassFetchPause: true,
	}); err != nil {
		t.Fatal(err)
	}
	var got model.Problem
	if err := uc.data.DB.First(&got, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ProblemStatusFailedPerm || got.ErrorMsg != p.ErrorMsg {
		t.Fatalf("status=%q error=%q, want permanent failure unchanged", got.Status, got.ErrorMsg)
	}
}
