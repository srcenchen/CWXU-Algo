package blogimg

import (
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fakeDeleter struct {
	mu      sync.Mutex
	base    string
	deleted []string
}

func (f *fakeDeleter) Configured() bool     { return true }
func (f *fakeDeleter) PublicBaseURL() string { return f.base }
func (f *fakeDeleter) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeDeleter) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleted))
	copy(out, f.deleted)
	return out
}

func gcTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:blogimg_gc_" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&imageAssetRow{}, &articleRefRow{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestGCUserImagesRemovesOrphansKeepsReferenced(t *testing.T) {
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(9)
	old := time.Now().Add(-3 * time.Hour)

	_ = db.Create(&articleRefRow{
		UserID:   userID,
		Content:  "![keep](http://zhiyuansofts.cn/blog/9/keep.webp)\n",
		CoverURL: "http://zhiyuansofts.cn/blog/9/cover.jpg",
	}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/keep.webp", URL: base + "/blog/9/keep.webp", CreatedAt: old}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/cover.jpg", URL: base + "/blog/9/cover.jpg", CreatedAt: old}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/orphan.webp", URL: base + "/blog/9/orphan.webp", CreatedAt: old}).Error

	del := &fakeDeleter{base: base}
	n := GCUserImages(db, del, userID)
	if n != 1 {
		t.Fatalf("orphan count=%d want 1, deleted=%v", n, del.Deleted())
	}
	got := del.Deleted()
	if len(got) != 1 || got[0] != "/blog/9/orphan.webp" {
		t.Fatalf("deleted=%v", got)
	}
	var left int64
	_ = db.Model(&imageAssetRow{}).Where("user_id = ?", userID).Count(&left).Error
	if left != 2 {
		t.Fatalf("remaining assets=%d want 2", left)
	}
}

func TestGCUserImagesKeepsCrossHostReferenced(t *testing.T) {
	// 正文是 https://zhiyuansofts.cn，PublicBase 是 http://… 或其它域时，旧逻辑会误删。
	db := gcTestDB(t)
	userID := uint(27)
	old := time.Now().Add(-3 * time.Hour)
	_ = db.Create(&articleRefRow{
		UserID:  userID,
		Content: "![x](https://zhiyuansofts.cn/blog/27/20260730_1cfd654d4a20de794600d47ad991590d.webp)\n",
	}).Error
	_ = db.Create(&imageAssetRow{
		UserID:    userID,
		ObjectKey: "/blog/27/20260730_1cfd654d4a20de794600d47ad991590d.webp",
		URL:       "https://zhiyuansofts.cn/blog/27/20260730_1cfd654d4a20de794600d47ad991590d.webp",
		CreatedAt: old,
	}).Error
	del := &fakeDeleter{base: "http://other-cdn.example"}
	if n := GCUserImages(db, del, userID); n != 0 {
		t.Fatalf("should keep cross-host ref, n=%d del=%v", n, del.Deleted())
	}
}

func TestGCUserImagesGracePeriod(t *testing.T) {
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(3)
	fresh := time.Now().Add(-10 * time.Minute)
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: "/blog/3/new.webp", URL: base + "/blog/3/new.webp", CreatedAt: fresh,
	}).Error
	del := &fakeDeleter{base: base}
	if n := GCUserImagesAt(db, del, userID, time.Now()); n != 0 {
		t.Fatalf("grace should keep, n=%d", n)
	}
}

func TestExistingURLsForUser(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(7)
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: "/blog/7/a.webp", URL: "https://zhiyuansofts.cn/blog/7/a.webp",
	}).Error
	ex, miss := ExistingURLsForUser(db, userID, []string{
		"https://zhiyuansofts.cn/blog/7/a.webp",
		"https://zhiyuansofts.cn/blog/7/gone.webp",
		"/blog/7/a.webp",
	})
	if len(ex) != 2 {
		t.Fatalf("existing=%v", ex)
	}
	if len(miss) != 1 || miss[0] != "https://zhiyuansofts.cn/blog/7/gone.webp" {
		t.Fatalf("missing=%v", miss)
	}
}

func TestGCUserImagesAfterContentCleared(t *testing.T) {
	// Mirrors 题解 UpsertFromSolution rewriting content without image refs.
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(5)
	old := time.Now().Add(-3 * time.Hour)
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/5/a.webp", URL: base + "/blog/5/a.webp", CreatedAt: old}).Error
	_ = db.Create(&articleRefRow{
		UserID:  userID,
		Content: "![a](http://zhiyuansofts.cn/blog/5/a.webp)",
	}).Error

	del := &fakeDeleter{base: base}
	if n := GCUserImages(db, del, userID); n != 0 {
		t.Fatalf("should keep referenced, got n=%d del=%v", n, del.Deleted())
	}

	_ = db.Model(&articleRefRow{}).Where("user_id = ?", userID).Update("content", "no images").Error
	n := GCUserImages(db, del, userID)
	if n != 1 {
		t.Fatalf("after clear n=%d del=%v", n, del.Deleted())
	}
}

func TestGCUserImagesAfterAllArticlesDeleted(t *testing.T) {
	// Mirrors 题解 DeleteBySolution removing the mirrored blog article.
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(4)
	old := time.Now().Add(-3 * time.Hour)
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/4/x.webp", URL: base + "/blog/4/x.webp", CreatedAt: old}).Error
	_ = db.Create(&articleRefRow{
		UserID:  userID,
		Content: "![x](http://zhiyuansofts.cn/blog/4/x.webp)",
	}).Error

	_ = db.Where("user_id = ?", userID).Delete(&articleRefRow{}).Error
	del := &fakeDeleter{base: base}
	n := GCUserImages(db, del, userID)
	if n != 1 {
		t.Fatalf("after article delete n=%d del=%v", n, del.Deleted())
	}
	if del.Deleted()[0] != "/blog/4/x.webp" {
		t.Fatalf("got %v", del.Deleted())
	}
}

func TestScheduleGCUserImagesRuns(t *testing.T) {
	ScheduleGCUserImages(nil, 0)
	time.Sleep(10 * time.Millisecond)
}
