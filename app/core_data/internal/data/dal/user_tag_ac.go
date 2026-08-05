package dal

import (
	"context"
	"fmt"
	"strings"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// IncUserTagAC 用户首次 AC 某题后，对该题标签各 +1
func IncUserTagAC(ctx context.Context, db *gorm.DB, userID int64, tags []string) error {
	if db == nil || userID <= 0 {
		return nil
	}
	tags = NormalizeTags(tags)
	for _, tag := range tags {
		row := model.UserTagAC{UserID: userID, Tag: tag, Count: 1}
		if err := db.WithContext(ctx).
			Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "user_id"}, {Name: "tag"}},
				DoUpdates: clause.Assignments(map[string]interface{}{
					"count": gorm.Expr("user_tag_ac.count + 1"),
				}),
			}).
			Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

// AdjustUserTagACForProblemTagsChange 题标签从 old→new 时，所有 AC 过该题的用户差分
func AdjustUserTagACForProblemTagsChange(ctx context.Context, db *gorm.DB, problemID uint, oldTags, newTags []string) error {
	if db == nil || problemID == 0 {
		return nil
	}
	oldTags = NormalizeTags(oldTags)
	newTags = NormalizeTags(newTags)
	if sameStringSet(oldTags, newTags) {
		return nil
	}
	removed, added := diffStringSets(oldTags, newTags)
	if len(removed) == 0 && len(added) == 0 {
		return nil
	}

	userIDs, err := listUsersACProblem(ctx, db, problemID)
	if err != nil {
		return err
	}
	if len(userIDs) == 0 {
		return nil
	}

	for _, uid := range userIDs {
		for _, tag := range removed {
			if err := db.WithContext(ctx).Exec(`
				UPDATE user_tag_ac SET count = count - 1
				WHERE user_id = ? AND tag = ? AND count > 0
			`, uid, tag).Error; err != nil {
				return err
			}
			_ = db.WithContext(ctx).Exec(`
				DELETE FROM user_tag_ac WHERE user_id = ? AND tag = ? AND count <= 0
			`, uid, tag).Error
		}
		if err := IncUserTagAC(ctx, db, uid, added); err != nil {
			return err
		}
	}
	return nil
}

// listUsersACProblem 找出 AC 过该题的用户：status 表 + user_ac_problems 的 p: 与 e: 键
func listUsersACProblem(ctx context.Context, db *gorm.DB, problemID uint) ([]int64, error) {
	seen := map[int64]struct{}{}
	var out []int64
	add := func(ids []int64) {
		for _, id := range ids {
			if id <= 0 {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}

	var fromStatus []int64
	if err := db.WithContext(ctx).Model(&model.UserProblemStatus{}).
		Where("problem_id = ? AND status = ?", problemID, model.UserProblemStatusAC).
		Pluck("user_id", &fromStatus).Error; err != nil {
		return nil, err
	}
	add(fromStatus)

	keys := []string{fmt.Sprintf("p:%d", problemID)}
	var p model.Problem
	if err := db.WithContext(ctx).Select("id", "platform", "external_id").First(&p, problemID).Error; err == nil {
		ext := strings.TrimSpace(p.ExternalID)
		plat := strings.TrimSpace(p.Platform)
		if ext != "" && plat != "" {
			keys = append(keys, fmt.Sprintf("e:%s:%s", plat, ext))
		}
	}
	var fromAC []int64
	if err := db.WithContext(ctx).Model(&model.UserACProblem{}).
		Where("problem_key IN ?", keys).
		Pluck("user_id", &fromAC).Error; err != nil {
		return out, err
	}
	add(fromAC)
	return out, nil
}

func diffStringSets(old, neu []string) (removed, added []string) {
	om := map[string]struct{}{}
	nm := map[string]struct{}{}
	for _, t := range old {
		om[t] = struct{}{}
	}
	for _, t := range neu {
		nm[t] = struct{}{}
	}
	for t := range om {
		if _, ok := nm[t]; !ok {
			removed = append(removed, t)
		}
	}
	for t := range nm {
		if _, ok := om[t]; !ok {
			added = append(added, t)
		}
	}
	return
}

// IncUserTagACForFirstProblemAC 用户首次绑定 AC 某 problem_id 时按题当前 tags +1
func IncUserTagACForFirstProblemAC(ctx context.Context, db *gorm.DB, userID int64, problemID uint) error {
	if db == nil || userID <= 0 || problemID == 0 {
		return nil
	}
	var p model.Problem
	if err := db.WithContext(ctx).Select("id", "tags", "status").First(&p, problemID).Error; err != nil {
		return nil // 题不存在则跳过
	}
	tags := NormalizeTags([]string(p.Tags))
	if len(tags) == 0 {
		return nil
	}
	return IncUserTagAC(ctx, db, userID, tags)
}

// ListUserTagAC 画像雷达
func ListUserTagAC(ctx context.Context, db *gorm.DB, userID int64, limit int) ([]struct {
	Tag   string
	Count int64
}, error) {
	if limit <= 0 {
		limit = 20
	}
	type row struct {
		Tag   string
		Count int64
	}
	var rows []row
	err := db.WithContext(ctx).
		Model(&model.UserTagAC{}).
		Select("tag, count").
		Where("user_id = ? AND count > 0", userID).
		Order("count DESC, tag ASC").
		Limit(limit).
		Find(&rows).Error
	out := make([]struct {
		Tag   string
		Count int64
	}, 0, len(rows))
	for _, r := range rows {
		out = append(out, struct {
			Tag   string
			Count int64
		}{r.Tag, r.Count})
	}
	return out, err
}

// CountUserTagAC 用户雷达标签行数（count>0）
func CountUserTagAC(ctx context.Context, db *gorm.DB, userID int64) (int64, error) {
	if db == nil || userID <= 0 {
		return 0, nil
	}
	var n int64
	err := db.WithContext(ctx).Model(&model.UserTagAC{}).
		Where("user_id = ? AND count > 0", userID).
		Count(&n).Error
	return n, err
}

// UserHasTaggedAC 用户是否有「已 AC 且题库有标签」的题（用于判断雷达是否应非空）
func UserHasTaggedAC(ctx context.Context, db *gorm.DB, userID int64) (bool, error) {
	if db == nil || userID <= 0 {
		return false, nil
	}
	var exists bool
	err := db.WithContext(ctx).Raw(`
		SELECT EXISTS (
			SELECT 1
			FROM (
				SELECT u.problem_key
				FROM user_ac_problems u
				JOIN problems p ON p.id = NULLIF(substring(u.problem_key, 3), '')::bigint
				JOIN problem_tags pt ON pt.problem_id = p.id
				WHERE u.user_id = ?
				  AND u.problem_key LIKE 'p:%'
				  AND u.problem_key ~ '^p:[0-9]+$'
				UNION ALL
				SELECT u.problem_key
				FROM user_ac_problems u
				JOIN problems p
				  ON p.platform = split_part(substring(u.problem_key, 3), ':', 1)
				 AND p.external_id = substring(substring(u.problem_key, 3) FROM position(':' IN substring(u.problem_key, 3)) + 1)
				 AND p.external_id IS NOT NULL AND btrim(p.external_id) <> ''
				JOIN problem_tags pt ON pt.problem_id = p.id
				WHERE u.user_id = ?
				  AND u.problem_key LIKE 'e:%'
			) t
			LIMIT 1
		)
	`, userID, userID).Scan(&exists).Error
	return exists, err
}

// ListUserIDsWithACButEmptyTagAC 有过题但雷达预聚合为空的用户（补刷候选，限 limit）
func ListUserIDsWithACButEmptyTagAC(ctx context.Context, db *gorm.DB, limit int) ([]int64, error) {
	if db == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 500
	}
	var ids []int64
	err := db.WithContext(ctx).Raw(`
		SELECT DISTINCT u.user_id
		FROM user_ac_problems u
		WHERE NOT EXISTS (
			SELECT 1 FROM user_tag_ac t
			WHERE t.user_id = u.user_id AND t.count > 0
		)
		ORDER BY u.user_id
		LIMIT ?
	`, limit).Scan(&ids).Error
	return ids, err
}

// RebuildUserTagACForUser 按 user_ac_problems × problem_tags 全量重建该用户雷达预聚合。
// 修复：爬虫写 AC 时 problem_id 多为空 → 未走 IncUserTagAC；绑题后也未补写；
// 题已有标签时不会触发标签差分，导致雷达长期为空。本函数可在 MQ 画像任务中安全调用。
//
// 查询从 user_ac_problems（按 user_id 索引）驱动，p:/e: 两个分支分别用
// problems 主键 / (platform, external_id) 唯一索引探针，避免整表 Seq Scan + OR 重 JOIN。
func RebuildUserTagACForUser(ctx context.Context, db *gorm.DB, userID int64) error {
	if db == nil || userID <= 0 {
		return nil
	}
	if !db.Migrator().HasTable(&model.UserTagAC{}) || !db.Migrator().HasTable(&model.ProblemTag{}) {
		return nil
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).Delete(&model.UserTagAC{}).Error; err != nil {
			return err
		}
		// 不强制 COMPLETED：人工打标/部分完成态只要 problem_tags 有行即计入
		res := tx.Exec(`
			INSERT INTO user_tag_ac (user_id, tag, count)
			SELECT user_id, tag, COUNT(DISTINCT pid)::bigint
			FROM (
				SELECT u.user_id AS user_id, pt.tag AS tag, p.id AS pid
				FROM user_ac_problems u
				JOIN problems p ON p.id = NULLIF(substring(u.problem_key, 3), '')::bigint
				JOIN problem_tags pt ON pt.problem_id = p.id
				WHERE u.user_id = ?
				  AND u.problem_key LIKE 'p:%'
				  AND u.problem_key ~ '^p:[0-9]+$'
				  AND pt.tag IS NOT NULL AND btrim(pt.tag) <> ''
				UNION ALL
				SELECT u.user_id, pt.tag, p.id
				FROM user_ac_problems u
				JOIN problems p
				  ON p.platform = split_part(substring(u.problem_key, 3), ':', 1)
				 AND p.external_id = substring(substring(u.problem_key, 3) FROM position(':' IN substring(u.problem_key, 3)) + 1)
				 AND p.external_id IS NOT NULL AND btrim(p.external_id) <> ''
				JOIN problem_tags pt ON pt.problem_id = p.id
				WHERE u.user_id = ?
				  AND u.problem_key LIKE 'e:%'
				  AND pt.tag IS NOT NULL AND btrim(pt.tag) <> ''
			) t
			GROUP BY user_id, tag
		`, userID, userID)
		return res.Error
	})
}


