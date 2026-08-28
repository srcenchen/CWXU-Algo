package data

import (
	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"gorm.io/gorm"
)

func backfillProblemTagsIfEmpty(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.ProblemTag{}) {
		return
	}
	var n int64
	if err := db.Model(&model.ProblemTag{}).Count(&n).Error; err != nil {
		log.Warnf("problem_tags count: %v", err)
		return
	}
	if n > 0 {
		return
	}
	log.Infof("problem_tags empty, backfill from problems.tags…")
	res := db.Exec(`
		INSERT INTO problem_tags (problem_id, tag)
		SELECT p.id, BTRIM(tag)
		FROM problems p
		CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(p.tags::jsonb, '[]'::jsonb)) AS tag
		WHERE p.tags IS NOT NULL
		  AND p.tags::text NOT IN ('', '[]', 'null')
		  AND BTRIM(tag) <> ''
		ON CONFLICT DO NOTHING
	`)
	if res.Error != nil {
		log.Warnf("problem_tags backfill failed: %v", res.Error)
		return
	}
	log.Infof("problem_tags backfill rows=%d", res.RowsAffected)
}

func backfillUserProblemStatusIfEmpty(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.UserProblemStatus{}) {
		return
	}
	var n int64
	if err := db.Model(&model.UserProblemStatus{}).Count(&n).Error; err != nil {
		log.Warnf("user_problem_status count: %v", err)
		return
	}
	if n > 0 {
		return
	}
	log.Infof("user_problem_status empty, backfill from submit_logs…")
	res1 := db.Exec(`
		INSERT INTO user_problem_status (user_id, problem_id, status, updated_at)
		SELECT user_id, problem_id, 'TRIED', NOW()
		FROM submit_logs
		WHERE problem_id IS NOT NULL AND problem_id > 0 AND user_id > 0
		GROUP BY user_id, problem_id
		ON CONFLICT (user_id, problem_id) DO NOTHING
	`)
	if res1.Error != nil {
		log.Warnf("user_problem_status backfill tried failed: %v", res1.Error)
		return
	}
	res2 := db.Exec(`
		INSERT INTO user_problem_status (user_id, problem_id, status, updated_at)
		SELECT user_id, problem_id, 'AC', NOW()
		FROM submit_logs
		WHERE problem_id IS NOT NULL AND problem_id > 0 AND user_id > 0 AND is_ac = true
		GROUP BY user_id, problem_id
		ON CONFLICT (user_id, problem_id) DO UPDATE
		SET status = 'AC', updated_at = EXCLUDED.updated_at
	`)
	if res2.Error != nil {
		log.Warnf("user_problem_status backfill ac failed: %v", res2.Error)
		return
	}
	log.Infof("user_problem_status backfill tried≈%d ac_upsert≈%d", res1.RowsAffected, res2.RowsAffected)
}

func backfillUserTagACIfEmpty(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.UserTagAC{}) {
		return
	}
	var n int64
	if err := db.Model(&model.UserTagAC{}).Count(&n).Error; err != nil {
		log.Warnf("user_tag_ac count: %v", err)
		return
	}
	if n > 0 {
		return
	}
	if !db.Migrator().HasTable(&model.ProblemTag{}) {
		return
	}
	log.Infof("user_tag_ac empty, backfill from user_ac_problems + problem_tags…")
	res := db.Exec(`
		INSERT INTO user_tag_ac (user_id, tag, count, weight, score_version, model_version)
		SELECT user_id, tag, COUNT(*)::bigint, ROUND(SUM(weight)::numeric, 2), 0, 0
		FROM (
			SELECT user_id, tag, pid, MAX(weight) AS weight
			FROM (
				SELECT u.user_id AS user_id, pt.tag AS tag, p.id AS pid,
				       `+model.DifficultyWeightSQL+` AS weight
				FROM user_ac_problems u
				JOIN problems p ON u.problem_key = 'p:' || p.id::text
				JOIN problem_tags pt ON pt.problem_id = p.id
				WHERE p.status = ?
				UNION ALL
				SELECT u.user_id AS user_id, pt.tag AS tag, p.id AS pid,
				       `+model.DifficultyWeightSQL+` AS weight
				FROM user_ac_problems u
				JOIN problems p ON p.external_id IS NOT NULL AND btrim(p.external_id) <> ''
					AND u.problem_key = 'e:' || p.platform || ':' || p.external_id
				JOIN problem_tags pt ON pt.problem_id = p.id
				WHERE p.status = ?
			) raw
			GROUP BY user_id, tag, pid
		) t
		GROUP BY user_id, tag
		ON CONFLICT (user_id, tag) DO UPDATE SET
			count = EXCLUDED.count, weight = EXCLUDED.weight,
			score_version = 0, model_version = 0
	`, model.ProblemStatusCompleted, model.ProblemStatusCompleted)
	if res.Error != nil {
		log.Warnf("user_tag_ac backfill failed: %v", res.Error)
		return
	}
	log.Infof("user_tag_ac backfill rows=%d", res.RowsAffected)
}
