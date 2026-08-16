package ratelimit

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Allow 基于 Redis SETNX 的固定窗口限流：interval 内同一 key 只允许 1 次。
// 与原先进程内 rate.Limiter(Every(interval), 1) 语义一致，且多实例共享。
func Allow(ctx context.Context, rdb *redis.Client, key string, interval time.Duration) (bool, error) {
	if rdb == nil {
		return false, fmt.Errorf("redis unavailable")
	}
	if interval <= 0 {
		return true, nil
	}
	ok, err := rdb.SetNX(ctx, key, "1", interval).Result()
	if err != nil {
		// Expensive operations fail closed so a Redis outage cannot remove abuse protection.
		return false, err
	}
	return ok, nil
}

// SpiderUpdateKey 手动全量更新限流 key
func SpiderUpdateKey(userId int64) string {
	return fmt.Sprintf("ratelimit:spider:update:%d", userId)
}

// SpiderSetKey 按 OJ 账号限制绑定触发的全量抓取，摘要避免在 Redis key 中暴露用户名。
func SpiderSetKey(platform, username string) string {
	digest := sha256.Sum256([]byte(platform + "\x00" + username))
	return fmt.Sprintf("ratelimit:spider:set:account:%x", digest)
}

// SpiderUpdateAllKey 管理员全站更新限流 key
func SpiderUpdateAllKey(adminId int64) string {
	return fmt.Sprintf("ratelimit:spider:update_all:%d", adminId)
}
