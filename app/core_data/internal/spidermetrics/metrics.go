package spidermetrics

import (
	"context"
	"sync/atomic"
	"time"

	"cwxu-algo/app/common/opsmetrics"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
)

// Metrics 爬虫可观测计数（进程内；多实例各自统计）
type Metrics struct {
	Enqueued      atomic.Int64
	DedupSkipped  atomic.Int64
	Started       atomic.Int64
	Succeeded     atomic.Int64
	Failed        atomic.Int64
	FullJobs      atomic.Int64
	Incremental   atomic.Int64
	TotalDuration atomic.Int64 // 成功任务耗时毫秒累计
}

var global = &Metrics{}
var rdb *redis.Client

// BindRedis 绑定共享 Redis，写入日桶供站管访问统计读取
func BindRedis(client *redis.Client) { rdb = client }

func Snapshot() *Metrics { return global }

func IncEnqueued(platform string) {
	global.Enqueued.Add(1)
	opsmetrics.IncSpider(context.Background(), rdb, platform, "enqueued", 1)
}
func IncDedupSkipped() { global.DedupSkipped.Add(1) }

// IncRows 今日新写入提交记录条数
func IncRows(platform string, n int64) {
	if n > 0 {
		opsmetrics.IncSpider(context.Background(), rdb, platform, "rows", n)
	}
}

func (m *Metrics) logIfNeeded() {
	n := m.Started.Load()
	if n > 0 && n%20 == 0 {
		succ := m.Succeeded.Load()
		avgMs := int64(0)
		if succ > 0 {
			avgMs = m.TotalDuration.Load() / succ
		}
		log.Infof(
			"spider_metrics enqueued=%d dedup_skip=%d started=%d ok=%d fail=%d full=%d incr=%d avg_ms=%d",
			m.Enqueued.Load(),
			m.DedupSkipped.Load(),
			n,
			succ,
			m.Failed.Load(),
			m.FullJobs.Load(),
			m.Incremental.Load(),
			avgMs,
		)
	}
}

func RecordStart(platform string, needAll bool) time.Time {
	global.Started.Add(1)
	if needAll {
		global.FullJobs.Add(1)
	} else {
		global.Incremental.Add(1)
	}
	global.logIfNeeded()
	return time.Now()
}

func RecordEnd(platform string, start time.Time, err error) {
	if err != nil {
		global.Failed.Add(1)
		opsmetrics.IncSpider(context.Background(), rdb, platform, "fail", 1)
		return
	}
	global.Succeeded.Add(1)
	global.TotalDuration.Add(time.Since(start).Milliseconds())
	opsmetrics.IncSpider(context.Background(), rdb, platform, "ok", 1)
}
