package task

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spidermetrics"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
	"gorm.io/gorm"
)

const (
	// pendingTTL 待处理去重窗口：同用户在此时间内重复入队会被跳过
	pendingTTL = 15 * time.Minute
	// inflightTTL 执行中标记，防止重复消费叠跑
	inflightTTL = 45 * time.Minute
)

type SpiderTask struct {
	mq  *event.RabbitMQ
	rdb *redis.Client
	db  *gorm.DB
	// queueReady 避免每次入队都 QueueDeclare（50 用户 cron 高频时省 RTT）
	queueReady atomic.Bool
}

func NewSpiderTask(mq *event.RabbitMQ, rdb *redis.Client, db *gorm.DB) *SpiderTask {
	return &SpiderTask{mq: mq, rdb: rdb, db: db}
}

func (t *SpiderTask) ensureSpiderQueue() error {
	if t.queueReady.Load() {
		return nil
	}
	if _, err := t.mq.QueueDeclare("spider", true, false, false, false, nil); err != nil {
		return err
	}
	t.queueReady.Store(true)
	return nil
}

func pendingKey(userId int64, platform string) string {
	if platform == "" {
		return fmt.Sprintf("spider:pending:%d", userId)
	}
	return fmt.Sprintf("spider:pending:%d:%s", userId, platform)
}

func InflightKey(userId int64, platform string) string {
	if platform == "" {
		return fmt.Sprintf("spider:inflight:%d", userId)
	}
	return fmt.Sprintf("spider:inflight:%d:%s", userId, platform)
}

// LastOKKey 该用户最近一次爬虫成功时间（unix 秒字符串）
func LastOKKey(userId int64) string {
	return fmt.Sprintf("spider:last_ok:%d", userId)
}

// LastOKPlatformKey 用户×平台最近一次爬虫成功时间
func LastOKPlatformKey(userId int64, platform string) string {
	return fmt.Sprintf("spider:last_ok:%d:%s", userId, platform)
}

// LastFailPlatformKey 用户×平台最近一次爬虫失败时间
func LastFailPlatformKey(userId int64, platform string) string {
	return fmt.Sprintf("spider:last_fail:%d:%s", userId, platform)
}

// LastErrPlatformKey 用户×平台最近一次失败原因（短文本）
func LastErrPlatformKey(userId int64, platform string) string {
	return fmt.Sprintf("spider:last_err:%d:%s", userId, platform)
}

// OjLastOKKey 该 OJ 最近一次任意用户爬虫成功时间（站管监控聚合）
func OjLastOKKey(platform string) string {
	return fmt.Sprintf("spider:oj_last_ok:%s", platform)
}

// OjLastFailKey 该 OJ 最近一次任意用户爬虫失败时间
func OjLastFailKey(platform string) string {
	return fmt.Sprintf("spider:oj_last_fail:%s", platform)
}

// OjLastErrKey 该 OJ 最近一次失败短文案
func OjLastErrKey(platform string) string {
	return fmt.Sprintf("spider:oj_last_err:%s", platform)
}

const (
	// pausedPlatformsKey 站管暂停提交同步的 OJ 集合（SET；不带 TTL）
	pausedPlatformsKey = "spider:paused_platforms"
	pausedProblemKey   = "spider:paused_problem_platforms"
	proxyPlatformsKey  = "spider:proxy_platforms"
)

func isPlatformPaused(rdb *redis.Client, key, platform string) bool {
	if rdb == nil || strings.TrimSpace(platform) == "" {
		return false
	}
	ok, err := rdb.SIsMember(context.Background(), key, strings.TrimSpace(platform)).Result()
	if err != nil {
		log.Warnf("SpiderTask: SIsMember %s platform %s: %v", key, platform, err)
		return false
	}
	return ok
}

func setPlatformPaused(rdb *redis.Client, key, platform string, paused bool) error {
	if rdb == nil {
		return fmt.Errorf("set platform pause: redis unavailable")
	}
	p := strings.TrimSpace(platform)
	if p == "" {
		return fmt.Errorf("set platform pause: platform is empty")
	}
	if paused {
		if err := rdb.SAdd(context.Background(), key, p).Err(); err != nil {
			return fmt.Errorf("set platform pause: %w", err)
		}
		return nil
	}
	if err := rdb.SRem(context.Background(), key, p).Err(); err != nil {
		return fmt.Errorf("set platform pause: %w", err)
	}
	return nil
}

// IsPlatformPaused 站管是否已暂停某 OJ 的爬虫同步
func IsPlatformPaused(rdb *redis.Client, platform string) bool {
	return isPlatformPaused(rdb, pausedPlatformsKey, platform)
}

// SetPlatformPaused 暂停 / 恢复某 OJ 的爬虫同步
func SetPlatformPaused(rdb *redis.Client, platform string, paused bool) error {
	return setPlatformPaused(rdb, pausedPlatformsKey, platform, paused)
}

func IsProblemPaused(rdb *redis.Client, platform string) bool {
	return isPlatformPaused(rdb, pausedProblemKey, platform)
}

func SetProblemPaused(rdb *redis.Client, platform string, paused bool) error {
	return setPlatformPaused(rdb, pausedProblemKey, platform, paused)
}

func IsProxyEnabled(rdb *redis.Client, platform string) bool {
	return isPlatformPaused(rdb, proxyPlatformsKey, platform)
}

func SetProxyEnabled(rdb *redis.Client, platform string, enabled bool) error {
	return setPlatformPaused(rdb, proxyPlatformsKey, platform, !enabled)
}

// PausedPlatforms 返回已暂停同步的 OJ 集合（站管监控用）
func PausedPlatforms(rdb *redis.Client) map[string]bool {
	out := map[string]bool{}
	if rdb == nil {
		return out
	}
	members, err := rdb.SMembers(context.Background(), pausedPlatformsKey).Result()
	if err != nil {
		log.Warnf("SpiderTask: SMembers paused platforms: %v", err)
		return out
	}
	for _, m := range members {
		out[m] = true
	}
	return out
}

// IsPlatformPaused 该任务实例是否已暂停该 OJ 的爬虫同步（消费侧用；nil 安全）
func (t *SpiderTask) IsPlatformPaused(platform string) bool {
	if t == nil {
		return false
	}
	return IsPlatformPaused(t.rdb, platform)
}

// EnqueueResult 单次入队结果（供 cron claim 是否保留判断）
type EnqueueResult struct {
	Published int // MQ 成功条数
	Deduped   int // pending/inflight 跳过
	Failed    int // 声明/发布失败
	Platforms int // 尝试的平台数
}

// KeepClaim 有成功入队或已在途任务时保留周期 claim，避免空窗或重复轰炸
func (r EnqueueResult) KeepClaim() bool {
	return r.Published > 0 || r.Deduped > 0
}

// Do 为该用户每个已绑定平台各入队一条消息（一条消息 = 一个平台请求）。
func (t *SpiderTask) Do(userId int64, needAll bool) EnqueueResult {
	plats := t.listUserPlatforms(userId)
	if len(plats) == 0 {
		log.Debugf("SpiderTask: Do skip user=%d (no platform binding)", userId)
		return EnqueueResult{}
	}
	var res EnqueueResult
	for _, p := range plats {
		r := t.DoPlatform(userId, p, needAll)
		res.Published += r.Published
		res.Deduped += r.Deduped
		res.Failed += r.Failed
		res.Platforms += r.Platforms
	}
	return res
}

// listUserPlatforms 查用户已绑定 OJ 平台名
func (t *SpiderTask) listUserPlatforms(userId int64) []string {
	if t.db == nil || userId <= 0 {
		return nil
	}
	var names []string
	if err := t.db.Model(&model.Platform{}).
		Where("user_id = ?", userId).
		Pluck("platform", &names).Error; err != nil {
		log.Warnf("SpiderTask: list platforms user=%d: %v", userId, err)
		return nil
	}
	return names
}

// DoPlatform 入队单平台爬虫任务（platform 必须非空；空则按 Do 展开）。
// 去重：inflight 存在则跳过；pending 用 SetNX 原子占坑（多实例/并发安全）。
func (t *SpiderTask) DoPlatform(userId int64, platform string, needAll bool) EnqueueResult {
	if platform == "" {
		return t.Do(userId, needAll)
	}
	if IsLegacyServerCrawlerDisabled(platform) {
		log.Debugf("SpiderTask: skip enqueue user=%d platform=%q (browser sync only)", userId, platform)
		return EnqueueResult{Platforms: 1}
	}
	// 站管已暂停该 OJ：不占 pending、不入队（恢复后自然继续）
	if IsPlatformPaused(t.rdb, platform) {
		log.Debugf("SpiderTask: skip enqueue user=%d platform=%q (paused by ops)", userId, platform)
		return EnqueueResult{Platforms: 1}
	}
	if t.mq == nil {
		log.Errorf("SpiderTask: mq not ready")
		return EnqueueResult{Platforms: 1, Failed: 1}
	}
	pk := pendingKey(userId, platform)
	if t.rdb != nil {
		ctx := context.Background()
		// 正在执行：不重复入队
		if n, err := t.rdb.Exists(ctx, InflightKey(userId, platform)).Result(); err == nil && n > 0 {
			spidermetrics.IncDedupSkipped()
			log.Debugf("SpiderTask: dedup skip inflight user=%d platform=%q needAll=%v", userId, platform, needAll)
			return EnqueueResult{Platforms: 1, Deduped: 1}
		}
		// 原子占 pending；Exists+Set 有竞态，多副本会各塞一条
		ok, err := t.rdb.SetNX(ctx, pk, "1", pendingTTL).Result()
		if err != nil {
			log.Warnf("SpiderTask: setnx pending failed (allow): %v", err)
		} else if !ok {
			spidermetrics.IncDedupSkipped()
			log.Debugf("SpiderTask: dedup skip pending user=%d platform=%q needAll=%v", userId, platform, needAll)
			return EnqueueResult{Platforms: 1, Deduped: 1}
		}
	}
	if err := t.ensureSpiderQueue(); err != nil {
		log.Errorf("SpiderTask: QueueDeclare failed: %v", err)
		t.clearPending(userId, platform)
		return EnqueueResult{Platforms: 1, Failed: 1}
	}
	e := event.SpiderEvent{UserId: userId, NeedAll: needAll, Platform: platform}
	body, err := json.Marshal(e)
	if err != nil {
		log.Errorf("SpiderTask: json.Marshal failed: %v", err)
		t.clearPending(userId, platform)
		return EnqueueResult{Platforms: 1, Failed: 1}
	}
	if err := t.mq.Publish("", "spider", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
	}); err != nil {
		log.Errorf("SpiderTask: Publish failed: %v", err)
		// 连接可能已重置，下次重新 declare
		t.queueReady.Store(false)
		t.clearPending(userId, platform)
		return EnqueueResult{Platforms: 1, Failed: 1}
	}
	spidermetrics.IncEnqueued(platform)
	return EnqueueResult{Platforms: 1, Published: 1}
}

// IsLegacyServerCrawlerDisabled identifies platforms whose server-side crawler
// has been replaced by a browser-local synchronization flow.
func IsLegacyServerCrawlerDisabled(platform string) bool {
	return strings.EqualFold(strings.TrimSpace(platform), "LuoGu")
}

func (t *SpiderTask) clearPending(userId int64, platform string) {
	if t.rdb == nil {
		return
	}
	_ = t.rdb.Del(context.Background(), pendingKey(userId, platform)).Err()
}

// MarkInflight 消费开始时调用
func (t *SpiderTask) MarkInflight(userId int64, platform string) {
	if t.rdb == nil {
		return
	}
	ctx := context.Background()
	_ = t.rdb.Del(ctx, pendingKey(userId, platform)).Err()
	_ = t.rdb.Set(ctx, InflightKey(userId, platform), "1", inflightTTL).Err()
}

// ClearInflight 消费结束时调用
func (t *SpiderTask) ClearInflight(userId int64, platform string) {
	if t.rdb == nil {
		return
	}
	_ = t.rdb.Del(context.Background(), InflightKey(userId, platform)).Err()
}

// MarkLastOK 记录该用户最近一次爬虫成功时间（unix 秒，TTL 90 天防 key 膨胀）。
// platform 非空时同时写入用户×平台成功时间。
func (t *SpiderTask) MarkLastOK(userId int64, platform string) {
	if t.rdb == nil || userId <= 0 {
		return
	}
	ctx := context.Background()
	now := time.Now().Unix()
	ttl := 90 * 24 * time.Hour
	_ = t.rdb.Set(ctx, LastOKKey(userId), now, ttl).Err()
	if platform != "" {
		_ = t.rdb.Set(ctx, LastOKPlatformKey(userId, platform), now, ttl).Err()
		// OJ 级聚合最近成功时间（站管监控）
		_ = t.rdb.Set(ctx, OjLastOKKey(platform), now, ttl).Err()
		// 成功后清掉该平台失败标记（用户级 + OJ 级），避免 UI 一直显示异常/残留失败文案
		_ = t.rdb.Del(ctx,
			LastFailPlatformKey(userId, platform),
			LastErrPlatformKey(userId, platform),
			OjLastFailKey(platform),
			OjLastErrKey(platform),
		).Err()
	}
}

// MarkLastFail 记录用户×平台最近一次爬虫失败时间与短错误（供资料页展示）。
// userSide 为用户侧失败（绑定用户名错误等）：仍写用户级失败供本人提示，
// 但不写 OJ 级最近失败，避免站管监控把整 OJ 标成「同步异常」。
func (t *SpiderTask) MarkLastFail(userId int64, platform string, errMsg string, userSide bool) {
	if t.rdb == nil || userId <= 0 || platform == "" {
		return
	}
	ctx := context.Background()
	now := time.Now().Unix()
	_ = t.rdb.Set(ctx, LastFailPlatformKey(userId, platform), now, 90*24*time.Hour).Err()
	msg := strings.TrimSpace(errMsg)
	if msg == "" {
		msg = "同步失败"
	}
	// 控制长度，避免 Redis 塞进整段堆栈
	runes := []rune(msg)
	if len(runes) > 200 {
		msg = string(runes[:200])
	}
	_ = t.rdb.Set(ctx, LastErrPlatformKey(userId, platform), msg, 7*24*time.Hour).Err()
	if userSide {
		return
	}
	// OJ 级聚合最近失败时间 + 文案（站管监控）
	_ = t.rdb.Set(ctx, OjLastFailKey(platform), now, 90*24*time.Hour).Err()
	_ = t.rdb.Set(ctx, OjLastErrKey(platform), msg, 7*24*time.Hour).Err()
}

// GetLastOK 读取最近成功同步时间（unix 秒；无记录返回 0）
func (t *SpiderTask) GetLastOK(userId int64) int64 {
	if t.rdb == nil || userId <= 0 {
		return 0
	}
	v, err := t.rdb.Get(context.Background(), LastOKKey(userId)).Int64()
	if err != nil {
		return 0
	}
	return v
}

// GetPlatformSyncHealth 读取用户×平台的成功/失败时间与错误文案
func (t *SpiderTask) GetPlatformSyncHealth(userId int64, platform string) (lastOK, lastFail int64, lastErr string) {
	if t.rdb == nil || userId <= 0 || platform == "" {
		return 0, 0, ""
	}
	ctx := context.Background()
	if v, err := t.rdb.Get(ctx, LastOKPlatformKey(userId, platform)).Int64(); err == nil {
		lastOK = v
	}
	if v, err := t.rdb.Get(ctx, LastFailPlatformKey(userId, platform)).Int64(); err == nil {
		lastFail = v
	}
	if s, err := t.rdb.Get(ctx, LastErrPlatformKey(userId, platform)).Result(); err == nil {
		lastErr = s
	}
	return lastOK, lastFail, lastErr
}

// ClearLastOK 删除用户时清除上次同步标记
func (t *SpiderTask) ClearLastOK(userId int64) {
	if t.rdb == nil || userId <= 0 {
		return
	}
	_ = t.rdb.Del(context.Background(), LastOKKey(userId)).Err()
}

// ResetDedup 清除 pending/inflight，强制允许再次入队。
// 重绑 OJ 时调用：旧任务可能仍占着 pending/inflight，否则 DoPlatform 会被静默跳过，
// 用户已删旧明细却再也等不到新全量同步。
func (t *SpiderTask) ResetDedup(userId int64, platform string) {
	if t.rdb == nil {
		return
	}
	ctx := context.Background()
	_ = t.rdb.Del(ctx, pendingKey(userId, platform), InflightKey(userId, platform)).Err()
}

// BumpGeneration 递增 user+platform 爬取代数。重绑后旧任务写入前应校验代数，避免把已删数据写回。
func (t *SpiderTask) BumpGeneration(userId int64, platform string) (int64, error) {
	platform = strings.TrimSpace(platform)
	if t == nil || t.rdb == nil {
		return 0, fmt.Errorf("bump generation: redis unavailable")
	}
	if userId <= 0 || platform == "" {
		return 0, fmt.Errorf("bump generation: invalid user or platform")
	}
	n, err := t.rdb.Incr(context.Background(), GenerationKey(userId, platform)).Result()
	if err != nil {
		return 0, fmt.Errorf("bump generation user=%d platform=%s: %w", userId, platform, err)
	}
	// 避免 key 永不过期膨胀；绑定活跃用户会持续刷新
	if err := t.rdb.Expire(context.Background(), GenerationKey(userId, platform), 7*24*time.Hour).Err(); err != nil {
		log.Warnf("SpiderTask: expire generation user=%d platform=%s: %v", userId, platform, err)
	}
	return n, nil
}

// GenerationKey 爬取代数 Redis key
func GenerationKey(userId int64, platform string) string {
	return fmt.Sprintf("spider:gen:%d:%s", userId, platform)
}

// CurrentGeneration 读取当前代数（无 key 视为 0）。Redis 故障必须由调用方
// 显式处理，不能与合法的 generation 0 混为一谈。
func CurrentGeneration(ctx context.Context, rdb *redis.Client, userId int64, platform string) (int64, error) {
	if rdb == nil || platform == "" {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	v, err := rdb.Get(ctx, GenerationKey(userId, platform)).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return v, nil
}

// DoBatchPlatform 仅入队指定平台（如 LeetCode 回填）；force 时清 pending/inflight 去重以免刚 update-all 被跳过。
// 一次性全部入队（MQ 可扛）；ctx 取消时提前结束。
func (t *SpiderTask) DoBatchPlatform(ctx context.Context, platform string, needAll, force bool) (users, published int) {
	if ctx == nil {
		ctx = context.Background()
	}
	platform = strings.TrimSpace(platform)
	if platform == "" || t.db == nil {
		return 0, 0
	}
	var userIds []int64
	if err := t.db.Model(&model.Platform{}).
		Where("platform = ?", platform).
		Distinct("user_id").
		Pluck("user_id", &userIds).Error; err != nil {
		log.Errorf("SpiderTask: DoBatchPlatform list users %s: %v", platform, err)
		return 0, 0
	}
	if len(userIds) == 0 {
		return 0, 0
	}
	n := 0
	for i, uid := range userIds {
		select {
		case <-ctx.Done():
			log.Warnf("SpiderTask: DoBatchPlatform cancelled at %d/%d", i, len(userIds))
			return len(userIds), n
		default:
		}
		if force {
			t.ResetDedup(uid, platform)
		}
		n += t.DoPlatform(uid, platform, needAll).Published
	}
	log.Infof("SpiderTask: DoBatchPlatform platform=%s users=%d published=%d needAll=%v force=%v",
		platform, len(userIds), n, needAll, force)
	return len(userIds), n
}

// DoBatch 为给定用户的每个绑定平台各入队一条消息（一次 Publish = 一个平台）。
// batchSize：每批平台任务数（≤0 默认 30）；interval：批间休眠（≤0 默认 200ms），削峰防 MQ/DB 尖刺。
// ctx 取消时提前结束（进程停机）。
func (t *SpiderTask) DoBatch(ctx context.Context, userIds []int64, needAll bool, batchSize int, interval time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(userIds) == 0 {
		return
	}
	if batchSize <= 0 {
		batchSize = 30
	}
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	// 一次查出所有绑定，避免 per-user 查库
	type bind struct {
		UserID   int64  `gorm:"column:user_id"`
		Platform string `gorm:"column:platform"`
	}
	var binds []bind
	if t.db != nil {
		q := t.db.Model(&model.Platform{}).Select("user_id, platform")
		if len(userIds) == 1 {
			q = q.Where("user_id = ?", userIds[0])
		} else {
			q = q.Where("user_id IN ?", userIds)
		}
		if err := q.Find(&binds).Error; err != nil {
			log.Errorf("SpiderTask: DoBatch list platforms: %v", err)
			// 回退 per-user
			n := 0
			for i, uid := range userIds {
				select {
				case <-ctx.Done():
					log.Warnf("SpiderTask: DoBatch cancelled at user %d/%d", i, len(userIds))
					return
				default:
				}
				n += t.Do(uid, needAll).Published
				if (i+1)%batchSize == 0 && i+1 < len(userIds) {
					select {
					case <-ctx.Done():
						return
					case <-time.After(interval):
					}
				}
			}
			log.Infof("SpiderTask: DoBatch (fallback) published=%d users=%d needAll=%v", n, len(userIds), needAll)
			return
		}
	}
	published := 0
	for i, b := range binds {
		select {
		case <-ctx.Done():
			log.Warnf("SpiderTask: DoBatch cancelled at bind %d/%d", i, len(binds))
			return
		default:
		}
		if b.Platform == "" {
			continue
		}
		published += t.DoPlatform(b.UserID, b.Platform, needAll).Published
		if (i+1)%batchSize == 0 && i+1 < len(binds) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}
	log.Infof("SpiderTask: DoBatch published=%d platform jobs for %d users needAll=%v batch=%d interval=%v",
		published, len(userIds), needAll, batchSize, interval)
}

// pending 评测重爬：与 biz/service 共用 Redis key（进程重启后 cron 仍可扫）。
const (
	pendingVerdictDueZKey     = "spider:pending_retry_due"
	pendingVerdictSchedulePfx = "spider:pending_retry:"
)

// DrainPendingVerdictRetries 处理 ZSET 中已到期的 pending 重爬；返回入队次数。
func (t *SpiderTask) DrainPendingVerdictRetries(limit int64) int {
	if t == nil || t.rdb == nil {
		return 0
	}
	if limit <= 0 {
		limit = 50
	}
	ctx := context.Background()
	now := float64(time.Now().Unix())
	members, err := t.rdb.ZRangeByScore(ctx, pendingVerdictDueZKey, &redis.ZRangeBy{
		Min: "-inf", Max: fmt.Sprintf("%f", now), Offset: 0, Count: limit,
	}).Result()
	if err != nil || len(members) == 0 {
		return 0
	}
	n := 0
	for _, m := range members {
		removed, rerr := t.rdb.ZRem(ctx, pendingVerdictDueZKey, m).Result()
		if rerr != nil || removed == 0 {
			continue
		}
		i := strings.IndexByte(m, ':')
		if i <= 0 || i+1 >= len(m) {
			continue
		}
		var uid int64
		if _, err := fmt.Sscanf(m[:i], "%d", &uid); err != nil || uid <= 0 {
			continue
		}
		plat := m[i+1:]
		if plat == "" {
			continue
		}
		_ = t.rdb.Del(ctx, fmt.Sprintf("%s%d:%s", pendingVerdictSchedulePfx, uid, plat)).Err()
		res := t.DoPlatform(uid, plat, false)
		log.Infof("SpiderTask: pending-verdict retry enqueue user=%d platform=%s published=%d deduped=%d failed=%d",
			uid, plat, res.Published, res.Deduped, res.Failed)
		n++
	}
	return n
}
