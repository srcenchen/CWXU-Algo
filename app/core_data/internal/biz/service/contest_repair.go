package service

import (
	"fmt"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

// RepairContestCellSubmitData 修复站内榜 cell-submits 相关脏数据：
//  1. AtCoder submit_logs 空 external_id → problem
//  2. 赛后练习误标为 AC 的格子 → UPSOLVE（不计分展示），并清空 relative_sec
//  3. AtCoder relative_sec 按 history 结束时间 − 100min 重算
//
// 可安全重复执行（幂等）。
func RepairContestCellSubmitData(db *gorm.DB) (map[string]int64, error) {
	out := map[string]int64{}
	if db == nil {
		return out, nil
	}

	// 1) external_id 回填
	res := db.Exec(`
		UPDATE submit_logs
		SET external_id = problem
		WHERE platform = ?
		  AND (external_id IS NULL OR BTRIM(external_id) = '')
		  AND problem IS NOT NULL AND BTRIM(problem) <> ''
	`, spider.AtCoder)
	if res.Error != nil {
		// SQLite 单测无 BTRIM：降级
		res = db.Exec(`
			UPDATE submit_logs
			SET external_id = problem
			WHERE platform = ?
			  AND (external_id IS NULL OR external_id = '')
			  AND problem IS NOT NULL AND problem <> ''
		`, spider.AtCoder)
	}
	if res.Error != nil {
		return out, fmt.Errorf("backfill external_id: %w", res.Error)
	}
	out["submit_external_id"] = res.RowsAffected

	// 2) 赛后误标 AC → UPSOLVE（展示补题，不进罚时）
	// 优先日历 end_time（全平台真实结束）；AtCoder 无日历时用 contest_logs.time（= history EndTime）。
	// 禁止用牛客等「time=开赛」的日志当结束，否则会误伤赛时 AC。
	n, err := repairDowngradePracticeCells(db)
	if err != nil {
		return out, fmt.Errorf("downgrade practice cells: %w", err)
	}
	out["practice_cells_to_upsolve"] = n

	// 3) 重算 AtCoder relative_sec（end − 100min 为开赛；仅赛时 AC）
	nRel, err := repairAtCoderRelativeSec(db)
	if err != nil {
		return out, fmt.Errorf("relative_sec: %w", err)
	}
	out["relative_sec_updated"] = nRel

	log.Infof("RepairContestCellSubmitData: %+v", out)
	return out, nil
}

// repairDowngradePracticeCells 将 first_ac 明显晚于比赛结束的 AC 格降为 UPSOLVE。
func repairDowngradePracticeCells(db *gorm.DB) (int64, error) {
	type endKey struct {
		Platform  string
		ContestID string
	}
	ends := map[endKey]time.Time{}

	// 日历：真实 end（unix 秒）
	var cals []model.ContestCalendar
	if err := db.Where("end_time > start_time AND end_time > 0").Find(&cals).Error; err != nil {
		return 0, err
	}
	for _, cal := range cals {
		plat := NormalizeCalendarPlatform(cal.Platform)
		if plat == "" {
			plat = cal.Platform
		}
		k := endKey{Platform: plat, ContestID: cal.ExternalID}
		et := time.Unix(cal.EndTime, 0)
		if prev, ok := ends[k]; !ok || et.After(prev) {
			ends[k] = et
		}
		// 别名 platform 也挂一份，匹配 cup 里大小写不一的行
		for _, ap := range calendarPlatformAliases(plat) {
			ak := endKey{Platform: ap, ContestID: cal.ExternalID}
			if prev, ok := ends[ak]; !ok || et.After(prev) {
				ends[ak] = et
			}
		}
	}

	// AtCoder：history EndTime 落在 contest_logs.time，补日历缺口
	type logEnd struct {
		Platform  string
		ContestID string
		EndT      time.Time
	}
	var logEnds []logEnd
	if err := db.Model(&model.ContestLog{}).
		Select("platform, contest_id, MAX(time) AS end_t").
		Where("platform IN ?", calendarPlatformAliases(spider.AtCoder)).
		Group("platform, contest_id").
		Scan(&logEnds).Error; err != nil {
		return 0, err
	}
	for _, e := range logEnds {
		if e.EndT.IsZero() || e.ContestID == "" {
			continue
		}
		k := endKey{Platform: e.Platform, ContestID: e.ContestID}
		if prev, ok := ends[k]; !ok || e.EndT.After(prev) {
			ends[k] = e.EndT
		}
	}

	var total int64
	for k, endT := range ends {
		cutoff := endT.Add(contestInferEndBuffer)
		res := db.Model(&model.ContestUserProblem{}).Where(
			"platform = ? AND contest_id = ? AND status = ? AND first_ac_at IS NOT NULL AND first_ac_at > ?",
			k.Platform, k.ContestID, model.ContestCellAC, cutoff,
		).Updates(map[string]interface{}{
			"status":       model.ContestCellUpsolve,
			"relative_sec": nil,
		})
		if res.Error != nil {
			return total, res.Error
		}
		total += res.RowsAffected
	}
	return total, nil
}

func repairAtCoderRelativeSec(db *gorm.DB) (int64, error) {
	type endRow struct {
		Platform  string
		ContestID string
		EndT      time.Time
	}
	var ends []endRow
	if err := db.Model(&model.ContestLog{}).
		Select("platform, contest_id, MAX(time) AS end_t").
		Where("platform = ?", spider.AtCoder).
		Group("platform, contest_id").
		Scan(&ends).Error; err != nil {
		return 0, err
	}
	durSec := int64((100 * time.Minute).Seconds())
	var total int64
	for _, e := range ends {
		if e.EndT.IsZero() || e.ContestID == "" {
			continue
		}
		startUnix := e.EndT.Unix() - durSec
		var cells []model.ContestUserProblem
		if err := db.Where(
			"platform = ? AND contest_id = ? AND status = ? AND first_ac_at IS NOT NULL",
			e.Platform, e.ContestID, model.ContestCellAC,
		).Find(&cells).Error; err != nil {
			return total, err
		}
		for _, c := range cells {
			if c.FirstACAt == nil {
				continue
			}
			rel := int(c.FirstACAt.Unix() - startUnix)
			if rel < 0 {
				rel = 0
			}
			// 超过默认赛长+缓冲的视为脏，清空 relative
			if rel > int(durSec)+int(contestInferEndBuffer.Seconds()) {
				if err := db.Model(&model.ContestUserProblem{}).Where("id = ?", c.ID).
					Update("relative_sec", nil).Error; err != nil {
					return total, err
				}
				total++
				continue
			}
			if c.RelativeSec != nil && *c.RelativeSec == rel {
				continue
			}
			if err := db.Model(&model.ContestUserProblem{}).Where("id = ?", c.ID).
				Update("relative_sec", rel).Error; err != nil {
				return total, err
			}
			total++
		}
	}
	return total, nil
}
