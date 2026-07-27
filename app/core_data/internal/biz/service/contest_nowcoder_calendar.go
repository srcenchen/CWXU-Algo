package service

import (
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
	calspider "cwxu-algo/app/core_data/internal/spider/calendar"
	"cwxu-algo/app/core_data/internal/spider/platform"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// UpsertNowCoderContestCalendar 将官方起止写入 contest_calendars（供展示/Infer）。
// start/end 必须来自牛客实时数据（参赛历史或比赛页），禁止用默认 3h 写入。
func UpsertNowCoderContestCalendar(db *gorm.DB, contestID, name, url string, startSec, endSec int64) error {
	if db == nil || contestID == "" || startSec <= 0 || endSec <= startSec {
		return nil
	}
	// 最低合法性：end>start 已在上方校验。爬多少用多少（含 12h+ 长赛），不再砍赛长。
	if url == "" {
		url = "https://ac.nowcoder.com/acm/contest/" + contestID
	}
	if name == "" {
		name = "NowCoder " + contestID
	}
	_, err := dal.NewContestCalendarDalDB(db).UpsertItems([]calspider.Item{{
		Platform:     spider.NowCoder,
		PlatformName: "牛客",
		ExternalID:   strings.TrimSpace(contestID),
		Name:         name,
		URL:          url,
		StartTime:    startSec,
		EndTime:      endSec,
		Source:       model.CalSourceNowCoder,
	}})
	if err == nil {
		// 展示窗 memo 里可能还是兜底值，写入真实赛长后立刻作废
		ForgetContestDisplayWindow(spider.NowCoder, contestID)
	}
	return err
}

// nowCoderHasOfficialCalendar 是否已有牛客官方源时间窗（实时爬取写入）。
func nowCoderHasOfficialCalendar(db *gorm.DB, contestID string) bool {
	cal, found := lookupContestCalendar(db, spider.NowCoder, contestID)
	if !found || cal.StartTime <= 0 || cal.EndTime <= cal.StartTime {
		return false
	}
	return cal.Source == model.CalSourceNowCoder
}

// nowCoderHasAnyValidCalendar 任意来源的有效时间窗（避免无意义重复打页）。
func nowCoderHasAnyValidCalendar(db *gorm.DB, contestID string) bool {
	cal, found := lookupContestCalendar(db, spider.NowCoder, contestID)
	return found && cal.StartTime > 0 && cal.EndTime > cal.StartTime
}

// EnsureNowCoderContestCalendar 确保日历有官方起止（实时）：
//  1. 已有 source=nowcoder → 直接用
//  2. hint 带来参赛历史 start+end → 写入日历（各场真实赛长 2h/3h/4h/5h…，覆盖其它源）
//  3. 仍无有效日历 → 抓比赛页 HTML 内嵌 startTime/endTime
//  4. 拉页失败 → 不写假固定时长进日历；返回 false
//
// 返回展示用 [start,end]（无赛后缓冲）。ok=true 表示得到实时/已入库的有效窗。
func EnsureNowCoderContestCalendar(db *gorm.DB, contestID, name, url string, hintStart, hintEnd time.Time) (start, end time.Time, ok bool) {
	contestID = strings.TrimSpace(contestID)
	if contestID == "" {
		return time.Time{}, time.Time{}, false
	}

	// 参赛历史 end 优先：每次爬取都能用真实赛长覆盖
	if !hintStart.IsZero() && hintEnd.After(hintStart) {
		if err := UpsertNowCoderContestCalendar(db, contestID, name, url, hintStart.Unix(), hintEnd.Unix()); err != nil {
			log.Warnf("EnsureNowCoderContestCalendar upsert history %s: %v", contestID, err)
		} else {
			return hintStart, hintEnd, true
		}
	}

	if nowCoderHasOfficialCalendar(db, contestID) {
		if cal, found := lookupContestCalendar(db, spider.NowCoder, contestID); found {
			return time.Unix(cal.StartTime, 0), time.Unix(cal.EndTime, 0), true
		}
	}

	// 已有其它源有效窗且无 history end：不强制打页（列表/Infer 热路径）
	// 爬虫 Pass2 在无日历时仍会抓页
	if nowCoderHasAnyValidCalendar(db, contestID) && hintEnd.IsZero() {
		if cal, found := lookupContestCalendar(db, spider.NowCoder, contestID); found {
			return time.Unix(cal.StartTime, 0), time.Unix(cal.EndTime, 0), true
		}
	}

	// 比赛页官方 JSON（startTime/endTime 毫秒）——真实赛长
	s, e, pageName, err := platform.FetchNowCoderContestTimes(contestID)
	if err != nil {
		log.Warnf("EnsureNowCoderContestCalendar fetch %s: %v", contestID, err)
		return time.Time{}, time.Time{}, false
	}
	if name == "" {
		name = pageName
	}
	if err := UpsertNowCoderContestCalendar(db, contestID, name, url, s, e); err != nil {
		log.Warnf("EnsureNowCoderContestCalendar upsert page %s: %v", contestID, err)
	}
	return time.Unix(s, 0), time.Unix(e, 0), true
}

// ensureNowCoderCalendarsFromContestLogs 爬完参赛历史后写入官方时间窗。
//
// 策略（保证以后正常）：
//   - 有 history endTime 的场次：全部写入，不限 cap（零额外 HTTP，真实赛长）
//   - 缺 endTime 的场次：抓比赛页，pageCap 限制避免打爆（默认 30）
func ensureNowCoderCalendarsFromContestLogs(db *gorm.DB, logs []model.ContestLog, pageCap int) {
	if db == nil || len(logs) == 0 {
		return
	}
	if pageCap <= 0 {
		pageCap = 30
	}
	seen := map[string]struct{}{}
	pageN := 0

	// Pass 1：历史自带 end → 全量 upsert（2h/3h/4h/5h 各场真实值）
	for _, cl := range logs {
		if NormalizeCalendarPlatform(cl.Platform) != spider.NowCoder {
			continue
		}
		cid := strings.TrimSpace(cl.ContestId)
		if cid == "" {
			continue
		}
		if _, ok := seen[cid]; ok {
			continue
		}
		if cl.Time.IsZero() || !cl.EndTime.After(cl.Time) {
			continue
		}
		seen[cid] = struct{}{}
		// history 有真实 end：始终 upsert（覆盖旧默认/错窗）
		if err := UpsertNowCoderContestCalendar(db, cid, cl.ContestName, cl.ContestUrl, cl.Time.Unix(), cl.EndTime.Unix()); err != nil {
			log.Warnf("nowcoder calendar history upsert %s: %v", cid, err)
		}
	}

	// Pass 2：仍无任何有效日历 → 抓比赛页拿实时 start/end
	seenPage := map[string]struct{}{}
	for _, cl := range logs {
		if pageN >= pageCap {
			break
		}
		if NormalizeCalendarPlatform(cl.Platform) != spider.NowCoder {
			continue
		}
		cid := strings.TrimSpace(cl.ContestId)
		if cid == "" {
			continue
		}
		if _, ok := seenPage[cid]; ok {
			continue
		}
		seenPage[cid] = struct{}{}
		if nowCoderHasAnyValidCalendar(db, cid) {
			continue
		}
		// 无 history end 时 Ensure 会打比赛页
		_, _, ok := EnsureNowCoderContestCalendar(db, cid, cl.ContestName, cl.ContestUrl, cl.Time, cl.EndTime)
		if ok {
			pageN++
		}
	}
}
