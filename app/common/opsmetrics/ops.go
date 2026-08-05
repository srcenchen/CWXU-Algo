package opsmetrics

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
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

// RecordAPIRequest 记录一次 API 请求，并维护并发峰值。
// 返回的 done 需在请求结束时调用，传入耗时；内部完成延迟样本 + 并发释放。
func RecordAPIRequest(ctx context.Context, rdb *redis.Client, service string) func(dur time.Duration) {
	if rdb == nil {
		return func(time.Duration) {}
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

	return func(dur time.Duration) {
		dctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if dur > 0 {
			ms := int64(dur / time.Millisecond)
			if ms < 0 {
				ms = 0
			}
			latKey := fmt.Sprintf("ops:api:lat:%s", day)
			// 延迟样本：保留最近 2000 条（LIST），用 LRANGE 求 p50/p95/p99
			_ = rdb.RPush(dctx, latKey, ms).Err()
			_ = rdb.LTrim(dctx, latKey, -2000, -1).Err()
			_ = rdb.Expire(dctx, latKey, ttl).Err()
		}
		// 客户端取消不应导致计数漂移：用独立短超时 ctx 落 Decr
		n, err := rdb.Decr(dctx, inflightKey).Result()
		if err == nil && n < 0 {
			_ = rdb.Set(dctx, inflightKey, 0, inflightTTL).Err()
		}
	}
}

// IncSpider 爬虫日计数（platform 非空时同时写入按 OJ 分桶，兼容全局聚合）
func IncSpider(ctx context.Context, rdb *redis.Client, platform, kind string, n int64) {
	if rdb == nil || n == 0 {
		return
	}
	day := dayKey(time.Now())
	keys := []string{fmt.Sprintf("ops:spider:%s:%s", kind, day)}
	if strings.TrimSpace(platform) != "" {
		keys = append(keys, fmt.Sprintf("ops:spider:%s:%s:%s", platform, kind, day))
	}
	pipe := rdb.Pipeline()
	for _, key := range keys {
		pipe.IncrBy(ctx, key, n)
		pipe.Expire(ctx, key, ttl)
	}
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

// SpiderPlatformToday 单个 OJ 今日爬虫计数
type SpiderPlatformToday struct {
	Enqueued int64
	OK       int64
	Fail     int64
	Rows     int64
}

// ReadSpiderPlatformToday 读取某 OJ 今日爬虫计数（enqueued/ok/fail/rows）
func ReadSpiderPlatformToday(ctx context.Context, rdb *redis.Client, platform string) SpiderPlatformToday {
	var out SpiderPlatformToday
	if rdb == nil || strings.TrimSpace(platform) == "" {
		return out
	}
	day := dayKey(time.Now())
	cmds := map[string]*redis.StringCmd{}
	pipe := rdb.Pipeline()
	for _, kind := range []string{"enqueued", "ok", "fail", "rows"} {
		cmds[kind] = pipe.Get(ctx, fmt.Sprintf("ops:spider:%s:%s:%s", platform, kind, day))
	}
	_, _ = pipe.Exec(ctx)
	if v, err := cmds["enqueued"].Int64(); err == nil {
		out.Enqueued = v
	}
	if v, err := cmds["ok"].Int64(); err == nil {
		out.OK = v
	}
	if v, err := cmds["fail"].Int64(); err == nil {
		out.Fail = v
	}
	if v, err := cmds["rows"].Int64(); err == nil {
		out.Rows = v
	}
	return out
}

// ReadSnapshot 读取运维日指标
type Snapshot struct {
	APIRequestsToday int64
	APIPeakToday     int64
	APIInflight      int64
	SpiderEnqueued   int64
	SpiderOK         int64
	SpiderFail       int64
	SpiderRows       int64
	MAU              int64
	// API 延迟（ms）：avg / p50 / p95 / p99，来自当日最近样本
	APILatencyAvg int64
	APILatencyP50 int64
	APILatencyP95 int64
	APILatencyP99 int64
}

func percentile(sorted []int64, p float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	idx := int(float64(n-1)*p + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
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
	// 延迟样本
	if samples, err := rdb.LRange(ctx, "ops:api:lat:"+day, 0, -1).Result(); err == nil && len(samples) > 0 {
		vals := make([]int64, 0, len(samples))
		for _, raw := range samples {
			var v int64
			for _, c := range raw {
				if c < '0' || c > '9' {
					v = 0
					break
				}
				v = v*10 + int64(c-'0')
			}
			if v >= 0 {
				vals = append(vals, v)
			}
		}
		if len(vals) > 0 {
			var sum int64
			for _, v := range vals {
				sum += v
			}
			s.APILatencyAvg = sum / int64(len(vals))
			sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
			s.APILatencyP50 = percentile(vals, 0.5)
			s.APILatencyP95 = percentile(vals, 0.95)
			s.APILatencyP99 = percentile(vals, 0.99)
		}
	}
	return s
}
