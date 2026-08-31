package service

import (
	"context"
	"errors"
	"testing"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/sitesettings"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestProblemFetchConsumerEarlyPauseIgnoresMessagePlatform(t *testing.T) {
	pipelineControl.SetFetchPaused(false)
	t.Cleanup(func() { pipelineControl.SetFetchPaused(false) })

	if problemFetchConsumerPaused() {
		t.Fatal("全局爬取未暂停时 consumer 不应提前拦截消息，平台暂停交给 ProcessFetch 按 DB 平台判断")
	}
}

func TestProblemAnalyzeCoordinationErrorsAreRetryable(t *testing.T) {
	for _, message := range []string{
		"profile invalidation intent changed",
		"profile invalidation already in progress",
		"profile invalidation ownership changed",
	} {
		if !isProblemAnalyzeCoordinationError(errors.New(message)) {
			t.Fatalf("%q should be treated as an indefinitely retryable coordination error", message)
		}
	}
}

func TestProblemFetchConsumerPauseHonorsExplicitBypass(t *testing.T) {
	wasPaused := pipelineControl.IsFetchPaused()
	pipelineControl.SetFetchPaused(true)
	t.Cleanup(func() { pipelineControl.SetFetchPaused(wasPaused) })

	if !problemFetchEventPaused(event.ProblemFetchEvent{}) {
		t.Fatal("普通题面任务在全局暂停时应被 consumer 提前拦截")
	}
	if problemFetchEventPaused(event.ProblemFetchEvent{BypassFetchPause: true}) {
		t.Fatal("主动补爬任务应绕过 consumer 的全局暂停拦截")
	}
}

func TestRuntimeConcurrencyPrefersRedisAndFallsBackToEnvironment(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Setenv("CWXU_SPIDER_CONCURRENCY", "13")
	t.Setenv("CWXU_PROBLEM_ANALYZE_CONCURRENCY", "14")

	if got := runtimeConcurrency(context.Background(), rdb, spiderConcurrencySetting); got != 13 {
		t.Fatalf("spider fallback = %d, want 13", got)
	}
	if got := runtimeConcurrency(context.Background(), rdb, analyzeConcurrencySetting); got != 14 {
		t.Fatalf("analyze fallback = %d, want 14", got)
	}
	if err := sitesettings.PublishRedis(context.Background(), rdb, &sitesettings.Runtime{
		ConfigVersion: 1, SpiderConcurrency: 7, ProblemAnalyzeConcurrency: 9,
	}); err != nil {
		t.Fatal(err)
	}
	if got := runtimeConcurrency(context.Background(), rdb, spiderConcurrencySetting); got != 7 {
		t.Fatalf("spider runtime = %d, want 7", got)
	}
	if got := runtimeConcurrency(context.Background(), rdb, analyzeConcurrencySetting); got != 9 {
		t.Fatalf("analyze runtime = %d, want 9", got)
	}
}

func TestRuntimeConcurrencySourceKeepsLastValueOnRedisFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Setenv("CWXU_SPIDER_CONCURRENCY", "13")
	if err := sitesettings.PublishRedis(context.Background(), rdb, &sitesettings.Runtime{
		ConfigVersion: 1, SpiderConcurrency: 7,
	}); err != nil {
		t.Fatal(err)
	}

	source := runtimeConcurrencySource(rdb, spiderConcurrencySetting)
	if got := source(); got != 7 {
		t.Fatalf("initial runtime concurrency = %d, want 7", got)
	}
	mr.Close()
	if got := source(); got != 7 {
		t.Fatalf("concurrency after Redis failure = %d, want last value 7", got)
	}
}

func TestRuntimeConcurrencyDoesNotUseEnvironmentOnRedisFailure(t *testing.T) {
	t.Setenv("CWXU_SPIDER_CONCURRENCY", "13")
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	if got := runtimeConcurrency(context.Background(), rdb, spiderConcurrencySetting); got != 4 {
		t.Fatalf("Redis failure concurrency = %d, want safe default 4", got)
	}
	source := runtimeConcurrencySource(rdb, spiderConcurrencySetting)
	if got := source(); got != 4 {
		t.Fatalf("Redis failure source concurrency = %d, want safe default 4", got)
	}
}

func TestClassifyProblemFetchPauseOnlyClassifiesGlobal(t *testing.T) {
	if got := classifyProblemFetchPause(errProblemFetchPaused); got != problemFetchPauseGlobal {
		t.Fatalf("全局暂停分类错误: %v", got)
	}
	if got := classifyProblemFetchPause(errors.New("problem platform paused")); got != problemFetchPauseNone {
		t.Fatalf("旧平台暂停错误不应再分类为暂停: %v", got)
	}
	if got := classifyProblemFetchPause(errors.New("fetch failed")); got != problemFetchPauseNone {
		t.Fatalf("普通错误不应归为暂停: %v", got)
	}
}
