package data

import (
	"context"
	"os"
	"strings"
	"time"

	"cwxu-algo/app/common/conf"
	gorm2 "cwxu-algo/app/common/data/gorm"
	redis2 "cwxu-algo/app/common/data/redis"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// siteSettingsRefreshInterval 定期把 site_configs 刷进 Redis，供 core_data/agent 读 SMTP。
// 与 sitesettings.RedisTTL 配合：即使缓存被误清/毒缓存被剔除，也会在数分钟内恢复。
const siteSettingsRefreshInterval = 3 * time.Minute

// ProviderSet is data providers.
var ProviderSet = wire.NewSet(NewData)

// Data .
type Data struct {
	DB     *gorm.DB
	CoreDB *gorm.DB // optional: algo_core_data for site backup
	RDB    *redis.Client
}

// NewData .
func NewData(c *conf.Data) (*Data, func(), error) {
	data := &Data{DB: gorm2.InitGorm(c), RDB: redis2.InitRedis(c)}
	if core := openCoreDB(c); core != nil {
		data.CoreDB = core
		log.Info("backup: core database connected")
	} else {
		log.Warn("backup: core database not configured; full site export/import of training data will fail")
	}
	migrateModels(data.DB)
	PublishSiteSettings(data)
	stopRefresh := startSiteSettingsRefresh(data)
	cleanup := func() {
		stopRefresh()
		log.Info("closing the data resources")
		sql, _ := data.DB.DB()
		sql.Close()
		if data.CoreDB != nil {
			if s, err := data.CoreDB.DB(); err == nil {
				_ = s.Close()
			}
		}
		data.RDB.Close()
	}
	return data, cleanup, nil
}

// openCoreDB connects to algo_core_data for backup.
// Priority: CWXU_CORE_DATABASE_SOURCE env → derive from user DSN (algo_user → algo_core_data).
func openCoreDB(c *conf.Data) *gorm.DB {
	src := strings.TrimSpace(os.Getenv("CWXU_CORE_DATABASE_SOURCE"))
	if src == "" && c != nil && c.Database != nil {
		u := c.Database.Source
		if strings.Contains(u, "dbname=algo_user") {
			src = strings.Replace(u, "dbname=algo_user", "dbname=algo_core_data", 1)
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
		log.Warnf("backup: open core database failed: %v", err)
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Warnf("backup: core database pool: %v", err)
		return nil
	}
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(2)
	if err := sqlDB.Ping(); err != nil {
		log.Warnf("backup: core database ping failed: %v", err)
		_ = sqlDB.Close()
		return nil
	}
	return db
}

// migrateModels 合并
func migrateModels(db *gorm.DB) {
	reconcileOrgJoinRequestDuplicates(db)
	err := db.AutoMigrate(
		&model.User{},
		&model.Group{},
		&model.SiteConfig{},
		&model.Org{},
		&model.OrgMember{},
		&model.OrgJoinRequest{},
			&model.Squad{},
			&model.SquadMember{},
			&model.OrgScopeGrant{},
		&model.PlanQuota{},
		&model.Paste{},
		&model.SiteVisitDaily{},
		&model.BackupJob{},
		&model.UserFollow{},
		&model.Notification{},
		&model.BlogArticle{},
		&model.BlogCategory{},
		&model.BlogArticleOrg{},
		&model.BlogTag{},
		&model.BlogArticleTag{},
		&model.BlogComment{},
		&model.BlogCommentLike{},
		&model.BlogLike{},
		&model.BlogArticleViewUV{},
		&model.BlogReport{},
		&model.SchemaPatch{},
		&model.BlogThemeFlag{},
		&model.BlogSiteConfig{},
		&model.BlogImageAsset{},
		&model.Role{},
		&model.RolePermission{},
		&model.UserRole{},
		&model.OrgRolePerm{},
	)
	if err != nil {
		panic("数据库：数据库自动合并失败")
	}
	seedPlanQuotas(db)
	seedGoAlgoFramework(db)
	seedRbac(db)
	backfillLastLoginAt(db)
	ensureSiteInactiveDays(db)
	ensureBackupActiveJobIndex(db)
	backfillBlogModerationApproved(db)
	backfillBlogActivationForExistingAuthors(db)
	backfillBlogAutoSurfaceAndZeroViews(db)
}

// claimSchemaPatch 认领一次性数据修补：以 key 唯一插入，成功者执行、失败者跳过。
// 避免每次发版全表回填与多实例并发重复执行。
func claimSchemaPatch(db *gorm.DB, key string) bool {
	if db == nil || !db.Migrator().HasTable(&model.SchemaPatch{}) {
		return false
	}
	res := db.Exec(`INSERT INTO schema_patches (key, applied_at) VALUES (?, NOW()) ON CONFLICT (key) DO NOTHING`, key)
	if res.Error != nil {
		log.Warnf("claim schema patch %s: %v", key, res.Error)
		return false
	}
	return res.RowsAffected > 0
}

// ensureBackupActiveJobIndex 并发保护：同 kind 同时只允许一个 pending/running 备份任务。
// 与 service 层 hasActiveJob 检查配合，消除「检查-创建」竞态。
func ensureBackupActiveJobIndex(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.BackupJob{}) {
		return
	}
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uniq_backup_jobs_active_kind
		ON backup_jobs (kind) WHERE status IN ('pending', 'running')`).Error; err != nil {
		log.Warnf("ensure backup active job unique index: %v", err)
	}
}

// backfillBlogModerationApproved 旧文章默认视为已通过审核（SchemaPatch 一次性执行）
func backfillBlogModerationApproved(db *gorm.DB) {
	if db == nil || !db.Migrator().HasColumn(&model.BlogArticle{}, "moderation_status") {
		return
	}
	if !claimSchemaPatch(db, "blog_moderation_approved_backfill_v1") {
		return
	}
	_ = db.Exec(`
		UPDATE blog_articles
		SET moderation_status = 'approved'
		WHERE moderation_status IS NULL OR moderation_status = ''
	`).Error
}

// backfillBlogActivationForExistingAuthors 已有文章/主题配置的用户视为已开通（免二次签署；SchemaPatch 一次性执行）
func backfillBlogActivationForExistingAuthors(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.BlogSiteConfig{}) {
		return
	}
	if !claimSchemaPatch(db, "blog_activation_legacy_backfill_v1") {
		return
	}
	// 已有 site_config 但未签协议
	_ = db.Exec(`
		UPDATE blog_site_configs
		SET agreement_version = COALESCE(NULLIF(agreement_version, ''), 'v1-cn-2026-legacy'),
		    agreement_accepted_at = COALESCE(agreement_accepted_at, created_at, NOW()),
		    activated_at = COALESCE(activated_at, created_at, NOW())
		WHERE agreement_accepted_at IS NULL
	`).Error
	// 有文章但无 site_config 的作者
	_ = db.Exec(`
		INSERT INTO blog_site_configs (created_at, updated_at, user_id, theme_id, subtitle, social_links,
			activated_at, agreement_version, agreement_accepted_at, email_notify_enabled, email_notify_strategy)
		SELECT NOW(), NOW(), a.user_id, 'mizuki', '', '[]',
			MIN(a.created_at), 'v1-cn-2026-legacy', MIN(a.created_at), false, 'off'
		FROM blog_articles a
		WHERE NOT EXISTS (SELECT 1 FROM blog_site_configs c WHERE c.user_id = a.user_id)
		GROUP BY a.user_id
		ON CONFLICT (user_id) DO NOTHING
	`).Error
}

const (
	patchBlogAutoSurfaceUV       = "blog_auto_surface_uv_v1"
	patchBlogRecommendManualOnly = "blog_recommend_manual_only_v2"
)

// backfillBlogAutoSurfaceAndZeroViews:
// 1) 公开文章自动 sync_to_main_profile（历史回填，SchemaPatch 一次性）；recommend 改由站管/审核员手动设精选
// 2) 为公开文补全作者所属组织的发现同步（历史回填，SchemaPatch 一次性；新文章由写入路径 applyAutoOrgSurface 保证）
// 3) 浏览量按 UV 重计：历史 view_count 清零（一次性）
// 4) 一次性清空历史自动 recommend，避免广场「精选」默认全量
func backfillBlogAutoSurfaceAndZeroViews(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.BlogArticle{}) {
		return
	}
	// 历史数据回填挂在 SchemaPatch 下：避免每次发版全表 UPDATE/INSERT-SELECT 与多实例并发执行；
	// UPDATE 追加幂等 WHERE，仅改写需要变更的行
	if claimSchemaPatch(db, "blog_auto_surface_backfill_v1") {
		// 资料同步仍自动；精选(recommend) 不再自动打开
		_ = db.Exec(`
UPDATE blog_articles
SET sync_to_main_profile = true
WHERE visibility = 'public' AND sync_to_main_profile = false
`).Error
		_ = db.Exec(`
UPDATE blog_articles
SET sync_to_main_profile = false, recommend = false
WHERE visibility <> 'public' AND (sync_to_main_profile = true OR recommend = true)
`).Error
		// ensure public articles have org sync rows for all author memberships
		_ = db.Exec(`
INSERT INTO blog_article_orgs (created_at, article_id, org_id)
SELECT NOW(), a.id, m.org_id
FROM blog_articles a
JOIN org_members m ON m.user_id = a.user_id
WHERE a.visibility = 'public'
  AND NOT EXISTS (
    SELECT 1 FROM blog_article_orgs o
    WHERE o.article_id = a.id AND o.org_id = m.org_id
  )
`).Error
		// private org sync also implies public domain
		_ = db.Exec(`
INSERT INTO blog_article_orgs (created_at, article_id, org_id)
SELECT NOW(), o.article_id, pub.id
FROM blog_article_orgs o
JOIN orgs priv ON priv.id = o.org_id AND priv.is_system = false
CROSS JOIN orgs pub ON pub.is_system = true
WHERE NOT EXISTS (
  SELECT 1 FROM blog_article_orgs x
  WHERE x.article_id = o.article_id AND x.org_id = pub.id
)
`).Error
	}

	// one-shot zero views for UV migration
	if claimSchemaPatch(db, patchBlogAutoSurfaceUV) {
		_ = db.Exec(`UPDATE blog_articles SET view_count = 0`).Error
	}
	// one-shot: clear auto-featured recommend so 精选 only after staff picks
	if claimSchemaPatch(db, patchBlogRecommendManualOnly) {
		_ = db.Exec(`UPDATE blog_articles SET recommend = false`).Error
	}
}

// backfillLastLoginAt 避免上线瞬间全员被判休眠
func backfillLastLoginAt(db *gorm.DB) {
	if db == nil || !db.Migrator().HasColumn(&model.User{}, "last_login_at") {
		return
	}
	if err := db.Exec(`
		UPDATE users
		SET last_login_at = COALESCE(updated_at, created_at, NOW())
		WHERE last_login_at IS NULL
	`).Error; err != nil {
		log.Warnf("backfill last_login_at: %v", err)
	}
}

// ensureSiteInactiveDays 旧行补默认 14
func ensureSiteInactiveDays(db *gorm.DB) {
	if db == nil || !db.Migrator().HasColumn(&model.SiteConfig{}, "inactive_days") {
		return
	}
	if err := db.Exec(`
		UPDATE site_configs SET inactive_days = 14
		WHERE inactive_days IS NULL OR inactive_days <= 0
	`).Error; err != nil {
		log.Warnf("ensure inactive_days: %v", err)
	}
}

// reconcileOrgJoinRequestDuplicates prepares legacy data for the composite
// unique index. Older deployments allowed repeated applications; keep the most
// recently inserted row (highest id) and remove only older duplicates.
func reconcileOrgJoinRequestDuplicates(db *gorm.DB) {
	if db == nil || !db.Migrator().HasTable(&model.OrgJoinRequest{}) {
		return
	}
	result := db.Exec(`
		DELETE FROM org_join_requests
		WHERE id IN (
			SELECT id FROM (
				SELECT id,
					ROW_NUMBER() OVER (PARTITION BY org_id, user_id ORDER BY id DESC) AS duplicate_rank
				FROM org_join_requests
			) AS duplicate_rows
			WHERE duplicate_rank > 1
		)
	`)
	if result.Error != nil {
		panic("数据库：组织加入申请历史重复数据归并失败")
	}
	if result.RowsAffected > 0 {
		log.Warnf("database migration removed %d duplicate org join requests", result.RowsAffected)
	}
}

// PublishSiteSettings 将站点业务配置写入 Redis，供 agent/core_data 热读
func PublishSiteSettings(d *Data) {
	if d == nil || d.DB == nil || d.RDB == nil {
		return
	}
	rt, err := sitesettings.LoadFromDB(d.DB)
	if err != nil || rt == nil {
		return
	}
	if err := sitesettings.PublishRedis(context.Background(), d.RDB, rt); err != nil {
		log.Warnf("publish site settings: %v", err)
	}
}

// startSiteSettingsRefresh 后台定时回填 Redis；返回 stop 在 Data cleanup 时调用。
func startSiteSettingsRefresh(d *Data) func() {
	if d == nil || d.DB == nil || d.RDB == nil {
		return func() {}
	}
	stopCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(siteSettingsRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				PublishSiteSettings(d)
			}
		}
	}()
	return func() { close(stopCh) }
}

// seedPlanQuotas 幂等写入默认套餐配额模板
func seedPlanQuotas(db *gorm.DB) {
	defaults := []model.PlanQuota{
		{Plan: "free", SeatLimit: 0, DailySyncPerUser: 4, AISummaryPerMonth: 5},
		{Plan: "team", SeatLimit: 50, DailySyncPerUser: 24, AISummaryPerMonth: 200},
		{Plan: "pro", SeatLimit: 200, DailySyncPerUser: 48, AISummaryPerMonth: 1000},
	}
	for _, p := range defaults {
		var n int64
		if db.Model(&model.PlanQuota{}).Where("plan = ?", p.Plan).Count(&n); n == 0 {
			_ = db.Create(&p).Error
		}
	}
}
