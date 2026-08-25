package dal

import (
	"context"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func sameLocalDay(a, b time.Time) bool {
	ay, am, ad := a.In(time.Local).Date()
	by, bm, bd := b.In(time.Local).Date()
	return ay == by && am == bm && ad == bd
}

// UpsertLuoGuPublicSnapshot advances public totals monotonically and attributes only positive deltas to today.
func UpsertLuoGuPublicSnapshot(ctx context.Context, db *gorm.DB, userID, remoteUID, totalSolved, totalSubmit, realSolved, realSubmit, realTodaySolved, realTodayAC, realTodaySubmit int64, observedAt time.Time) (bool, error) {
	if db == nil || userID <= 0 || remoteUID <= 0 {
		return false, gorm.ErrInvalidData
	}
	changed := false
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var old model.LuoGuPublicSnapshot
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&old, "user_id = ? AND platform = ?", userID, spider.LuoGu).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			return err
		}
		if err == gorm.ErrRecordNotFound || !old.Active {
			row := model.LuoGuPublicSnapshot{
				UserID: userID, Platform: spider.LuoGu, RemoteUID: remoteUID,
				TotalSolved: max64(totalSolved, realSolved), TotalSubmit: max64(totalSubmit, realSubmit),
				TodaySolved: max64(totalSolved-realSolved, 0), TodaySubmit: max64(totalSubmit-realSubmit, 0),
				RealTodaySolvedBaseline: realTodaySolved, RealTodayACBaseline: realTodayAC, RealTodaySubmitBaseline: realTodaySubmit,
				Active: true, RecoveryRequired: true, ObservedAt: observedAt,
			}
			changed = true
			return tx.Save(&row).Error
		}
		if old.RemoteUID != remoteUID {
			row := model.LuoGuPublicSnapshot{
				UserID: userID, Platform: spider.LuoGu, RemoteUID: remoteUID,
				TotalSolved: max64(totalSolved, realSolved), TotalSubmit: max64(totalSubmit, realSubmit),
				TodaySolved: max64(totalSolved-realSolved, 0), TodaySubmit: max64(totalSubmit-realSubmit, 0),
				RealTodaySolvedBaseline: realTodaySolved, RealTodayACBaseline: realTodayAC, RealTodaySubmitBaseline: realTodaySubmit,
				Active: true, RecoveryRequired: true, ObservedAt: observedAt,
			}
			changed = true
			return tx.Save(&row).Error
		}

		newSolved := max64(old.TotalSolved, totalSolved)
		newSubmit := max64(old.TotalSubmit, totalSubmit)
		solvedDelta := newSolved - old.TotalSolved
		submitDelta := newSubmit - old.TotalSubmit
		newTodaySolved, newTodaySubmit := solvedDelta, submitDelta
		newSolvedBaseline, newACBaseline, newSubmitBaseline := realTodaySolved, realTodayAC, realTodaySubmit
		if sameLocalDay(old.ObservedAt, observedAt) {
			newTodaySolved += old.TodaySolved
			newTodaySubmit += old.TodaySubmit
			newSolvedBaseline, newACBaseline, newSubmitBaseline = old.RealTodaySolvedBaseline, old.RealTodayACBaseline, old.RealTodaySubmitBaseline
		}
		changed = newSolved != old.TotalSolved || newSubmit != old.TotalSubmit ||
			newTodaySolved != old.TodaySolved || newTodaySubmit != old.TodaySubmit || old.RemoteUID != remoteUID
		return tx.Model(&old).Updates(map[string]interface{}{
			"remote_uid": remoteUID, "total_solved": newSolved, "total_submit": newSubmit,
			"today_solved": newTodaySolved, "today_submit": newTodaySubmit,
			"real_today_solved_baseline": newSolvedBaseline, "real_today_ac_baseline": newACBaseline,
			"real_today_submit_baseline": newSubmitBaseline,
			"active":                     true, "recovery_required": true, "observed_at": observedAt,
		}).Error
	})
	return changed, err
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// LuoGuRealCounts returns the real persisted LuoGu contribution used as the first fallback baseline.
func LuoGuRealCounts(ctx context.Context, db *gorm.DB, userID int64) (solved, submit int64, err error) {
	err = db.WithContext(ctx).Table("user_ac_problems").Where("user_id = ? AND platform = ?", userID, spider.LuoGu).Count(&solved).Error
	if err != nil {
		return
	}
	err = db.WithContext(ctx).Table("daily_user_stats").Where("user_id = ? AND platform = ?", userID, spider.LuoGu).Select("COALESCE(SUM(submit_cnt), 0)").Scan(&submit).Error
	return
}

func LuoGuRealTodayCounts(ctx context.Context, db *gorm.DB, userID int64, now time.Time) (solved, ac, submit int64, err error) {
	day := time.Date(now.In(time.Local).Year(), now.In(time.Local).Month(), now.In(time.Local).Day(), 0, 0, 0, 0, time.Local)
	err = db.WithContext(ctx).Table("user_ac_problem_days").Where("user_id = ? AND platform = ? AND day = ?", userID, spider.LuoGu, day).Count(&solved).Error
	if err != nil {
		return
	}
	err = db.WithContext(ctx).Table("daily_user_stats").Where("user_id = ? AND platform = ? AND day = ?", userID, spider.LuoGu, day).
		Select("COALESCE(SUM(ac_cnt),0) AS ac, COALESCE(SUM(submit_cnt),0) AS submit").Row().Scan(&ac, &submit)
	return
}

func HasActiveLuoGuPublicSnapshot(ctx context.Context, db *gorm.DB, userID int64) (bool, error) {
	var n int64
	err := db.WithContext(ctx).Model(&model.LuoGuPublicSnapshot{}).
		Where("user_id = ? AND platform = ? AND active = ?", userID, spider.LuoGu, true).Count(&n).Error
	return n > 0, err
}

func ActiveLuoGuPublicSnapshot(ctx context.Context, db *gorm.DB, userID int64) (model.LuoGuPublicSnapshot, bool, error) {
	var snapshot model.LuoGuPublicSnapshot
	err := db.WithContext(ctx).Where("user_id = ? AND platform = ? AND active = ?", userID, spider.LuoGu, true).First(&snapshot).Error
	if err == gorm.ErrRecordNotFound {
		return snapshot, false, nil
	}
	return snapshot, err == nil, err
}

// CloseLuoGuPublicSnapshot never closes an override unless the caller proved a complete full fetch.
func CloseLuoGuPublicSnapshot(ctx context.Context, db *gorm.DB, userID, expectedRemoteUID int64, expectedObservedAt time.Time, complete bool) (bool, error) {
	if !complete {
		return false, nil
	}
	r := db.WithContext(ctx).Model(&model.LuoGuPublicSnapshot{}).
		Where("user_id = ? AND platform = ? AND active = ? AND remote_uid = ? AND observed_at = ?", userID, spider.LuoGu, true, expectedRemoteUID, expectedObservedAt).
		Updates(map[string]interface{}{"active": false, "recovery_required": false})
	return r.RowsAffected > 0, r.Error
}

type luoguOverrideContribution struct {
	PublicSolved int64
	PublicSubmit int64
	TodaySolved  int64
	TodaySubmit  int64
	RealSolved   int64
	RealSubmit   int64
}

type luoguSnapshotToday struct {
	UserID                  int64
	TodaySolved             int64
	TodaySubmit             int64
	RealTodaySolvedBaseline int64
	RealTodayACBaseline     int64
	RealTodaySubmitBaseline int64
	ObservedAt              time.Time
}

func loadLuoGuOverrideContribution(db *gorm.DB, userID int64, memberIDs []int64, personal bool) (luoguOverrideContribution, error) {
	var out luoguOverrideContribution
	active := func() *gorm.DB {
		q := db.Table("luogu_public_snapshots").Where("platform = ? AND active = ?", spider.LuoGu, true)
		if personal {
			return q.Where("user_id = ?", userID)
		}
		if memberIDs != nil {
			return q.Where("user_id IN ?", memberIDs)
		}
		return q
	}
	var ids []int64
	if err := active().Pluck("user_id", &ids).Error; err != nil || len(ids) == 0 {
		return out, err
	}
	if err := active().Select("COALESCE(SUM(total_solved),0) AS public_solved, COALESCE(SUM(total_submit),0) AS public_submit").Scan(&out).Error; err != nil {
		return out, err
	}
	var snapshots []luoguSnapshotToday
	if err := active().Select("user_id, today_solved, today_submit, real_today_solved_baseline, real_today_ac_baseline, real_today_submit_baseline, observed_at").Scan(&snapshots).Error; err != nil {
		return out, err
	}
	if personal {
		if err := db.Table("user_ac_problems").Where("user_id IN ? AND platform = ?", ids, spider.LuoGu).Count(&out.RealSolved).Error; err != nil {
			return out, err
		}
	} else {
		if err := db.Table("daily_user_stats").Where("user_id IN ? AND platform = ?", ids, spider.LuoGu).Select("COALESCE(SUM(ac_cnt),0)").Scan(&out.RealSolved).Error; err != nil {
			return out, err
		}
	}
	if err := db.Table("daily_user_stats").Where("user_id IN ? AND platform = ?", ids, spider.LuoGu).Select("COALESCE(SUM(submit_cnt),0)").Scan(&out.RealSubmit).Error; err != nil {
		return out, err
	}
	today := time.Now()
	for _, snapshot := range snapshots {
		if !sameLocalDay(snapshot.ObservedAt, today) {
			continue
		}
		realTodaySolved, realTodayAC, realTodaySubmit, err := LuoGuRealTodayCounts(context.Background(), db, snapshot.UserID, today)
		if err != nil {
			return out, err
		}
		baselineSolved := snapshot.RealTodaySolvedBaseline
		currentSolved := realTodaySolved
		if !personal {
			baselineSolved = snapshot.RealTodayACBaseline
			currentSolved = realTodayAC
		}
		out.TodaySolved += max64(snapshot.TodaySolved-max64(currentSolved-baselineSolved, 0), 0)
		out.TodaySubmit += max64(snapshot.TodaySubmit-max64(realTodaySubmit-snapshot.RealTodaySubmitBaseline, 0), 0)
	}
	return out, nil
}

// ApplyLuoGuPublicPeriodOverride replaces only LuoGu lifetime/today contributions.
func ApplyLuoGuPublicPeriodOverride(db *gorm.DB, userID int64, memberIDs []int64, submit PeriodSubmitCount, ac PeriodAcCount) (PeriodSubmitCount, PeriodAcCount, error) {
	c, err := loadLuoGuOverrideContribution(db, userID, memberIDs, userID != -1)
	if err != nil {
		return submit, ac, err
	}
	submit.Total += c.PublicSubmit - c.RealSubmit
	submit.Today += c.TodaySubmit
	ac.Total += c.PublicSolved - c.RealSolved
	ac.Today += c.TodaySolved
	if ac.TotalRaw < ac.Total {
		ac.TotalRaw = ac.Total
	}
	return submit, ac, nil
}

// ApplyLuoGuPublicPlatformACOverride replaces the LuoGu item and leaves other platforms untouched.
func ApplyLuoGuPublicPlatformACOverride(db *gorm.DB, userID int64, counts []PlatformACCount) ([]PlatformACCount, error) {
	var snapshot model.LuoGuPublicSnapshot
	err := db.Where("user_id = ? AND platform = ? AND active = ?", userID, spider.LuoGu, true).First(&snapshot).Error
	if err == gorm.ErrRecordNotFound {
		return counts, nil
	}
	if err != nil {
		return counts, err
	}
	found := false
	for i := range counts {
		if counts[i].Name == spider.LuoGu {
			counts[i].Count = snapshot.TotalSolved
			found = true
		}
	}
	if !found {
		counts = append(counts, PlatformACCount{Name: spider.LuoGu, Count: snapshot.TotalSolved})
	}
	for i := 0; i < len(counts); i++ {
		for j := i + 1; j < len(counts); j++ {
			if counts[j].Count > counts[i].Count {
				counts[i], counts[j] = counts[j], counts[i]
			}
		}
	}
	return counts, nil
}
