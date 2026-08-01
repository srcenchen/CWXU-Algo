package dal

import (
	"context"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// leetCodeStatsLoc 力扣「同日去重 / 今日 AC」用上海自然日（与产品主用户时区一致）
var leetCodeStatsLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// DailyDelta 某用户某平台某日的增量
type DailyDelta struct {
	UserID    int64
	Day       time.Time // 截断到日 00:00 local
	Platform  string
	SubmitCnt int64
	AcCnt     int64
}

// AggregateSubmitDeltas 将新插入的提交聚合成日增量（仅新行，可重复调用不会用旧行）
func AggregateSubmitDeltas(logs []model.SubmitLog) []DailyDelta {
	if len(logs) == 0 {
		return nil
	}
	type key struct {
		uid  int64
		day  string
		plat string
	}
	m := make(map[key]*DailyDelta, len(logs)/4+1)
	for i := range logs {
		l := &logs[i]
		day := time.Date(l.Time.Year(), l.Time.Month(), l.Time.Day(), 0, 0, 0, 0, l.Time.Location())
		plat := strings.TrimSpace(l.Platform)
		k := key{uid: l.UserID, day: day.Format("2006-01-02"), plat: plat}
		d, ok := m[k]
		if !ok {
			d = &DailyDelta{UserID: l.UserID, Day: day, Platform: plat}
			m[k] = d
		}
		// 力扣合成 AC / 最近通过明细不计提交（避免与日历双计）
		if model.CountsTowardSubmitStat(l.Platform, l.SubmitID) {
			d.SubmitCnt++
		}
		// 日 AC：lc-ac 不计入（仅生涯）；lc-prob 与其它平台真实 AC 计入
		if l.IsAC && model.CountsTowardDailyAC(l.Platform, l.SubmitID) {
			d.AcCnt++
		}
	}
	out := make([]DailyDelta, 0, len(m))
	for _, d := range m {
		if d.SubmitCnt == 0 && d.AcCnt == 0 {
			continue
		}
		out = append(out, *d)
	}
	return out
}

// ApplyDailyDeltas 原子累加日汇总（按 user+day+platform）；分批 CreateInBatches 降大包 round-trip
func ApplyDailyDeltas(ctx context.Context, db *gorm.DB, deltas []DailyDelta) error {
	if len(deltas) == 0 || db == nil {
		return nil
	}
	rows := make([]model.DailyUserStat, 0, len(deltas))
	for _, d := range deltas {
		rows = append(rows, model.DailyUserStat{
			UserID:    d.UserID,
			Day:       d.Day,
			Platform:  d.Platform,
			SubmitCnt: d.SubmitCnt,
			AcCnt:     d.AcCnt,
		})
	}
	const batch = 100
	return db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}, {Name: "day"}, {Name: "platform"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"submit_cnt": gorm.Expr("daily_user_stats.submit_cnt + EXCLUDED.submit_cnt"),
				"ac_cnt":     gorm.Expr("daily_user_stats.ac_cnt + EXCLUDED.ac_cnt"),
			}),
		}).
		CreateInBatches(&rows, batch).Error
}

// FilterNewSubmitLogs 入库前去重（submit_logs (platform, submit_id) 唯一约束为真相）：
//  1) 本批内按 (platform, submit_id) 去重
//  2) 去掉 submit_logs 已有（防全量重爬对 daily/user_ac 双计）
//  3) 力扣 lc-prob：同一用户同一 titleSlug **同一自然日**已有则跳过
//     （隔日重刷仍入库 → 动态可见 + 今日 AC；同日多次 AC 仍只留一条）
//
// 注意：daily_user_stats 是累加语义；必须只对「将新插入」的行 ApplyDaily。
// OnConflict DoNothing 无法区分新旧行，故在插入前用 submit_logs 过滤。
func FilterNewSubmitLogs(ctx context.Context, db *gorm.DB, logs []model.SubmitLog) ([]model.SubmitLog, error) {
	if len(logs) == 0 {
		return nil, nil
	}

	model.NormalizeSubmitIDs(logs)
	logs = dedupeSubmitLogsBySubmitID(logs)
	if len(logs) == 0 {
		return nil, nil
	}

	// 按平台分组查已有 submit_id，避免跨平台撞号误判为已入库
	byPlat := make(map[string][]string, 2)
	for i := range logs {
		byPlat[logs[i].Platform] = append(byPlat[logs[i].Platform], logs[i].SubmitID)
	}
	const chunk = 500
	exist := make(map[string]struct{}, len(logs)/2)
	for plat, ids := range byPlat {
		for i := 0; i < len(ids); i += chunk {
			j := i + chunk
			if j > len(ids) {
				j = len(ids)
			}
			part := ids[i:j]
			var found []string
			if err := db.WithContext(ctx).Model(&model.SubmitLog{}).
				Where("platform = ? AND submit_id IN ?", plat, part).
				Pluck("submit_id", &found).Error; err != nil {
				return nil, err
			}
			for _, id := range found {
				exist[plat+"\x00"+id] = struct{}{}
			}
		}
	}
	out := make([]model.SubmitLog, 0, len(logs))
	for i := range logs {
		if logs[i].SubmitID == "" {
			continue
		}
		if _, ok := exist[logs[i].Platform+"\x00"+logs[i].SubmitID]; ok {
			continue
		}
		out = append(out, logs[i])
	}
	if len(out) == 0 {
		return nil, nil
	}

	out, err := filterLeetCodeProbAlreadyHaveSlug(ctx, db, out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FilterUncountedSubmits 兼容旧名：等价 FilterNewSubmitLogs
func FilterUncountedSubmits(ctx context.Context, db *gorm.DB, logs []model.SubmitLog) ([]model.SubmitLog, error) {
	return FilterNewSubmitLogs(ctx, db, logs)
}

// BackfillDailyUserStatsIfEmpty 表为空时从 submit_logs 全量聚合一次（启动幂等）
// 含 platform 维度。
func BackfillDailyUserStatsIfEmpty(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.DailyUserStat{}) {
		return
	}
	var n int64
	if err := db.Model(&model.DailyUserStat{}).Count(&n).Error; err != nil {
		log.Warnf("daily_user_stats count failed: %v", err)
		return
	}
	if n > 0 {
		return
	}
	log.Infof("daily_user_stats empty, backfill from submit_logs…")
	res := db.Exec(`
		INSERT INTO daily_user_stats (user_id, day, platform, submit_cnt, ac_cnt)
		SELECT
			user_id,
			date_trunc('day', time)::date AS day,
			COALESCE(NULLIF(btrim(platform), ''), '?') AS platform,
			COUNT(*) FILTER (
				WHERE ` + model.SQLExcludeLeetCodeNonSubmit + `
			) AS submit_cnt,
			COUNT(*) FILTER (WHERE is_ac = true AND ` + model.SQLExcludeLeetCodeOfficialACSubmit + `) AS ac_cnt
		FROM submit_logs
		GROUP BY user_id, date_trunc('day', time)::date, COALESCE(NULLIF(btrim(platform), ''), '?')
		HAVING
			COUNT(*) FILTER (WHERE ` + model.SQLExcludeLeetCodeNonSubmit + `) > 0
			OR COUNT(*) FILTER (WHERE is_ac = true AND ` + model.SQLExcludeLeetCodeOfficialACSubmit + `) > 0
		ON CONFLICT (user_id, day, platform) DO NOTHING
	`)
	if res.Error != nil {
		log.Warnf("daily_user_stats backfill failed: %v", res.Error)
		return
	}
	log.Infof("daily_user_stats backfill done rows=%d", res.RowsAffected)
}

// RefreshLeetCodeProbMeta 回写已入库 lc-prob 的 lang / problem 展示字段。
// 早期实现把 Lang 写死为 "-"、Problem 仅 "{slug} {title}"（LCR 题像 iIQa4I 每日温度）；
// 重爬时 FilterNew 按 submit_id / titleSlug 去重会跳过，动态永久错乱。
// 本函数只改展示列，不碰 is_ac / 日统计。
func RefreshLeetCodeProbMeta(ctx context.Context, db *gorm.DB, fetched []model.SubmitLog) (int64, error) {
	if db == nil || len(fetched) == 0 {
		return 0, nil
	}
	var updated int64
	for i := range fetched {
		l := fetched[i]
		if l.Platform != "LeetCode" || !strings.HasPrefix(l.SubmitID, "lc-prob-") {
			continue
		}
		lang := strings.TrimSpace(l.Lang)
		problem := strings.TrimSpace(l.Problem)
		ext := strings.TrimSpace(l.ExternalID)
		if lang == "" || lang == "-" {
			lang = ""
		}
		if problem == "" && lang == "" {
			continue
		}

		// 1) 同 submit_id：直接补 lang / problem
		if l.SubmitID != "" {
			sets := map[string]interface{}{}
			if lang != "" {
				sets["lang"] = lang
			}
			if problem != "" {
				sets["problem"] = problem
			}
			if len(sets) > 0 {
				q := db.WithContext(ctx).Model(&model.SubmitLog{}).
					Where("platform = ? AND user_id = ? AND submit_id = ?", l.Platform, l.UserID, l.SubmitID)
				// 仅当旧值更差时更新，避免无意义写
				conds := make([]string, 0, 2)
				args := make([]interface{}, 0, 2)
				if lang != "" {
					conds = append(conds, "(lang IS NULL OR btrim(lang) = '' OR btrim(lang) = '-')")
				}
				if problem != "" {
					conds = append(conds, "(problem IS DISTINCT FROM ?)")
					args = append(args, problem)
				}
				if len(conds) > 0 {
					res := q.Where(strings.Join(conds, " OR "), args...).Updates(sets)
					if res.Error != nil {
						return updated, res.Error
					}
					updated += res.RowsAffected
				}
			}
		}

		// 2) 同 titleSlug 已有其它 submit_id（filterLeetCodeProbAlreadyHaveSlug 会拦新行）：
		//    把保留行的展示字段也修掉
		if ext == "" || l.UserID == 0 {
			continue
		}
		sets := map[string]interface{}{}
		if lang != "" {
			sets["lang"] = lang
		}
		if problem != "" {
			sets["problem"] = problem
		}
		if len(sets) == 0 {
			continue
		}
		q := db.WithContext(ctx).Model(&model.SubmitLog{}).
			Where("platform = ? AND user_id = ? AND submit_id LIKE 'lc-prob-%' AND external_id = ?",
				l.Platform, l.UserID, ext)
		if l.SubmitID != "" {
			q = q.Where("submit_id <> ?", l.SubmitID)
		}
		conds := make([]string, 0, 2)
		args := make([]interface{}, 0, 2)
		if lang != "" {
			conds = append(conds, "(lang IS NULL OR btrim(lang) = '' OR btrim(lang) = '-')")
		}
		if problem != "" {
			conds = append(conds, "(problem IS DISTINCT FROM ?)")
			args = append(args, problem)
		}
		if len(conds) == 0 {
			continue
		}
		res := q.Where(strings.Join(conds, " OR "), args...).Updates(sets)
		if res.Error != nil {
			return updated, res.Error
		}
		updated += res.RowsAffected
	}
	return updated, nil
}

// PruneLeetCodeProbDuplicates 清理某用户「同一自然日」重复的 lc-prob（同 external_id 只留最新）。
// 隔日重刷保留多条，供动态与今日 AC；同日 recentAC 多 submissionId 仍压成一条。
// 自然日按 Asia/Shanghai，与站内「今日 AC」口径一致。
func PruneLeetCodeProbDuplicates(ctx context.Context, db *gorm.DB, userID int64) (int64, error) {
	if db == nil || userID == 0 {
		return 0, nil
	}
	res := db.WithContext(ctx).Exec(`
		DELETE FROM submit_logs a
		USING submit_logs b
		WHERE a.user_id = ?
		  AND a.platform = 'LeetCode'
		  AND a.submit_id LIKE 'lc-prob-%'
		  AND b.user_id = a.user_id
		  AND b.platform = a.platform
		  AND b.submit_id LIKE 'lc-prob-%'
		  AND a.external_id IS NOT NULL AND a.external_id <> ''
		  AND a.external_id = b.external_id
		  AND ((a.time AT TIME ZONE 'Asia/Shanghai')::date = (b.time AT TIME ZONE 'Asia/Shanghai')::date)
		  AND (a.time < b.time OR (a.time = b.time AND a.id < b.id))
	`, userID)
	return res.RowsAffected, res.Error
}

// dedupeSubmitLogsBySubmitID 本批内按 (platform, submit_id) 去重，保留首次出现
func dedupeSubmitLogsBySubmitID(logs []model.SubmitLog) []model.SubmitLog {
	if len(logs) <= 1 {
		return logs
	}
	seen := make(map[string]struct{}, len(logs))
	out := make([]model.SubmitLog, 0, len(logs))
	for i := range logs {
		id := logs[i].SubmitID
		if id == "" {
			continue
		}
		key := logs[i].Platform + "\x00" + id
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, logs[i])
	}
	return out
}

// filterLeetCodeProbAlreadyHaveSlug 去掉「该用户该 titleSlug 在同一自然日已有 lc-prob」的候选。
//
// 历史 bug：按 lifetime slug 去重 → 隔日重刷（如 7/12 过 329、8/1 再过）被整行丢弃，
// 日历仍计「今日提交」、动态与「今日 AC」却看不到。现改为同日去重；隔日放行。
// 不再用 user_ac_problems 终身拦截（生涯去重仍由 user_ac_problems 自身 OnConflict 保证）。
func filterLeetCodeProbAlreadyHaveSlug(ctx context.Context, db *gorm.DB, logs []model.SubmitLog) ([]model.SubmitLog, error) {
	type dayKey struct {
		uid int64
		ext string
		day string // YYYY-MM-DD in submit local time
	}
	type cand struct {
		idx int
		ext string
		day string
	}
	var cands []cand
	userSet := make(map[int64]struct{})
	extSet := make(map[string]struct{})
	for i := range logs {
		l := &logs[i]
		if l.Platform != "LeetCode" || !strings.HasPrefix(l.SubmitID, "lc-prob-") {
			continue
		}
		ext := strings.TrimSpace(l.ExternalID)
		if ext == "" {
			if f := strings.Fields(l.Problem); len(f) > 0 {
				ext = f[0]
			}
		}
		if ext == "" {
			continue
		}
		if l.ExternalID == "" {
			l.ExternalID = ext
		}
		day := ""
		if !l.Time.IsZero() {
			day = l.Time.In(leetCodeStatsLoc).Format("2006-01-02")
		}
		cands = append(cands, cand{idx: i, ext: ext, day: day})
		userSet[l.UserID] = struct{}{}
		extSet[ext] = struct{}{}
	}
	if len(cands) == 0 {
		return logs, nil
	}

	uids := make([]int64, 0, len(userSet))
	for u := range userSet {
		uids = append(uids, u)
	}
	exts := make([]string, 0, len(extSet))
	for e := range extSet {
		exts = append(exts, e)
	}

	// 已有：user + external_id + 自然日（上海）
	have := make(map[dayKey]struct{}, len(cands))
	type row struct {
		UserID     int64     `gorm:"column:user_id"`
		ExternalID string    `gorm:"column:external_id"`
		Time       time.Time `gorm:"column:time"`
	}
	var rows []row
	if err := db.WithContext(ctx).Model(&model.SubmitLog{}).
		Select("user_id, external_id, time").
		Where("platform = ? AND submit_id LIKE ? AND user_id IN ? AND external_id IN ?",
			"LeetCode", "lc-prob-%", uids, exts).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Time.IsZero() || r.ExternalID == "" {
			continue
		}
		day := r.Time.In(leetCodeStatsLoc).Format("2006-01-02")
		have[dayKey{uid: r.UserID, ext: r.ExternalID, day: day}] = struct{}{}
	}

	batchSeen := make(map[dayKey]struct{}, len(cands))
	skipIdx := make(map[int]struct{}, len(cands))
	for _, c := range cands {
		l := &logs[c.idx]
		k := dayKey{uid: l.UserID, ext: c.ext, day: c.day}
		if c.day != "" {
			if _, ok := have[k]; ok {
				skipIdx[c.idx] = struct{}{}
				continue
			}
		}
		// 本批同日同 slug 只留一条（recentAC 常对一题返回多次）
		if _, ok := batchSeen[k]; ok {
			skipIdx[c.idx] = struct{}{}
			continue
		}
		batchSeen[k] = struct{}{}
	}

	if len(skipIdx) == 0 {
		return logs, nil
	}
	out := make([]model.SubmitLog, 0, len(logs)-len(skipIdx))
	for i := range logs {
		if _, skip := skipIdx[i]; skip {
			continue
		}
		out = append(out, logs[i])
	}
	return out, nil
}

// DeletePlatformDailyStats 换绑：删某用户某平台日汇总
func DeletePlatformDailyStats(ctx context.Context, db *gorm.DB, userID int64, platform string) error {
	if db == nil || userID <= 0 || platform == "" {
		return nil
	}
	if !db.Migrator().HasTable(&model.DailyUserStat{}) {
		return nil
	}
	return db.WithContext(ctx).
		Where("user_id = ? AND platform = ?", userID, platform).
		Delete(&model.DailyUserStat{}).Error
}



