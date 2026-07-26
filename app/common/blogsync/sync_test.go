package blogsync

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Category{}, &Article{}, &articleOrg{}, &articleComment{}, &articleLike{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestEnsureDefaultCategory(t *testing.T) {
	db := testDB(t)
	id1, err := EnsureDefaultCategory(db, 7)
	if err != nil || id1 == 0 {
		t.Fatalf("ensure: %v id=%d", err, id1)
	}
	id2, err := EnsureDefaultCategory(db, 7)
	if err != nil || id2 != id1 {
		t.Fatalf("idempotent: %v id=%d want %d", err, id2, id1)
	}
	var c Category
	if db.First(&c, id1).Error != nil || !c.IsDefault || c.Name != DefaultCategoryName {
		t.Fatalf("row=%+v", c)
	}
}

func TestUpsertFromSolution(t *testing.T) {
	db := testDB(t)
	aid, slug, err := UpsertFromSolution(db, 3, 99, 0, "差分题解", "## 思路\nO(n)")
	if err != nil {
		t.Fatal(err)
	}
	if slug != "solution-99" || aid == 0 {
		t.Fatalf("aid=%d slug=%s", aid, slug)
	}
	var created Article
	_ = db.First(&created, aid).Error
	if created.Summary == "" {
		t.Fatalf("solution sync should auto-fill default summary")
	}
	// 精选已改为站管/审核员手动标记（083e1f0），题解镜像不再自动 recommend
	if created.Recommend {
		t.Fatal("solution mirror must not auto recommend")
	}

	// 作者手填摘要后，题解再更新不应覆盖
	_ = db.Model(&created).Update("summary", "我手写的摘要").Error

	// update
	aid2, slug2, err := UpsertFromSolution(db, 3, 99, aid, "差分题解 v2", "新内容")
	if err != nil || aid2 != aid || slug2 != slug {
		t.Fatalf("update aid=%d/%d slug=%s err=%v", aid, aid2, slug2, err)
	}
	var a Article
	_ = db.First(&a, aid).Error
	if a.Title != "差分题解 v2" || a.Content != "新内容" {
		t.Fatalf("article=%+v", a)
	}
	if a.Summary != "我手写的摘要" {
		t.Fatalf("update must keep author summary, got %q", a.Summary)
	}
	if a.SourceSolutionID == nil || *a.SourceSolutionID != 99 {
		t.Fatalf("source=%v", a.SourceSolutionID)
	}
	if a.CategoryID == nil {
		t.Fatal("missing category")
	}
	var cat Category
	_ = db.First(&cat, *a.CategoryID).Error
	if !cat.IsDefault {
		t.Fatalf("cat=%+v", cat)
	}

	id, s, ok := LookupBySolution(db, 99)
	if !ok || id != aid || s != slug {
		t.Fatalf("lookup %v %d %s", ok, id, s)
	}

	DeleteBySolution(db, 3, 99, aid)
	if _, _, ok := LookupBySolution(db, 99); ok {
		t.Fatal("should be deleted")
	}
}
