package data

import (
	"context"
	"os"
	"strings"
	"time"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/conf"
	gorm2 "cwxu-algo/app/common/data/gorm"
	redis2 "cwxu-algo/app/common/data/redis"
	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spidermetrics"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// ProviderSet is data providers.
var ProviderSet = wire.NewSet(NewData, NewDataDB, NewDataRDB)

const solutionImageMigrationBatchSize = 200

// NewDataDB 从 Data 中提取 DB
func NewDataDB(data *Data) *gorm.DB {
	return data.DB
}

// NewDataRDB 从 Data 中提取 RDB
func NewDataRDB(data *Data) *redis.Client {
	return data.RDB
}

// Data .
type Data struct {
	DB     *gorm.DB
	UserDB *gorm.DB // optional: algo_user（写站内通知）
	RDB    *redis.Client
}

// NewData .
func NewData(c *conf.Data) (*Data, func(), error) {
	data := &Data{DB: gorm2.InitGorm(c), RDB: redis2.InitRedis(c)}
	mail.SetStatusReporter(func(ok bool, errMsg string) {
		st, msg := sitesettings.StatusFail, errMsg
		if ok {
			st, msg = sitesettings.StatusOK, ""
		}
		sitesettings.SetServiceStatus(context.Background(), data.RDB, sitesettings.ServiceSmtp, st, msg)
	})
	if udb := openUserDB(c); udb != nil {
		data.UserDB = udb
		log.Info("notify: user database connected")
	} else {
		log.Warn("notify: user database not configured; mention/review notifications will be skipped")
	}
	migrateModels(data.DB)
	spidermetrics.BindRedis(data.RDB)
	cleanup := func() {
		log.Info("closing the data resources")
		sql, _ := data.DB.DB()
		sql.Close()
		if data.UserDB != nil {
			if s, err := data.UserDB.DB(); err == nil {
				_ = s.Close()
			}
		}
	}
	return data, cleanup, nil
}

// openUserDB 连接 algo_user 以便 core 写站内通知。
// Priority: CWXU_USER_DATABASE_SOURCE → derive from core DSN (algo_core_data → algo_user).
func openUserDB(c *conf.Data) *gorm.DB {
	src := strings.TrimSpace(os.Getenv("CWXU_USER_DATABASE_SOURCE"))
	if src == "" && c != nil && c.Database != nil {
		u := c.Database.Source
		if strings.Contains(u, "dbname=algo_core_data") {
			src = strings.Replace(u, "dbname=algo_core_data", "dbname=algo_user", 1)
		}
	}
	if src == "" {
		return nil
	}
	db, err := gorm.Open(postgres.Open(src), &gorm.Config{
		Logger:      logger.Default.LogMode(logger.Warn),
		PrepareStmt: true,
	})
	if err != nil {
		log.Warnf("notify: open user database failed: %v", err)
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Warnf("notify: user database pool: %v", err)
		return nil
	}
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(2)
	if err := sqlDB.Ping(); err != nil {
		log.Warnf("notify: user database ping failed: %v", err)
		_ = sqlDB.Close()
		return nil
	}
	return db
}

// migrateModels 合并
func migrateModels(db *gorm.DB) {
	reconcilePlatformDuplicates(db)
	// 旧 daily_user_stats 无 platform：先改名，再 AutoMigrate 建新表
	prepareDailyUserStatsPlatform(db)
	err := db.AutoMigrate(
		&model.SubmitLog{},
		&model.Platform{},
		&model.ContestLog{},
		&model.ContestProblemEnsure{},
		&model.ContestProblem{},
		&model.ContestUserProblem{},
		&model.Bulletin{},
		&model.Problem{},
		&model.ProblemEditRequest{},
		&model.EmergencyNotice{},
		&model.DailyUserStat{},
		&model.UserACProblem{},
		&model.UserACProblemDay{},
		&model.SpiderRepairState{},
		&model.ContestCalendar{},
		&model.ContestCalendarSub{},
		&model.ContestCalendarNotifyLog{},
		&model.ProblemTag{},
		&model.UserProblemStatus{},
		&model.UserTagAC{},
		&model.ProblemComment{},
		&model.ProblemUserSolution{},
		&model.ActivityFeed{},
		&model.CommunityLike{},
		&model.CommunityReport{},
		&model.CommunityViewUV{},
		&model.Problemset{},
		&model.ProblemsetItem{},
		&model.ProblemsetLike{},
		&model.ProblemsetFavorite{},
		&schemaPatch{},
	)
	if err != nil {
		panic("数据库：数据库自动合并失败")
	}
	migrateContestLogUnique(db)
	migrateContestLogListIndexes(db)
	// 旧顶层评论 root_id=0 → 回填为自身 id（层级评论依赖）
	_ = db.Exec(`UPDATE problem_comments SET root_id = id WHERE parent_id = 0 AND (root_id = 0 OR root_id IS NULL)`).Error
	// 丢弃旧无 platform 日汇总（清洗任务会从 submit_logs 全量重建）
	_ = db.Exec(`DROP TABLE IF EXISTS daily_user_stats_pre_platform`).Error
	// 废弃：预聚合去重改以 submit_logs.submit_id 为准，不再维护账本表
	_ = db.Exec(`DROP TABLE IF EXISTS counted_submit_ids`).Error
	ensureSubmitLogPerf(db)
	// 唯一键 (platform, submit_id)：先建复合唯一，再丢弃旧单列唯一（跨平台撞号）
	migrateSubmitIDPlatformUnique(db)
	// 一次性：submit_id 误带「平台:」前缀（如 LuoGu:123）→ 纯数字/原站 id
	migrateStripPlatformPrefixSubmitIDs(db)
	// 空表兜底回填（清洗任务会覆盖重建；新环境无历史时有用）
	backfillDailyUserStatsIfEmpty(db)
	backfillUserACIfEmpty(db)
	// P8 预聚合：标签倒排 → 用户题状态 → 用户标签 AC（顺序有依赖）
	backfillProblemTagsIfEmpty(db)
	backfillUserProblemStatusIfEmpty(db)
	backfillUserTagACIfEmpty(db)
	// one-shot: zero solution view counts for UV migration
	zeroSolutionViewsOnce(db)
	// 力扣官方 ac-* 不再进日表/日 ac_cnt（与 lc-prob 双计修复）
	purgeLeetCodeOfficialACFromDailyOnce(db)
	// 题解正文图床 URL → path-only（与博客一致，换域读时展开）
	migrateSolutionImageURLsToPathOnly(db)
}

// schemaPatch one-shot migration markers (core DB).
type schemaPatch struct {
	Key       string    `gorm:"primaryKey;size:64"`
	AppliedAt time.Time `gorm:"not null"`
}

func (schemaPatch) TableName() string { return "schema_patches" }

// claimCoreSchemaPatch inserts key if absent; true when this process should run the patch.
func claimCoreSchemaPatch(db *gorm.DB, key string) bool {
	if db == nil || !db.Migrator().HasTable(&schemaPatch{}) {
		return false
	}
	var n int64
	_ = db.Model(&schemaPatch{}).Where("key = ?", key).Count(&n).Error
	if n > 0 {
		return false
	}
	if err := db.Create(&schemaPatch{Key: key, AppliedAt: time.Now()}).Error; err != nil {
		// concurrent claim
		return false
	}
	return true
}

// migrateSolutionImageURLsToPathOnly normalizes absolute blog-object image URLs
// in problem_user_solutions.content_md to /blog/{uid}/… path form.
func migrateSolutionImageURLsToPathOnly(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.ProblemUserSolution{}) {
		return
	}
	const patchKey = "solution_image_url_path_only_v1"
	var markerCount int64
	if err := db.Model(&schemaPatch{}).Where("key = ?", patchKey).Count(&markerCount).Error; err != nil {
		log.Warnf("solution image path-only query marker: %v", err)
		return
	}
	if markerCount > 0 {
		return
	}
	updated, scanned := 0, 0
	var afterID uint
	for {
		var rows []model.ProblemUserSolution
		if err := db.Select("id", "updated_at", "content_md").Where("id > ?", afterID).
			Order("id ASC").Limit(solutionImageMigrationBatchSize).Find(&rows).Error; err != nil {
			log.Warnf("solution image path-only migrate list: %v", err)
			return
		}
		if len(rows) == 0 {
			break
		}
		scanned += len(rows)
		for _, r := range rows {
			newMD := blogimg.NormalizeStoredImageRefs(r.ContentMD)
			if newMD == r.ContentMD {
				continue
			}
			res := db.Model(&model.ProblemUserSolution{}).
				Where("id = ? AND updated_at = ? AND content_md = ?", r.ID, r.UpdatedAt, r.ContentMD).
				UpdateColumn("content_md", newMD)
			if res.Error != nil {
				log.Warnf("solution image path-only migrate id=%d: %v", r.ID, res.Error)
				return
			}
			if res.RowsAffected > 0 {
				updated++
			}
		}
		afterID = rows[len(rows)-1].ID
	}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&schemaPatch{
		Key: patchKey, AppliedAt: time.Now(),
	}).Error; err != nil {
		log.Warnf("solution image path-only complete marker: %v", err)
		return
	}
	log.Infof("solution image path-only migrate: scanned=%d updated=%d", scanned, updated)
}

// purgeLeetCodeOfficialACFromDailyOnce 一次性清理：
// 1) user_ac_problem_days 中 e:LeetCode:ac-* 行（官方合成不应进「今日 AC」）
// 2) 重算力扣 daily_user_stats.ac_cnt（排除 lc-ac-*，保留 lc-prob 等）
func purgeLeetCodeOfficialACFromDailyOnce(db *gorm.DB) {
	if db == nil {
		return
	}
	const key = "lc_official_ac_daily_dedupe_v1"
	var n int64
	_ = db.Model(&schemaPatch{}).Where("key = ?", key).Count(&n).Error
	if n > 0 {
		return
	}
	if db.Migrator().HasTable(&model.UserACProblemDay{}) {
		res := db.Exec(`DELETE FROM user_ac_problem_days WHERE problem_key LIKE 'e:LeetCode:ac-%'`)
		if res.Error != nil {
			log.Warnf("purge lc official ac days: %v", res.Error)
		} else if res.RowsAffected > 0 {
			log.Infof("purged user_ac_problem_days official ac keys rows=%d", res.RowsAffected)
		}
	}
	// 重算所有用户的 LeetCode 日 ac_cnt（仅 ac 列；submit_cnt 不变）
	if db.Migrator().HasTable(&model.DailyUserStat{}) && db.Migrator().HasTable(&model.SubmitLog{}) {
		res := db.Exec(`
			UPDATE daily_user_stats d
			SET ac_cnt = COALESCE((
				SELECT COUNT(*)::bigint
				FROM submit_logs s
				WHERE s.user_id = d.user_id
				  AND date_trunc('day', s.time)::date = d.day
				  AND COALESCE(NULLIF(btrim(s.platform), ''), '?') = d.platform
				  AND s.is_ac = true
				  AND NOT (s.platform = 'LeetCode' AND s.submit_id LIKE 'lc-ac-%')
			), 0)
			WHERE d.platform = 'LeetCode'
		`)
		if res.Error != nil {
			log.Warnf("recompute leetcode daily ac_cnt: %v", res.Error)
		} else {
			log.Infof("recomputed leetcode daily ac_cnt rows=%d", res.RowsAffected)
		}
	}
	// 个人 period/heatmap 缓存靠 ver 自然失效；bump 全站 period ver 加速
	// （无 redis 句柄时跳过；下次爬虫也会 INCR）
	_ = db.Create(&schemaPatch{Key: key, AppliedAt: time.Now()}).Error
}

func zeroSolutionViewsOnce(db *gorm.DB) {
	if db == nil {
		return
	}
	const key = "solution_uv_zero_v1"
	var n int64
	_ = db.Model(&schemaPatch{}).Where("key = ?", key).Count(&n).Error
	if n > 0 {
		return
	}
	_ = db.Exec(`UPDATE problem_user_solutions SET view_count = 0 WHERE view_count IS NOT NULL OR view_count IS NULL`).Error
	// also recompute comment_count from problem_comments when column exists
	_ = db.Exec(`
UPDATE problem_user_solutions s
SET comment_count = (
  SELECT COUNT(*) FROM problem_comments c WHERE c.solution_id = s.id
)
`).Error
	_ = db.Create(&schemaPatch{Key: key, AppliedAt: time.Now()}).Error
}

// migrateSubmitIDPlatformUnique 唯一键从单列 submit_id 迁到 (platform, submit_id)。
// 旧单列唯一保证无重复，先建复合唯一必成功；随后按 pg_constraint / pg_indexes
// 实际名字防御性删除旧单列唯一约束/索引（gorm 历史版本命名不一）。
func migrateSubmitIDPlatformUnique(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.SubmitLog{}) {
		return
	}
	const key = "submit_id_platform_unique_v1"
	var n int64
	_ = db.Model(&schemaPatch{}).Where("key = ?", key).Count(&n).Error
	if n > 0 {
		return
	}
	// 1) 复合唯一索引（AutoMigrate 也会建；IF NOT EXISTS 幂等兜底）
	if err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_submit_plat_sid
		ON submit_logs (platform, submit_id)
	`).Error; err != nil {
		log.Warnf("database: create (platform, submit_id) unique index: %v", err)
		return
	}
	// 2) 查旧单列唯一约束（如 submit_logs_submit_id_key / uni_submit_logs_submit_id）
	var conNames []string
	_ = db.Raw(`
		SELECT c.conname
		FROM pg_constraint c
		WHERE c.conrelid = 'submit_logs'::regclass
		  AND c.contype = 'u'
		  AND array_length(c.conkey, 1) = 1
		  AND (
			SELECT a.attname FROM pg_attribute a
			WHERE a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
		  ) = 'submit_id'
	`).Scan(&conNames).Error
	for _, name := range conNames {
		if err := db.Exec(`ALTER TABLE submit_logs DROP CONSTRAINT IF EXISTS ` + pgQuoteIdent(name)).Error; err != nil {
			log.Warnf("database: drop old submit_id unique constraint %s: %v", name, err)
			return
		}
		log.Infof("database: dropped submit_logs unique constraint %s", name)
	}
	// 3) 查残留的旧单列唯一索引（如 idx_submit_logs_submit_id）
	var idxNames []string
	_ = db.Raw(`
		SELECT indexname FROM pg_indexes
		WHERE tablename = 'submit_logs'
		  AND indexname <> 'idx_submit_plat_sid'
		  AND indexdef LIKE 'CREATE UNIQUE INDEX%'
		  AND indexdef LIKE '%(submit_id)%'
	`).Scan(&idxNames).Error
	for _, name := range idxNames {
		if err := db.Exec(`DROP INDEX IF EXISTS ` + pgQuoteIdent(name)).Error; err != nil {
			log.Warnf("database: drop old submit_id unique index %s: %v", name, err)
			return
		}
		log.Infof("database: dropped submit_logs unique index %s", name)
	}
	_ = db.Create(&schemaPatch{Key: key, AppliedAt: time.Now()}).Error
	log.Infof("database: submit_logs unique key migrated to (platform, submit_id)")
}

// pgQuoteIdent 双引号安全包裹标识符（名字来自系统目录，防御性转义）
func pgQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// migrateStripPlatformPrefixSubmitIDs 一次性清洗 submit_id 误带平台前缀的脏数据。
// 例：LuoGu:286690434 → 286690434（否则外链变成 /record/LuoGu:286690434）。
// 若清洗后 id 与已有行冲突，删除带前缀的脏行，保留已有纯 id 行。
func migrateStripPlatformPrefixSubmitIDs(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.SubmitLog{}) {
		return
	}
	const key = "submit_id_strip_platform_prefix_v1"
	var n int64
	_ = db.Model(&schemaPatch{}).Where("key = ?", key).Count(&n).Error
	if n > 0 {
		return
	}
	// PostgreSQL：已知平台前缀
	const prefixRe = `^(LuoGu|Luogu|LUOGU|CodeForces|Codeforces|CODEFORCES|CF|AtCoder|Atcoder|ATCODER|NowCoder|Nowcoder|NOWCODER|LeetCode|Leetcode|LEETCODE|QOJ|Qoj):`
	// 1) 已有纯 id 时删掉带前缀重复行
	delRes := db.Exec(`
		DELETE FROM submit_logs a
		USING submit_logs b
		WHERE a.submit_id ~ ?
		  AND b.submit_id = regexp_replace(a.submit_id, ?, '')
		  AND a.id <> b.id
	`, prefixRe, prefixRe)
	if delRes.Error != nil {
		log.Warnf("database: strip submit_id prefix delete dups: %v", delRes.Error)
		return
	}
	// 2) 其余带前缀行改为纯 id
	updRes := db.Exec(`
		UPDATE submit_logs
		SET submit_id = regexp_replace(submit_id, ?, '')
		WHERE submit_id ~ ?
	`, prefixRe, prefixRe)
	if updRes.Error != nil {
		log.Warnf("database: strip submit_id prefix update: %v", updRes.Error)
		return
	}
	log.Infof("database: strip platform-prefix submit_id deleted=%d updated=%d",
		delRes.RowsAffected, updRes.RowsAffected)
	_ = db.Create(&schemaPatch{Key: key, AppliedAt: time.Now()}).Error
}

// prepareDailyUserStatsPlatform 旧 PK (user_id,day) → 新 PK (user_id,day,platform)
func prepareDailyUserStatsPlatform(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable("daily_user_stats") {
		return
	}
	var n int64
	_ = db.Raw(`
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'daily_user_stats' AND column_name = 'platform'
	`).Scan(&n).Error
	if n > 0 {
		return
	}
	log.Infof("database: renaming daily_user_stats → daily_user_stats_pre_platform (add platform PK)")
	if err := db.Exec(`ALTER TABLE daily_user_stats RENAME TO daily_user_stats_pre_platform`).Error; err != nil {
		log.Warnf("database: rename daily_user_stats failed: %v", err)
	}
}

// backfillDailyUserStatsIfEmpty 空表时从 submit_logs 全量聚合（含 platform）
func backfillDailyUserStatsIfEmpty(db *gorm.DB) {
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

// migrateContestLogUnique 唯一键改为 (platform, user_id, contest_id)，避免力扣与其它平台撞 contest_id。
func migrateContestLogUnique(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.ContestLog{}) {
		return
	}
	// 旧 GORM 索引名可能是 idx_contest_user
	for _, name := range []string{
		"idx_contest_user",
		"uni_contest_logs_user_id_contest_id",
		"contest_logs_user_id_contest_id_key",
	} {
		_ = db.Exec(`DROP INDEX IF EXISTS ` + name).Error
	}
	// 新唯一索引（IF NOT EXISTS 可重复执行）
	if err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_contest_plat_user_cid
		ON contest_logs (platform, user_id, contest_id)
	`).Error; err != nil {
		log.Warnf("migrate contest_logs unique index: %v", err)
	}
}

// migrateContestLogListIndexes /core/contest/list 去重翻页用的覆盖索引。
// DISTINCT ON (platform, contest_id) ORDER BY time DESC 与按 user_id 过滤都受益。
func migrateContestLogListIndexes(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.ContestLog{}) {
		return
	}
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS idx_contest_logs_time_id ON contest_logs (time DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_contest_logs_user_time ON contest_logs (user_id, time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_contest_logs_plat_cid_time ON contest_logs (platform, contest_id, time DESC, id DESC)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			log.Warnf("migrate contest_logs list index: %v", err)
		}
	}
}

// backfillUserACIfEmpty 生涯/按日 AC 去重表空则从明细回填
func backfillUserACIfEmpty(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.UserACProblem{}) {
		return
	}
	var n int64
	if err := db.Model(&model.UserACProblem{}).Count(&n).Error; err != nil {
		log.Warnf("user_ac_problems count failed: %v", err)
		return
	}
	if n == 0 {
		log.Infof("user_ac_problems empty, backfill from submit_logs…")
		res := db.Exec(`
			INSERT INTO user_ac_problems (user_id, problem_key, platform, first_ac_at)
			SELECT user_id, problem_key, platform, MIN(time) AS first_ac_at
			FROM (
				SELECT
					user_id,
					time,
					COALESCE(NULLIF(btrim(platform), ''), '?') AS platform,
					COALESCE(
						CASE WHEN problem_id IS NOT NULL AND problem_id <> 0 THEN 'p:' || problem_id::text END,
						CASE WHEN external_id IS NOT NULL AND btrim(external_id) <> '' THEN 'e:' || platform || ':' || external_id END,
						'n:' || platform || ':' || COALESCE(problem, '')
					) AS problem_key
				FROM submit_logs
				WHERE is_ac = true
			) t
			GROUP BY user_id, problem_key, platform
			ON CONFLICT (user_id, problem_key) DO NOTHING
		`)
		if res.Error != nil {
			log.Warnf("user_ac_problems backfill failed: %v", res.Error)
		} else {
			log.Infof("user_ac_problems backfill rows=%d", res.RowsAffected)
		}
	}

	if !db.Migrator().HasTable(&model.UserACProblemDay{}) {
		return
	}
	var nd int64
	if err := db.Model(&model.UserACProblemDay{}).Count(&nd).Error; err != nil {
		return
	}
	if nd > 0 {
		return
	}
	log.Infof("user_ac_problem_days empty, backfill from submit_logs…")
	res := db.Exec(`
		INSERT INTO user_ac_problem_days (user_id, day, problem_key, platform)
		SELECT DISTINCT
			user_id,
			date_trunc('day', time)::date AS day,
			problem_key,
			platform
		FROM (
			SELECT
				user_id,
				time,
				COALESCE(NULLIF(btrim(platform), ''), '?') AS platform,
				COALESCE(
					CASE WHEN problem_id IS NOT NULL AND problem_id <> 0 THEN 'p:' || problem_id::text END,
					CASE WHEN external_id IS NOT NULL AND btrim(external_id) <> '' THEN 'e:' || platform || ':' || external_id END,
					'n:' || platform || ':' || COALESCE(problem, '')
				) AS problem_key
			FROM submit_logs
			WHERE is_ac = true
			  AND NOT (platform = 'LeetCode' AND submit_id LIKE 'lc-ac-%')
		) t
		WHERE problem_key NOT LIKE 'e:LeetCode:ac-%'
		ON CONFLICT (user_id, day, problem_key) DO NOTHING
	`)
	if res.Error != nil {
		log.Warnf("user_ac_problem_days backfill failed: %v", res.Error)
		return
	}
	log.Infof("user_ac_problem_days backfill rows=%d", res.RowsAffected)
}

// ensureSubmitLogPerf 回填 is_ac + 补性能索引（10w+ 提交 / 日增 2w 场景）
// 幂等：可重复执行；启动时同步建索引（数据量尚小时可接受）。
func ensureSubmitLogPerf(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.SubmitLog{}) {
		return
	}
	// 历史行回填 is_ac（仅 false → true，可重复）
	res := db.Exec(`
		UPDATE submit_logs
		SET is_ac = true
		WHERE is_ac = false
		  AND UPPER(BTRIM(status)) IN ('AC', 'OK', 'ACCEPTED', '正确', '答案正确')
	`)
	if res.Error != nil {
		log.Warnf("submit_logs is_ac backfill failed: %v", res.Error)
	} else if res.RowsAffected > 0 {
		log.Infof("submit_logs is_ac backfill rows=%d", res.RowsAffected)
	}

	indexSQLs := []string{
		`CREATE INDEX IF NOT EXISTS idx_submit_user_isac_time ON submit_logs (user_id, is_ac, time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_submit_isac_time ON submit_logs (time DESC) WHERE is_ac = true`,
		// 热题聚合：近窗 time 过滤 + 有 problem_id
		`CREATE INDEX IF NOT EXISTS idx_submit_time_problem ON submit_logs (time DESC)
			WHERE problem_id IS NOT NULL AND problem_id > 0`,
		`CREATE INDEX IF NOT EXISTS idx_submit_user_time_nonsynthetic ON submit_logs (user_id, time DESC)
			WHERE ` + model.SQLExcludeLeetCodeNonSubmit,
		`CREATE INDEX IF NOT EXISTS idx_submit_time_nonsynthetic ON submit_logs (time DESC)
			WHERE ` + model.SQLExcludeLeetCodeNonSubmit,
		`CREATE INDEX IF NOT EXISTS idx_daily_stats_day ON daily_user_stats (day)`,
		`CREATE INDEX IF NOT EXISTS idx_daily_stats_day_user ON daily_user_stats (day, user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_daily_stats_user_plat ON daily_user_stats (user_id, platform)`,
		`CREATE INDEX IF NOT EXISTS idx_uac_day_user ON user_ac_problem_days (user_id, day)`,
		`CREATE INDEX IF NOT EXISTS idx_uac_user_first ON user_ac_problems (user_id, first_ac_at)`,
		`CREATE INDEX IF NOT EXISTS idx_uac_user_plat ON user_ac_problems (user_id, platform)`,
		`CREATE INDEX IF NOT EXISTS idx_uac_day_plat ON user_ac_problem_days (user_id, platform)`,
	}
	for _, sql := range indexSQLs {
		if err := db.Exec(sql).Error; err != nil {
			log.Warnf("submit_logs index ensure failed: %v sql=%s", err, sql)
		}
	}
}

// reconcilePlatformDuplicates prepares historical bindings for the new
// (user_id, platform) unique index. Submission and contest rows reference that
// natural key rather than platforms.id, so retaining the newest binding is safe.
func reconcilePlatformDuplicates(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.Platform{}) {
		return
	}
	result := db.Exec(`
		DELETE FROM platforms
		WHERE id IN (
			SELECT id FROM (
				SELECT id,
					ROW_NUMBER() OVER (PARTITION BY user_id, platform ORDER BY id DESC) AS duplicate_rank
				FROM platforms
			) AS duplicate_rows
			WHERE duplicate_rank > 1
		)
	`)
	if result.Error != nil {
		panic("数据库：OJ 绑定历史重复数据归并失败")
	}
	if result.RowsAffected > 0 {
		log.Warnf("database migration removed %d duplicate platform bindings", result.RowsAffected)
	}
}
