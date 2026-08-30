package task

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const profileRebuildAfterBindingTTL = 24 * time.Hour

// ProfileRebuildAfterBindingKey identifies a rebuild that must wait for the
// matching OJ binding's full crawl and problem binding to finish.
func ProfileRebuildAfterBindingKey(userID int64, platform string) string {
	return fmt.Sprintf("user_profile:rebuild-after-binding:%d:%s", userID, strings.TrimSpace(platform))
}

// MarkProfileRebuildAfterBinding leaves a durable-enough marker for the
// post-crawl binding pass. The marker is intentionally platform-scoped so a
// normal sync on another OJ cannot consume it early.
func MarkProfileRebuildAfterBinding(rdb *redis.Client, userID int64, platform string) error {
	if rdb == nil || userID <= 0 || strings.TrimSpace(platform) == "" {
		return nil
	}
	return rdb.Set(context.Background(), ProfileRebuildAfterBindingKey(userID, platform), "1", profileRebuildAfterBindingTTL).Err()
}

func HasProfileRebuildAfterBinding(ctx context.Context, rdb *redis.Client, userID int64, platform string) (bool, error) {
	if rdb == nil || userID <= 0 || strings.TrimSpace(platform) == "" {
		return false, nil
	}
	exists, err := rdb.Exists(ctx, ProfileRebuildAfterBindingKey(userID, platform)).Result()
	return exists > 0, err
}

func ClearProfileRebuildAfterBinding(ctx context.Context, rdb *redis.Client, userID int64, platform string) error {
	if rdb == nil || userID <= 0 || strings.TrimSpace(platform) == "" {
		return nil
	}
	return rdb.Del(ctx, ProfileRebuildAfterBindingKey(userID, platform)).Err()
}
