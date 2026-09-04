package service

import (
	"testing"
	"time"

	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newBlogPinnedSQLite(t *testing.T) (*BlogService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.BlogArticle{}); err != nil {
		t.Fatalf("migrate blog article: %v", err)
	}
	return &BlogService{db: db}, db
}

func TestListPublicBlogArticlesUsesPinnedOrderOnlyForUnfilteredList(t *testing.T) {
	_, db := newBlogPinnedSQLite(t)
	now := time.Now()
	categoryID := uint(1)
	rows := []model.BlogArticle{
		{UserID: 1, Slug: "old-pinned", Title: "old", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &now, PinOrder: 2, CreatedAt: now.Add(-10 * time.Hour)},
		{UserID: 1, Slug: "new-pinned", Title: "new", Content: "x", Visibility: model.BlogVisPublic, CategoryID: &categoryID, PinnedAt: &now, PinOrder: 1, CreatedAt: now.Add(-20 * time.Hour)},
		{UserID: 1, Slug: "latest", Title: "latest", Content: "x", Visibility: model.BlogVisPublic, CreatedAt: now},
		{UserID: 1, Slug: "private-pinned", Title: "private", Content: "x", Visibility: model.BlogVisPrivate, PinnedAt: &now, PinOrder: 0, CreatedAt: now.Add(time.Hour)},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed articles: %v", err)
	}

	q := db.Where("user_id = ? AND visibility IN ?", 1, []string{model.BlogVisPublic, model.BlogVisPassword})
	got, err := findBlogArticles(q, true, 0, 20)
	if err != nil || len(got) != 3 {
		t.Fatalf("list articles: %v %+v", err, got)
	}
	if got[0].Slug != "new-pinned" || got[1].Slug != "old-pinned" || got[2].Slug != "latest" {
		t.Fatalf("unexpected pinned order: %+v", got)
	}

	got, err = findBlogArticles(q.Where("category_id = ?", 1), false, 0, 20)
	if err != nil || len(got) != 1 || got[0].Slug != "new-pinned" {
		t.Fatalf("filtered list must use time order and category filter: %v %+v", err, got)
	}
}

func TestUsePinnedArticleOrderDoesNotDependOnViewer(t *testing.T) {
	if !usePinnedArticleOrder(true, 0, "", "") {
		t.Fatal("unfiltered blog home must use pinned order for its owner and visitors")
	}
	if usePinnedArticleOrder(false, 0, "", "") {
		t.Fatal("unfiltered non-home lists must keep published-time order")
	}
	for _, tc := range []struct {
		categoryID uint
		keyword    string
		tag        string
	}{
		{categoryID: 1},
		{keyword: "graph"},
		{tag: "dp"},
	} {
		if usePinnedArticleOrder(true, tc.categoryID, tc.keyword, tc.tag) {
			t.Fatalf("filtered list must ignore pinned order: %+v", tc)
		}
	}
}

func TestPinArticleIsVisibleOnlyForPublicOrPasswordAndIdempotent(t *testing.T) {
	svc, db := newBlogPinnedSQLite(t)
	rows := []model.BlogArticle{
		{UserID: 1, Slug: "public", Title: "public", Content: "x", Visibility: model.BlogVisPublic},
		{UserID: 1, Slug: "password", Title: "password", Content: "x", Visibility: model.BlogVisPassword},
		{UserID: 1, Slug: "private", Title: "private", Content: "x", Visibility: model.BlogVisPrivate, PinnedAt: ptrTime(time.Now()), PinOrder: 9},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed articles: %v", err)
	}
	if err := svc.pinArticle(1, rows[0].ID, true); err != nil {
		t.Fatalf("pin public: %v", err)
	}
	first := rows[0]
	if err := db.First(&first, rows[0].ID).Error; err != nil || first.PinnedAt == nil || first.PinOrder != 1 {
		t.Fatalf("public article not pinned first: %+v %v", first, err)
	}
	if err := svc.pinArticle(1, rows[0].ID, true); err != nil {
		t.Fatalf("repeat pin: %v", err)
	}
	var repeated model.BlogArticle
	db.First(&repeated, rows[0].ID)
	if repeated.PinOrder != 1 || repeated.PinnedAt == nil || !repeated.PinnedAt.Equal(*first.PinnedAt) {
		t.Fatalf("repeat pin must be idempotent: %+v", repeated)
	}
	if err := svc.pinArticle(1, rows[1].ID, true); err != nil {
		t.Fatalf("pin password article: %v", err)
	}
	var password model.BlogArticle
	db.First(&password, rows[1].ID)
	db.First(&repeated, rows[0].ID)
	if password.PinOrder != 1 || repeated.PinOrder != 2 {
		t.Fatalf("new pin must be first: password=%+v previous=%+v", password, repeated)
	}
	if err := svc.pinArticle(1, rows[2].ID, true); err == nil {
		t.Fatal("private article must not be pinnable")
	}
	if err := svc.pinArticle(1, rows[0].ID, false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	var unpinned model.BlogArticle
	db.First(&unpinned, rows[0].ID)
	if unpinned.PinnedAt != nil || unpinned.PinOrder != 0 {
		t.Fatalf("unpin must clear both fields: %+v", unpinned)
	}
	db.First(&password, rows[1].ID)
	if password.PinOrder != 1 {
		t.Fatalf("unpin must compact the remaining order, got %+v", password)
	}
}

func TestReorderPinnedArticlesRequiresExactSetAndIsAtomic(t *testing.T) {
	svc, db := newBlogPinnedSQLite(t)
	stamp := time.Now()
	rows := []model.BlogArticle{
		{UserID: 1, Slug: "a", Title: "a", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &stamp, PinOrder: 1},
		{UserID: 1, Slug: "b", Title: "b", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &stamp, PinOrder: 2},
		{UserID: 2, Slug: "other", Title: "other", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &stamp, PinOrder: 1},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed articles: %v", err)
	}
	if err := svc.reorderPinnedArticles(1, []uint{rows[1].ID, rows[0].ID}); err != nil {
		t.Fatalf("reorder: %v", err)
	}
	var got []model.BlogArticle
	db.Where("user_id = ?", 1).Order("pin_order").Find(&got)
	if got[0].ID != rows[1].ID || got[0].PinOrder != 1 || got[1].PinOrder != 2 {
		t.Fatalf("unexpected reordered articles: %+v", got)
	}
	if err := svc.reorderPinnedArticles(1, []uint{rows[0].ID, rows[2].ID}); err == nil {
		t.Fatal("reorder with extra/cross-owner id must fail")
	}
	for name, ids := range map[string][]uint{
		"zero":      {rows[0].ID, 0},
		"duplicate": {rows[0].ID, rows[0].ID},
		"missing":   {rows[0].ID},
		"extra":     {rows[0].ID, rows[1].ID, rows[2].ID},
	} {
		t.Run(name, func(t *testing.T) {
			if err := svc.reorderPinnedArticles(1, ids); err == nil {
				t.Fatalf("invalid ids must fail: %v", ids)
			}
		})
	}
	db.Where("user_id = ?", 1).Order("pin_order").Find(&got)
	if got[0].ID != rows[1].ID || got[0].PinOrder != 1 {
		t.Fatalf("failed reorder must be atomic: %+v", got)
	}
}

func TestPrivateVisibilityClearsPin(t *testing.T) {
	_, db := newBlogPinnedSQLite(t)
	article := model.BlogArticle{UserID: 1, Slug: "a", Title: "a", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: ptrTime(time.Now()), PinOrder: 1}
	if err := db.Create(&article).Error; err != nil {
		t.Fatalf("seed article: %v", err)
	}
	var existing model.BlogArticle
	db.First(&existing, article.ID)
	updated := existing
	updated.Visibility = model.BlogVisPrivate
	preserveArticlePin(&updated, &existing)
	if err := db.Save(&updated).Error; err != nil {
		t.Fatalf("save private article: %v", err)
	}
	var got model.BlogArticle
	db.First(&got, article.ID)
	if got.Visibility != model.BlogVisPrivate || got.PinnedAt != nil || got.PinOrder != 0 {
		t.Fatalf("private visibility must clear pin: %+v", got)
	}
}

func TestCompactPinnedArticleOrderAfterVisibilityChange(t *testing.T) {
	_, db := newBlogPinnedSQLite(t)
	stamp := time.Now()
	rows := []model.BlogArticle{
		{UserID: 1, Slug: "a", Title: "a", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &stamp, PinOrder: 1},
		{UserID: 1, Slug: "b", Title: "b", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &stamp, PinOrder: 2},
		{UserID: 1, Slug: "c", Title: "c", Content: "x", Visibility: model.BlogVisPassword, PinnedAt: &stamp, PinOrder: 3},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed articles: %v", err)
	}
	if err := compactPinnedArticleOrder(db, 1, 2); err != nil {
		t.Fatalf("compact pin order: %v", err)
	}
	var last model.BlogArticle
	db.First(&last, rows[2].ID)
	if last.PinOrder != 2 {
		t.Fatalf("later pinned article must move forward, got %+v", last)
	}
}

func TestDeleteBlogArticleAndCompactPinnedOrder(t *testing.T) {
	_, db := newBlogPinnedSQLite(t)
	stamp := time.Now()
	rows := []model.BlogArticle{
		{UserID: 1, Slug: "a", Title: "a", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &stamp, PinOrder: 1},
		{UserID: 1, Slug: "b", Title: "b", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &stamp, PinOrder: 2},
		{UserID: 1, Slug: "c", Title: "c", Content: "x", Visibility: model.BlogVisPublic, PinnedAt: &stamp, PinOrder: 3},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed articles: %v", err)
	}
	if err := deleteBlogArticleAndCompact(db, &rows[1]); err != nil {
		t.Fatalf("delete pinned article: %v", err)
	}
	var remaining []model.BlogArticle
	db.Where("user_id = ?", 1).Order("pin_order").Find(&remaining)
	if len(remaining) != 2 || remaining[0].PinOrder != 1 || remaining[1].PinOrder != 2 {
		t.Fatalf("delete must compact remaining pin order: %+v", remaining)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
