package opsmetrics

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const ttl = 72 * time.Hour
const mauTTL = 40 * 24 * time.Hour

// inflightTTL 并发计数键的 TTL：随每次 Incr 续期。进程异常退出导致的
// 计数残留最多 10 分钟自愈，不再永久漂移。
const inflightTTL = 10 * time.Minute

// recordAPIScript 打点合并为单次 Lua：req 计数 + 服务计数 + inflight（带 TTL）+ 峰值，
// 避免原先 pipeline + GET/GET/SET 多次串行往返。
// KEYS: 1=reqKey 2=svcKey 3=inflightKey 4=peakKey
// ARGV: 1=ttl 秒 2=inflight ttl 秒 3=是否写服务计数("1"/"0")
var recordAPIScript = redis.NewScript(`
redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[1])
if ARGV[3] == '1' then
  redis.call('INCR', KEYS[2])
  redis.call('EXPIRE', KEYS[2], ARGV[1])
end
local cur = redis.call('INCR', KEYS[3])
redis.call('EXPIRE', KEYS[3], ARGV[2])
local peak = tonumber(redis.call('GET', KEYS[4]) or '0')
if cur > peak then
  redis.call('SET', KEYS[4], cur, 'EX', ARGV[1])
end
return cur
`)

var loc *time.Location

func init() {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		l = time.FixedZone("CST", 8*3600)
	}
	loc = l
}

func dayKey(t time.Time) string {
	return t.In(loc).Format("20060102")
}

func monthKey(t time.Time) string {
	return t.In(loc).Format("200601")
}

// RecordAPIRequest 记录一次 API 请求，并维护并发峰值
func RecordAPIRequest(ctx context.Context, rdb *redis.Client, service string) func() {
	if rdb == nil {
		return func() {}
	}
	day := dayKey(time.Now())
	reqKey := fmt.Sprintf("ops:api:req:%s", day)
	svcKey := fmt.Sprintf("ops:api:req:%s:%s", day, service)
	inflightKey := "ops:api:inflight"
	peakKey := fmt.Sprintf("ops:api:peak:%s", day)

	// 单次 Lua 完成全部打点（含峰值），减少串行往返
	hasSvc := "1"
	if service == "" {
		hasSvc = "0"
		svcKey = reqKey // 占位：脚本在 ARGV[3]=0 时不会碰 KEYS[2]
	}
	_, _ = recordAPIScript.Run(ctx, rdb,
		[]string{reqKey, svcKey, inflightKey, peakKey},
		int(ttl/time.Second), int(inflightTTL/time.Second), hasSvc,
	).Result()

	return func() {
		// 客户端取消不应导致计数漂移：用独立短超时 ctx 落 Decr
		dctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		n, err := rdb.Decr(dctx, inflightKey).Result()
		if err == nil && n < 0 {
			_ = rdb.Set(dctx, inflightKey, 0, inflightTTL).Err()
		}
	}
}

// IncSpider 爬虫日计数
func IncSpider(ctx context.Context, rdb *redis.Client, kind string, n int64) {
	if rdb == nil || n == 0 {
		return
	}
	day := dayKey(time.Now())
	key := fmt.Sprintf("ops:spider:%s:%s", kind, day)
	pipe := rdb.Pipeline()
	pipe.IncrBy(ctx, key, n)
	pipe.Expire(ctx, key, ttl)
	_, _ = pipe.Exec(ctx)
}

// TouchMAU 登录用户写入月活集合
func TouchMAU(ctx context.Context, rdb *redis.Client, userID uint) {
	if rdb == nil || userID == 0 {
		return
	}
	key := fmt.Sprintf("visit:mau:%s", monthKey(time.Now()))
	pipe := rdb.Pipeline()
	pipe.SAdd(ctx, key, strconv.FormatUint(uint64(userID), 10))
	pipe.Expire(ctx, key, mauTTL)
	_, _ = pipe.Exec(ctx)
}

// Snapshot 读取运维日指标
type Snapshot struct {
	APIRequestsToday int64
	APIPeakToday     int64
	APIInflight      int64
	SpiderEnqueued   int64
	SpiderOK         int64
	SpiderFail       int64
	SpiderRows       int64
	MAU              int64
}

func ReadSnapshot(ctx context.Context, rdb *redis.Client) Snapshot {
	var s Snapshot
	if rdb == nil {
		return s
	}
	day := dayKey(time.Now())
	month := monthKey(time.Now())
	if v, err := rdb.Get(ctx, "ops:api:req:"+day).Int64(); err == nil {
		s.APIRequestsToday = v
	}
	if v, err := rdb.Get(ctx, "ops:api:peak:"+day).Int64(); err == nil {
		s.APIPeakToday = v
	}
	if v, err := rdb.Get(ctx, "ops:api:inflight").Int64(); err == nil && v > 0 {
		s.APIInflight = v
	}
	if v, err := rdb.Get(ctx, "ops:spider:enqueued:"+day).Int64(); err == nil {
		s.SpiderEnqueued = v
	}
	if v, err := rdb.Get(ctx, "ops:spider:ok:"+day).Int64(); err == nil {
		s.SpiderOK = v
	}
	if v, err := rdb.Get(ctx, "ops:spider:fail:"+day).Int64(); err == nil {
		s.SpiderFail = v
	}
	if v, err := rdb.Get(ctx, "ops:spider:rows:"+day).Int64(); err == nil {
		s.SpiderRows = v
	}
	if v, err := rdb.SCard(ctx, "visit:mau:"+month).Result(); err == nil {
		s.MAU = v
	}
	return s
}
