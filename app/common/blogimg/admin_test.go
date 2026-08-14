package blogimg

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type failingAdminDeleter struct {
	base string
}

func (f *failingAdminDeleter) Configured() bool      { return true }
func (f *failingAdminDeleter) PublicBaseURL() string { return f.base }
func (f *failingAdminDeleter) Delete(string) error   { return errors.New("upyun unavailable") }

type adminTestAsset struct {
	ID          uint `gorm:"primaryKey"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	UserID      uint
	ObjectKey   string
	URL         string
	ContentHash string
	Purpose     string
}

func (adminTestAsset) TableName() string { return "blog_image_assets" }

type adminTestUser struct {
	ID       uint `gorm:"primaryKey"`
	Username string
	Name     string
}

func (adminTestUser) TableName() string { return "users" }

type adminTestSiteConfig struct {
	ID          uint `gorm:"primaryKey"`
	AboutMD     string
	HomeIntroMD string
	FriendsMD   string
}

func (adminTestSiteConfig) TableName() string { return "blog_site_configs" }

func adminImageTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:blogimg_admin_" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&adminTestAsset{}, &adminTestUser{}, &articleRefRow{}, &pageRefRow{}, &adminTestSiteConfig{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestListAdminImageAssetsProtectsFixedSiteMarkdown(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	id := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/about.webp", URL: "/blog/1/about.webp",
	})
	if err := db.Create(&adminTestSiteConfig{AboutMD: "![about](/blog/1/about.webp)"}).Error; err != nil {
		t.Fatal(err)
	}

	result, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{
		Mode: "cleanup", Page: 1, PageSize: 20,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CandidateIDs) != 0 {
		t.Fatalf("fixed-page image %d leaked into candidates %v", id, result.CandidateIDs)
	}
}

func TestAdminImageRecentUploadInvalidatesOldSnapshot(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	id := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/reuploaded.webp", URL: "/blog/1/reuploaded.webp",
	})
	preview, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{Mode: "cleanup"}, now)
	if err != nil || len(preview.CandidateIDs) != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if err := db.Model(&adminTestAsset{}).Where("id = ?", id).
		UpdateColumn("updated_at", now.Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	current, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{Mode: "cleanup"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.CandidateIDs) != 0 {
		t.Fatalf("recently reuploaded image remained cleanup candidate: %v", current.CandidateIDs)
	}
	deleted, err := DeleteAdminImagesSnapshotAt(
		db, &fakeDeleter{base: "https://cdn.example.com"}, []uint{id}, preview.Snapshot, now,
	)
	if deleted != 0 || !errors.Is(err, ErrAdminImageSnapshotStale) {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
}

func createAdminAsset(t *testing.T, db *gorm.DB, row adminTestAsset) uint {
	t.Helper()
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func TestListAdminImageAssetsFiltersCleanupCandidates(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	base := "https://cdn.example.com"
	_ = db.Create(&adminTestUser{ID: 1, Username: "alice", Name: "Alice"}).Error
	_ = db.Create(&adminTestUser{ID: 2, Username: "bob", Name: "Bob"}).Error

	oldOrphanID := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-13 * time.Hour), UpdatedAt: now.Add(-13 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/orphan.webp", URL: "/blog/1/orphan.webp", Purpose: "content",
	})
	referencedID := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/shared.webp", URL: base + "/blog/1/shared.webp", Purpose: "content",
	})
	freshID := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-11 * time.Hour), UpdatedAt: now.Add(-11 * time.Hour), UserID: 2,
		ObjectKey: "/blog/2/fresh.webp", URL: "/blog/2/fresh.webp", Purpose: "cover",
	})
	_ = db.Create(&articleRefRow{
		UserID:  2,
		Content: "![cross-user](https://another.example/blog/1/shared.webp)",
	}).Error

	result, err := ListAdminImageAssetsAt(db, base, AdminImageListOptions{
		Mode: "cleanup", Page: 1, PageSize: 20,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || len(result.List) != 1 {
		t.Fatalf("cleanup result=%+v", result)
	}
	if got := result.CandidateIDs; len(got) != 1 || got[0] != oldOrphanID {
		t.Fatalf("candidate ids=%v want [%d]", got, oldOrphanID)
	}
	if result.List[0].Username != "alice" || result.List[0].Referenced {
		t.Fatalf("candidate=%+v", result.List[0])
	}
	if result.Snapshot == "" {
		t.Fatal("cleanup snapshot must not be empty")
	}
	for _, id := range result.CandidateIDs {
		if id == referencedID || id == freshID {
			t.Fatalf("protected id %d leaked into candidates %v", id, result.CandidateIDs)
		}
	}
}

func TestListAdminImageAssetsIncludesExactTwelveHourBoundary(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	id := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-AdminImageCleanupGracePeriod), UpdatedAt: now.Add(-AdminImageCleanupGracePeriod), UserID: 1,
		ObjectKey: "/blog/1/boundary.webp", URL: "/blog/1/boundary.webp",
	})
	result, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{
		Mode: "cleanup", Page: 1, PageSize: 20,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CandidateIDs) != 1 || result.CandidateIDs[0] != id {
		t.Fatalf("boundary candidates=%v want [%d]", result.CandidateIDs, id)
	}
}

func TestListAdminImageAssetsAllModeMarksReferences(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	hash := ContentHash([]byte("page-image"))
	key := ObjectKeyForHash(3, hash, ".webp")
	id := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-24 * time.Hour), UserID: 3,
		ObjectKey: key, URL: key, ContentHash: hash, Purpose: "content",
	})
	_ = db.Create(&pageRefRow{UserID: 9, ContentMD: "no url", ImageHashes: EncodeImageHashes([]string{hash})}).Error

	result, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{
		Mode: "all", Page: 1, PageSize: 20,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || len(result.List) != 1 || result.List[0].ID != id || !result.List[0].Referenced {
		t.Fatalf("all result=%+v", result)
	}
	if len(result.CandidateIDs) != 0 || result.Snapshot != "" {
		t.Fatalf("all mode must not expose cleanup snapshot: %+v", result)
	}
}

func TestListAdminImageAssetsFailsClosedOnReferenceQueryError(t *testing.T) {
	db := adminImageTestDB(t)
	if err := db.Migrator().DropTable(&pageRefRow{}); err != nil {
		t.Fatal(err)
	}
	_, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{
		Mode: "cleanup", Page: 1, PageSize: 20,
	}, time.Now())
	if err == nil {
		t.Fatal("missing reference table must fail closed")
	}
}

func TestDeleteAdminImageRechecksCandidate(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	id := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-13 * time.Hour), UpdatedAt: now.Add(-13 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/delete.webp", URL: "/blog/1/delete.webp",
	})
	deleter := &fakeDeleter{base: "https://cdn.example.com"}
	deleted, err := DeleteAdminImageAt(db, deleter, id, now)
	if err != nil || !deleted {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	if got := deleter.Deleted(); len(got) != 1 || got[0] != "/blog/1/delete.webp" {
		t.Fatalf("remote deletions=%v", got)
	}
	var count int64
	_ = db.Model(&adminTestAsset{}).Where("id = ?", id).Count(&count).Error
	if count != 0 {
		t.Fatalf("asset row %d still exists", id)
	}
}

func TestDeleteAdminImageRejectsNewReference(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	id := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-13 * time.Hour), UpdatedAt: now.Add(-13 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/now-used.webp", URL: "/blog/1/now-used.webp",
	})
	preview, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{Mode: "cleanup"}, now)
	if err != nil || len(preview.CandidateIDs) != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	_ = db.Create(&articleRefRow{UserID: 9, Content: "![](/blog/1/now-used.webp)"}).Error
	deleter := &fakeDeleter{base: "https://cdn.example.com"}
	deleted, err := DeleteAdminImageAt(db, deleter, id, now)
	if deleted || !errors.Is(err, ErrAdminImageNotCandidate) {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	if len(deleter.Deleted()) != 0 {
		t.Fatalf("referenced image reached remote delete: %v", deleter.Deleted())
	}
}

func TestDeleteAdminImagesSnapshotRejectsChangedCandidates(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	firstID := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-13 * time.Hour), UpdatedAt: now.Add(-13 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/first.webp", URL: "/blog/1/first.webp",
	})
	preview, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{Mode: "cleanup"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_ = createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-14 * time.Hour), UpdatedAt: now.Add(-14 * time.Hour), UserID: 2,
		ObjectKey: "/blog/2/second.webp", URL: "/blog/2/second.webp",
	})
	deleter := &fakeDeleter{base: "https://cdn.example.com"}
	count, err := DeleteAdminImagesSnapshotAt(db, deleter, []uint{firstID}, preview.Snapshot, now)
	if count != 0 || !errors.Is(err, ErrAdminImageSnapshotStale) {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if len(deleter.Deleted()) != 0 {
		t.Fatalf("stale batch reached remote delete: %v", deleter.Deleted())
	}
}

func TestDeleteAdminImagesSnapshotDeletesAllInOneLock(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	idA := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-13 * time.Hour), UpdatedAt: now.Add(-13 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/a.webp", URL: "/blog/1/a.webp",
	})
	idB := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-14 * time.Hour), UpdatedAt: now.Add(-14 * time.Hour), UserID: 2,
		ObjectKey: "/blog/2/b.webp", URL: "/blog/2/b.webp",
	})
	preview, err := ListAdminImageAssetsAt(db, "https://cdn.example.com", AdminImageListOptions{Mode: "cleanup"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.CandidateIDs) != 2 || preview.Snapshot == "" {
		t.Fatalf("preview=%+v", preview)
	}
	deleter := &fakeDeleter{base: "https://cdn.example.com"}
	count, err := DeleteAdminImagesSnapshotAt(db, deleter, preview.CandidateIDs, preview.Snapshot, now)
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v remote=%v", count, err, deleter.Deleted())
	}
	if len(deleter.Deleted()) != 2 {
		t.Fatalf("remote deletions=%v", deleter.Deleted())
	}
	var left int64
	_ = db.Model(&adminTestAsset{}).Where("id IN ?", []uint{idA, idB}).Count(&left).Error
	if left != 0 {
		t.Fatalf("rows left=%d", left)
	}
}

func TestDeleteAdminImageKeepsRowWhenRemoteDeleteFails(t *testing.T) {
	db := adminImageTestDB(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	id := createAdminAsset(t, db, adminTestAsset{
		CreatedAt: now.Add(-13 * time.Hour), UpdatedAt: now.Add(-13 * time.Hour), UserID: 1,
		ObjectKey: "/blog/1/remote-fail.webp", URL: "/blog/1/remote-fail.webp",
	})
	deleted, err := DeleteAdminImageAt(db, &failingAdminDeleter{base: "https://cdn.example.com"}, id, now)
	if deleted || err == nil {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	var count int64
	_ = db.Model(&adminTestAsset{}).Where("id = ?", id).Count(&count).Error
	if count != 1 {
		t.Fatalf("asset row must remain after remote failure, count=%d", count)
	}
}
