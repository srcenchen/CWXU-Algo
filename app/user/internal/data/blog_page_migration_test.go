package data

import (
	"testing"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestBackfillBlogFixedPagesIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.BlogSiteConfig{}, &model.BlogPage{}, &model.SchemaPatch{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	configs := []model.BlogSiteConfig{
		{UserID: 1, ThemeID: "mizuki", ColorScheme: "system", AboutMD: "# About", FriendsMD: "# Friends"},
		{UserID: 2, ThemeID: "mizuki", ColorScheme: "system", AboutMD: "legacy about"},
	}
	if err := db.Create(&configs).Error; err != nil {
		t.Fatalf("seed configs: %v", err)
	}
	existing := model.BlogPage{
		UserID: 2, Title: "自定义关于", Slug: "about", ContentMD: "keep me",
		Status: model.BlogPagePublished, ShowInNav: true, NavLabel: "关于", NavOrder: 100,
	}
	if err := db.Create(&existing).Error; err != nil {
		t.Fatalf("seed existing page: %v", err)
	}

	if err := backfillBlogFixedPages(db); err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	if err := backfillBlogFixedPages(db); err != nil {
		t.Fatalf("second backfill: %v", err)
	}

	var pages []model.BlogPage
	if err := db.Order("user_id ASC, slug ASC").Find(&pages).Error; err != nil {
		t.Fatalf("list pages: %v", err)
	}
	if len(pages) != 3 {
		t.Fatalf("expected 3 pages, got %+v", pages)
	}
	var kept model.BlogPage
	if err := db.Where("user_id = ? AND slug = ?", 2, "about").First(&kept).Error; err != nil {
		t.Fatalf("load existing page: %v", err)
	}
	if kept.ContentMD != "keep me" || kept.Title != "自定义关于" {
		t.Fatalf("migration must not overwrite existing page: %+v", kept)
	}
}
