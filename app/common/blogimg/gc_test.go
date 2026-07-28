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

	_ = db.Create(&articleRefRow{
		UserID:   userID,
		Content:  "![keep](http://zhiyuansofts.cn/blog/9/keep.webp)\n",
		CoverURL: "http://zhiyuansofts.cn/blog/9/cover.jpg",
	}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/keep.webp", URL: base + "/blog/9/keep.webp"}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/cover.jpg", URL: base + "/blog/9/cover.jpg"}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/orphan.webp", URL: base + "/blog/9/orphan.webp"}).Error

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

func TestGCUserImagesAfterContentCleared(t *testing.T) {
	// Mirrors 题解 UpsertFromSolution rewriting content without image refs.
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(5)
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/5/a.webp", URL: base + "/blog/5/a.webp"}).Error
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
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/4/x.webp", URL: base + "/blog/4/x.webp"}).Error
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
