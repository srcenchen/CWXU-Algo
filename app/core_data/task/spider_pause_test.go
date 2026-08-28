package task

import (
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestSetPlatformPausedRoundtrip(t *testing.T) {
	_, rdb := newTestRedis(t)

	if IsPlatformPaused(rdb, "NowCoder") {
		t.Fatal("默认不应处于暂停状态")
	}
	if err := SetPlatformPaused(rdb, "NowCoder", true); err != nil {
		t.Fatalf("暂停提交爬虫: %v", err)
	}
	if !IsPlatformPaused(rdb, "NowCoder") {
		t.Fatal("暂停后应判定为暂停")
	}
	if IsPlatformPaused(rdb, "AtCoder") {
		t.Fatal("其它平台不应受影响")
	}
	got := PausedPlatforms(rdb)
	if !got["NowCoder"] || got["AtCoder"] {
		t.Fatalf("PausedPlatforms 异常: %v", got)
	}
	if err := SetPlatformPaused(rdb, "NowCoder", false); err != nil {
		t.Fatalf("恢复提交爬虫: %v", err)
	}
	if IsPlatformPaused(rdb, "NowCoder") {
		t.Fatal("恢复后不应再判定为暂停")
	}
}

func TestSubmitAndProblemPlatformPauseAreIsolated(t *testing.T) {
	_, rdb := newTestRedis(t)

	if err := SetPlatformPaused(rdb, "NowCoder", true); err != nil {
		t.Fatal(err)
	}
	if IsProblemPlatformPaused(rdb, "NowCoder") {
		t.Fatal("提交暂停不应影响题面爬虫")
	}
	if IsPlatformPaused(rdb, "AtCoder") {
		t.Fatal("NowCoder 提交暂停不应影响 AtCoder")
	}

	if err := SetProblemPlatformPaused(rdb, "AtCoder", true); err != nil {
		t.Fatal(err)
	}
	if !IsProblemPlatformPaused(rdb, "AtCoder") {
		t.Fatal("AtCoder 题面爬虫应暂停")
	}
	if IsProblemPlatformPaused(rdb, "NowCoder") || IsPlatformPaused(rdb, "AtCoder") {
		t.Fatal("题面暂停不应影响其它平台或提交爬虫")
	}
}

func TestIsProblemPlatformPausedSafeReturnsRedisError(t *testing.T) {
	mr, rdb := newTestRedis(t)
	mr.Close()

	paused, err := IsProblemPlatformPausedSafe(rdb, "NowCoder")
	if err == nil {
		t.Fatal("Redis 读取失败必须返回错误，不能按未暂停继续外抓")
	}
	if paused {
		t.Fatal("读取失败不是已确认的平台暂停")
	}
}

func TestSetPlatformPausedReturnsUnavailableAndValidationErrors(t *testing.T) {
	setters := []struct {
		name string
		set  func(*redis.Client, string, bool) error
	}{
		{name: "submit", set: SetPlatformPaused},
		{name: "problem", set: SetProblemPlatformPaused},
	}
	for _, tt := range setters {
		t.Run(tt.name+" nil redis", func(t *testing.T) {
			if err := tt.set(nil, "NowCoder", true); err == nil {
				t.Fatal("nil redis 必须返回错误")
			}
		})
		t.Run(tt.name+" empty platform", func(t *testing.T) {
			_, rdb := newTestRedis(t)
			if err := tt.set(rdb, "  ", true); err == nil {
				t.Fatal("空平台必须返回错误")
			}
		})
		t.Run(tt.name+" write failure", func(t *testing.T) {
			mr, rdb := newTestRedis(t)
			mr.Close()
			err := tt.set(rdb, "NowCoder", true)
			if err == nil || !strings.Contains(err.Error(), "pause") {
				t.Fatalf("Redis 写失败应返回清晰错误，got %v", err)
			}
		})
	}
}

func TestDoPlatformSkipsWhenPaused(t *testing.T) {
	_, rdb := newTestRedis(t)
	// mq=nil：若未跳过会报 mq not ready；暂停时应直接跳过不入队
	tk := NewSpiderTask(nil, rdb, nil)
	if err := SetPlatformPaused(rdb, "NowCoder", true); err != nil {
		t.Fatal(err)
	}
	res := tk.DoPlatform(1, "NowCoder", false)
	if res.Published != 0 || res.Failed != 0 || res.Deduped != 0 {
		t.Fatalf("暂停平台不应入队/失败/去重，got %+v", res)
	}
	if res.Platforms != 1 {
		t.Fatalf("暂停平台仍应计入尝试平台数，got %d", res.Platforms)
	}
}
