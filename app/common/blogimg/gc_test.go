package blogimg

import (
	"errors"
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
	err     error
	deleted []string
}

func (f *fakeDeleter) Configured() bool      { return true }
func (f *fakeDeleter) PublicBaseURL() string { return f.base }
func (f *fakeDeleter) Delete(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, key)
	return f.err
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
	if err := db.AutoMigrate(&imageAssetRow{}, &articleRefRow{}, &pageRefRow{}); err != nil {
		t.Fatal(err)
	}
	// blog_site_configs is queried by ListUserImageOrphansCheckedAt (fixed pages).
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS blog_site_configs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER,
		about_md TEXT,
		home_intro_md TEXT,
		friends_md TEXT
	)`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func gcOld() time.Time { return time.Now().Add(-25 * time.Hour) }

// runConfirmedUserGC lists orphans and deletes via snapshot confirmation (only safe path).
func runConfirmedUserGC(t *testing.T, db *gorm.DB, del ObjectDeleter, userID uint) int {
	t.Helper()
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, del.PublicBaseURL(), time.Now())
	if err != nil {
		t.Fatalf("list orphans: %v", err)
	}
	if len(orphans) == 0 {
		return 0
	}
	ids, snapshot := BuildOrphanSnapshot(userID, orphans)
	n, err := GCUserImagesSnapshot(db, del, userID, ids, snapshot)
	if err != nil {
		t.Fatalf("GCUserImagesSnapshot: %v", err)
	}
	return n
}

func runConfirmedUserGCErr(t *testing.T, db *gorm.DB, del ObjectDeleter, userID uint) (int, error) {
	t.Helper()
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, del.PublicBaseURL(), time.Now())
	if err != nil {
		return 0, err
	}
	if len(orphans) == 0 {
		return 0, nil
	}
	ids, snapshot := BuildOrphanSnapshot(userID, orphans)
	return GCUserImagesSnapshot(db, del, userID, ids, snapshot)
}

func TestGCUserImagesRemovesOrphansKeepsReferenced(t *testing.T) {
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(9)
	old := gcOld()

	_ = db.Create(&articleRefRow{
		UserID:   userID,
		Content:  "![keep](http://zhiyuansofts.cn/blog/9/keep.webp)\n",
		CoverURL: "http://zhiyuansofts.cn/blog/9/cover.jpg",
	}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/keep.webp", URL: base + "/blog/9/keep.webp", CreatedAt: old}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/cover.jpg", URL: base + "/blog/9/cover.jpg", CreatedAt: old}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/9/orphan.webp", URL: base + "/blog/9/orphan.webp", CreatedAt: old}).Error

	del := &fakeDeleter{base: base}
	n := runConfirmedUserGC(t, db, del, userID)
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

func TestGCUserImagesKeepsByImageHashesColumn(t *testing.T) {
	// 正文被改写/无 URL，但 ImageHashes 仍声明 hash → 不得删
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(11)
	old := gcOld()
	h := ContentHash([]byte("keep-by-hash"))
	key := ObjectKeyForHash(userID, h, ".webp")
	_ = db.Create(&articleRefRow{
		UserID:      userID,
		Content:     "no image urls here",
		ImageHashes: EncodeImageHashes([]string{h}),
	}).Error
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: key, URL: key, ContentHash: h, CreatedAt: old,
	}).Error
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: "/blog/11/orphan.webp", URL: base + "/blog/11/orphan.webp", CreatedAt: old,
	}).Error
	del := &fakeDeleter{base: base}
	n := runConfirmedUserGC(t, db, del, userID)
	if n != 1 || len(del.Deleted()) != 1 || del.Deleted()[0] != "/blog/11/orphan.webp" {
		t.Fatalf("n=%d del=%v", n, del.Deleted())
	}
}

func TestGCUserImagesKeepsPageReferenced(t *testing.T) {
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(12)
	old := gcOld()
	_ = db.Create(&pageRefRow{
		UserID:    userID,
		ContentMD: "![p](/blog/12/page.webp)\n",
	}).Error
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: "/blog/12/page.webp", URL: "/blog/12/page.webp", CreatedAt: old,
	}).Error
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: "/blog/12/gone.webp", URL: "/blog/12/gone.webp", CreatedAt: old,
	}).Error
	del := &fakeDeleter{base: base}
	n := runConfirmedUserGC(t, db, del, userID)
	if n != 1 || del.Deleted()[0] != "/blog/12/gone.webp" {
		t.Fatalf("n=%d del=%v", n, del.Deleted())
	}
}

func TestGCUserImagesKeepsCrossHostReferenced(t *testing.T) {
	// 正文是 https://zhiyuansofts.cn，PublicBase 是 http://… 或其它域时，旧逻辑会误删。
	db := gcTestDB(t)
	userID := uint(27)
	old := gcOld()
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
	if n := runConfirmedUserGC(t, db, del, userID); n != 0 {
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
	orphans := ListUserImageOrphansAt(db, userID, base, time.Now())
	if len(orphans) != 1 || !orphans[0].Protected {
		t.Fatalf("fresh upload should be listed as protected orphan, got %+v", orphans)
	}
	// Snapshot GC force-deletes protected; product path is admin grace exclusion + manual confirm.
	// User silent GC removed — only assert list marks Protected.

}

func TestListUserImageOrphansIncludesFreshAsProtected(t *testing.T) {
	db := gcTestDB(t)
	base := "https://zhiyuansofts.cn"
	userID := uint(31)
	now := time.Now()
	old := now.Add(-25 * time.Hour)
	fresh := now.Add(-10 * time.Minute)

	_ = db.Create(&articleRefRow{
		UserID:  userID,
		Content: "![keep](/blog/31/keep.webp)",
	}).Error
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: "/blog/31/keep.webp", URL: base + "/blog/31/keep.webp", CreatedAt: old,
	}).Error
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: "/blog/31/old.webp", URL: base + "/blog/31/old.webp", CreatedAt: old,
	}).Error
	_ = db.Create(&imageAssetRow{
		UserID: userID, ObjectKey: "/blog/31/fresh.webp", URL: base + "/blog/31/fresh.webp", CreatedAt: fresh,
	}).Error

	orphans := ListUserImageOrphansAt(db, userID, base, now)
	if len(orphans) != 2 {
		t.Fatalf("orphans=%v want 2", orphans)
	}
	protected := map[string]bool{}
	for _, asset := range orphans {
		protected[asset.ObjectKey] = asset.Protected
	}
	if protected["/blog/31/old.webp"] {
		t.Fatal("old orphan must not be protected")
	}
	if !protected["/blog/31/fresh.webp"] {
		t.Fatal("fresh orphan must be marked protected")
	}
}

func TestListUserImageOrphansKeepsSiteConfigReferenced(t *testing.T) {
	db := gcTestDB(t)
	base := "https://zhiyuansofts.cn"
	userID := uint(88)
	old := gcOld()
	key := "/blog/88/about.webp"
	if err := db.Exec(`INSERT INTO blog_site_configs(user_id, about_md, home_intro_md, friends_md) VALUES (?, ?, '', '')`,
		userID, "![a]("+base+key+")\n").Error; err != nil {
		t.Fatal(err)
	}
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: key, URL: base + key, CreatedAt: old}).Error
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/88/orphan.webp", URL: base + "/blog/88/orphan.webp", CreatedAt: old}).Error
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, base, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].ObjectKey != "/blog/88/orphan.webp" {
		t.Fatalf("orphans=%+v want only orphan", orphans)
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
	old := gcOld()
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/5/a.webp", URL: base + "/blog/5/a.webp", CreatedAt: old}).Error
	_ = db.Create(&articleRefRow{
		UserID:  userID,
		Content: "![a](http://zhiyuansofts.cn/blog/5/a.webp)",
	}).Error

	del := &fakeDeleter{base: base}
	if n := runConfirmedUserGC(t, db, del, userID); n != 0 {
		t.Fatalf("should keep referenced, got n=%d del=%v", n, del.Deleted())
	}

	// 清正文同时清 ImageHashes（写入路径会一起更新）
	_ = db.Model(&articleRefRow{}).Where("user_id = ?", userID).
		Updates(map[string]interface{}{"content": "no images", "image_hashes": "[]"}).Error
	n := runConfirmedUserGC(t, db, del, userID)
	if n != 1 {
		t.Fatalf("after clear n=%d del=%v", n, del.Deleted())
	}
}

func TestGCUserImagesAfterAllArticlesDeleted(t *testing.T) {
	// Mirrors 题解 DeleteBySolution removing the mirrored blog article.
	db := gcTestDB(t)
	base := "http://zhiyuansofts.cn"
	userID := uint(4)
	old := gcOld()
	_ = db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/4/x.webp", URL: base + "/blog/4/x.webp", CreatedAt: old}).Error
	_ = db.Create(&articleRefRow{
		UserID:  userID,
		Content: "![x](http://zhiyuansofts.cn/blog/4/x.webp)",
	}).Error

	_ = db.Where("user_id = ?", userID).Delete(&articleRefRow{}).Error
	del := &fakeDeleter{base: base}
	n := runConfirmedUserGC(t, db, del, userID)
	if n != 1 {
		t.Fatalf("after article delete n=%d del=%v", n, del.Deleted())
	}
	if del.Deleted()[0] != "/blog/4/x.webp" {
		t.Fatalf("got %v", del.Deleted())
	}
}

func TestGCUserImagesStopsWhenReferenceQueryFails(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(41)
	old := gcOld()
	if err := db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/41/orphan.webp", URL: "/blog/41/orphan.webp", CreatedAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&pageRefRow{}); err != nil {
		t.Fatal(err)
	}
	del := &fakeDeleter{base: "https://zhiyuansofts.cn"}

	n, err := runConfirmedUserGCErr(t, db, del, userID)
	if err == nil || n != 0 {
		t.Fatalf("query failure must err and delete nothing, n=%d err=%v deleted=%v", n, err, del.Deleted())
	}
	if len(del.Deleted()) != 0 {
		t.Fatalf("remote deletion attempted after query failure: %v", del.Deleted())
	}
	var count int64
	if err := db.Model(&imageAssetRow{}).Where("user_id = ?", userID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("asset row changed after query failure: count=%d err=%v", count, err)
	}
}

func TestGCUserImagesKeepsDatabaseRowWhenRemoteDeleteFails(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(42)
	old := gcOld()
	if err := db.Create(&imageAssetRow{UserID: userID, ObjectKey: "/blog/42/orphan.webp", URL: "/blog/42/orphan.webp", CreatedAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	del := &fakeDeleter{base: "https://zhiyuansofts.cn", err: errors.New("remote unavailable")}

	n, err := runConfirmedUserGCErr(t, db, del, userID)
	if err == nil || n != 0 {
		t.Fatalf("failed remote deletion must not count as success, n=%d err=%v", n, err)
	}
	var count int64
	if err := db.Model(&imageAssetRow{}).Where("user_id = ?", userID).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("asset row removed after remote failure: count=%d err=%v", count, err)
	}
}

func TestGCUserImagesSnapshotRejectsEmptyConfirmation(t *testing.T) {
	db := gcTestDB(t)
	del := &fakeDeleter{base: "https://zhiyuansofts.cn"}
	if _, err := GCUserImagesSnapshot(db, del, 51, nil, ""); !errors.Is(err, ErrGCPreviewRequired) {
		t.Fatalf("empty confirmation error=%v want ErrGCPreviewRequired", err)
	}
}

func TestGCUserImagesSnapshotRejectsStalePreviewAfterReferenceChange(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(52)
	old := gcOld()
	asset := imageAssetRow{UserID: userID, ObjectKey: "/blog/52/orphan.webp", URL: "/blog/52/orphan.webp", CreatedAt: old}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, "https://zhiyuansofts.cn", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids, snapshot := BuildOrphanSnapshot(userID, orphans)
	if err := db.Create(&articleRefRow{UserID: userID, Content: "![keep](/blog/52/orphan.webp)"}).Error; err != nil {
		t.Fatal(err)
	}
	del := &fakeDeleter{base: "https://zhiyuansofts.cn"}
	if _, err := GCUserImagesSnapshot(db, del, userID, ids, snapshot); !errors.Is(err, ErrGCStaleSnapshot) {
		t.Fatalf("stale confirmation error=%v want ErrGCStaleSnapshot", err)
	}
	if len(del.Deleted()) != 0 {
		t.Fatalf("stale snapshot deleted remote objects: %v", del.Deleted())
	}
}

func TestGCUserImagesSnapshotDeletesExactPreview(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(53)
	old := gcOld()
	for _, key := range []string{"/blog/53/a.webp", "/blog/53/b.webp"} {
		if err := db.Create(&imageAssetRow{UserID: userID, ObjectKey: key, URL: key, CreatedAt: old}).Error; err != nil {
			t.Fatal(err)
		}
	}
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, "https://zhiyuansofts.cn", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids, snapshot := BuildOrphanSnapshot(userID, orphans)
	del := &fakeDeleter{base: "https://zhiyuansofts.cn"}
	deleted, err := GCUserImagesSnapshot(db, del, userID, ids, snapshot)
	if err != nil || deleted != 2 {
		t.Fatalf("deleted=%d err=%v remote=%v", deleted, err, del.Deleted())
	}
}

func TestFreshPendingReservationIsInvisibleToCheckAndGC(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(92)
	key := "/blog/92/pending.webp"
	if err := db.Create(&imageAssetRow{UserID: userID, ObjectKey: key, URL: key, Status: "pending", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	existing, missing := ExistingURLsForUser(db, userID, []string{key})
	if len(existing) != 0 || len(missing) != 1 {
		t.Fatalf("existing=%v missing=%v", existing, missing)
	}
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, "https://zhiyuansofts.cn", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("fresh pending reservation exposed to GC: %+v", orphans)
	}
}

func TestStalePendingReservationIsReferenceAware(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(93)
	key := "/blog/93/pending.webp"
	old := time.Now().Add(-GCGracePeriod - time.Hour)
	if err := db.Create(&imageAssetRow{UserID: userID, ObjectKey: key, URL: key, Status: "pending", CreatedAt: old}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&articleRefRow{UserID: userID, Content: "![keep](" + key + ")"}).Error; err != nil {
		t.Fatal(err)
	}
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, "https://zhiyuansofts.cn", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Fatalf("referenced stale reservation exposed to GC: %+v", orphans)
	}
	if err := db.Where("user_id = ?", userID).Delete(&articleRefRow{}).Error; err != nil {
		t.Fatal(err)
	}
	orphans, err = ListUserImageOrphansCheckedAt(db, userID, "https://zhiyuansofts.cn", time.Now())
	if err != nil || len(orphans) != 1 || orphans[0].ObjectKey != key {
		t.Fatalf("stale unreferenced reservation orphans=%+v err=%v", orphans, err)
	}
}
