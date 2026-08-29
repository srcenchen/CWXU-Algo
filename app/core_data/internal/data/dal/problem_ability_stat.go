package dal

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var problemAbilityRefreshMu sync.Mutex

type problemAbilityAggregate struct {
	ProblemID    uint
	Platform     string
	Difficulty   string
	AttemptCount float64
	ACUserCount  float64
}

type problemAbilityGroupKey struct {
	platform   string
	difficulty string
}

// The single source CTE is portable to SQLite and PostgreSQL. It normalizes
// platform identity, filters/deduplicates real terminal evidence, compresses
// sequences in the database, and returns just one aggregate record per problem.
const problemAbilityEvidenceSQL = `
WITH terminal AS (
	SELECT s.id, s.user_id, s.problem_id, s.time,
		lower(trim(s.platform)) AS platform_key, trim(s.submit_id) AS submit_id,
		CASE WHEN upper(trim(s.status)) IN ('AC', 'OK', 'ACCEPTED')
			OR trim(s.status) IN ('正确', '答案正确') THEN 1 ELSE 0 END AS is_ac
	FROM submit_logs s
	WHERE s.user_id > 0 AND trim(coalesce(s.submit_id, '')) <> '' AND trim(coalesce(s.status, '')) <> ''
		AND trim(s.status) NOT IN ('正在评测', '评测中', '等待评测', '排队中')
		AND upper(trim(s.status)) NOT IN ('TESTING', 'PENDING', 'JUDGING', 'IN_QUEUE', 'IN QUEUE', 'WAITING', 'WJ', 'QUEUE')
		AND NOT (lower(trim(s.platform)) = 'leetcode' AND lower(trim(s.submit_id)) LIKE 'lc-%')
		AND NOT (lower(trim(s.platform)) = 'uoj' AND lower(trim(s.submit_id)) LIKE 'uoj-ac-%')
),
terminal_with_problem AS (
	SELECT terminal.*, lower(trim(p.platform)) AS problem_platform_key,
		trim(coalesce(p.difficulty, '')) AS difficulty,
		CASE WHEN terminal.problem_id IS NOT NULL AND terminal.problem_id <> 0
			AND lower(trim(p.platform)) = terminal.platform_key THEN 1 ELSE 0 END AS valid_bound
	FROM terminal LEFT JOIN problems p ON p.id = terminal.problem_id
),
deduplicated AS (
	SELECT terminal_with_problem.*,
		row_number() OVER (
			PARTITION BY platform_key, submit_id
			ORDER BY valid_bound DESC, CASE WHEN problem_id IS NULL OR problem_id = 0 THEN 1 ELSE 0 END DESC, time ASC, id ASC
		) AS duplicate_rank
	FROM terminal_with_problem
),
unbound_keys AS (
	SELECT DISTINCT user_id, platform_key
	FROM deduplicated
	WHERE duplicate_rank = 1 AND valid_bound = 0
),
complete_platform_keys AS (
	SELECT DISTINCT user_id, lower(trim(platform)) AS platform_key
	FROM platforms
	WHERE lower(trim(platform)) = 'luogu' AND client_sync_completed_at IS NOT NULL
),
bound AS (
	SELECT d.id, d.user_id, d.problem_id, d.time, d.problem_platform_key AS platform_key, d.is_ac, d.difficulty
	FROM deduplicated d
	WHERE d.duplicate_rank = 1 AND d.valid_bound = 1
),
sequenced AS (
	SELECT bound.*, sum(is_ac) OVER (PARTITION BY user_id, problem_id ORDER BY time ASC, id ASC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS ac_seen
	FROM bound
),
before_or_first_ac AS (
	SELECT * FROM sequenced WHERE ac_seen = 0 OR (ac_seen = 1 AND is_ac = 1)
),
per_user_problem AS (
	SELECT b.problem_id, b.platform_key, b.difficulty, b.user_id, count(*) AS raw_attempts,
		max(b.is_ac) AS has_ac, max(CASE WHEN b.is_ac = 0 THEN 1 ELSE 0 END) AS has_failure,
		max(CASE WHEN complete.user_id IS NOT NULL AND unbound.user_id IS NULL THEN 1 ELSE 0 END) AS complete_history
	FROM before_or_first_ac b
	LEFT JOIN complete_platform_keys complete ON complete.user_id = b.user_id AND complete.platform_key = b.platform_key
	LEFT JOIN unbound_keys unbound ON unbound.user_id = b.user_id AND unbound.platform_key = b.platform_key
	GROUP BY b.problem_id, b.platform_key, b.difficulty, b.user_id
),
weighted AS (
	SELECT problem_id, platform_key, difficulty,
		CASE WHEN raw_attempts > 20 THEN 20 ELSE raw_attempts END AS capped_attempts, has_ac,
		CASE WHEN complete_history = 1 THEN 1.0 WHEN has_ac = 1 AND has_failure = 0 THEN 0.0 ELSE 0.6 END AS evidence_weight
	FROM per_user_problem
)
SELECT problem_id AS problem_id, platform_key AS platform, difficulty AS difficulty,
	sum(capped_attempts * evidence_weight) AS attempt_count, sum(has_ac * evidence_weight) AS ac_user_count
FROM weighted WHERE evidence_weight > 0
GROUP BY problem_id, platform_key, difficulty ORDER BY problem_id ASC`

// RefreshProblemAbilityStats locks the singleton before its CTE source read;
// a stale concurrent builder therefore cannot publish after a newer snapshot.
func RefreshProblemAbilityStats(ctx context.Context, db *gorm.DB) error {
	_, _, err := refreshProblemAbilityStats(ctx, db, "", nil)
	return err
}

// RefreshProblemAbilityStatsForMaintenance publishes a new active snapshot and
// the caller's durable maintenance transition in the same database commit.
// A transition failure rolls the active pointer and snapshot rows back too.
func RefreshProblemAbilityStatsForMaintenance(ctx context.Context, db *gorm.DB, transition func(context.Context, *gorm.DB, uint64) error) (uint64, error) {
	if transition == nil {
		return 0, errors.New("problem ability stats: nil maintenance transition")
	}
	version, _, err := refreshProblemAbilityStats(ctx, db, "", transition)
	return version, err
}

// RefreshProblemAbilityStatsForPeriod atomically coalesces scheduled refreshes
// for one logical period. Ad-hoc/admin RefreshProblemAbilityStats calls do not
// modify the period marker and therefore remain independent force refreshes.
func RefreshProblemAbilityStatsForPeriod(ctx context.Context, db *gorm.DB, period string) (version uint64, refreshed bool, err error) {
	period = strings.TrimSpace(period)
	if period == "" {
		return 0, false, errors.New("problem ability stats: empty refresh period")
	}
	return refreshProblemAbilityStats(ctx, db, period, nil)
}

func refreshProblemAbilityStats(ctx context.Context, db *gorm.DB, scheduledPeriod string, transition func(context.Context, *gorm.DB, uint64) error) (version uint64, refreshed bool, err error) {
	if db == nil {
		return 0, false, errors.New("problem ability stats: nil database")
	}
	// SQLite's write locking needs process serialization; PostgreSQL cross-process
	// serialization is supplied by the state-row lock inside the transaction.
	problemAbilityRefreshMu.Lock()
	defer problemAbilityRefreshMu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		now := time.Now()
		if err := db.WithContext(ctx).Exec(`INSERT INTO ability_model_state (id, active_version, built_at, updated_at)
			VALUES (?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`, 1, 0, now, now).Error; err != nil {
			return 0, false, err
		}
		var baseline model.AbilityModelState
		if err := db.WithContext(ctx).First(&baseline, 1).Error; err != nil {
			return 0, false, err
		}
		if scheduledPeriod != "" && baseline.LastScheduledRefreshPeriod == scheduledPeriod {
			return baseline.ActiveVersion, false, nil
		}
		var evidence []problemAbilityAggregate
		if err := db.WithContext(ctx).Raw(problemAbilityEvidenceSQL).Scan(&evidence).Error; err != nil {
			return 0, false, err
		}
		// Group/posterior reduction is CPU work and belongs outside the state-row
		// publication lock together with the full evidence scan.
		rows := reduceProblemAbilityStats(evidence, 0, now)
		retry := false
		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var state model.AbilityModelState
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&state, 1).Error; err != nil {
				return err
			}
			if scheduledPeriod != "" && state.LastScheduledRefreshPeriod == scheduledPeriod {
				version = state.ActiveVersion
				refreshed = false
				return nil
			}
			if state.ActiveVersion != baseline.ActiveVersion {
				retry = true
				return nil
			}
			newVersion, err := nextAbilityModelVersion(state.ActiveVersion)
			if err != nil {
				return err
			}
			for i := range rows {
				rows[i].ModelVersion = newVersion
			}
			if len(rows) > 0 {
				if err := tx.CreateInBatches(rows, 500).Error; err != nil {
					return err
				}
			}
			if state.ActiveVersion > 0 {
				if err := tx.Where("model_version <> ? AND model_version <> ?", state.ActiveVersion, newVersion).Delete(&model.ProblemAbilityStat{}).Error; err != nil {
					return err
				}
			}
			// ActiveVersion is the ready/status marker, updated only after all rows.
			updates := map[string]interface{}{
				"active_version": newVersion, "built_at": now, "updated_at": now,
			}
			if scheduledPeriod != "" {
				updates["last_scheduled_refresh_period"] = scheduledPeriod
			}
			res := tx.Model(&model.AbilityModelState{}).Where("id = ?", state.ID).Updates(updates)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return errors.New("problem ability stats: active state publication updated zero rows")
			}
			if transition != nil {
				if err := transition(ctx, tx, newVersion); err != nil {
					return err
				}
			}
			version = newVersion
			refreshed = true
			return nil
		})
		if err != nil {
			return 0, false, err
		}
		if retry {
			continue
		}
		return version, refreshed, nil
	}
}

func nextAbilityModelVersion(active uint64) (uint64, error) {
	if active >= uint64(math.MaxInt64) {
		return 0, errors.New("ability model version exhausted")
	}
	return active + 1, nil
}

// reduceProblemAbilityStats is pure and receives only compact per-problem N/A.
func reduceProblemAbilityStats(evidence []problemAbilityAggregate, version uint64, builtAt time.Time) []model.ProblemAbilityStat {
	groups := make(map[problemAbilityGroupKey]problemAbilityAggregate)
	for _, e := range evidence {
		e.Platform = normalizeAbilityPlatform(e.Platform)
		profile := DifficultyAbilityProfile(e.Difficulty)
		key := problemAbilityGroupKey{platform: e.Platform, difficulty: profile.Key}
		g := groups[key]
		g.AttemptCount += e.AttemptCount
		g.ACUserCount += e.ACUserCount
		groups[key] = g
	}
	rows := make([]model.ProblemAbilityStat, 0, len(evidence))
	for _, e := range evidence {
		e.Platform = normalizeAbilityPlatform(e.Platform)
		profile := DifficultyAbilityProfile(e.Difficulty)
		g := groups[problemAbilityGroupKey{platform: e.Platform, difficulty: profile.Key}]
		mu := (g.ACUserCount + 200*profile.Prior) / (g.AttemptCount + 200)
		posterior := (e.ACUserCount + 30*mu) / (e.AttemptCount + 30)
		rows = append(rows, model.ProblemAbilityStat{
			ModelVersion: version, ProblemID: e.ProblemID, Platform: e.Platform, Difficulty: profile.Key,
			AttemptCount: e.AttemptCount, ACUserCount: e.ACUserCount, GroupPriorRate: mu, PosteriorACRate: posterior,
			Hardness: ProblemHardness(e.Difficulty, g.ACUserCount, g.AttemptCount, e.ACUserCount, e.AttemptCount),
			BuiltAt:  builtAt, UpdatedAt: builtAt,
		})
	}
	return rows
}

func normalizeAbilityPlatform(platform string) string {
	return strings.ToLower(strings.TrimSpace(platform))
}

// ActiveAbilityModelVersion reads the pointer directly and remains useful when
// the current snapshot contains no problem rows.
func ActiveAbilityModelVersion(ctx context.Context, db *gorm.DB) (uint64, error) {
	if db == nil {
		return 0, errors.New("problem ability stats: nil database")
	}
	var state model.AbilityModelState
	if err := db.WithContext(ctx).First(&state, 1).Error; err != nil {
		return 0, err
	}
	return state.ActiveVersion, nil
}

// ActiveProblemAbilityStats reads through the active-version pointer in the
// same SQL statement, preventing consumers from observing a mixed snapshot.
func ActiveProblemAbilityStats(ctx context.Context, db *gorm.DB, problemIDs []uint) ([]model.ProblemAbilityStat, error) {
	if db == nil {
		return nil, errors.New("problem ability stats: nil database")
	}
	q := db.WithContext(ctx).Where("model_version = (SELECT active_version FROM ability_model_state WHERE id = ?)", 1)
	if len(problemIDs) > 0 {
		q = q.Where("problem_id IN ?", problemIDs)
	}
	var rows []model.ProblemAbilityStat
	if err := q.Order("problem_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
