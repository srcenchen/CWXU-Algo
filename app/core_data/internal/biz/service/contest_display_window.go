package service

import (
	"context"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// 比赛列表/详情等读路径「绝不同步打 OJ」的展示时间解析。
//
// 背景：ResolveContestDisplayWindow 在缺日历时会实时抓牛客比赛页（ojhttp 超时 30s），
// 而 /core/contest/list 每页最多可能有 8 场缺日历的牛客赛，串行抓页会让单次请求
// 挂到几十秒甚至分钟级——前端 30s 超时报「请求未完成」，同时把网关 /v1/core/*
// 的熔断器（10s 窗口成功率 < 60%）拖到 503，表现就是「时不时抽风」。
//
// 这里改成：读路径只查库（日历 + hint 兜底），缺官方窗的牛客场次丢给后台限流补，
// 补到后清掉 memo，下一次请求就是真实赛长。

const (
	// contestWindowMemoTTL 解析结果进程内缓存（比赛起止基本不变）
	contestWindowMemoTTL = 10 * time.Minute
	// contestWindowMemoNegTTL 未解析出窗口的负缓存（短一些，等后台补完就能翻新）
	contestWindowMemoNegTTL = 2 * time.Minute
	// contestWindowMemoMax 上限，超了整体清空（简单够用）
	contestWindowMemoMax = 4096
)

type contestWindowMemoEntry struct {
	start int64
	end   int64
	ok    bool
	at    time.Time
}

var (
	contestWindowMemoMu sync.Mutex
	contestWindowMemo   = map[string]contestWindowMemoEntry{}
)

func contestWindowMemoKey(platform, contestID string) string {
	return NormalizeCalendarPlatform(platform) + "\x00" + strings.TrimSpace(contestID)
}

func loadContestWindowMemo(platform, contestID string) (contestWindowMemoEntry, bool) {
	key := contestWindowMemoKey(platform, contestID)
	contestWindowMemoMu.Lock()
	defer contestWindowMemoMu.Unlock()
	e, ok := contestWindowMemo[key]
	if !ok {
		return contestWindowMemoEntry{}, false
	}
	ttl := contestWindowMemoTTL
	if !e.ok {
		ttl = contestWindowMemoNegTTL
	}
	if time.Since(e.at) >= ttl {
		delete(contestWindowMemo, key)
		return contestWindowMemoEntry{}, false
	}
	return e, true
}

func storeContestWindowMemo(platform, contestID string, start, end time.Time, ok bool) {
	key := contestWindowMemoKey(platform, contestID)
	e := contestWindowMemoEntry{ok: ok, at: time.Now()}
	if ok {
		e.start, e.end = start.Unix(), end.Unix()
	}
	contestWindowMemoMu.Lock()
	defer contestWindowMemoMu.Unlock()
	if len(contestWindowMemo) >= contestWindowMemoMax {
		contestWindowMemo = map[string]contestWindowMemoEntry{}
	}
	contestWindowMemo[key] = e
}

// ForgetContestDisplayWindow 日历被更新后清 memo（后台补窗成功 / 爬虫写入官方赛长）。
func ForgetContestDisplayWindow(platform, contestID string) {
	key := contestWindowMemoKey(platform, contestID)
	contestWindowMemoMu.Lock()
	delete(contestWindowMemo, key)
	contestWindowMemoMu.Unlock()
}

// ResolveContestDisplayWindowCached 读路径用的展示窗解析：只查库，不打 OJ。
// 牛客缺官方窗时排队后台补，本次先返回日历/hint 兜底窗。
func ResolveContestDisplayWindowCached(db *gorm.DB, platform, contestID string, hintTime time.Time) (start, end time.Time, ok bool) {
	if e, hit := loadContestWindowMemo(platform, contestID); hit {
		if !e.ok {
			return time.Time{}, time.Time{}, false
		}
		return time.Unix(e.start, 0), time.Unix(e.end, 0), true
	}
	start, end, ok = resolveContestDisplayWindow(db, platform, contestID, hintTime, false)
	storeContestWindowMemo(platform, contestID, start, end, ok)
	return start, end, ok
}

// ResolveContestWindowCached 读路径用的扫题时间窗（end 含赛后缓冲）：只查库，不打 OJ。
func ResolveContestWindowCached(db *gorm.DB, platform, contestID string, hintTime time.Time) (start, end time.Time) {
	return resolveContestWindow(db, platform, contestID, hintTime, false)
}

// --- 牛客日历后台补窗 ---

const (
	// ncBackfillRetryTTL 同一场两次尝试的最小间隔（失败也算，作负缓存防打爆牛客）
	ncBackfillRetryTTL = 30 * time.Minute
	// ncBackfillConcurrency 后台抓页并发上限
	ncBackfillConcurrency = 2
	// ncBackfillPerCall 单次请求最多排队多少场
	ncBackfillPerCall = 8
	// ncBackfillAttemptMax 尝试记录表上限，超了整体清空
	ncBackfillAttemptMax = 4096
)

var (
	ncBackfillMu       sync.Mutex
	ncBackfillAttempt  = map[string]time.Time{}
	ncBackfillInflight = map[string]struct{}{}
	ncBackfillSlots    = make(chan struct{}, ncBackfillConcurrency)
	// ncBackfillEnabled 单测里关掉，避免真的去打牛客
	ncBackfillEnabled = true
)

// ncBackfillClaim 取得某场的补窗资格（去重 + 负缓存节流）。
func ncBackfillClaim(contestID string) bool {
	ncBackfillMu.Lock()
	defer ncBackfillMu.Unlock()
	if _, running := ncBackfillInflight[contestID]; running {
		return false
	}
	if at, ok := ncBackfillAttempt[contestID]; ok && time.Since(at) < ncBackfillRetryTTL {
		return false
	}
	if len(ncBackfillAttempt) >= ncBackfillAttemptMax {
		ncBackfillAttempt = map[string]time.Time{}
	}
	ncBackfillAttempt[contestID] = time.Now()
	ncBackfillInflight[contestID] = struct{}{}
	return true
}

func ncBackfillRelease(contestID string) {
	ncBackfillMu.Lock()
	delete(ncBackfillInflight, contestID)
	ncBackfillMu.Unlock()
}

// scheduleNowCoderCalendarBackfill 后台补牛客官方起止（读路径调用，绝不阻塞）。
// 只该对「已确认缺有效日历」的场次调用；成功后清 memo 让下次请求拿到真实赛长。
func scheduleNowCoderCalendarBackfill(db *gorm.DB, logs []model.ContestLog) {
	if db == nil || len(logs) == 0 || !ncBackfillEnabled {
		return
	}
	// 调用方常传请求 ctx 绑定的 db；后台补窗必须脱离请求生命周期，否则请求一结束就被取消
	db = db.WithContext(context.Background())
	queued := 0
	seen := map[string]struct{}{}
	for _, cl := range logs {
		if queued >= ncBackfillPerCall {
			return
		}
		if NormalizeCalendarPlatform(cl.Platform) != spider.NowCoder {
			continue
		}
		cid := strings.TrimSpace(cl.ContestId)
		if cid == "" {
			continue
		}
		if _, dup := seen[cid]; dup {
			continue
		}
		seen[cid] = struct{}{}
		if !ncBackfillClaim(cid) {
			continue
		}
		queued++
		name, url, hintStart, hintEnd := cl.ContestName, cl.ContestUrl, cl.Time, cl.EndTime
		go func(cid, name, url string, hintStart, hintEnd time.Time) {
			defer ncBackfillRelease(cid)
			ncBackfillSlots <- struct{}{}
			defer func() { <-ncBackfillSlots }()
			// 排队期间可能已被别的路径补上
			if nowCoderHasAnyValidCalendar(db, cid) && hintEnd.IsZero() {
				ForgetContestDisplayWindow(spider.NowCoder, cid)
				return
			}
			if name == "" || url == "" || hintStart.IsZero() {
				n, u, e := nowCoderHintsFromLogs(db, cid, &hintStart)
				if name == "" {
					name = n
				}
				if url == "" {
					url = u
				}
				if hintEnd.IsZero() {
					hintEnd = e
				}
			}
			if _, _, ok := EnsureNowCoderContestCalendar(db, cid, name, url, hintStart, hintEnd); ok {
				ForgetContestDisplayWindow(spider.NowCoder, cid)
			} else {
				log.Warnf("nowcoder calendar backfill miss %s", cid)
			}
		}(cid, name, url, hintStart, hintEnd)
	}
}
