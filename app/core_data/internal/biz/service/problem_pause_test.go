package service

import (
	"context"
	"strings"
	"testing"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestProcessFetchRejectsPausedProblemPlatformAfterDatabaseRead(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := task.SetProblemPlatformPaused(rdb, "NowCoder", true); err != nil {
		t.Fatal(err)
	}

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Problem{}); err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "NowCoder", ExternalID: "abc", Status: model.ProblemStatusPending}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}}
	err = uc.ProcessFetch(context.Background(), event.ProblemFetchEvent{
		ProblemID: p.ID,
		Platform:  "NowCoder",
		Force:     true,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "paused") {
		t.Fatalf("DB 读取后暂停平台应返回 paused 错误，got %v", err)
	}
}

func TestProblemPlatformPauseCheckDoesNotMisreportUnpausedPlatform(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := task.SetProblemPlatformPaused(rdb, "NowCoder", true); err != nil {
		t.Fatal(err)
	}

	uc := &ProblemUseCase{data: &data.Data{RDB: rdb}}
	if uc.isProblemPlatformPaused("AtCoder") {
		t.Fatal("未暂停平台不应被误报为暂停")
	}
}

func TestProcessFetchUsesDatabasePlatformWhenEventPlatformDiffers(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := task.SetProblemPlatformPaused(rdb, "AtCoder", true); err != nil {
		t.Fatal(err)
	}

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Problem{}); err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "AtCoder", ExternalID: "abc", Status: model.ProblemStatusPending}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}}
	err = uc.ProcessFetch(context.Background(), event.ProblemFetchEvent{
		ProblemID: p.ID,
		Platform:  "NowCoder",
		Force:     true,
	})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "paused") {
		t.Fatalf("DB 平台已暂停且事件平台不一致时应返回 paused，got %v", err)
	}
	var got model.Problem
	if err := db.First(&got, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ProblemStatusPending {
		t.Fatalf("暂停应在外抓前返回，状态不应改变为 FETCHING，got %s", got.Status)
	}
}

func TestRestorePausedProblemFetchReturnsPendingForEmptyContent(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := task.SetProblemPlatformPaused(rdb, "AtCoder", true); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Problem{}); err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "AtCoder", ExternalID: "abc", Status: model.ProblemStatusFetching}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}}
	err = uc.restorePausedProblemFetch(&p)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "paused") {
		t.Fatalf("暂停竞态应返回 paused 错误，got %v", err)
	}
	var got model.Problem
	if err := db.First(&got, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ProblemStatusPending {
		t.Fatalf("无题面暂停竞态应恢复 PENDING，got %s", got.Status)
	}
}

func TestRestorePausedProblemFetchDoesNotOverwriteConcurrentCompletion(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := task.SetProblemPlatformPaused(rdb, "AtCoder", true); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Problem{}); err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "AtCoder", ExternalID: "abc", Status: model.ProblemStatusFetching}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Problem{}).Where("id = ?", p.ID).Updates(map[string]interface{}{
		"status":     model.ProblemStatusCompleted,
		"content_md": "concurrent result",
	}).Error; err != nil {
		t.Fatal(err)
	}

	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}}
	err = uc.restorePausedProblemFetch(&p)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "paused") {
		t.Fatalf("暂停竞态应返回 paused 错误，got %v", err)
	}
	var got model.Problem
	if err := db.First(&got, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ProblemStatusCompleted || got.ContentMD != "concurrent result" {
		t.Fatalf("并发完成结果不得被恢复覆盖，got status=%s content=%q", got.Status, got.ContentMD)
	}
}

func TestRestorePausedProblemFetchRestoresPendingWhenGlobalPauseStartsBeforeFetch(t *testing.T) {
	wasPaused := pipelineControl.IsFetchPaused()
	pipelineControl.SetFetchPaused(true)
	t.Cleanup(func() { pipelineControl.SetFetchPaused(wasPaused) })
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Problem{}); err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "AtCoder", ExternalID: "abc", Status: model.ProblemStatusFetching}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	err = uc.restorePausedProblemFetch(&p)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "paused") {
		t.Fatalf("外抓前出现全局暂停应返回 paused，got %v", err)
	}
	var got model.Problem
	if err := db.First(&got, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ProblemStatusPending {
		t.Fatalf("全局暂停竞态应恢复 PENDING，got %s", got.Status)
	}
}

func TestProcessFetchFailsSafeWhenProblemPauseRedisUnavailable(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Problem{}); err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "AtCoder", ExternalID: "abc", Status: model.ProblemStatusPending}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}

	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}}
	err = uc.ProcessFetch(context.Background(), event.ProblemFetchEvent{ProblemID: p.ID, Force: true})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unavailable") {
		t.Fatalf("暂停状态不可用时必须阻止外抓，got %v", err)
	}
	var got model.Problem
	if err := db.First(&got, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != model.ProblemStatusPending {
		t.Fatalf("暂停读取失败不得进入 FETCHING，got %s", got.Status)
	}
}
