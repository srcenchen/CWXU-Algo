package service

import (
	"context"
	"strings"
	"testing"

	"cwxu-algo/app/common/event"
	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider/problem_fetch"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
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

func TestMarkProblemAnalysisQueuedInvalidatesDetailCache(t *testing.T) {
	uc, p := refetchTestUseCase(t, model.ProblemStatusCompleted)
	mr := miniredis.RunT(t)
	uc.data.RDB = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if err := uc.markProblemAnalysisQueued(&p); err != nil {
		t.Fatal(err)
	}
	if got, _ := mr.Get(problemDetailVerKey(p.ID)); got != "1" {
		t.Fatalf("detail cache version = %q, want 1", got)
	}
	var saved model.Problem
	if err := uc.data.DB.First(&saved, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Status != model.ProblemStatusTagging || saved.ErrorMsg != "" {
		t.Fatalf("status=%q error=%q", saved.Status, saved.ErrorMsg)
	}
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

func TestShouldFetchProblemContentOnlyWhenMissingOrBroken(t *testing.T) {
	tests := []struct {
		name string
		md   string
		want bool
	}{
		{name: "missing", md: " \n ", want: true},
		{name: "broken", md: "### Problem StatementYou are given", want: true},
		{name: "normal", md: "### Problem Statement\nYou are given", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldFetchProblemContent(tt.md); got != tt.want {
				t.Fatalf("shouldFetchProblemContent(%q)=%v, want %v", tt.md, got, tt.want)
			}
		})
	}
}

func TestShouldForceEnqueueContestFetchDoesNotRepeatActiveOrExhaustedProblems(t *testing.T) {
	if !shouldForceEnqueueContestFetch(model.Problem{Status: model.ProblemStatusPending}) {
		t.Fatal("a never-attempted pending problem should be queued")
	}
	for _, p := range []model.Problem{
		{Status: model.ProblemStatusFetching},
		{Status: model.ProblemStatusFailed, FetchAttempts: maxFetchAttempts},
		{Status: model.ProblemStatusFailedPerm},
	} {
		if shouldForceEnqueueContestFetch(p) {
			t.Fatalf("problem status=%q attempts=%d should not be queued again", p.Status, p.FetchAttempts)
		}
	}
}

func TestContestMissingContentRequeueOnlyForNewContest(t *testing.T) {
	if !shouldRequeueMissingContestContent(true) {
		t.Fatal("new contest should allow initial content fetch")
	}
	if shouldRequeueMissingContestContent(false) {
		t.Fatal("existing contest must not refill missing content")
	}
}

func TestFetchedProblemContentMustNotBeEmpty(t *testing.T) {
	if fetchedProblemContentEmpty(nil) == false {
		t.Fatal("nil fetch result must be treated as empty")
	}
	if fetchedProblemContentEmpty(&problem_fetch.FetchedContent{ContentMD: " \n"}) == false {
		t.Fatal("blank fetch result must be treated as empty")
	}
	if fetchedProblemContentEmpty(&problem_fetch.FetchedContent{ContentMD: "statement"}) {
		t.Fatal("non-empty fetch result must be accepted")
	}
}

func TestEmptyAIAnalyzeResultIsNotCommitted(t *testing.T) {
	if !emptyAIAnalyzeResult(nil) || !emptyAIAnalyzeResult(&aiAnalyzeResult{}) {
		t.Fatal("nil and blank AI results must be rejected")
	}
	if emptyAIAnalyzeResult(&aiAnalyzeResult{Difficulty: "中等"}) {
		t.Fatal("a result with useful facts must be accepted")
	}
}
