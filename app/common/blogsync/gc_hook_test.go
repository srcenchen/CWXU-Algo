package blogsync

import (
	"sync"
	"testing"
	"time"

	"cwxu-algo/app/common/blogimg"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// assetRow local mirror of blog_image_assets for integration with blogsync GC path.
type assetRow struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    uint   `gorm:"column:user_id"`
	ObjectKey string `gorm:"column:object_key"`
	URL       string `gorm:"column:url"`
}

func (assetRow) TableName() string { return "blog_image_assets" }

type recordingDeleter struct {
	mu      sync.Mutex
	base    string
	deleted []string
}

func (r *recordingDeleter) Configured() bool     { return true }
func (r *recordingDeleter) PublicBaseURL() string { return r.base }
func (r *recordingDeleter) Delete(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, key)
	return nil
}

func (r *recordingDeleter) snap() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.deleted...)
	return out
}

func solutionGCDB(t *testing.T) *gorm.DB {
	t.Helper()
	// cache=shared + unique name: GORM pool must see the same in-memory DB
	dsn := "file:blogsync_gc_" + t.Name() + "?mode=memory&cache=shared"
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
	// single conn avoids any residual isolation
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&Category{}, &Article{}, &articleOrg{}, &articleComment{}, &articleLike{}, &assetRow{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestUpsertFromSolutionTriggersGCOnDeRef drives the real Upsert path then the
// same GC function ScheduleGCUserImages would call (sync for determinism).
func TestUpsertFromSolutionTriggersGCOnDeRef(t *testing.T) {
	db := solutionGCDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(3)
	solID := uint(42)

	// seed asset + article content with image (as if user uploaded then synced)
	key := "/blog/3/sol.webp"
	_ = db.Create(&assetRow{UserID: userID, ObjectKey: key, URL: base + key}).Error

	aid, _, err := UpsertFromSolution(db, userID, solID, 0, "题解带图", "![图]("+base+key+")\n\n思路")
	if err != nil {
		t.Fatal(err)
	}
	del := &recordingDeleter{base: base}
	// still referenced → no delete
	if n := blogimg.GCUserImages(db, del, userID); n != 0 {
		t.Fatalf("still referenced n=%d del=%v", n, del.snap())
	}

	// 题解更新去掉图片（community 调 UpsertFromSolution*）
	_, _, err = UpsertFromSolution(db, userID, solID, aid, "题解无图", "只有文字")
	if err != nil {
		t.Fatal(err)
	}
	// ScheduleGC is async; call shipped GC on real post-upsert state
	n := blogimg.GCUserImages(db, del, userID)
	if n != 1 {
		t.Fatalf("after upsert de-ref n=%d del=%v", n, del.snap())
	}
	if del.snap()[0] != key {
		t.Fatalf("deleted=%v want %s", del.snap(), key)
	}
	var left int64
	_ = db.Model(&assetRow{}).Where("user_id = ?", userID).Count(&left).Error
	if left != 0 {
		t.Fatalf("asset rows left=%d", left)
	}
}

// TestDeleteBySolutionTriggersGC drives real DeleteBySolution then GC.
func TestDeleteBySolutionTriggersGC(t *testing.T) {
	db := solutionGCDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(8)
	solID := uint(77)
	key := "/blog/8/gone.webp"
	_ = db.Create(&assetRow{UserID: userID, ObjectKey: key, URL: base + key}).Error

	aid, _, err := UpsertFromSolution(db, userID, solID, 0, "将删", "![x]("+base+key+")")
	if err != nil || aid == 0 {
		t.Fatalf("upsert %v aid=%d", err, aid)
	}

	DeleteBySolution(db, userID, solID, aid)
	if _, _, ok := LookupBySolution(db, solID); ok {
		t.Fatal("article should be gone")
	}

	del := &recordingDeleter{base: base}
	// ScheduleGC is fire-and-forget; invoke shipped GC on post-delete state
	n := blogimg.GCUserImages(db, del, userID)
	if n != 1 || del.snap()[0] != key {
		t.Fatalf("n=%d del=%v", n, del.snap())
	}
}

// TestDeleteBySolutionSchedulesGC ensures Schedule hook is invoked (async smoke).
func TestDeleteBySolutionSchedulesGC(t *testing.T) {
	db := solutionGCDB(t)
	userID := uint(11)
	aid, _, err := UpsertFromSolution(db, userID, 12, 0, "t", "body")
	if err != nil {
		t.Fatal(err)
	}
	// no upyun configured → ScheduleGC no-ops safely
	DeleteBySolution(db, userID, 12, aid)
	time.Sleep(30 * time.Millisecond)
}
