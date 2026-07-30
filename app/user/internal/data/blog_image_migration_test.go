package data

import (
	"fmt"
	"testing"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func blogImageMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SchemaPatch{}, &model.BlogArticle{}, &model.BlogPage{}, &model.BlogImageAsset{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateBlogImageURLsMarksPatchOnlyAfterSuccessfulCompletion(t *testing.T) {
	db := blogImageMigrationDB(t)
	article := model.BlogArticle{UserID: 1, Slug: "migration", Title: "migration", Content: "![x](https://zhiyuansofts.cn/blog/1/x.webp)"}
	if err := db.Create(&article).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_article_path BEFORE UPDATE ON blog_articles BEGIN SELECT RAISE(FAIL, 'path failed'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	migrateBlogImageURLsToPathOnly(db)
	var markerCount int64
	if err := db.Model(&model.SchemaPatch{}).Where("key = ?", "blog_image_url_path_only_v1").Count(&markerCount).Error; err != nil {
		t.Fatal(err)
	}
	if markerCount != 0 {
		t.Fatal("failed path migration must not write schema patch marker")
	}
	if err := db.Exec(`DROP TRIGGER fail_article_path`).Error; err != nil {
		t.Fatal(err)
	}
	migrateBlogImageURLsToPathOnly(db)
	if err := db.First(&article, article.ID).Error; err != nil {
		t.Fatal(err)
	}
	if article.Content != "![x](/blog/1/x.webp)" {
		t.Fatalf("restart did not rerun path migration: %q", article.Content)
	}
	if err := db.Model(&model.SchemaPatch{}).Where("key = ?", "blog_image_url_path_only_v1").Count(&markerCount).Error; err != nil || markerCount != 1 {
		t.Fatalf("successful migration marker count=%d err=%v", markerCount, err)
	}
}

func TestBackfillBlogImageHashesMarksPatchOnlyAfterSuccessfulCompletion(t *testing.T) {
	db := blogImageMigrationDB(t)
	hash := blogimg.ContentHash([]byte("migration hash"))
	asset := model.BlogImageAsset{UserID: 2, ObjectKey: blogimg.ObjectKeyForHash(2, hash, ".webp"), URL: "/blog/2/x.webp", Purpose: "content"}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_asset_hash BEFORE UPDATE ON blog_image_assets BEGIN SELECT RAISE(FAIL, 'hash failed'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	backfillBlogImageContentHashes(db)
	var markerCount int64
	if err := db.Model(&model.SchemaPatch{}).Where("key = ?", "blog_image_content_hash_v1").Count(&markerCount).Error; err != nil {
		t.Fatal(err)
	}
	if markerCount != 0 {
		t.Fatal("failed hash backfill must not write schema patch marker")
	}
	if err := db.Exec(`DROP TRIGGER fail_asset_hash`).Error; err != nil {
		t.Fatal(err)
	}
	backfillBlogImageContentHashes(db)
	if err := db.First(&asset, asset.ID).Error; err != nil {
		t.Fatal(err)
	}
	if asset.ContentHash != hash {
		t.Fatalf("restart did not rerun hash backfill: %q", asset.ContentHash)
	}
	if err := db.Model(&model.SchemaPatch{}).Where("key = ?", "blog_image_content_hash_v1").Count(&markerCount).Error; err != nil || markerCount != 1 {
		t.Fatalf("successful hash marker count=%d err=%v", markerCount, err)
	}
}

func TestMigrateBlogImageURLsDoesNotOverwriteConcurrentArticleEdit(t *testing.T) {
	db := blogImageMigrationDB(t)
	article := model.BlogArticle{UserID: 3, Slug: "concurrent-path", Title: "migration", Content: "![x](https://zhiyuansofts.cn/blog/3/x.webp)"}
	if err := db.Create(&article).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := db.Callback().Update().Before("gorm:update").Register("test:concurrent-article-path", func(tx *gorm.DB) {
		if changed || tx.Statement.Table != "blog_articles" {
			return
		}
		changed = true
		if err := db.Session(&gorm.Session{SkipHooks: true}).Model(&model.BlogArticle{}).
			Where("id = ?", article.ID).UpdateColumn("content", "concurrent edit").Error; err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	migrateBlogImageURLsToPathOnly(db)
	if err := db.First(&article, article.ID).Error; err != nil {
		t.Fatal(err)
	}
	if article.Content != "concurrent edit" {
		t.Fatalf("migration overwrote concurrent article edit: %q", article.Content)
	}
}

func TestBackfillBlogImageHashesDoesNotAttachStaleHashAfterConcurrentEdit(t *testing.T) {
	db := blogImageMigrationDB(t)
	hash := blogimg.ContentHash([]byte("concurrent hash"))
	key := blogimg.ObjectKeyForHash(4, hash, ".webp")
	if err := db.Create(&model.BlogImageAsset{UserID: 4, ObjectKey: key, URL: key, ContentHash: hash, Purpose: "content"}).Error; err != nil {
		t.Fatal(err)
	}
	article := model.BlogArticle{UserID: 4, Slug: "concurrent-hash", Title: "migration", Content: "![x](" + key + ")", ImageHashes: "[]"}
	if err := db.Create(&article).Error; err != nil {
		t.Fatal(err)
	}
	changed := false
	if err := db.Callback().Update().Before("gorm:update").Register("test:concurrent-article-hash", func(tx *gorm.DB) {
		if changed || tx.Statement.Table != "blog_articles" {
			return
		}
		changed = true
		if err := db.Session(&gorm.Session{SkipHooks: true}).Model(&model.BlogArticle{}).
			Where("id = ?", article.ID).Updates(map[string]interface{}{"content": "concurrent no image", "image_hashes": "[]"}).Error; err != nil {
			t.Errorf("concurrent edit: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	backfillBlogImageContentHashes(db)
	if err := db.First(&article, article.ID).Error; err != nil {
		t.Fatal(err)
	}
	if article.Content != "concurrent no image" || article.ImageHashes != "[]" {
		t.Fatalf("stale hash attached after concurrent edit: content=%q hashes=%q", article.Content, article.ImageHashes)
	}
}

func TestMigrateBlogImageURLsUsesBoundedKeysetBatches(t *testing.T) {
	db := blogImageMigrationDB(t)
	for i := 0; i < blogImageMigrationBatchSize+5; i++ {
		row := model.BlogArticle{
			UserID: 30, Slug: fmt.Sprintf("batch-%d", i), Title: "batch",
			Content: "![x](https://zhiyuansofts.cn/blog/30/x.webp)",
		}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	queries := 0
	missingLimit := false
	if err := db.Callback().Query().Before("gorm:query").Register("test:bounded-blog-image-path", func(tx *gorm.DB) {
		if tx.Statement.Table != "blog_articles" {
			return
		}
		queries++
		if _, ok := tx.Statement.Clauses["LIMIT"]; !ok {
			missingLimit = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	migrateBlogImageURLsToPathOnly(db)
	if missingLimit {
		t.Fatal("article migration issued an unbounded table query")
	}
	if queries < 3 {
		t.Fatalf("article keyset queries=%d, want multiple batches plus terminating query", queries)
	}
}

func TestBackfillBlogImageHashesUsesOneAssetLookupPerBatch(t *testing.T) {
	db := blogImageMigrationDB(t)
	hash := blogimg.ContentHash([]byte("batch hash"))
	key := blogimg.ObjectKeyForHash(31, hash, ".webp")
	if err := db.Create(&model.BlogImageAsset{UserID: 31, ObjectKey: key, URL: key, ContentHash: hash, Purpose: "content"}).Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		row := model.BlogArticle{UserID: 31, Slug: fmt.Sprintf("hash-%d", i), Title: "hash", Content: "![x](" + key + ")", ImageHashes: "[]"}
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	assetQueries := 0
	if err := db.Callback().Query().Before("gorm:query").Register("test:batch-hash-assets", func(tx *gorm.DB) {
		if tx.Statement.Table == "blog_image_assets" {
			assetQueries++
		}
	}); err != nil {
		t.Fatal(err)
	}
	backfillBlogImageContentHashes(db)
	if assetQueries > 3 {
		t.Fatalf("asset queries=%d, want one bounded asset scan (plus terminator) and one article batch lookup", assetQueries)
	}
}

func TestMigrateBlogImageURLsHandlesNullCoverOptimistically(t *testing.T) {
	db := blogImageMigrationDB(t)
	article := model.BlogArticle{UserID: 51, Slug: "null-cover", Title: "null", Content: "![x](https://zhiyuansofts.cn/blog/51/x.webp)"}
	if err := db.Create(&article).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.BlogArticle{}).Where("id = ?", article.ID).UpdateColumn("cover_url", nil).Error; err != nil {
		t.Fatal(err)
	}
	migrateBlogImageURLsToPathOnly(db)
	type stored struct {
		Content  string
		CoverURL *string `gorm:"column:cover_url"`
	}
	var got stored
	if err := db.Table("blog_articles").Select("content", "cover_url").Where("id = ?", article.ID).Scan(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Content != "![x](/blog/51/x.webp)" {
		t.Fatalf("NULL-cover article was not migrated: %q", got.Content)
	}
	var marker int64
	if err := db.Model(&model.SchemaPatch{}).Where("key = ?", "blog_image_url_path_only_v1").Count(&marker).Error; err != nil || marker != 1 {
		t.Fatalf("marker=%d err=%v", marker, err)
	}
}

func TestBackfillBlogImageHashesHandlesNullHashColumns(t *testing.T) {
	db := blogImageMigrationDB(t)
	hash := blogimg.ContentHash([]byte("null hashes"))
	key := blogimg.ObjectKeyForHash(52, hash, ".webp")
	if err := db.Create(&model.BlogImageAsset{UserID: 52, ObjectKey: key, URL: key, ContentHash: hash, Purpose: "content"}).Error; err != nil {
		t.Fatal(err)
	}
	article := model.BlogArticle{UserID: 52, Slug: "null-hash", Title: "null", Content: "![x](" + key + ")"}
	page := model.BlogPage{UserID: 52, Slug: "null-hash", Title: "null", ContentMD: "![x](" + key + ")", Status: model.BlogPagePublished}
	if err := db.Create(&article).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&page).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.BlogArticle{}).Where("id = ?", article.ID).UpdateColumn("image_hashes", nil).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.BlogPage{}).Where("id = ?", page.ID).UpdateColumn("image_hashes", nil).Error; err != nil {
		t.Fatal(err)
	}
	backfillBlogImageContentHashes(db)
	var articleHashes, pageHashes *string
	if err := db.Table("blog_articles").Select("image_hashes").Where("id = ?", article.ID).Scan(&articleHashes).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("blog_pages").Select("image_hashes").Where("id = ?", page.ID).Scan(&pageHashes).Error; err != nil {
		t.Fatal(err)
	}
	want := blogimg.EncodeImageHashes([]string{hash})
	if articleHashes == nil || *articleHashes != want || pageHashes == nil || *pageHashes != want {
		t.Fatalf("article=%v page=%v want=%q", articleHashes, pageHashes, want)
	}
}

func TestEnsureBlogImageAssetStatusesTreatsLegacyRowsAsReady(t *testing.T) {
	db := blogImageMigrationDB(t)
	asset := model.BlogImageAsset{UserID: 95, ObjectKey: "/blog/95/legacy.webp", URL: "/blog/95/legacy.webp", Purpose: "content"}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.BlogImageAsset{}).Where("id = ?", asset.ID).UpdateColumn("status", "").Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureBlogImageAssetStatuses(db); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&asset, asset.ID).Error; err != nil {
		t.Fatal(err)
	}
	if asset.Status != model.BlogImageAssetReady {
		t.Fatalf("legacy status=%q want ready", asset.Status)
	}
}
