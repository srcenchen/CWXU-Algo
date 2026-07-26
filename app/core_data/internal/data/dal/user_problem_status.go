package dal

import (
	"context"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ApplyUserProblemStatusFromSubmits 维护 user_problem_status（需 problem_id）
func ApplyUserProblemStatusFromSubmits(ctx context.Context, db *gorm.DB, logs []model.SubmitLog) error {
	if db == nil || len(logs) == 0 {
		return nil
	}
	// user+problem → 最优状态
	type key struct {
		uid int64
		pid uint
	}
	best := map[key]string{}
	for i := range logs {
		l := &logs[i]
		if l.UserID <= 0 || l.ProblemID == nil || *l.ProblemID == 0 {
			continue
		}
		st := model.UserProblemStatusTried
		if l.IsAC {
			st = model.UserProblemStatusAC
		}
		k := key{uid: l.UserID, pid: *l.ProblemID}
		if cur, ok := best[k]; ok {
			if cur == model.UserProblemStatusAC {
				continue
			}
			if st == model.UserProblemStatusAC {
				best[k] = st
			}
			continue
		}
		best[k] = st
	}
	if len(best) == 0 {
		return nil
	}
	now := time.Now()
	// 一次批量读旧状态（(user_id, problem_id) IN），内存比对是否首次升到 AC
	pairs := make([][]interface{}, 0, len(best))
	for k := range best {
		pairs = append(pairs, []interface{}{k.uid, k.pid})
	}
	prevMap := make(map[key]string, len(best))
	var prevRows []model.UserProblemStatus
	if err := db.WithContext(ctx).
		Where("(user_id, problem_id) IN ?", pairs).
		Find(&prevRows).Error; err != nil {
		return err
	}
	for _, r := range prevRows {
		prevMap[key{uid: r.UserID, pid: r.ProblemID}] = r.Status
	}

	// 批量 upsert；AC 永不被 TRIED 覆盖
	rows := make([]model.UserProblemStatus, 0, len(best))
	for k, st := range best {
		rows = append(rows, model.UserProblemStatus{
			UserID:    k.uid,
			ProblemID: k.pid,
			Status:    st,
			UpdatedAt: now,
		})
	}
	doUpdates := clause.Assignments(map[string]interface{}{
		"updated_at": now,
		"status": gorm.Expr(
			`CASE WHEN user_problem_status.status = 'AC' THEN 'AC' ELSE EXCLUDED.status END`,
		),
	})
	if err := db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "user_id"}, {Name: "problem_id"}},
			DoUpdates: doUpdates,
		}).
		CreateInBatches(&rows, 200).Error; err != nil {
		return err
	}
	// 首次变为 AC：标签画像 +1；待做题单自动剔除
	for k, st := range best {
		if st != model.UserProblemStatusAC || prevMap[k] == model.UserProblemStatusAC {
			continue
		}
		if err := IncUserTagACForFirstProblemAC(ctx, db, k.uid, k.pid); err != nil {
			log.Warnf("user_tag_ac first AC user=%d problem=%d: %v", k.uid, k.pid, err)
		}
		if err := RemoveFromTodoOnAC(ctx, db, k.uid, k.pid); err != nil {
			log.Warnf("RemoveFromTodoOnAC user=%d problem=%d: %v", k.uid, k.pid, err)
		}
	}
	return nil
}

// GetUserProblemStatuses 批量读状态
func GetUserProblemStatuses(ctx context.Context, db *gorm.DB, userID int64, problemIDs []uint) (map[uint]string, error) {
	out := map[uint]string{}
	if db == nil || userID <= 0 || len(problemIDs) == 0 {
		return out, nil
	}
	var rows []model.UserProblemStatus
	if err := db.WithContext(ctx).
		Where("user_id = ? AND problem_id IN ?", userID, problemIDs).
		Find(&rows).Error; err != nil {
		return out, err
	}
	for _, r := range rows {
		out[r.ProblemID] = r.Status
	}
	return out, nil
}

// GetFollowingProblemStatuses 关注用户对本题状态
func GetFollowingProblemStatuses(ctx context.Context, db *gorm.DB, problemID uint, userIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if db == nil || problemID == 0 || len(userIDs) == 0 {
		return out, nil
	}
	var rows []model.UserProblemStatus
	if err := db.WithContext(ctx).
		Where("problem_id = ? AND user_id IN ?", problemID, userIDs).
		Find(&rows).Error; err != nil {
		return out, err
	}
	for _, r := range rows {
		out[r.UserID] = r.Status
	}
	return out, nil
}


