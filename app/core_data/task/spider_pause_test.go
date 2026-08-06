package task

import (
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
	defer rdb.Close()

	if IsPlatformPaused(rdb, "NowCoder") {
		t.Fatal("默认不应处于暂停状态")
	}
	SetPlatformPaused(rdb, "NowCoder", true)
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
	SetPlatformPaused(rdb, "NowCoder", false)
	if IsPlatformPaused(rdb, "NowCoder") {
		t.Fatal("恢复后不应再判定为暂停")
	}
}

func TestDoPlatformSkipsWhenPaused(t *testing.T) {
	_, rdb := newTestRedis(t)
	defer rdb.Close()

	// mq=nil：若未跳过会报 mq not ready；暂停时应直接跳过不入队
	tk := NewSpiderTask(nil, rdb, nil)
	SetPlatformPaused(rdb, "NowCoder", true)
	res := tk.DoPlatform(1, "NowCoder", false)
	if res.Published != 0 || res.Failed != 0 || res.Deduped != 0 {
		t.Fatalf("暂停平台不应入队/失败/去重，got %+v", res)
	}
	if res.Platforms != 1 {
		t.Fatalf("暂停平台仍应计入尝试平台数，got %d", res.Platforms)
	}
}
