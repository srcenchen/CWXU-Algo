package service

import (
	"context"
	"fmt"
	"time"

	data2 "cwxu-algo/app/common/data"
	"cwxu-algo/app/common/utils"
	"cwxu-algo/app/core_data/internal/data/dal"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const (
	// userHeatZSetKey 有序集合：score=访问热度（period 读累加）
	userHeatZSetKey = "statistic:user:heat"
	// heatWarmThreshold 爬虫写入后：热度 ≥ 该值则预热 period 缓存
	heatWarmThreshold = 5
	// heatZSetTTL 热度集合过期
	heatZSetTTL = 7 * 24 * time.Hour
	// 与 PeriodCount 缓存 schema 对齐（statistic.go periodCacheSchema=s10）
	periodWarmSchema = "9"
)

// TouchUserHeat 记录用户统计访问热度（period 读路径调用）
func TouchUserHeat(ctx context.Context, rdb *redis.Client, userId int64) {
	if rdb == nil || userId <= 0 {
		return
	}
	pipe := rdb.Pipeline()
	pipe.ZIncrBy(ctx, userHeatZSetKey, 1, fmt.Sprintf("%d", userId))
	pipe.Expire(ctx, userHeatZSetKey, heatZSetTTL)
	_, _ = pipe.Exec(ctx)
}

// UserHeatScore 当前热度
func UserHeatScore(ctx context.Context, rdb *redis.Client, userId int64) float64 {
	if rdb == nil || userId <= 0 {
		return 0
	}
	s, err := rdb.ZScore(ctx, userHeatZSetKey, fmt.Sprintf("%d", userId)).Result()
	if err != nil {
		return 0
	}
	return s
}

// IsHotUser 是否达到预热阈值
func IsHotUser(ctx context.Context, rdb *redis.Client, userId int64) bool {
	return UserHeatScore(ctx, rdb, userId) >= heatWarmThreshold
}

// MaybeWarmUserPeriod 若用户够热，则回源算 period + 生涯热力并写入 Redis。
// 在爬虫 invalidate（已 INCR user ver）之后调用：
//   - 保证下次打开首页读到的是新 ver 下的热数据（即时性）
//   - 避免热用户 miss 后再打 DB（性能）
func MaybeWarmUserPeriod(ctx context.Context, db *gorm.DB, rdb *redis.Client, userId int64) {
	if db == nil || rdb == nil || userId <= 0 {
		return
	}
	if !IsHotUser(ctx, rdb, userId) {
		return
	}
	// 防抖：同一用户 30s 内只预热一次
	lockKey := fmt.Sprintf("statistic:warm:lock:%d", userId)
	ok, err := rdb.SetNX(ctx, lockKey, "1", 30*time.Second).Result()
	if err != nil || !ok {
		return
	}

	statDal := dal.NewStatisticDal(db, rdb)
	ver := "0"
	if v, e := rdb.Get(ctx, fmt.Sprintf("statistic:user:%d:ver", userId)).Result(); e == nil && v != "" {
		ver = v
	}

	// 1) period
	submit, ac, err := statDal.GetPeriodCountScoped(userId, nil)
	if err != nil {
		log.Debugf("warm period user=%d: %v", userId, err)
	} else {
		type PeriodCountData struct {
			Submit dal.PeriodSubmitCount
			Ac     dal.PeriodAcCount
		}
		data := PeriodCountData{Submit: submit, Ac: ac}
		if b, e := utils.GobEncoder(&data); e == nil {
			cacheKey := fmt.Sprintf("statistic:period:s%s:u%d:v%s", periodWarmSchema, userId, ver)
			_ = rdb.Set(ctx, cacheKey, b, data2.DefaultCacheTTL).Err()
		}
	}

	// 2) 生涯热力（submit + AC 各一份；与 Heatmap 稳定 key 对齐）
	careerStart, careerEnd := personalHeatmapCareerRange()
	for _, isAc := range []bool{false, true} {
		rows, qerr := statDal.HeatmapQueryScoped(ctx, careerStart, careerEnd, userId, isAc, nil)
		if qerr != nil {
			log.Debugf("warm heatmap user=%d isAc=%v: %v", userId, isAc, qerr)
			continue
		}
		if rows == nil {
			rows = []dal.DailyCount{}
		}
		if b, e := utils.GobEncoder(&rows); e == nil {
			cacheKey := fmt.Sprintf("statistic:heatmap:s%s:u%d:%t:v%s", heatmapCacheSchema, userId, isAc, ver)
			_ = rdb.Set(ctx, cacheKey, b, data2.DefaultCacheTTL).Err()
		}
	}

	log.Debugf("warmed period+heatmap cache user=%d heat=%.0f ver=%s", userId, UserHeatScore(ctx, rdb, userId), ver)
}
