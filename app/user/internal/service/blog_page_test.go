package service

import (
	"testing"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newBlogPageSQLite(t *testing.T) (*BlogService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.BlogPage{}); err != nil {
		t.Fatalf("migrate blog page: %v", err)
	}
	return &BlogService{db: db}, db
}

func TestNormalizeBlogPageWrite(t *testing.T) {
	got, msg := normalizeBlogPageWrite(nil, 0, blogPageWriteReq{
		Title:     "  我的手记  ",
		Slug:      " Notes ",
		ContentMD: "# hi\r\n",
		Status:    "published",
		ShowInNav: true,
		NavLabel:  " 手记 ",
		NavOrder:  3,
	})
	if msg != "" {
		t.Fatalf("unexpected validation error: %s", msg)
	}
	if got.Slug != "notes" || got.Title != "我的手记" || got.NavLabel != "手记" {
		t.Fatalf("unexpected normalized page: %+v", got)
	}
	if got.ContentMD != "# hi\n" || got.Status != model.BlogPagePublished {
		t.Fatalf("content/status not normalized: %+v", got)
	}
	if got.ImageHashes != "[]" && got.ImageHashes != "" {
		// no images → empty JSON array
		t.Fatalf("image hashes: %q", got.ImageHashes)
	}
}

func TestNormalizeBlogPageWriteRejectsInvalidFields(t *testing.T) {
	cases := []struct {
		name string
		req  blogPageWriteReq
	}{
		{name: "empty title", req: blogPageWriteReq{Slug: "notes", ContentMD: "x"}},
		{name: "empty slug", req: blogPageWriteReq{Title: "x", ContentMD: "x"}},
		{name: "reserved slug", req: blogPageWriteReq{Title: "x", Slug: "manage", ContentMD: "x"}},
		{name: "unsafe slug", req: blogPageWriteReq{Title: "x", Slug: "a/b", ContentMD: "x"}},
		{name: "empty content", req: blogPageWriteReq{Title: "x", Slug: "notes"}},
		{name: "invalid status", req: blogPageWriteReq{Title: "x", Slug: "notes", ContentMD: "x", Status: "hidden"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, msg := normalizeBlogPageWrite(nil, 0, tc.req); msg == "" {
				t.Fatalf("expected validation error for %+v", tc.req)
			}
		})
	}
}

func TestBlogPageUniqueSlugPerAuthor(t *testing.T) {
	_, db := newBlogPageSQLite(t)
	first := model.BlogPage{
		UserID: 1, Title: "A", Slug: "notes", ContentMD: "a", Status: model.BlogPagePublished,
	}
	if err := db.Create(&first).Error; err != nil {
		t.Fatalf("create first: %v", err)
	}
	duplicate := model.BlogPage{
		UserID: 1, Title: "B", Slug: "notes", ContentMD: "b", Status: model.BlogPagePublished,
	}
	if err := db.Create(&duplicate).Error; err == nil {
		t.Fatal("same author and slug must be unique")
	}
	otherAuthor := model.BlogPage{
		UserID: 2, Title: "B", Slug: "notes", ContentMD: "b", Status: model.BlogPagePublished,
	}
	if err := db.Create(&otherAuthor).Error; err != nil {
		t.Fatalf("same slug for another author should be allowed: %v", err)
	}
}

func TestListPublicBlogPages(t *testing.T) {
	svc, db := newBlogPageSQLite(t)
	rows := []model.BlogPage{
		{UserID: 1, Title: "B", Slug: "b", ContentMD: "b", Status: model.BlogPagePublished, ShowInNav: true, NavOrder: 2},
		{UserID: 1, Title: "A", Slug: "a", ContentMD: "a", Status: model.BlogPagePublished, ShowInNav: true, NavOrder: 1},
		{UserID: 1, Title: "Hidden", Slug: "hidden", ContentMD: "hidden", Status: model.BlogPagePublished, ShowInNav: false, NavOrder: 0},
		{UserID: 1, Title: "Draft", Slug: "draft", ContentMD: "draft", Status: model.BlogPageDraft, ShowInNav: true, NavOrder: 0},
		{UserID: 2, Title: "Other", Slug: "other", ContentMD: "other", Status: model.BlogPagePublished, ShowInNav: true, NavOrder: 0},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed pages: %v", err)
	}

	got, err := svc.listPublicBlogPages(1)
	if err != nil {
		t.Fatalf("list public pages: %v", err)
	}
	if len(got) != 2 || got[0].Slug != "a" || got[1].Slug != "b" {
		t.Fatalf("unexpected public nav pages: %+v", got)
	}

	page, err := svc.getPublicBlogPage(1, "hidden")
	if err != nil || page.Slug != "hidden" {
		t.Fatalf("published page remains addressable when hidden from nav: %+v %v", page, err)
	}
	if _, err := svc.getPublicBlogPage(1, "draft"); err == nil {
		t.Fatal("draft page must not be public")
	}

	mine, err := svc.listMineBlogPages(1)
	if err != nil || len(mine) != 4 {
		t.Fatalf("owner must see all own pages: %+v %v", mine, err)
	}
}

func TestReorderBlogPagesIsOwnerScopedAndAtomic(t *testing.T) {
	svc, db := newBlogPageSQLite(t)
	rows := []model.BlogPage{
		{UserID: 1, Title: "A", Slug: "a", ContentMD: "a", Status: model.BlogPagePublished, ShowInNav: true, NavOrder: 1},
		{UserID: 1, Title: "B", Slug: "b", ContentMD: "b", Status: model.BlogPagePublished, ShowInNav: true, NavOrder: 2},
		{UserID: 2, Title: "Other", Slug: "other", ContentMD: "o", Status: model.BlogPagePublished, ShowInNav: true, NavOrder: 7},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed pages: %v", err)
	}

	if err := svc.reorderBlogPages(1, []blogPageOrderItem{{ID: rows[0].ID, NavOrder: 20}, {ID: rows[1].ID, NavOrder: 10}}); err != nil {
		t.Fatalf("reorder own pages: %v", err)
	}
	var own []model.BlogPage
	db.Where("user_id = ?", 1).Order("nav_order ASC").Find(&own)
	if len(own) != 2 || own[0].Slug != "b" || own[1].Slug != "a" {
		t.Fatalf("unexpected order: %+v", own)
	}

	if err := svc.reorderBlogPages(1, []blogPageOrderItem{{ID: rows[0].ID, NavOrder: 30}, {ID: rows[2].ID, NavOrder: 0}}); err == nil {
		t.Fatal("cross-author page must fail the whole reorder")
	}
	var a model.BlogPage
	db.First(&a, rows[0].ID)
	if a.NavOrder != 20 {
		t.Fatalf("failed transaction must roll back earlier updates, got %d", a.NavOrder)
	}
}
