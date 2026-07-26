package dal

import (
	"context"
	"strings"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// sqlRefreshableStatus 库内可能被回写的行：pending/空状态，或 CF 长 verdict（可归一为短码）。
// 与 model.IsPendingSubmitStatus / shouldRewriteFinalStatus 覆盖集合一致，
// 用于回读时跳过绝大多数已终态短码行，避免整批回读扫描。
const sqlRefreshableStatus = `(
	status IS NULL
	OR btrim(status) = ''
	OR btrim(status) IN ('正在评测', '评测中', '等待评测', '排队中')
	OR UPPER(btrim(status)) IN (
		'TESTING', 'PENDING', 'JUDGING', 'IN_QUEUE', 'IN QUEUE', 'WAITING', 'WJ', 'QUEUE',
		'WRONG_ANSWER', 'TIME_LIMIT_EXCEEDED', 'MEMORY_LIMIT_EXCEEDED', 'RUNTIME_ERROR',
		'COMPILATION_ERROR', 'PRESENTATION_ERROR', 'IDLENESS_LIMIT_EXCEEDED', 'SECURITY_VIOLATED'
	)
)`

// RefreshPendingSubmitVerdicts 回写已入库但状态仍为评测中/空的提交。
// 场景：CF 首次爬到时 verdict 为空或 TESTING，已在 submit_logs 后 FilterNew 会跳过，
// 若不回写则 UI 永久空白。不重计 submit_cnt；若 is_ac 0→1 则补 daily.ac 与 user_ac。
// 另：允许把历史长字面量（WRONG_ANSWER）归一为短码（WA），不改 is_ac 统计。
// 匹配与回写均带 (platform, user_id) 条件，防跨平台/跨用户撞 submit_id。
func RefreshPendingSubmitVerdicts(ctx context.Context, db *gorm.DB, fetched []model.SubmitLog) (int64, error) {
	if db == nil || len(fetched) == 0 {
		return 0, nil
	}
	type group struct {
		plat string
		uid  int64
	}
	want := make(map[string]model.SubmitLog, len(fetched))
	groups := make(map[group][]string, 2)
	for i := range fetched {
		l := fetched[i]
		if l.SubmitID == "" {
			continue
		}
		if model.IsPendingSubmitStatus(l.Status) {
			continue
		}
		wk := l.Platform + "\x00" + l.SubmitID
		if _, ok := want[wk]; !ok {
			g := group{plat: l.Platform, uid: l.UserID}
			groups[g] = append(groups[g], l.SubmitID)
		}
		want[wk] = l
	}
	if len(want) == 0 {
		return 0, nil
	}

	var updated int64
	const chunk = 300
	for g, ids := range groups {
		for i := 0; i < len(ids); i += chunk {
			j := i + chunk
			if j > len(ids) {
				j = len(ids)
			}
			part := ids[i:j]
			var existing []model.SubmitLog
			// 只回读必要列 + 库内仍可回写（pending/长码）的行
			if err := db.WithContext(ctx).Model(&model.SubmitLog{}).
				Select("submit_id", "platform", "user_id", "status", "is_ac", "time").
				Where("platform = ? AND user_id = ? AND submit_id IN ?", g.plat, g.uid, part).
				Where(sqlRefreshableStatus).
				Find(&existing).Error; err != nil {
				return updated, err
			}
			n, err := refreshExistingVerdicts(ctx, db, existing, want)
			updated += n
			if err != nil {
				return updated, err
			}
		}
	}
	return updated, nil
}

// refreshExistingVerdicts 对已回读的旧行逐条比对回写；返回更新行数
func refreshExistingVerdicts(ctx context.Context, db *gorm.DB, existing []model.SubmitLog, want map[string]model.SubmitLog) (int64, error) {
	var updated int64
	for _, old := range existing {
		neu, ok := want[old.Platform+"\x00"+old.SubmitID]
		if !ok {
			continue
		}
		newStatus := strings.TrimSpace(neu.Status)
		if newStatus == "" {
			continue
		}
		oldStatus := strings.TrimSpace(old.Status)
		if oldStatus == newStatus {
			continue
		}
		// 仅：旧 pending，或长名→短码归一
		if !model.IsPendingSubmitStatus(oldStatus) && !shouldRewriteFinalStatus(oldStatus, newStatus) {
			continue
		}

		newIsAC := model.IsAcceptedStatus(newStatus)
		oldIsAC := old.IsAC || model.IsAcceptedStatus(oldStatus)

		res := db.WithContext(ctx).Model(&model.SubmitLog{}).
			Where("platform = ? AND user_id = ? AND submit_id = ? AND status = ?",
				old.Platform, old.UserID, old.SubmitID, old.Status).
			Updates(map[string]interface{}{
				"status": newStatus,
				"is_ac":  newIsAC,
			})
		if res.Error != nil {
			return updated, res.Error
		}
		if res.RowsAffected == 0 {
			continue
		}
		updated += res.RowsAffected

		if !oldIsAC && newIsAC {
			row := old
			row.Status = newStatus
			row.IsAC = true
			if err := ApplyUserACFromSubmits(ctx, db, []model.SubmitLog{row}); err != nil {
				log.Warnf("RefreshPending: ApplyUserAC submit=%s: %v", old.SubmitID, err)
			}
			day := time.Date(row.Time.Year(), row.Time.Month(), row.Time.Day(), 0, 0, 0, 0, row.Time.Location())
			plat := strings.TrimSpace(row.Platform)
			if plat == "" {
				plat = "?"
			}
			if err := ApplyDailyDeltas(ctx, db, []DailyDelta{{
				UserID:   row.UserID,
				Day:      day,
				Platform: plat,
				AcCnt:    1,
			}}); err != nil {
				log.Warnf("RefreshPending: ApplyDailyAC submit=%s: %v", old.SubmitID, err)
			}
		}
	}
	return updated, nil
}

// shouldRewriteFinalStatus 允许把 CF 原始长 verdict 归一成短码（不重计）
func shouldRewriteFinalStatus(oldStatus, newStatus string) bool {
	o := strings.ToUpper(strings.TrimSpace(oldStatus))
	n := strings.ToUpper(strings.TrimSpace(newStatus))
	if o == n || n == "" {
		return false
	}
	longToShort := map[string]string{
		"WRONG_ANSWER":            "WA",
		"TIME_LIMIT_EXCEEDED":     "TLE",
		"MEMORY_LIMIT_EXCEEDED":   "MLE",
		"RUNTIME_ERROR":           "RE",
		"COMPILATION_ERROR":       "CE",
		"PRESENTATION_ERROR":      "PE",
		"IDLENESS_LIMIT_EXCEEDED": "ILE",
		"SECURITY_VIOLATED":       "SV",
	}
	if short, ok := longToShort[o]; ok && short == n {
		return true
	}
	return false
}
