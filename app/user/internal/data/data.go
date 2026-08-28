package data

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/common/conf"
	gorm2 "cwxu-algo/app/common/data/gorm"
	redis2 "cwxu-algo/app/common/data/redis"
	"cwxu-algo/app/common/mail"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/legacysecret"
	"cwxu-algo/app/user/internal/data/model"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/wire"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

// siteSettingsRefreshInterval 定期把 site_configs 刷进 Redis，供 core_data/agent 读 SMTP。
// 与 sitesettings.RedisTTL 配合：即使缓存被误清/毒缓存被剔除，也会在数分钟内恢复。
const siteSettingsRefreshInterval = 3 * time.Minute

// paymentOrderCloseInterval 支付订单关单轮询：每 1 分钟关闭超过 5 分钟未支付的订单
// （GuadArt OrderCloser 移植；closed 后支付FM回调仍可履约，条件更新已覆盖）
const paymentOrderCloseInterval = time.Minute

// paymentOrderPendingTTL 订单待支付超时时间
const paymentOrderPendingTTL = 5 * time.Minute

const blogImageMigrationBatchSize = 200
const blogFixedPageMigrationBatchSize = 100

type blogArticleImageMigrationRow struct {
	ID          uint
	UpdatedAt   time.Time
	UserID      uint
	Content     string
	CoverURL    sql.NullString
	ImageHashes sql.NullString
}

type blogPageImageMigrationRow struct {
	ID          uint
	UpdatedAt   time.Time
	UserID      uint
	ContentMD   string
	ImageHashes sql.NullString
}

type blogAssetImageMigrationRow struct {
	ID          uint
	UpdatedAt   time.Time
	ObjectKey   string
	URL         string
	ContentHash sql.NullString
}

// ProviderSet is data providers.
var ProviderSet = wire.NewSet(NewData)

// Data .
type Data struct {
	DB              *gorm.DB
	CoreDB          *gorm.DB // optional: algo_core_data for site backup
	RDB             *redis.Client
	LegacyConfigKey string
}

// NewData .
func NewData(c *conf.Data, legacy LegacySiteConfig) (*Data, func(), error) {
	data := &Data{
		DB: gorm2.InitGorm(c), RDB: redis2.InitRedis(c),
		LegacyConfigKey: legacysecret.ResolveKey(legacy.ConfigEncryptionKey),
	}
	// 邮件发送结果 → 站点 SMTP 状态（Redis）
	mail.SetStatusReporter(func(ok bool, errMsg string) {
		st, msg := sitesettings.StatusFail, errMsg
		if ok {
			st, msg = sitesettings.StatusOK, ""
		}
		sitesettings.SetServiceStatus(context.Background(), data.RDB, sitesettings.ServiceSmtp, st, msg)
	})
	if core := openCoreDB(c); core != nil {
		data.CoreDB = core
		log.Info("backup: core database connected")
	} else {
		log.Warn("backup: core database not configured; full site export/import of training data will fail")
	}
	migrateModels(data.DB)
	if err := migrateLegacySiteSecretsAtStartup(data.DB, legacy); err != nil {
		return nil, nil, fmt.Errorf("site secret migration failed: %w", err)
	}
	if err := migrateLegacyBootstrapSiteConfig(data.DB, legacy); err != nil {
		return nil, nil, fmt.Errorf("legacy YAML site config migration failed: %w", err)
	}
	PublishSiteSettings(data)
	stopRefresh := startSiteSettingsRefresh(data)
	stopOrderCloser := startPaymentOrderCloser(data)
	cleanup := func() {
		stopRefresh()
		stopOrderCloser()
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
		&model.OrgInvite{},
		&model.Squad{},
		&model.SquadMember{},
		&model.OrgScopeGrant{},
		&model.PlanQuota{},
		&model.Paste{},
		&model.SiteVisitDaily{},
		&model.UserFollow{},
		&model.Notification{},
		&model.BlogArticle{},
		&model.BlogPage{},
		&model.BlogCategory{},
		&model.BlogArticleOrg{},
		&model.BlogTag{},
		&model.BlogArticleTag{},
		&model.BlogComment{},
		&model.BlogCommentLike{},
		&model.BlogLike{},
		&model.SupportEvent{},
		&model.BlogArticleViewUV{},
		&model.BlogReport{},
		&model.SchemaPatch{},
		&model.BlogThemeFlag{},
		&model.BlogSiteConfig{},
		&model.BlogImageAsset{},
		&model.BlogImageUploadRequest{},
		&model.ObsidianPluginMeta{},
		&model.PluginAuthorization{},
		&model.Role{},
		&model.RolePermission{},
		&model.UserRole{},
		&model.OrgRolePerm{},
		&model.SubscriptionPlan{},
		&model.PaymentOrder{},
	)
	if err != nil {
		panic("数据库：数据库自动合并失败")
	}
	if err := ensureBlogImageAssetStatuses(db); err != nil {
		panic("数据库：图片资产状态迁移失败: " + err.Error())
	}
	if err := ensureBlogImagePendingUniqueIndex(db); err != nil {
		panic("数据库：图片上传待审唯一索引迁移失败: " + err.Error())
	}
	seedPlanQuotas(db)
	seedSubscriptionPlans(db)
	seedGoAlgoFramework(db)
	seedRbac(db)
	backfillLastLoginAt(db)
	ensureSiteInactiveDays(db)
	backfillBlogModerationApproved(db)
	backfillBlogActivationForExistingAuthors(db)
	if err := backfillBlogFixedPages(db); err != nil {
		log.Warnf("backfill blog fixed pages: %v", err)
	}
	backfillBlogAutoSurfaceAndZeroViews(db)
	backfillBlogCoverFromFirstImage(db)
	// v2：GC 误删/未识别后再次补空头图；幂等 key 与 v1 分离
	backfillBlogCoverFromFirstImageV2(db)
	// 图床 URL 改为 path-only 存储，读时按当前域名展开（换域无需改写正文）
	migrateBlogImageURLsToPathOnly(db)
	// 文章/页面落 ImageHashes；资产补 content_hash（供 hash-GC）
	backfillBlogImageContentHashes(db)
}

func ensureBlogImageAssetStatuses(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable(&model.BlogImageAsset{}) {
		return nil
	}
	return db.Model(&model.BlogImageAsset{}).
		Where("status IS NULL OR status = ''").Update("status", model.BlogImageAssetReady).Error
}

// ensureBlogImagePendingUniqueIndex reconciles legacy duplicate pending rows and
// installs the production partial unique index. PostgreSQL builds concurrently so
// startup migration does not block writes for the duration of the index scan.
func ensureBlogImagePendingUniqueIndex(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable(&model.BlogImageUploadRequest{}) {
		return nil
	}
	if db.Dialector.Name() != "postgres" {
		if err := reconcileBlogImagePendingDuplicates(db); err != nil {
			return err
		}
		return db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uniq_blog_img_req_pending_user
			ON blog_image_upload_requests (user_id) WHERE status = 'pending'`).Error
	}
	return ensurePostgresBlogImagePendingUniqueIndex(db)
}

func reconcileBlogImagePendingDuplicates(db *gorm.DB) error {
	return db.Exec(`UPDATE blog_image_upload_requests SET status = 'rejected'
		WHERE status = 'pending' AND id NOT IN (
			SELECT MAX(id) FROM blog_image_upload_requests WHERE status = 'pending' GROUP BY user_id
		)`).Error
}

type blogImagePendingIndexState struct {
	Exists     bool   `gorm:"column:index_exists"`
	Valid      bool   `gorm:"column:valid"`
	Unique     bool   `gorm:"column:is_unique"`
	UserIDOnly bool   `gorm:"column:user_id_only"`
	Predicate  string `gorm:"column:predicate"`
}

func (state blogImagePendingIndexState) matchesProductionDefinition() bool {
	if !state.Exists || !state.Valid || !state.Unique || !state.UserIDOnly {
		return false
	}
	predicate := strings.ToLower(state.Predicate)
	for _, remove := range []string{"::character varying", "::text", "(", ")", " ", "\t", "\n", `"`} {
		predicate = strings.ReplaceAll(predicate, remove, "")
	}
	return predicate == "status='pending'"
}

func loadPostgresBlogImagePendingIndexState(db *gorm.DB) (blogImagePendingIndexState, error) {
	const indexName = "uniq_blog_img_req_pending_user"
	var state blogImagePendingIndexState
	err := db.Raw(`SELECT TRUE AS index_exists,
			i.indisvalid AS valid,
			i.indisunique AS is_unique,
			(i.indnkeyatts = 1 AND i.indnatts = 1 AND
			 (SELECT a.attname
			  FROM unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
			  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			  ORDER BY k.ord LIMIT 1) = 'user_id') AS user_id_only,
			COALESCE(pg_get_expr(i.indpred, i.indrelid), '') AS predicate
		FROM pg_class c
		JOIN pg_index i ON i.indexrelid = c.oid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = ? AND n.nspname = current_schema()
		LIMIT 1`, indexName).Scan(&state).Error
	return state, err
}

func ensurePostgresBlogImagePendingUniqueIndex(db *gorm.DB) error {
	const maxAttempts = 3
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := reconcileBlogImagePendingDuplicates(db); err != nil {
			return err
		}
		state, err := loadPostgresBlogImagePendingIndexState(db)
		if err != nil {
			return err
		}
		if state.matchesProductionDefinition() {
			return nil
		}
		if state.Exists {
			if err := db.Exec(`DROP INDEX CONCURRENTLY IF EXISTS uniq_blog_img_req_pending_user`).Error; err != nil {
				return err
			}
		}
		if err := db.Exec(`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_blog_img_req_pending_user
			ON blog_image_upload_requests (user_id) WHERE status = 'pending'`).Error; err != nil {
			lastErr = err
			continue
		}
		state, err = loadPostgresBlogImagePendingIndexState(db)
		if err != nil {
			return err
		}
		if state.matchesProductionDefinition() {
			return nil
		}
		lastErr = fmt.Errorf("pending image upload index definition mismatch after create")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("pending image upload index creation exhausted retries")
	}
	return lastErr
}

// backfillBlogCoverFromFirstImage 旧文 cover 为空且正文有图时，写入第一张 http(s) 图。
func backfillBlogCoverFromFirstImage(db *gorm.DB) {
	runBlogCoverFirstImageBackfill(db, "blog_cover_first_image_backfill_v1")
}

func backfillBlogCoverFromFirstImageV2(db *gorm.DB) {
	runBlogCoverFirstImageBackfill(db, "blog_cover_first_image_backfill_v2")
}

func runBlogCoverFirstImageBackfill(db *gorm.DB, patchKey string) {
	if db == nil || !db.Migrator().HasTable(&model.BlogArticle{}) {
		return
	}
	if !claimSchemaPatch(db, patchKey) {
		return
	}
	const maxCover = 1024
	var articles []model.BlogArticle
	if err := db.Select("id", "content", "cover_url").
		Where("cover_url IS NULL OR cover_url = '' OR BTRIM(cover_url) = ''").
		Find(&articles).Error; err != nil {
		log.Warnf("blog cover first-image backfill (%s) list: %v", patchKey, err)
		return
	}
	updated := 0
	for _, a := range articles {
		if strings.TrimSpace(a.CoverURL) != "" {
			continue
		}
		cover := blogimg.ResolveCoverURL("", a.Content, true, maxCover)
		if cover == "" {
			continue
		}
		res := db.Model(&model.BlogArticle{}).
			Where("id = ? AND (cover_url IS NULL OR cover_url = '' OR BTRIM(cover_url) = '')", a.ID).
			Update("cover_url", cover)
		if res.Error != nil {
			log.Warnf("blog cover first-image backfill (%s) id=%d: %v", patchKey, a.ID, res.Error)
			continue
		}
		if res.RowsAffected > 0 {
			updated++
		}
	}
	log.Infof("blog cover first-image backfill (%s): scanned=%d updated=%d", patchKey, len(articles), updated)
}

// migrateBlogImageURLsToPathOnly 将历史绝对图床 URL 规范为 /blog/{uid}/… 路径：
// blog_articles.content / cover_url、blog_pages.content_md、blog_image_assets.url。
// 幂等：已是 path-only 的行不会被破坏；外链图保留。
func migrateBlogImageURLsToPathOnly(db *gorm.DB) {
	if db == nil {
		return
	}
	const patchKey = "blog_image_url_path_only_v1"
	if schemaPatchApplied(db, patchKey) {
		return
	}
	articlesUpdated, pagesUpdated, assetsUpdated := 0, 0, 0

	if db.Migrator().HasTable(&model.BlogArticle{}) {
		var afterID uint
		for {
			var articles []blogArticleImageMigrationRow
			if err := db.Model(&model.BlogArticle{}).Select("id", "updated_at", "content", "cover_url").Where("id > ?", afterID).
				Order("id ASC").Limit(blogImageMigrationBatchSize).Find(&articles).Error; err != nil {
				log.Warnf("blog image path-only migrate list articles: %v", err)
				return
			}
			if len(articles) == 0 {
				break
			}
			for _, a := range articles {
				cover := ""
				if a.CoverURL.Valid {
					cover = a.CoverURL.String
				}
				newContent := blogimg.NormalizeStoredImageRefs(a.Content)
				newCover := blogimg.NormalizeCoverURL(cover)
				if newContent == a.Content && newCover == strings.TrimSpace(cover) {
					continue
				}
				q := db.Model(&model.BlogArticle{}).
					Where("id = ? AND updated_at = ? AND content = ?", a.ID, a.UpdatedAt, a.Content)
				if a.CoverURL.Valid {
					q = q.Where("cover_url = ?", a.CoverURL.String)
				} else {
					q = q.Where("cover_url IS NULL")
				}
				res := q.UpdateColumns(map[string]interface{}{
					"content":   newContent,
					"cover_url": newCover,
				})
				if res.Error != nil {
					log.Warnf("blog image path-only migrate article id=%d: %v", a.ID, res.Error)
					return
				}
				if res.RowsAffected > 0 {
					articlesUpdated++
				}
			}
			afterID = articles[len(articles)-1].ID
		}
	}

	if db.Migrator().HasTable(&model.BlogPage{}) {
		var afterID uint
		for {
			var pages []model.BlogPage
			if err := db.Select("id", "updated_at", "content_md").Where("id > ?", afterID).
				Order("id ASC").Limit(blogImageMigrationBatchSize).Find(&pages).Error; err != nil {
				log.Warnf("blog image path-only migrate list pages: %v", err)
				return
			}
			if len(pages) == 0 {
				break
			}
			for _, p := range pages {
				newMD := blogimg.NormalizeStoredImageRefs(p.ContentMD)
				if newMD == p.ContentMD {
					continue
				}
				res := db.Model(&model.BlogPage{}).
					Where("id = ? AND updated_at = ? AND content_md = ?", p.ID, p.UpdatedAt, p.ContentMD).
					UpdateColumn("content_md", newMD)
				if res.Error != nil {
					log.Warnf("blog image path-only migrate page id=%d: %v", p.ID, res.Error)
					return
				}
				if res.RowsAffected > 0 {
					pagesUpdated++
				}
			}
			afterID = pages[len(pages)-1].ID
		}
	}

	if db.Migrator().HasTable(&model.BlogImageAsset{}) {
		var afterID uint
		for {
			var assets []blogAssetImageMigrationRow
			if err := db.Model(&model.BlogImageAsset{}).Select("id", "updated_at", "object_key", "url").Where("id > ?", afterID).
				Order("id ASC").Limit(blogImageMigrationBatchSize).Find(&assets).Error; err != nil {
				log.Warnf("blog image path-only migrate list assets: %v", err)
				return
			}
			if len(assets) == 0 {
				break
			}
			for _, a := range assets {
				key := blogimg.NormalizeObjectKey(a.ObjectKey)
				if key == "" {
					key = blogimg.BlogObjectKeyFromAnyURL(a.URL)
				}
				if key == "" {
					continue
				}
				if strings.TrimSpace(a.URL) == key && blogimg.NormalizeObjectKey(a.ObjectKey) == key {
					continue
				}
				res := db.Model(&model.BlogImageAsset{}).
					Where("id = ? AND updated_at = ? AND object_key = ? AND url = ?", a.ID, a.UpdatedAt, a.ObjectKey, a.URL).
					UpdateColumns(map[string]interface{}{
						"object_key": key,
						"url":        key,
					})
				if res.Error != nil {
					log.Warnf("blog image path-only migrate asset id=%d: %v", a.ID, res.Error)
					return
				}
				if res.RowsAffected > 0 {
					assetsUpdated++
				}
			}
			afterID = assets[len(assets)-1].ID
		}
	}
	if err := completeSchemaPatch(db, patchKey); err != nil {
		log.Warnf("complete schema patch %s: %v", patchKey, err)
		return
	}

	log.Infof("blog image path-only migrate: articles=%d pages=%d assets=%d",
		articlesUpdated, pagesUpdated, assetsUpdated)
}

// backfillBlogImageContentHashes：
// 1) 资产表：从 object_key 可解析的内容 hash 写回 content_hash
// 2) 文章/页面：按正文引用解析 ImageHashes，供 hash-GC
func backfillBlogImageContentHashes(db *gorm.DB) {
	if db == nil {
		return
	}
	const patchKey = "blog_image_content_hash_v1"
	if schemaPatchApplied(db, patchKey) {
		return
	}
	assetsUpdated, articlesUpdated, pagesUpdated := 0, 0, 0

	if db.Migrator().HasTable(&model.BlogImageAsset{}) {
		var afterID uint
		for {
			var assets []blogAssetImageMigrationRow
			if err := db.Model(&model.BlogImageAsset{}).Select("id", "updated_at", "object_key", "content_hash").Where("id > ?", afterID).
				Order("id ASC").Limit(blogImageMigrationBatchSize).Find(&assets).Error; err != nil {
				log.Warnf("blog image hash backfill list assets: %v", err)
				return
			}
			if len(assets) == 0 {
				break
			}
			for _, a := range assets {
				contentHash := ""
				if a.ContentHash.Valid {
					contentHash = a.ContentHash.String
				}
				if blogimg.NormalizeHash(contentHash) != "" {
					continue
				}
				h := blogimg.HashFromObjectKey(a.ObjectKey)
				if h == "" {
					continue
				}
				q := db.Model(&model.BlogImageAsset{}).
					Where("id = ? AND updated_at = ? AND object_key = ?", a.ID, a.UpdatedAt, a.ObjectKey)
				if a.ContentHash.Valid {
					q = q.Where("content_hash = ?", a.ContentHash.String)
				} else {
					q = q.Where("content_hash IS NULL")
				}
				res := q.Update("content_hash", h)
				if res.Error != nil {
					log.Warnf("blog image hash backfill asset id=%d: %v", a.ID, res.Error)
					return
				}
				if res.RowsAffected > 0 {
					assetsUpdated++
				}
			}
			afterID = assets[len(assets)-1].ID
		}
	}

	if db.Migrator().HasTable(&model.BlogArticle{}) {
		var afterID uint
		for {
			var articles []blogArticleImageMigrationRow
			if err := db.Model(&model.BlogArticle{}).Select("id", "updated_at", "user_id", "content", "cover_url", "image_hashes").
				Where("id > ?", afterID).Order("id ASC").Limit(blogImageMigrationBatchSize).Find(&articles).Error; err != nil {
				log.Warnf("blog image hash backfill list articles: %v", err)
				return
			}
			if len(articles) == 0 {
				break
			}
			inputs := make([]blogimg.ContentHashInput, 0, len(articles))
			for _, a := range articles {
				imageHashes := ""
				if a.ImageHashes.Valid {
					imageHashes = a.ImageHashes.String
				}
				cover := ""
				if a.CoverURL.Valid {
					cover = a.CoverURL.String
				}
				if len(blogimg.DecodeImageHashes(imageHashes)) == 0 {
					inputs = append(inputs, blogimg.ContentHashInput{ID: a.ID, UserID: a.UserID, Content: a.Content, Cover: cover})
				}
			}
			resolved, err := blogimg.ResolveContentHashesBatchChecked(db, inputs)
			if err != nil {
				log.Warnf("blog image hash backfill resolve article batch: %v", err)
				return
			}
			for _, a := range articles {
				imageHashes := ""
				if a.ImageHashes.Valid {
					imageHashes = a.ImageHashes.String
				}
				if len(blogimg.DecodeImageHashes(imageHashes)) > 0 {
					continue
				}
				encoded := blogimg.EncodeImageHashes(resolved[a.ID])
				if encoded == "[]" || encoded == "" {
					continue
				}
				q := db.Model(&model.BlogArticle{}).
					Where("id = ? AND updated_at = ? AND content = ?", a.ID, a.UpdatedAt, a.Content)
				if a.CoverURL.Valid {
					q = q.Where("cover_url = ?", a.CoverURL.String)
				} else {
					q = q.Where("cover_url IS NULL")
				}
				if a.ImageHashes.Valid {
					q = q.Where("image_hashes = ?", a.ImageHashes.String)
				} else {
					q = q.Where("image_hashes IS NULL")
				}
				res := q.Update("image_hashes", encoded)
				if res.Error != nil {
					log.Warnf("blog image hash backfill article id=%d: %v", a.ID, res.Error)
					return
				}
				if res.RowsAffected > 0 {
					articlesUpdated++
				}
			}
			afterID = articles[len(articles)-1].ID
		}
	}

	if db.Migrator().HasTable(&model.BlogPage{}) {
		var afterID uint
		for {
			var pages []blogPageImageMigrationRow
			if err := db.Model(&model.BlogPage{}).Select("id", "updated_at", "user_id", "content_md", "image_hashes").
				Where("id > ?", afterID).Order("id ASC").Limit(blogImageMigrationBatchSize).Find(&pages).Error; err != nil {
				log.Warnf("blog image hash backfill list pages: %v", err)
				return
			}
			if len(pages) == 0 {
				break
			}
			inputs := make([]blogimg.ContentHashInput, 0, len(pages))
			for _, p := range pages {
				imageHashes := ""
				if p.ImageHashes.Valid {
					imageHashes = p.ImageHashes.String
				}
				if len(blogimg.DecodeImageHashes(imageHashes)) == 0 {
					inputs = append(inputs, blogimg.ContentHashInput{ID: p.ID, UserID: p.UserID, Content: p.ContentMD})
				}
			}
			resolved, err := blogimg.ResolveContentHashesBatchChecked(db, inputs)
			if err != nil {
				log.Warnf("blog image hash backfill resolve page batch: %v", err)
				return
			}
			for _, p := range pages {
				imageHashes := ""
				if p.ImageHashes.Valid {
					imageHashes = p.ImageHashes.String
				}
				if len(blogimg.DecodeImageHashes(imageHashes)) > 0 {
					continue
				}
				encoded := blogimg.EncodeImageHashes(resolved[p.ID])
				if encoded == "[]" || encoded == "" {
					continue
				}
				q := db.Model(&model.BlogPage{}).
					Where("id = ? AND updated_at = ? AND content_md = ?", p.ID, p.UpdatedAt, p.ContentMD)
				if p.ImageHashes.Valid {
					q = q.Where("image_hashes = ?", p.ImageHashes.String)
				} else {
					q = q.Where("image_hashes IS NULL")
				}
				res := q.Update("image_hashes", encoded)
				if res.Error != nil {
					log.Warnf("blog image hash backfill page id=%d: %v", p.ID, res.Error)
					return
				}
				if res.RowsAffected > 0 {
					pagesUpdated++
				}
			}
			afterID = pages[len(pages)-1].ID
		}
	}
	if err := completeSchemaPatch(db, patchKey); err != nil {
		log.Warnf("complete schema patch %s: %v", patchKey, err)
		return
	}

	log.Infof("blog image content-hash backfill: assets=%d articles=%d pages=%d",
		assetsUpdated, articlesUpdated, pagesUpdated)
}

// claimSchemaPatch 认领一次性数据修补：以 key 唯一插入，成功者执行、失败者跳过。
// 避免每次发版全表回填与多实例并发重复执行。
func claimSchemaPatch(db *gorm.DB, key string) bool {
	if db == nil || !db.Migrator().HasTable(&model.SchemaPatch{}) {
		return false
	}
	res := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SchemaPatch{Key: key, AppliedAt: time.Now()})
	if res.Error != nil {
		log.Warnf("claim schema patch %s: %v", key, res.Error)
		return false
	}
	return res.RowsAffected > 0
}

func schemaPatchApplied(db *gorm.DB, key string) bool {
	if db == nil || !db.Migrator().HasTable(&model.SchemaPatch{}) {
		return true
	}
	var count int64
	if err := db.Model(&model.SchemaPatch{}).Where("key = ?", key).Count(&count).Error; err != nil {
		log.Warnf("query schema patch %s: %v", key, err)
		return true
	}
	return count > 0
}

func completeSchemaPatch(db *gorm.DB, key string) error {
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.SchemaPatch{
		Key: key, AppliedAt: time.Now(),
	}).Error
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

// backfillBlogFixedPages migrates the legacy about/friends Markdown slots into
// first-class standalone pages. ON CONFLICT makes it safe across restarts and
// concurrent instances, while preserving an author's existing page content.
func backfillBlogFixedPages(db *gorm.DB) error {
	if db == nil || !db.Migrator().HasTable(&model.BlogSiteConfig{}) || !db.Migrator().HasTable(&model.BlogPage{}) {
		return nil
	}
	var afterUserID uint
	for {
		var configs []model.BlogSiteConfig
		if err := db.Select("user_id", "about_md", "friends_md").
			Where("user_id > ? AND ((about_md IS NOT NULL AND about_md <> '') OR (friends_md IS NOT NULL AND friends_md <> ''))", afterUserID).
			Order("user_id ASC").Limit(blogFixedPageMigrationBatchSize).Find(&configs).Error; err != nil {
			return err
		}
		if len(configs) == 0 {
			return nil
		}
		for _, cfg := range configs {
			pages := make([]model.BlogPage, 0, 2)
			if strings.TrimSpace(cfg.AboutMD) != "" {
				pages = append(pages, model.BlogPage{
					UserID: cfg.UserID, Title: "关于", Slug: "about", ContentMD: cfg.AboutMD,
					Status: model.BlogPagePublished, ShowInNav: true, NavLabel: "关于", NavOrder: 100,
				})
			}
			if strings.TrimSpace(cfg.FriendsMD) != "" {
				pages = append(pages, model.BlogPage{
					UserID: cfg.UserID, Title: "友链", Slug: "friends", ContentMD: cfg.FriendsMD,
					Status: model.BlogPagePublished, ShowInNav: true, NavLabel: "友链", NavOrder: 110,
				})
			}
			if len(pages) == 0 {
				continue
			}
			err := blogimg.WithUserImageReferenceTx(db, cfg.UserID, func(tx *gorm.DB) error {
				return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&pages).Error
			})
			if err != nil {
				return err
			}
		}
		afterUserID = configs[len(configs)-1].UserID
	}
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
		if err != nil {
			log.Errorf("publish site settings load: %v", err)
		}
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

// startPaymentOrderCloser 后台关单：pending 超过 5 分钟置 closed（GuadArt OrderCloser 移植）。
// closed 后支付FM回调仍可履约（ClaimAndFulfillPaidOrder 覆盖 pending/closed）。
func startPaymentOrderCloser(d *Data) func() {
	if d == nil || d.DB == nil {
		return func() {}
	}
	stopCh := make(chan struct{})
	go func() {
		ticker := time.NewTicker(paymentOrderCloseInterval)
		defer ticker.Stop()
		ctx := context.Background()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				n, err := closeStalePendingOrders(ctx, d)
				if err != nil {
					log.Warnf("payment order closer: %v", err)
				} else if n > 0 {
					log.Infof("payment order closer closed %d stale orders", n)
				}
			}
		}
	}()
	return func() { close(stopCh) }
}

// closeStalePendingOrders 关单实现（独立函数便于测试）
func closeStalePendingOrders(ctx context.Context, d *Data) (int64, error) {
	res := d.DB.WithContext(ctx).Model(&model.PaymentOrder{}).
		Where("status = ? AND created_at < ?", model.OrderStatusPending, time.Now().Add(-paymentOrderPendingTTL)).
		Update("status", model.OrderStatusClosed)
	return res.RowsAffected, res.Error
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

// seedSubscriptionPlans 幂等 upsert 默认 C 端订阅套餐模板（站管可改；改后以库为准）
func seedSubscriptionPlans(db *gorm.DB) {
	if db == nil {
		return
	}
	defaults := []model.SubscriptionPlan{
		{Plan: "free", PriceCents: 0, ManualRefreshDaily: 2, SyncIntervalMin: 180, AiAnalyzeMonth: 0, EnableFetchProblem: false, EnableAiAnalyze: false, EnableAiDaily: false, EnableRegularDaily: true, Days: 30, Enabled: true},
		{Plan: "plus", PriceCents: 200, ManualRefreshDaily: 15, SyncIntervalMin: 60, AiAnalyzeMonth: 0, EnableFetchProblem: false, EnableAiAnalyze: false, EnableAiDaily: false, EnableRegularDaily: true, Days: 30, Enabled: true},
		{Plan: "pro", PriceCents: 700, ManualRefreshDaily: 15, SyncIntervalMin: 60, AiAnalyzeMonth: 400, EnableFetchProblem: true, EnableAiAnalyze: true, EnableAiDaily: true, EnableRegularDaily: true, Days: 30, Enabled: true},
	}
	for _, p := range defaults {
		var n int64
		_ = db.Model(&model.SubscriptionPlan{}).Where("plan = ?", p.Plan).Count(&n).Error
		if n == 0 {
			_ = db.Create(&p).Error
		}
	}
}
