package service

import (
	"testing"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newBlogTagsSQLite(t *testing.T) (*BlogService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.BlogArticle{}, &model.BlogTag{}, &model.BlogArticleTag{}); err != nil {
		t.Fatalf("migrate blog tags: %v", err)
	}
	return &BlogService{db: db}, db
}

func seedBlogTagSearch(t *testing.T, db *gorm.DB) {
	t.Helper()
	articles := []model.BlogArticle{
		{UserID: 1, Slug: "a", Title: "A", Content: "a", Visibility: "public", ModerationStatus: model.BlogModerationApproved},
		{UserID: 1, Slug: "b", Title: "B", Content: "b", Visibility: "public", ModerationStatus: model.BlogModerationApproved},
		{UserID: 1, Slug: "private", Title: "P", Content: "p", Visibility: "private", ModerationStatus: model.BlogModerationApproved},
		{UserID: 2, Slug: "other", Title: "O", Content: "o", Visibility: "public", ModerationStatus: model.BlogModerationApproved},
	}
	if err := db.Create(&articles).Error; err != nil {
		t.Fatalf("seed articles: %v", err)
	}
	tags := []model.BlogTag{
		{UserID: 1, Name: "线段树", NameLower: "线段树"},
		{UserID: 1, Name: "线性 DP", NameLower: "线性 dp"},
		{UserID: 1, Name: "Graph", NameLower: "graph"},
		{UserID: 2, Name: "线稿", NameLower: "线稿"},
	}
	if err := db.Create(&tags).Error; err != nil {
		t.Fatalf("seed tags: %v", err)
	}
	links := []model.BlogArticleTag{
		{ArticleID: articles[0].ID, TagID: tags[0].ID},
		{ArticleID: articles[1].ID, TagID: tags[0].ID},
		{ArticleID: articles[0].ID, TagID: tags[1].ID},
		{ArticleID: articles[2].ID, TagID: tags[2].ID},
		{ArticleID: articles[3].ID, TagID: tags[3].ID},
	}
	if err := db.Create(&links).Error; err != nil {
		t.Fatalf("seed tag links: %v", err)
	}
}

func TestListBlogTagCountsFuzzyPublic(t *testing.T) {
	svc, db := newBlogTagsSQLite(t)
	seedBlogTagSearch(t, db)

	got, err := svc.listBlogTagCounts(1, 0, "线", 10)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if len(got) != 2 || got[0].Name != "线段树" || got[0].Count != 2 || got[1].Name != "线性 DP" {
		t.Fatalf("unexpected fuzzy tags: %+v", got)
	}
	for _, row := range got {
		if row.Name == "线稿" || row.Name == "Graph" {
			t.Fatalf("must isolate authors and private articles: %+v", got)
		}
	}
}

func TestListBlogTagCountsOwnerCaseInsensitiveAndLimit(t *testing.T) {
	svc, db := newBlogTagsSQLite(t)
	seedBlogTagSearch(t, db)

	private, err := svc.listBlogTagCounts(1, 1, "gRa", 10)
	if err != nil || len(private) != 1 || private[0].Name != "Graph" {
		t.Fatalf("owner case-insensitive search failed: %+v %v", private, err)
	}
	top, err := svc.listBlogTagCounts(1, 0, "", 1)
	if err != nil || len(top) != 1 || top[0].Name != "线段树" {
		t.Fatalf("limit/hot ordering failed: %+v %v", top, err)
	}
}
