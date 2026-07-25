package service

import (
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 平台默认赛长（无日历/官方 duration 时用）；宁可略宽避免漏 AC。
// 牛客赛长不固定（2h/3h/4h/5h…），正常路径必须用参赛历史/比赛页实时 start+end，
// 写入 contest_calendars 后展示与 Infer 都走真实赛长；此处仅作拉官方失败时的最后兜底。
var defaultContestDuration = map[string]time.Duration{
	spider.CodeForces: 2 * time.Hour,
	spider.AtCoder:    100 * time.Minute,
	spider.NowCoder:   3 * time.Hour, // 仅兜底，禁止当真实赛长展示；见 EnsureNowCoderContestCalendar
	spider.LeetCode:   90 * time.Minute,
	spider.LuoGu:      3 * time.Hour,
	spider.QOJ:        5 * time.Hour,
}

const (
	// contestInferEndBuffer 赛后缓冲：排队/延迟交题
	contestInferEndBuffer = 15 * time.Minute
	// contestInferSubmitLimit 单次反推最多扫提交条数
	contestInferSubmitLimit = 8000
	// contestUpsolveMaxHorizon 补题回填：赛后最多扫多久（避免全历史）
	contestUpsolveMaxHorizon = 30 * 24 * time.Hour
)

// calendarPlatformAliases 赛程表 platform 与爬虫 platform 不一致（cpolar 小写）。
// 查询/归一时同时匹配这些别名。
func calendarPlatformAliases(platform string) []string {
	platform = strings.TrimSpace(platform)
	if platform == "" {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	add(platform)
	add(strings.ToLower(platform))
	switch strings.ToLower(platform) {
	case "atcoder":
		add(spider.AtCoder)
		add("atcoder")
	case "codeforces", "cf":
		add(spider.CodeForces)
		add("Codeforces")
		add("codeforces")
	case "nowcoder", "牛客":
		add(spider.NowCoder)
		add("nowcoder")
	case "leetcode", "力扣":
		add(spider.LeetCode)
		add("leetcode")
	case "luogu", "洛谷":
		add(spider.LuoGu)
		add("luogu")
	case "qoj":
		add(spider.QOJ)
		add("qoj")
	}
	return out
}

// NormalizeCalendarPlatform 将 cpolar/leetcode 源的 platform 规范为爬虫侧常量。
func NormalizeCalendarPlatform(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "atcoder":
		return spider.AtCoder
	case "codeforces", "cf":
		return spider.CodeForces
	case "nowcoder":
		return spider.NowCoder
	case "leetcode":
		return spider.LeetCode
	case "luogu":
		return spider.LuoGu
	case "qoj":
		return spider.QOJ
	default:
		return strings.TrimSpace(raw)
	}
}

// lookupContestCalendar 按 platform+external_id（及模糊）查赛程日历。
// platform 大小写/别名不敏感（日历表常为 atcoder，参赛记录为 AtCoder）。
func lookupContestCalendar(db *gorm.DB, platform, contestID string) (model.ContestCalendar, bool) {
	var cal model.ContestCalendar
	if db == nil || contestID == "" {
		return cal, false
	}
	contestID = strings.TrimSpace(contestID)
	plats := calendarPlatformAliases(platform)
	if len(plats) == 0 {
		return cal, false
	}
	err := db.Where("platform IN ? AND external_id = ?", plats, contestID).First(&cal).Error
	if err != nil {
		_ = db.Where("platform IN ? AND (external_id LIKE ? OR url LIKE ?)",
			plats, "%"+contestID+"%", "%"+contestID+"%").
			Order("start_time DESC").First(&cal).Error
	}
	if cal.ID > 0 && cal.StartTime > 0 {
		return cal, true
	}
	return cal, false
}

// minContestFirstAC 本场最早首次 AC（用于校验日历/时间窗是否偏晚）。
func minContestFirstAC(db *gorm.DB, platform, contestID string) (time.Time, bool) {
	if db == nil || contestID == "" {
		return time.Time{}, false
	}
	plats := calendarPlatformAliases(platform)
	var t *time.Time
	err := db.Model(&model.ContestUserProblem{}).
		Select("MIN(first_ac_at)").
		Where("platform IN ? AND contest_id = ? AND first_ac_at IS NOT NULL", plats, contestID).
		Scan(&t).Error
	if err != nil || t == nil || t.IsZero() {
		return time.Time{}, false
	}
	return *t, true
}

// calendarWindowValid 日历窗最低合法性：有开赛且 end>start。爬到多少用多少，不做赛长/最早 AC 二次否决。
func calendarWindowValid(start, end time.Time) bool {
	return !start.IsZero() && end.After(start)
}

// platformDuration 平台默认赛长。
func platformDuration(platform string) time.Duration {
	dur := defaultContestDuration[strings.TrimSpace(platform)]
	if dur <= 0 {
		// 小写日历名
		dur = defaultContestDuration[NormalizeCalendarPlatform(platform)]
	}
	if dur <= 0 {
		return 5 * time.Hour
	}
	return dur
}

// hintAsContestEnd AtCoder history 的 EndTime；CF rating 结算在赛后。
func hintAsContestEnd(platform string) bool {
	switch NormalizeCalendarPlatform(platform) {
	case spider.AtCoder:
		return true
	default:
		return false
	}
}

// windowFromHint 按平台语义解释 contest_logs.time 等 hint。
func windowFromHint(platform string, hintTime time.Time, withEndBuffer bool) (start, end time.Time) {
	if hintTime.IsZero() {
		return time.Time{}, time.Time{}
	}
	dur := platformDuration(platform)
	plat := NormalizeCalendarPlatform(platform)
	switch {
	case plat == spider.AtCoder || hintAsContestEnd(platform):
		// history JSON EndTime → 结束；向前默认赛长
		end = hintTime
		start = hintTime.Add(-dur)
	case plat == spider.CodeForces || plat == "Codeforces":
		// rating 结算偏晚：向前多看一段
		start = hintTime.Add(-(dur + 3*time.Hour))
		end = hintTime
	default:
		start = hintTime
		end = hintTime.Add(dur)
	}
	if withEndBuffer {
		end = end.Add(contestInferEndBuffer)
	}
	return start, end
}

// ResolveContestDisplayWindow 给人看的起止时间（无赛后缓冲）。
// 1) 日历 2) 牛客官方实时 start/end 3) hint/粗估。
// 返回的 ok 表示至少有可信开赛时间。
func ResolveContestDisplayWindow(db *gorm.DB, platform, contestID string, hintTime time.Time) (start, end time.Time, ok bool) {
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	dur := platformDuration(platform)

	// 1) 已有日历（含爬虫写入的 source=nowcoder 真实赛长；爬多少用多少）
	if cal, found := lookupContestCalendar(db, platform, contestID); found {
		cs := time.Unix(cal.StartTime, 0)
		var ce time.Time
		if cal.EndTime > cal.StartTime {
			ce = time.Unix(cal.EndTime, 0)
		} else {
			ce = cs.Add(dur)
		}
		if calendarWindowValid(cs, ce) {
			return cs, ce, true
		}
	}

	// 2) 牛客：无日历时拉官方实时（history end / 比赛页），禁止直接用固定 3h 当真相
	if NormalizeCalendarPlatform(platform) == spider.NowCoder && contestID != "" {
		name, url, hintEnd := nowCoderHintsFromLogs(db, contestID, &hintTime)
		if s, e, okNC := EnsureNowCoderContestCalendar(db, contestID, name, url, hintTime, hintEnd); okNC {
			return s, e, true
		}
	}

	if !hintTime.IsZero() {
		start, end = windowFromHint(platform, hintTime, false)
		if !start.IsZero() {
			return start, end, true
		}
	}

	// 用最早 AC 粗估（展示用）
	if minAC, okMin := minContestFirstAC(db, platform, contestID); okMin {
		start = minAC.Add(-2 * time.Minute)
		end = start.Add(dur)
		return start, end, true
	}

	return time.Time{}, time.Time{}, false
}

// ResolveContestWindow 解析比赛时间窗 [start, end]（end 含赛后缓冲，供 Infer 扫提交）。
// hintTime：contest_logs.time 等提示；AtCoder 为结束时间，不可当开赛。
// 牛客：日历/官方实时优先；默认 3h 仅在拉官方失败时兜底。
func ResolveContestWindow(db *gorm.DB, platform, contestID string, hintTime time.Time) (start, end time.Time) {
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	dur := platformDuration(platform)

	// 1) 日历（爬多少用多少，不做赛长二次否决）
	if cal, found := lookupContestCalendar(db, platform, contestID); found {
		cs := time.Unix(cal.StartTime, 0)
		var ce time.Time
		if cal.EndTime > cal.StartTime {
			ce = time.Unix(cal.EndTime, 0)
		} else {
			ce = cs.Add(dur)
		}
		if calendarWindowValid(cs, ce) {
			return cs, ce.Add(contestInferEndBuffer)
		}
	}

	// 2) 牛客：无日历 → 官方实时 start/end（再加缓冲）
	if NormalizeCalendarPlatform(platform) == spider.NowCoder && contestID != "" {
		name, url, hintEnd := nowCoderHintsFromLogs(db, contestID, &hintTime)
		if s, e, okNC := EnsureNowCoderContestCalendar(db, contestID, name, url, hintTime, hintEnd); okNC {
			return s, e.Add(contestInferEndBuffer)
		}
	}

	// 3) hint（平台语义；牛客仅为拉官方失败后的最后兜底）
	if !hintTime.IsZero() {
		return windowFromHint(platform, hintTime, true)
	}

	// 4) contest_logs.time 兜底
	if db != nil && contestID != "" {
		var cl model.ContestLog
		plats := calendarPlatformAliases(platform)
		if db.Where("platform IN ? AND contest_id = ?", plats, contestID).
			Order("time DESC").First(&cl).Error == nil && !cl.Time.IsZero() {
			return windowFromHint(platform, cl.Time, true)
		}
	}

	// 5) 仍无：用 now-dur 宽窗
	end = time.Now()
	start = end.Add(-dur)
	end = end.Add(contestInferEndBuffer)
	return start, end
}

// nowCoderHintsFromLogs 从已有参赛记录取名称/链接；hintStart 为空时补开赛时间。
// EndTime 不落库，hintEnd 一般为零，依赖 history 内存或比赛页。
func nowCoderHintsFromLogs(db *gorm.DB, contestID string, hintStart *time.Time) (name, url string, hintEnd time.Time) {
	if db == nil || contestID == "" {
		return "", "", time.Time{}
	}
	var cl model.ContestLog
	if db.Where("platform IN ? AND contest_id = ?", calendarPlatformAliases(spider.NowCoder), contestID).
		Order("time DESC").First(&cl).Error != nil {
		return "", "", time.Time{}
	}
	if hintStart != nil && hintStart.IsZero() && !cl.Time.IsZero() {
		*hintStart = cl.Time
	}
	return cl.ContestName, cl.ContestUrl, cl.EndTime
}

// BatchContestDisplayTimes 批量解析 (platform, contestId) → (start, end) unix。
// 先一次查出相关日历行，缺的再按默认赛长用 hint 估算。
// 牛客缺官方窗时限量补日历（history endTime / 比赛页），避免默认 3h 截断 4h 赛。
func BatchContestDisplayTimes(db *gorm.DB, logs []model.ContestLog) map[string][2]int64 {
	out := map[string][2]int64{}
	if len(logs) == 0 {
		return out
	}
	ensureNowCoderCalendarsFromContestLogs(db, logs, 8)

	type key struct{ p, c string }
	need := map[key]time.Time{}
	for _, l := range logs {
		k := key{p: l.Platform, c: l.ContestId}
		if _, ok := need[k]; !ok {
			need[k] = l.Time
		}
	}
	// 日历批量：按平台别名分组查（atcoder ↔ AtCoder）
	byPlat := map[string][]string{}
	for k := range need {
		for _, ap := range calendarPlatformAliases(k.p) {
			byPlat[ap] = append(byPlat[ap], k.c)
		}
	}
	// external_id → cal（忽略 platform 大小写，按 need 的 canonical key 回填）
	calByExt := map[string]model.ContestCalendar{}
	if db != nil {
		for plat, ids := range byPlat {
			var cals []model.ContestCalendar
			_ = db.Where("platform = ? AND external_id IN ?", plat, ids).Find(&cals).Error
			for _, cal := range cals {
				ext := strings.TrimSpace(cal.ExternalID)
				if ext == "" {
					continue
				}
				// 同 ext 保留 start 更早的
				if prev, ok := calByExt[ext]; !ok || cal.StartTime < prev.StartTime {
					calByExt[ext] = cal
				}
			}
		}
	}
	for k, hint := range need {
		mapKey := k.p + "\x00" + k.c
		if cal, ok := calByExt[k.c]; ok && cal.StartTime > 0 {
			cs := time.Unix(cal.StartTime, 0)
			ce := time.Unix(cal.EndTime, 0)
			if cal.EndTime <= cal.StartTime {
				ce = cs.Add(platformDuration(k.p))
			}
			if calendarWindowValid(cs, ce) {
				out[mapKey] = [2]int64{cs.Unix(), ce.Unix()}
				continue
			}
		}
		start, end, ok := ResolveContestDisplayWindow(db, k.p, k.c, hint)
		if ok {
			out[mapKey] = [2]int64{start.Unix(), end.Unix()}
		}
	}
	return out
}

// loadContestProblemSet 本场 external_id → label
func loadContestProblemSet(db *gorm.DB, platform, contestID string) map[string]string {
	out := map[string]string{}
	if db == nil {
		return out
	}
	var items []model.ContestProblem
	_ = db.Where("platform = ? AND contest_id = ?", platform, contestID).Find(&items).Error
	for _, it := range items {
		ext := strings.TrimSpace(it.ExternalID)
		if ext == "" {
			continue
		}
		label := strings.TrimSpace(it.Label)
		if label == "" {
			label = ext
		}
		out[ext] = label
		// 大小写变体
		out[strings.ToLower(ext)] = label
	}
	return out
}

// InferContestUserProblems 通用兜底：submit_logs ∩ 题目集 ∩ 时间窗 → contest_user_problems。
// 任意平台原生明细缺失时使用；有题目目录时更准，无目录时用 contest 字段匹配。
// 返回写入/更新行数（近似）。
func InferContestUserProblems(db *gorm.DB, platform, contestID string, userIDs []int64, hintTime time.Time) (int, error) {
	if db == nil || len(userIDs) == 0 {
		return 0, nil
	}
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	if platform == "" || contestID == "" {
		return 0, nil
	}

	start, end := ResolveContestWindow(db, platform, contestID, hintTime)
	probSet := loadContestProblemSet(db, platform, contestID)
	hasProbSet := len(probSet) > 0

	type row struct {
		UserID     int64     `gorm:"column:user_id"`
		Contest    string    `gorm:"column:contest"`
		Problem    string    `gorm:"column:problem"`
		Status     string    `gorm:"column:status"`
		SubmitID   string    `gorm:"column:submit_id"`
		ExternalID string    `gorm:"column:external_id"`
		Time       time.Time `gorm:"column:time"`
	}
	var rows []row
	q := db.Model(&model.SubmitLog{}).
		Select("user_id, contest, problem, status, submit_id, external_id, time").
		Where("platform = ? AND user_id IN ?", platform, userIDs).
		Where("time >= ? AND time <= ?", start, end).
		Where("problem <> '' AND problem IS NOT NULL")

	// 力扣排除合成日历/补齐/合成 AC
	if platform == spider.LeetCode {
		q = q.Where("submit_id LIKE ?", "lc-prob-%")
	}

	if err := q.Order("time ASC").Limit(contestInferSubmitLimit).Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	type agg struct {
		label    string
		ext      string
		attempts int
		ac       bool
		firstAC  *time.Time
	}
	byUser := map[int64]map[string]*agg{}

	for _, r := range rows {
		// 合成提交再防一层（力扣补齐 / UOJ AC 列表）
		if model.IsSyntheticSubmitForFeed(platform, r.SubmitID) {
			continue
		}

		ext, label := resolveSubmitExternal(platform, contestID, r.Contest, r.Problem, r.ExternalID)
		if ext == "" {
			continue
		}

		// 归属：题目集 或 contest 字段 或（无题目集时）contest 匹配
		belong := false
		if hasProbSet {
			if _, ok := probSet[ext]; ok {
				belong = true
			} else if _, ok := probSet[strings.ToLower(ext)]; ok {
				belong = true
				// 用集合里的规范 label
				if lb := probSet[ext]; lb != "" {
					label = lb
				} else if lb := probSet[strings.ToLower(ext)]; lb != "" {
					label = lb
				}
			}
		}
		// contest 字段精确/模糊
		cField := strings.TrimSpace(r.Contest)
		if !belong && cField != "" {
			if cField == contestID || cField == "-"+contestID ||
				strings.EqualFold(cField, contestID) ||
				strings.Contains(cField, contestID) {
				belong = true
			}
		}
		// 无题目集且 contest 空：无法安全归属（避免全库同题误算）
		if !belong {
			continue
		}
		if label == "" {
			if lb, ok := probSet[ext]; ok {
				label = lb
			} else {
				label = ext
			}
		}

		m := byUser[r.UserID]
		if m == nil {
			m = map[string]*agg{}
			byUser[r.UserID] = m
		}
		a := m[ext]
		if a == nil {
			a = &agg{label: label, ext: ext}
			m[ext] = a
		}
		if a.ac {
			continue
		}
		if model.IsAcceptedStatus(r.Status) {
			a.ac = true
			t := r.Time
			a.firstAC = &t
			continue
		}
		st := strings.ToUpper(strings.TrimSpace(r.Status))
		if st == "" || st == "TESTING" || st == "PENDING" || st == "JUDGING" || st == "IN_QUEUE" ||
			st == "CE" || st == "COMPILATION_ERROR" || st == "编译错误" || st == "SUBMIT" {
			continue
		}
		a.attempts++
	}

	var upserts []model.ContestUserProblem
	for uid, m := range byUser {
		for _, a := range m {
			st := model.ContestCellTried
			var rel *int
			if a.ac {
				st = model.ContestCellAC
				if a.firstAC != nil && !start.IsZero() {
					sec := int(a.firstAC.Sub(start).Seconds())
					if sec < 0 {
						sec = 0
					}
					rel = &sec
				}
			}
			upserts = append(upserts, model.ContestUserProblem{
				Platform:    platform,
				ContestID:   contestID,
				UserID:      uid,
				Label:       a.label,
				ExternalID:  a.ext,
				Status:      st,
				Attempts:    a.attempts,
				FirstACAt:   a.firstAC,
				RelativeSec: rel,
			})
		}
	}
	if len(upserts) == 0 {
		return 0, nil
	}
	// 合并已有格子：禁止把 UPSOLVE 降级成 TRIED、禁止覆盖赛时 AC
	upserts = mergeContestCellUpserts(db, platform, contestID, upserts)
	if len(upserts) == 0 {
		return 0, nil
	}
	err := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "platform"}, {Name: "contest_id"}, {Name: "user_id"}, {Name: "external_id"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"label", "status", "attempts", "first_ac_at", "relative_sec", "updated_at",
		}),
	}).CreateInBatches(&upserts, 100).Error
	if err != nil {
		return 0, err
	}
	return len(upserts), nil
}

// cupKey 内存索引键：external_id + user
func cupKey(userID int64, externalID string) string {
	return strings.ToLower(strings.TrimSpace(externalID)) + "\x00" + strconv.FormatInt(userID, 10)
}

// loadContestUserProblemIndex 本场若干用户已有格子。
func loadContestUserProblemIndex(db *gorm.DB, platform, contestID string, userIDs []int64) map[string]model.ContestUserProblem {
	out := map[string]model.ContestUserProblem{}
	if db == nil || len(userIDs) == 0 {
		return out
	}
	var rows []model.ContestUserProblem
	_ = db.Where("platform = ? AND contest_id = ? AND user_id IN ?", platform, contestID, userIDs).
		Find(&rows).Error
	for _, r := range rows {
		out[cupKey(r.UserID, r.ExternalID)] = r
	}
	return out
}

// mergeContestCellIncoming 合并「即将写入」与「已有」：
// AC 最高；UPSOLVE 不被 TRIED 覆盖；赛时 AC 不被补题覆盖。
func mergeContestCellIncoming(prev *model.ContestUserProblem, next model.ContestUserProblem) (model.ContestUserProblem, bool) {
	if prev == nil {
		return next, true
	}
	// 已有赛时 AC：仅允许用新的赛时 AC 刷新，禁止降级
	if prev.Status == model.ContestCellAC {
		if next.Status == model.ContestCellAC {
			return next, true
		}
		return *prev, false
	}
	// 新赛时 AC：覆盖 TRIED / UPSOLVE
	if next.Status == model.ContestCellAC {
		return next, true
	}
	// 已有补题：TRIED 只合并 attempts，不改 status
	if prev.Status == model.ContestCellUpsolve {
		if next.Status == model.ContestCellTried {
			out := *prev
			if next.Attempts > out.Attempts {
				out.Attempts = next.Attempts
			}
			if strings.TrimSpace(next.Label) != "" {
				out.Label = next.Label
			}
			out.RelativeSec = nil
			return out, true
		}
		if next.Status == model.ContestCellUpsolve {
			out := next
			// 保留更早的补题 AC
			if prev.FirstACAt != nil && (out.FirstACAt == nil || prev.FirstACAt.Before(*out.FirstACAt)) {
				out.FirstACAt = prev.FirstACAt
			}
			if next.Attempts < prev.Attempts {
				out.Attempts = prev.Attempts
			}
			out.RelativeSec = nil
			return out, true
		}
	}
	// 新补题覆盖 TRIED / 空
	if next.Status == model.ContestCellUpsolve {
		out := next
		out.RelativeSec = nil
		if prev.Status == model.ContestCellTried && prev.Attempts > out.Attempts {
			out.Attempts = prev.Attempts
		}
		if strings.TrimSpace(out.Label) == "" {
			out.Label = prev.Label
		}
		return out, true
	}
	// 默认：用 next
	return next, true
}

// mergeContestCellUpserts 对一批待写入格子做已有行合并；丢弃无需写入的项。
func mergeContestCellUpserts(db *gorm.DB, platform, contestID string, incoming []model.ContestUserProblem) []model.ContestUserProblem {
	if len(incoming) == 0 {
		return nil
	}
	uids := make([]int64, 0, len(incoming))
	seen := map[int64]struct{}{}
	for _, r := range incoming {
		if _, ok := seen[r.UserID]; ok {
			continue
		}
		seen[r.UserID] = struct{}{}
		uids = append(uids, r.UserID)
	}
	idx := loadContestUserProblemIndex(db, platform, contestID, uids)
	out := make([]model.ContestUserProblem, 0, len(incoming))
	for _, next := range incoming {
		var prevPtr *model.ContestUserProblem
		if p, ok := idx[cupKey(next.UserID, next.ExternalID)]; ok {
			pp := p
			prevPtr = &pp
		}
		merged, write := mergeContestCellIncoming(prevPtr, next)
		if !write {
			continue
		}
		// 与已有完全一致则跳过（减少无意义 upsert）
		if prevPtr != nil &&
			prevPtr.Status == merged.Status &&
			prevPtr.Attempts == merged.Attempts &&
			prevPtr.Label == merged.Label &&
			((prevPtr.RelativeSec == nil && merged.RelativeSec == nil) ||
				(prevPtr.RelativeSec != nil && merged.RelativeSec != nil && *prevPtr.RelativeSec == *merged.RelativeSec)) &&
			((prevPtr.FirstACAt == nil && merged.FirstACAt == nil) ||
				(prevPtr.FirstACAt != nil && merged.FirstACAt != nil && prevPtr.FirstACAt.Equal(*merged.FirstACAt))) {
			continue
		}
		out = append(out, merged)
	}
	return out
}

// InferContestUpsolves 赛后补题：submit_logs 在赛时窗外的首次 AC → status=UPSOLVE。
// 不计分；不覆盖已有赛时 AC。horizon 默认赛后 30 天。
func InferContestUpsolves(db *gorm.DB, platform, contestID string, userIDs []int64, hintTime time.Time) (int, error) {
	if db == nil || len(userIDs) == 0 {
		return 0, nil
	}
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	if platform == "" || contestID == "" {
		return 0, nil
	}

	_, end := ResolveContestWindow(db, platform, contestID, hintTime)
	if end.IsZero() {
		return 0, nil
	}
	// 严格晚于赛时窗（含缓冲）才算补题
	upsolveStart := end.Add(time.Second)
	upsolveEnd := end.Add(contestUpsolveMaxHorizon)
	if now := time.Now(); now.Before(upsolveEnd) {
		upsolveEnd = now
	}
	if !upsolveEnd.After(upsolveStart) {
		return 0, nil
	}

	probSet := loadContestProblemSet(db, platform, contestID)
	hasProbSet := len(probSet) > 0

	type row struct {
		UserID     int64     `gorm:"column:user_id"`
		Contest    string    `gorm:"column:contest"`
		Problem    string    `gorm:"column:problem"`
		Status     string    `gorm:"column:status"`
		SubmitID   string    `gorm:"column:submit_id"`
		ExternalID string    `gorm:"column:external_id"`
		Time       time.Time `gorm:"column:time"`
	}
	var rows []row
	q := db.Model(&model.SubmitLog{}).
		Select("user_id, contest, problem, status, submit_id, external_id, time").
		Where("platform = ? AND user_id IN ?", platform, userIDs).
		Where("time > ? AND time <= ?", end, upsolveEnd).
		Where("problem <> '' AND problem IS NOT NULL")
	if platform == spider.LeetCode {
		q = q.Where("submit_id LIKE ?", "lc-prob-%")
	}
	if err := q.Order("time ASC").Limit(contestInferSubmitLimit).Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	// 每用户每题首次赛后 AC
	type firstAC struct {
		label string
		ext   string
		at    time.Time
	}
	byUser := map[int64]map[string]*firstAC{}
	for _, r := range rows {
		if model.IsSyntheticSubmitForFeed(platform, r.SubmitID) {
			continue
		}
		if !model.IsAcceptedStatus(r.Status) {
			continue
		}
		ext, label := resolveSubmitExternal(platform, contestID, r.Contest, r.Problem, r.ExternalID)
		if ext == "" {
			continue
		}
		belong := false
		if hasProbSet {
			if _, ok := probSet[ext]; ok {
				belong = true
			} else if _, ok := probSet[strings.ToLower(ext)]; ok {
				belong = true
			}
			if belong {
				if lb := probSet[ext]; lb != "" {
					label = lb
				} else if lb := probSet[strings.ToLower(ext)]; lb != "" {
					label = lb
				}
			}
		}
		cField := strings.TrimSpace(r.Contest)
		if !belong && cField != "" {
			if cField == contestID || cField == "-"+contestID ||
				strings.EqualFold(cField, contestID) ||
				strings.Contains(cField, contestID) {
				belong = true
			}
		}
		if !belong {
			continue
		}
		if label == "" {
			if lb, ok := probSet[ext]; ok {
				label = lb
			} else {
				label = ext
			}
		}
		m := byUser[r.UserID]
		if m == nil {
			m = map[string]*firstAC{}
			byUser[r.UserID] = m
		}
		if _, ok := m[ext]; ok {
			continue // 已记首次
		}
		m[ext] = &firstAC{label: label, ext: ext, at: r.Time}
	}

	var upserts []model.ContestUserProblem
	for uid, m := range byUser {
		for _, a := range m {
			t := a.at
			upserts = append(upserts, model.ContestUserProblem{
				Platform:    platform,
				ContestID:   contestID,
				UserID:      uid,
				Label:       a.label,
				ExternalID:  a.ext,
				Status:      model.ContestCellUpsolve,
				Attempts:    0,
				FirstACAt:   &t,
				RelativeSec: nil,
			})
		}
	}
	if len(upserts) == 0 {
		return 0, nil
	}
	upserts = mergeContestCellUpserts(db, platform, contestID, upserts)
	if len(upserts) == 0 {
		return 0, nil
	}
	err := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "platform"}, {Name: "contest_id"}, {Name: "user_id"}, {Name: "external_id"},
		},
		DoUpdates: clause.AssignmentColumns([]string{
			"label", "status", "attempts", "first_ac_at", "relative_sec", "updated_at",
		}),
	}).CreateInBatches(&upserts, 100).Error
	if err != nil {
		return 0, err
	}
	return len(upserts), nil
}

// ListContestPracticeCells 直接从全量 submit_logs 推导赛后补题状态，不依赖再次爬取。
// userIDs=nil 表示不限制用户；返回 UPSOLVE（已通过）或 UPSOLVE_TRIED（尝试未过）。
func ListContestPracticeCells(db *gorm.DB, platform, contestID string, userIDs []int64, hintTime time.Time) ([]model.ContestUserProblem, error) {
	if db == nil || strings.TrimSpace(platform) == "" || strings.TrimSpace(contestID) == "" {
		return nil, nil
	}
	start, end := ResolveContestWindow(db, platform, contestID, hintTime)
	if end.IsZero() {
		return nil, nil
	}
	practiceEnd := end.Add(contestUpsolveMaxHorizon)
	if now := time.Now(); now.Before(practiceEnd) {
		practiceEnd = now
	}
	if !practiceEnd.After(end) {
		return nil, nil
	}
	probSet := loadContestProblemSet(db, platform, contestID)
	hasProbSet := len(probSet) > 0
	type row struct {
		UserID                                         int64 `gorm:"column:user_id"`
		Contest, Problem, Status, SubmitID, ExternalID string
		Time                                           time.Time
	}
	var rows []row
	q := db.Model(&model.SubmitLog{}).
		Select("user_id, contest, problem, status, submit_id, external_id, time").
		Where("platform = ? AND time >= ? AND time <= ?", platform, start, practiceEnd).
		Where("problem <> '' AND problem IS NOT NULL")
	// 先在 SQL 层收窄到本场，避免热门平台其它比赛的提交挤占扫描上限。
	if hasProbSet {
		exts := make([]string, 0, len(probSet))
		problemLikes := make([]string, 0, len(probSet))
		seen := map[string]struct{}{}
		for ext := range probSet {
			key := strings.ToLower(strings.TrimSpace(ext))
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			exts = append(exts, ext)
			problemLikes = append(problemLikes, ext+"%")
		}
		parts := []string{"external_id IN ?", "LOWER(contest) LIKE ?"}
		args := []interface{}{exts, "%" + strings.ToLower(contestID) + "%"}
		for _, like := range problemLikes {
			parts = append(parts, "problem LIKE ?")
			args = append(args, like)
		}
		q = q.Where("("+strings.Join(parts, " OR ")+")", args...)
	} else {
		q = q.Where("LOWER(contest) LIKE ?", "%"+strings.ToLower(contestID)+"%")
	}
	if userIDs != nil {
		if len(userIDs) == 0 {
			return nil, nil
		}
		q = q.Where("user_id IN ?", userIDs)
	}
	if platform == spider.LeetCode {
		q = q.Where("submit_id LIKE ?", "lc-prob-%")
	}
	if err := q.Order("time ASC").Limit(contestInferSubmitLimit).Find(&rows).Error; err != nil {
		return nil, err
	}
	type agg struct {
		label, ext string
		attempts   int
		ac         bool
		contestAC  bool
		postSeen   bool
		firstAC    *time.Time
	}
	byUser := map[int64]map[string]*agg{}
	for _, r := range rows {
		if model.IsSyntheticSubmitForFeed(platform, r.SubmitID) {
			continue
		}
		ext, label := resolveSubmitExternal(platform, contestID, r.Contest, r.Problem, r.ExternalID)
		if ext == "" {
			continue
		}
		belong := false
		if hasProbSet {
			_, belong = probSet[ext]
			if !belong {
				_, belong = probSet[strings.ToLower(ext)]
			}
			if lb := probSet[ext]; lb != "" {
				label = lb
			} else if lb := probSet[strings.ToLower(ext)]; lb != "" {
				label = lb
			}
		}
		cf := strings.TrimSpace(r.Contest)
		if !belong && cf != "" && (cf == contestID || cf == "-"+contestID || strings.EqualFold(cf, contestID) || strings.Contains(cf, contestID)) {
			belong = true
		}
		if !belong {
			continue
		}
		if label == "" {
			label = ext
		}
		m := byUser[r.UserID]
		if m == nil {
			m = map[string]*agg{}
			byUser[r.UserID] = m
		}
		a := m[ext]
		if a == nil {
			a = &agg{label: label, ext: ext}
			m[ext] = a
		}
		if !r.Time.After(end) {
			if model.IsAcceptedStatus(r.Status) {
				a.contestAC = true
			}
			continue
		}
		a.postSeen = true
		if a.ac {
			continue
		}
		if model.IsAcceptedStatus(r.Status) {
			a.ac = true
			t := r.Time
			a.firstAC = &t
			continue
		}
		if !model.IsPendingSubmitStatus(r.Status) {
			a.attempts++
		}
	}
	out := []model.ContestUserProblem{}
	for uid, m := range byUser {
		for _, a := range m {
			// 赛时已 AC 的题，赛后再提交也始终是赛时 AC；没有赛后提交则不生成补题格。
			if a.contestAC || !a.postSeen {
				continue
			}
			status := model.ContestCellUpsolveTried
			if a.ac {
				status = model.ContestCellUpsolve
			}
			out = append(out, model.ContestUserProblem{Platform: platform, ContestID: contestID, UserID: uid, Label: a.label, ExternalID: a.ext, Status: status, Attempts: a.attempts, FirstACAt: a.firstAC})
		}
	}
	return out, nil
}

// resolveSubmitExternal 从提交行得到 external_id + 展示 label。
func resolveSubmitExternal(platform, contestID, contestField, problem, storedExt string) (ext, label string) {
	storedExt = strings.TrimSpace(storedExt)
	if storedExt != "" && !strings.HasPrefix(storedExt, "ac-") {
		ext = storedExt
		label = storedExt
		// 仍尝试 parse 拿更好 label
		if parsed, err := ParseProblemIdentity(platform, firstNonEmpty(contestField, contestID), problem); err == nil && parsed != nil {
			if parsed.ExternalID != "" {
				ext = parsed.ExternalID
			}
			if parsed.Title != "" && len(parsed.Title) < 40 {
				// label 优先短 index
			}
		}
		label = shortLabelFromExt(platform, ext, problem)
		return ext, label
	}

	contestForParse := contestField
	if contestForParse == "" || contestForParse == "leetcode" {
		contestForParse = contestID
	}
	parsed, err := ParseProblemIdentity(platform, contestForParse, problem)
	if err == nil && parsed != nil && parsed.ExternalID != "" {
		return parsed.ExternalID, shortLabelFromExt(platform, parsed.ExternalID, problem)
	}

	// 牛客：题目前缀数字
	if platform == spider.NowCoder {
		if id := leadingDigits(problem); id != "" {
			return id, id
		}
	}
	// 力扣：problem 以 slug 开头
	if platform == spider.LeetCode {
		parts := strings.Fields(problem)
		if len(parts) > 0 && reLeetCodeSlug.MatchString(parts[0]) {
			return parts[0], parts[0]
		}
	}
	return "", ""
}

func shortLabelFromExt(platform, ext, problem string) string {
	ext = strings.TrimSpace(ext)
	if platform == spider.CodeForces || platform == "Codeforces" {
		// 2247A / gym102861A → A
		e := ext
		if strings.HasPrefix(strings.ToLower(e), "gym") {
			e = e[3:]
		}
		for i := 0; i < len(e); i++ {
			if (e[i] >= 'A' && e[i] <= 'Z') || (e[i] >= 'a' && e[i] <= 'z') {
				return e[i:]
			}
		}
	}
	if platform == spider.AtCoder {
		if i := strings.LastIndex(ext, "_"); i >= 0 && i+1 < len(ext) {
			return strings.ToUpper(ext[i+1:])
		}
	}
	if platform == spider.NowCoder || platform == spider.QOJ {
		return ext
	}
	if platform == spider.LuoGu {
		return ext
	}
	// 从 problem 取首段
	if p := strings.TrimSpace(problem); p != "" {
		if i := strings.IndexAny(p, " \t-"); i > 0 && i <= 8 {
			return p[:i]
		}
	}
	if len(ext) > 12 {
		return ext[:12]
	}
	return ext
}

func leadingDigits(s string) string {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return ""
	}
	return s[:i]
}

// InferContestUserProblemsForUser 爬虫路径：单用户反推（原生明细失败或补洞）。
func InferContestUserProblemsForUser(db *gorm.DB, platform, contestID string, userID int64, hintTime time.Time) (int, error) {
	if userID == 0 {
		return 0, nil
	}
	return InferContestUserProblems(db, platform, contestID, []int64{userID}, hintTime)
}

// InferContestUpsolvesForUser 爬虫路径：单用户补题回填。
func InferContestUpsolvesForUser(db *gorm.DB, platform, contestID string, userID int64, hintTime time.Time) (int, error) {
	if userID == 0 {
		return 0, nil
	}
	return InferContestUpsolves(db, platform, contestID, []int64{userID}, hintTime)
}

// 格子提交时段（与榜单格子语义对齐，供弹窗区分展示）
const (
	CellSubmitPhaseContest = "contest" // 赛时（含短缓冲）
	CellSubmitPhaseUpsolve = "upsolve" // 赛后补题
)

// ContestCellSubmit 站内榜格子弹窗：单条提交（赛时或赛后）。
type ContestCellSubmit struct {
	ID          uint
	SubmitID    string
	Status      string
	Lang        string
	Time        time.Time
	RelativeSec *int   // 仅 phase=contest：相对开赛秒
	Phase       string // contest | upsolve
	Problem     string
	Contest     string
	ExternalID  string
	ProblemID   *uint
}

const contestCellSubmitLimit = 200

// resolveCellSubmitWindow 站内榜格子弹窗专用时间窗。
//
// 注意 contest_logs.time 语义因平台而异，不能一律当「开赛」：
//   - AtCoder history：结束时间
//   - Codeforces rating：出分/结算时间（赛后）
//   - 日历：start/end（爬多少用多少）
//   - 格子 FirstACAt − RelativeSec：仅多样本一致时采用（单条脏 relative_sec 会污染全场）
//
// 返回 start 供相对赛时展示；end 含短缓冲。
func resolveCellSubmitWindow(db *gorm.DB, platform, contestID string, hintTime time.Time) (start, end time.Time) {
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	dur := platformDuration(platform)

	// 1) 日历（爬多少用多少）
	if cal, found := lookupContestCalendar(db, platform, contestID); found {
		cs := time.Unix(cal.StartTime, 0)
		var ce time.Time
		if cal.EndTime > cal.StartTime {
			ce = time.Unix(cal.EndTime, 0)
		} else {
			ce = cs.Add(dur)
		}
		if calendarWindowValid(cs, ce) {
			return cs, ce.Add(contestInferEndBuffer)
		}
	}

	// 2) 平台语义 hint（AtCoder 必须优先于 relative_sec：线上曾出现单条脏 relative 把窗推到赛后）
	if !hintTime.IsZero() && (hintAsContestEnd(platform) || NormalizeCalendarPlatform(platform) == spider.CodeForces) {
		return windowFromHint(platform, hintTime, true)
	}

	// 3) 用本场格子 FirstACAt − RelativeSec 反推（需 ≥2 条一致，且不晚于最早 AC）
	if db != nil && contestID != "" {
		if ds, ok := deriveStartFromRelativeCells(db, platform, contestID); ok {
			start = ds.Add(-2 * time.Minute)
			end = ds.Add(dur + contestInferEndBuffer)
			if !hintTime.IsZero() && hintTime.After(end) {
				end = hintTime.Add(contestInferEndBuffer)
			}
			return start, end
		}
	}

	// 4) 其余平台：hint 当开赛
	if !hintTime.IsZero() {
		return windowFromHint(platform, hintTime, true)
	}

	// 5) 兜底
	return ResolveContestWindow(db, platform, contestID, hintTime)
}

// deriveStartFromRelativeCells 多样本 FirstACAt−RelativeSec 取最早；样本不足或与 minAC 矛盾则失败。
func deriveStartFromRelativeCells(db *gorm.DB, platform, contestID string) (time.Time, bool) {
	plats := calendarPlatformAliases(platform)
	var cells []model.ContestUserProblem
	_ = db.Where("platform IN ? AND contest_id = ? AND first_ac_at IS NOT NULL AND relative_sec IS NOT NULL",
		plats, contestID).
		Limit(50).Find(&cells).Error
	var derivedStart time.Time
	n := 0
	for _, c := range cells {
		if c.FirstACAt == nil || c.RelativeSec == nil || *c.RelativeSec < 0 {
			continue
		}
		s := c.FirstACAt.Add(-time.Duration(*c.RelativeSec) * time.Second)
		if s.IsZero() {
			continue
		}
		n++
		if derivedStart.IsZero() || s.Before(derivedStart) {
			derivedStart = s
		}
	}
	// 单条 relative_sec 不可信（赛后练习 AC + 错误窗反推会污染全场）
	if n < 2 || derivedStart.IsZero() {
		return time.Time{}, false
	}
	if minAC, ok := minContestFirstAC(db, platform, contestID); ok {
		// 反推开赛不应明显晚于最早 AC
		if derivedStart.After(minAC.Add(5 * time.Minute)) {
			return time.Time{}, false
		}
	}
	return derivedStart, true
}

// collectWantExternalIDs 格子 externalId + 题号 label → 要匹配的 external_id 集合。
func collectWantExternalIDs(db *gorm.DB, platform, contestID, label, externalID string) (want []string, outLabel, outExt string) {
	label = strings.TrimSpace(label)
	externalID = strings.TrimSpace(externalID)
	outLabel, outExt = label, externalID
	seen := map[string]struct{}{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		want = append(want, s)
	}
	add(externalID)

	probSet := loadContestProblemSet(db, platform, contestID)
	if externalID == "" && label != "" {
		for ext, lb := range probSet {
			if strings.EqualFold(lb, label) || strings.EqualFold(ext, label) {
				add(ext)
				if outExt == "" {
					outExt = ext
				}
				if outLabel == "" {
					outLabel = lb
				}
			}
		}
	}
	if label != "" {
		for ext, lb := range probSet {
			if strings.EqualFold(lb, label) {
				add(ext)
			}
		}
	}
	if outLabel == "" && externalID != "" {
		if lb, ok := probSet[externalID]; ok {
			outLabel = lb
		} else if lb, ok := probSet[strings.ToLower(externalID)]; ok {
			outLabel = lb
		}
	}
	// 无目录时 CF 常见 external = contestId+label
	if externalID == "" && label != "" && contestID != "" {
		if platform == spider.CodeForces || platform == "Codeforces" {
			add(contestID + label)
			if outExt == "" {
				outExt = contestID + label
			}
		}
		if platform == spider.AtCoder {
			// abc467 + A → abc467_a
			add(strings.ToLower(contestID + "_" + label))
			if outExt == "" {
				outExt = strings.ToLower(contestID + "_" + label)
			}
		}
	}
	return want, outLabel, outExt
}

// ListContestCellSubmits 按 external_id 反查 submit_logs：
// 含赛时窗 + 赛后补题窗（end 之后最多 contestUpsolveMaxHorizon，与 InferContestUpsolves 对齐）。
// 每条 Phase 标记 contest|upsolve；relativeSec 仅赛时有。
// label / externalID 至少一个非空；优先 externalID（与 contest_user_problems 一致）。
// 返回按提交时间逆序（新→旧）。
func ListContestCellSubmits(
	db *gorm.DB,
	platform, contestID string,
	userID int64,
	label, externalID string,
	hintTime time.Time,
) (list []ContestCellSubmit, start, end time.Time, err error) {
	if db == nil || userID == 0 {
		return nil, time.Time{}, time.Time{}, nil
	}
	platform = strings.TrimSpace(platform)
	contestID = strings.TrimSpace(contestID)
	if platform == "" || contestID == "" {
		return nil, time.Time{}, time.Time{}, nil
	}
	wantExt, _, _ := collectWantExternalIDs(db, platform, contestID, label, externalID)
	if len(wantExt) == 0 && strings.TrimSpace(label) == "" {
		return nil, time.Time{}, time.Time{}, nil
	}

	// endBuffered：扫提交用（含 15min 排队缓冲，避免漏收延迟到站的提交）
	// endOfficial：给人看的官方结束时间——phase 赛时/赛后分界必须用这个，
	// 否则 22:00 结束后 22:01 会被误标成「赛时」。
	start, endBuffered := resolveCellSubmitWindow(db, platform, contestID, hintTime)
	endOfficial := time.Time{}
	if ds, de, ok := ResolveContestDisplayWindow(db, platform, contestID, hintTime); ok {
		if start.IsZero() {
			start = ds
		}
		endOfficial = de
	}
	if endOfficial.IsZero() && !endBuffered.IsZero() {
		// 展示窗不可用时：从缓冲窗去掉缓冲得到近似官方结束
		endOfficial = endBuffered.Add(-contestInferEndBuffer)
	}
	// API 返回的 end 用官方结束时间（前端展示 / 调试）
	end = endOfficial
	if end.IsZero() {
		end = endBuffered
	}

	// 查询上界：缓冲 end + 补题 horizon（不超过现在）
	queryEnd := endBuffered
	if queryEnd.IsZero() {
		queryEnd = end
	}
	if !queryEnd.IsZero() {
		upsolveEnd := queryEnd.Add(contestUpsolveMaxHorizon)
		if now := time.Now(); now.Before(upsolveEnd) {
			upsolveEnd = now
		}
		if upsolveEnd.After(queryEnd) {
			queryEnd = upsolveEnd
		}
	}

	// 大小写不敏感匹配 external_id
	lowerExt := make([]string, 0, len(wantExt))
	for _, e := range wantExt {
		lowerExt = append(lowerExt, strings.ToLower(e))
	}

	var rows []model.SubmitLog
	q := db.Model(&model.SubmitLog{}).
		Where("platform = ? AND user_id = ?", platform, userID).
		Where("time >= ? AND time <= ?", start, queryEnd)
	if platform == spider.LeetCode {
		q = q.Where("submit_id LIKE ?", "lc-prob-%")
	}

	// 主路径：external_id 或 problem（AtCoder 历史行 problem=abc462_a、external 可空）
	// 辅：本场 contest + 展示 label（A / A- / A.）
	// 用 LOWER 兼容 Postgres 与单测 SQLite
	label = strings.TrimSpace(label)
	if len(wantExt) > 0 && label != "" {
		q = q.Where(
			`(LOWER(external_id) IN ?)
			 OR (LOWER(problem) IN ?)
			 OR (
			   (contest = ? OR contest = ?)
			   AND (problem = ? OR problem LIKE ? OR problem LIKE ? OR problem LIKE ?
			        OR LOWER(problem) LIKE ?)
			 )`,
			lowerExt,
			lowerExt,
			contestID, "-"+contestID,
			label, label+"-%", label+" %", label+".%",
			strings.ToLower("%_"+label),
		)
	} else if len(wantExt) > 0 {
		q = q.Where(
			`(LOWER(external_id) IN ?) OR (LOWER(problem) IN ?)
			 OR ((contest = ? OR contest = ?) AND LOWER(problem) IN ?)`,
			lowerExt, lowerExt,
			contestID, "-"+contestID, lowerExt,
		)
	} else {
		q = q.Where(
			`(contest = ? OR contest = ?)
			 AND (problem = ? OR problem LIKE ? OR problem LIKE ? OR problem LIKE ?
			      OR LOWER(problem) LIKE ?)`,
			contestID, "-"+contestID,
			label, label+"-%", label+" %", label+".%",
			strings.ToLower("%_"+label),
		)
	}

	if err = q.Order("time DESC").Limit(contestCellSubmitLimit).Find(&rows).Error; err != nil {
		return nil, start, end, err
	}

	// 二次确认：external_id / 解析后的 external 必须落在 wantExt（防 contest ILIKE 误伤）
	wantSet := map[string]struct{}{}
	for _, e := range lowerExt {
		wantSet[e] = struct{}{}
	}

	out := make([]ContestCellSubmit, 0, len(rows))
	for _, r := range rows {
		if model.IsSyntheticSubmitForFeed(platform, r.SubmitID) {
			continue
		}
		ext, _ := resolveSubmitExternal(platform, contestID, r.Contest, r.Problem, r.ExternalID)
		extKey := strings.ToLower(strings.TrimSpace(firstNonEmpty(ext, r.ExternalID)))
		if len(wantSet) > 0 {
			probKey := strings.ToLower(strings.TrimSpace(r.Problem))
			_, extOK := wantSet[extKey]
			_, probOK := wantSet[probKey]
			if !extOK && !probOK {
				// 允许 problem 前缀匹配 label 且 contest 精确为本场
				p := strings.TrimSpace(r.Problem)
				cField := strings.TrimSpace(r.Contest)
				labelOK := label != "" && (strings.EqualFold(p, label) ||
					strings.HasPrefix(p, label+"-") ||
					strings.HasPrefix(p, label+" ") ||
					strings.HasPrefix(strings.ToUpper(p), strings.ToUpper(label)+".") ||
					// AtCoder: abc462_a ↔ label A
					strings.HasSuffix(strings.ToLower(p), "_"+strings.ToLower(label)))
				contestOK := cField == contestID || cField == "-"+contestID || strings.EqualFold(cField, contestID)
				if !(labelOK && contestOK) {
					continue
				}
			}
		}
		// 严格按官方结束时间：time > endOfficial → 赛后（不用 15min 缓冲）
		phase := CellSubmitPhaseContest
		if !endOfficial.IsZero() && r.Time.After(endOfficial) {
			phase = CellSubmitPhaseUpsolve
		}
		item := ContestCellSubmit{
			ID:         r.ID,
			SubmitID:   r.SubmitID,
			Status:     r.Status,
			Lang:       r.Lang,
			Time:       r.Time,
			Phase:      phase,
			Problem:    r.Problem,
			Contest:    r.Contest,
			ExternalID: firstNonEmpty(ext, r.ExternalID),
			ProblemID:  r.ProblemID,
		}
		// 相对赛时仅对赛时提交有意义
		if phase == CellSubmitPhaseContest && !start.IsZero() && !r.Time.IsZero() {
			sec := int(r.Time.Sub(start).Seconds())
			if sec < 0 {
				sec = 0
			}
			item.RelativeSec = &sec
		}
		out = append(out, item)
	}
	return out, start, end, nil
}
