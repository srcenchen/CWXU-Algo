package service

import (
	"testing"
	"time"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestUpdateBlogArticleModerationWaitsForReferenceLockAndPreservesContent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.BlogArticle{}); err != nil {
		t.Fatal(err)
	}
	article := model.BlogArticle{UserID: 84, Slug: "moderate", Title: "moderate", Content: "new content", Visibility: model.BlogVisPublic}
	if err := db.Create(&article).Error; err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- blogimg.WithUserImageReferenceTx(db, article.UserID, func(tx *gorm.DB) error {
			close(entered)
			<-release
			return tx.Model(&model.BlogArticle{}).Where("id = ?", article.ID).UpdateColumn("content", "concurrent content").Error
		})
	}()
	<-entered
	moderateDone := make(chan error, 1)
	go func() {
		_, err := updateBlogArticleModeration(db, article.ID, 9, model.BlogModerationApproved, "ok")
		moderateDone <- err
	}()
	select {
	case err := <-moderateDone:
		t.Fatalf("moderation bypassed reference lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-moderateDone; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&article, article.ID).Error; err != nil {
		t.Fatal(err)
	}
	if article.Content != "concurrent content" {
		t.Fatalf("moderation restored stale content: %q", article.Content)
	}
}
