package service

import (
	"errors"
	"testing"
	"time"

	"cwxu-algo/app/common/blogimg"
	"cwxu-algo/app/user/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func imageRequestTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.BlogImageUploadRequest{}, &model.BlogSiteConfig{}, &model.BlogImageAsset{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRegisterBlogImageAssetWaitsForSameUserLock(t *testing.T) {
	db := imageRequestTestDB(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- blogimg.WithUserImageReferenceTx(db, 81, func(tx *gorm.DB) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	registerDone := make(chan error, 1)
	go func() {
		registerDone <- registerBlogImageAsset(db, 81, "/blog/81/x.webp", "/blog/81/x.webp", blogimg.ContentHash([]byte("x")), "content", nil)
	}()
	select {
	case err := <-registerDone:
		t.Fatalf("asset registration bypassed user lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-registerDone; err != nil {
		t.Fatal(err)
	}
}

type blockingBlogImageWriter struct {
	started chan struct{}
	release chan struct{}
}

type failingBlogImageWriter struct {
	err  error
	puts int
}

func (writer *failingBlogImageWriter) Put(string, []byte, string) error {
	writer.puts++
	return writer.err
}

func (writer *blockingBlogImageWriter) Put(string, []byte, string) error {
	close(writer.started)
	<-writer.release
	return nil
}

func TestPutAndRegisterBlogImageDoesNotHoldDatabaseLockDuringRemotePut(t *testing.T) {
	db := imageRequestTestDB(t)
	writer := &blockingBlogImageWriter{started: make(chan struct{}), release: make(chan struct{})}
	hash := blogimg.ContentHash([]byte("uploaded"))
	key := blogimg.ObjectKeyForHash(87, hash, ".webp")
	uploadDone := make(chan error, 1)
	go func() {
		uploadDone <- putAndRegisterBlogImage(db, writer, 87, key, []byte("uploaded"), "image/webp", key, hash, "content")
	}()
	<-writer.started
	gcEntered := make(chan struct{})
	gcDone := make(chan error, 1)
	go func() {
		gcDone <- blogimg.WithUserImageReferenceTx(db, 87, func(tx *gorm.DB) error {
			close(gcEntered)
			return nil
		})
	}()
	select {
	case <-gcEntered:
	case <-time.After(time.Second):
		t.Fatal("remote Put held the user advisory lock/database transaction")
	}
	if err := <-gcDone; err != nil {
		t.Fatal(err)
	}
	close(writer.release)
	if err := <-uploadDone; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.BlogImageAsset{}).Where("user_id = ? AND object_key = ?", 87, key).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("asset count=%d err=%v", count, err)
	}
}

func TestPutFailureLeavesDurablePendingReservation(t *testing.T) {
	db := imageRequestTestDB(t)
	hash := blogimg.ContentHash([]byte("pending"))
	key := blogimg.ObjectKeyForHash(88, hash, ".webp")
	writer := &failingBlogImageWriter{err: errors.New("put failed")}
	if err := putAndRegisterBlogImage(db, writer, 88, key, []byte("pending"), "image/webp", key, hash, "content"); err == nil {
		t.Fatal("Put failure should be returned")
	}
	var asset model.BlogImageAsset
	if err := db.Where("user_id = ? AND object_key = ?", 88, key).First(&asset).Error; err != nil {
		t.Fatalf("reservation missing after Put failure: %v", err)
	}
	if asset.Status != model.BlogImageAssetPending {
		t.Fatalf("status=%q want pending", asset.Status)
	}
}

func TestFinalizeFailureLeavesReservationForSafeRetry(t *testing.T) {
	db := imageRequestTestDB(t)
	hash := blogimg.ContentHash([]byte("finalize"))
	key := blogimg.ObjectKeyForHash(89, hash, ".webp")
	if err := db.Exec(`CREATE TRIGGER fail_asset_finalize BEFORE UPDATE OF status ON blog_image_assets
		WHEN NEW.status = 'ready' BEGIN SELECT RAISE(FAIL, 'finalize failed'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	writer := &failingBlogImageWriter{}
	if err := putAndRegisterBlogImage(db, writer, 89, key, []byte("finalize"), "image/webp", key, hash, "content"); err == nil {
		t.Fatal("finalize failure should be returned")
	}
	var asset model.BlogImageAsset
	if err := db.Where("object_key = ?", key).First(&asset).Error; err != nil {
		t.Fatal(err)
	}
	if asset.Status != model.BlogImageAssetPending || writer.puts != 1 {
		t.Fatalf("status=%q puts=%d", asset.Status, writer.puts)
	}
}

func TestPendingReservationRetryFinalizesSameAsset(t *testing.T) {
	db := imageRequestTestDB(t)
	hash := blogimg.ContentHash([]byte("retry"))
	key := blogimg.ObjectKeyForHash(90, hash, ".webp")
	if err := putAndRegisterBlogImage(db, &failingBlogImageWriter{err: errors.New("first failure")}, 90, key, []byte("retry"), "image/webp", key, hash, "content"); err == nil {
		t.Fatal("first Put should fail")
	}
	if err := putAndRegisterBlogImage(db, &failingBlogImageWriter{}, 90, key, []byte("retry"), "image/webp", key, hash, "content"); err != nil {
		t.Fatal(err)
	}
	var assets []model.BlogImageAsset
	if err := db.Where("user_id = ? AND object_key = ?", 90, key).Find(&assets).Error; err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].Status != model.BlogImageAssetReady {
		t.Fatalf("assets=%+v", assets)
	}
}

func TestExistingReadyAssetNeverDowngradesOnRetryFailure(t *testing.T) {
	db := imageRequestTestDB(t)
	hash := blogimg.ContentHash([]byte("ready"))
	key := blogimg.ObjectKeyForHash(91, hash, ".webp")
	asset := model.BlogImageAsset{UserID: 91, ObjectKey: key, URL: key, ContentHash: hash, Purpose: "content", Status: model.BlogImageAssetReady}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	if err := putAndRegisterBlogImage(db, &failingBlogImageWriter{err: errors.New("retry failed")}, 91, key, []byte("ready"), "image/webp", key, hash, "content"); err == nil {
		t.Fatal("retry Put should fail")
	}
	if err := db.First(&asset, asset.ID).Error; err != nil {
		t.Fatal(err)
	}
	if asset.Status != model.BlogImageAssetReady {
		t.Fatalf("ready asset downgraded to %q", asset.Status)
	}
}

func TestImagePermissionChangesWaitForSameUserLock(t *testing.T) {
	db := imageRequestTestDB(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- blogimg.WithUserImageReferenceTx(db, 82, func(tx *gorm.DB) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	changeDone := make(chan error, 1)
	go func() {
		_, err := setAdminImageUploadEnabled(db, 82, 7, true)
		changeDone <- err
	}()
	select {
	case err := <-changeDone:
		t.Fatalf("permission change bypassed user lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-changeDone; err != nil {
		t.Fatal(err)
	}
}

func TestCreatePendingImageUploadRequestRefusesAlreadyEnabledUser(t *testing.T) {
	db := imageRequestTestDB(t)
	if err := setUserImageUploadEnabledDB(db, 83, true); err != nil {
		t.Fatal(err)
	}
	if _, created, err := createPendingImageUploadRequest(db, 83, "already enabled"); !errors.Is(err, ErrImageUploadAlreadyEnabled) || created {
		t.Fatalf("created=%v err=%v want ErrImageUploadAlreadyEnabled", created, err)
	}
	var pending int64
	if err := db.Model(&model.BlogImageUploadRequest{}).Where("user_id = ? AND status = ?", 83, model.BlogImageUploadPending).Count(&pending).Error; err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
}

func TestConcurrentApplyCannotLeavePendingAfterEnable(t *testing.T) {
	db := imageRequestTestDB(t)
	userID := uint(85)
	entered := make(chan struct{})
	release := make(chan struct{})
	enableDone := make(chan error, 1)
	go func() {
		enableDone <- blogimg.WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
			if err := setUserImageUploadEnabledDB(tx, userID, true); err != nil {
				return err
			}
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	applyDone := make(chan error, 1)
	go func() {
		_, _, err := createPendingImageUploadRequest(db, userID, "concurrent apply")
		applyDone <- err
	}()
	select {
	case err := <-applyDone:
		t.Fatalf("apply bypassed enable lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err := <-enableDone; err != nil {
		t.Fatal(err)
	}
	if err := <-applyDone; !errors.Is(err, ErrImageUploadAlreadyEnabled) {
		t.Fatalf("apply err=%v want ErrImageUploadAlreadyEnabled", err)
	}
	var pending int64
	if err := db.Model(&model.BlogImageUploadRequest{}).Where("user_id = ? AND status = ?", userID, model.BlogImageUploadPending).Count(&pending).Error; err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
}

func TestDirectEnableAndReviewShareLockOrder(t *testing.T) {
	db := imageRequestTestDB(t)
	row := model.BlogImageUploadRequest{UserID: 86, Reason: "same lock order", Status: model.BlogImageUploadPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		<-start
		_, err := setAdminImageUploadEnabled(db, row.UserID, 10, true)
		done <- err
	}()
	go func() {
		<-start
		_, _, err := reviewImageUploadRequest(db, row.ID, 11, "approve", "ok")
		if errors.Is(err, ErrImageUploadAlreadyReviewed) {
			err = nil
		}
		done <- err
	}()
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("direct enable/review deadlocked")
		}
	}
	var pending int64
	if err := db.Model(&model.BlogImageUploadRequest{}).Where("user_id = ? AND status = ?", row.UserID, model.BlogImageUploadPending).Count(&pending).Error; err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	var cfg model.BlogSiteConfig
	if err := db.Where("user_id = ?", row.UserID).First(&cfg).Error; err != nil || !cfg.ImageUploadEnabled {
		t.Fatalf("enabled=%v err=%v", cfg.ImageUploadEnabled, err)
	}
}

func TestCreatePendingImageUploadRequestReturnsExistingWithoutDuplicate(t *testing.T) {
	db := imageRequestTestDB(t)
	first, created, err := createPendingImageUploadRequest(db, 17, "same reason")
	if err != nil || !created {
		t.Fatalf("first created=%v err=%v", created, err)
	}
	second, created, err := createPendingImageUploadRequest(db, 17, "same reason")
	if err != nil || created {
		t.Fatalf("second created=%v err=%v", created, err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate request returned id=%d want existing id=%d", second.ID, first.ID)
	}
	var count int64
	if err := db.Model(&model.BlogImageUploadRequest{}).
		Where("user_id = ? AND status = ?", 17, model.BlogImageUploadPending).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("pending count=%d err=%v", count, err)
	}
}

func TestReviewImageUploadRequestRollsBackStatusWhenApprovalGrantFails(t *testing.T) {
	db := imageRequestTestDB(t)
	row := model.BlogImageUploadRequest{UserID: 8, Reason: "approval reason", Status: model.BlogImageUploadPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_site_config BEFORE INSERT ON blog_site_configs BEGIN SELECT RAISE(FAIL, 'grant failed'); END;`).Error; err != nil {
		t.Fatal(err)
	}

	if _, _, err := reviewImageUploadRequest(db, row.ID, 99, "approve", ""); err == nil {
		t.Fatal("approval should fail when granting permission fails")
	}
	var stored model.BlogImageUploadRequest
	if err := db.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.BlogImageUploadPending {
		t.Fatalf("review status escaped rolled-back transaction: %q", stored.Status)
	}
}

func TestReviewImageUploadRequestOnlyTransitionsPendingOnce(t *testing.T) {
	db := imageRequestTestDB(t)
	row := model.BlogImageUploadRequest{UserID: 9, Reason: "approval reason", Status: model.BlogImageUploadPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if _, status, err := reviewImageUploadRequest(db, row.ID, 100, "approve", "ok"); err != nil || status != model.BlogImageUploadApproved {
		t.Fatalf("first review status=%q err=%v", status, err)
	}
	if _, _, err := reviewImageUploadRequest(db, row.ID, 101, "reject", "late"); !errors.Is(err, ErrImageUploadAlreadyReviewed) {
		t.Fatalf("second review error=%v want ErrImageUploadAlreadyReviewed", err)
	}
	var cfg model.BlogSiteConfig
	if err := db.Where("user_id = ?", row.UserID).First(&cfg).Error; err != nil || !cfg.ImageUploadEnabled {
		t.Fatalf("approval grant missing: enabled=%v err=%v", cfg.ImageUploadEnabled, err)
	}
}

func TestSetAdminImageUploadEnabledApprovesPendingInSameTransaction(t *testing.T) {
	db := imageRequestTestDB(t)
	row := model.BlogImageUploadRequest{UserID: 21, Reason: "direct approval", Status: model.BlogImageUploadPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	reviewed, err := setAdminImageUploadEnabled(db, row.UserID, 77, true)
	if err != nil || reviewed != 1 {
		t.Fatalf("reviewed=%d err=%v", reviewed, err)
	}
	var stored model.BlogImageUploadRequest
	if err := db.First(&stored, row.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != model.BlogImageUploadApproved || stored.ReviewerID != 77 {
		t.Fatalf("request status=%q reviewer=%d", stored.Status, stored.ReviewerID)
	}
	var cfg model.BlogSiteConfig
	if err := db.Where("user_id = ?", row.UserID).First(&cfg).Error; err != nil || !cfg.ImageUploadEnabled {
		t.Fatalf("permission enabled=%v err=%v", cfg.ImageUploadEnabled, err)
	}
}

func TestSetAdminImageUploadEnabledAllowsNoPendingRequest(t *testing.T) {
	db := imageRequestTestDB(t)
	reviewed, err := setAdminImageUploadEnabled(db, 22, 78, true)
	if err != nil || reviewed != 0 {
		t.Fatalf("reviewed=%d err=%v", reviewed, err)
	}
	var cfg model.BlogSiteConfig
	if err := db.Where("user_id = ?", 22).First(&cfg).Error; err != nil || !cfg.ImageUploadEnabled {
		t.Fatalf("permission enabled=%v err=%v", cfg.ImageUploadEnabled, err)
	}
}

func TestSetAdminImageUploadEnabledRollsBackPermissionWhenPendingUpdateFails(t *testing.T) {
	db := imageRequestTestDB(t)
	row := model.BlogImageUploadRequest{UserID: 23, Reason: "must rollback", Status: model.BlogImageUploadPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_direct_review BEFORE UPDATE ON blog_image_upload_requests BEGIN SELECT RAISE(FAIL, 'review failed'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := setAdminImageUploadEnabled(db, row.UserID, 79, true); err == nil {
		t.Fatal("direct enable should fail when pending transition fails")
	}
	var cfgCount int64
	if err := db.Model(&model.BlogSiteConfig{}).Where("user_id = ?", row.UserID).Count(&cfgCount).Error; err != nil {
		t.Fatal(err)
	}
	if cfgCount != 0 {
		t.Fatal("permission escaped rolled-back transaction")
	}
	if err := db.First(&row, row.ID).Error; err != nil || row.Status != model.BlogImageUploadPending {
		t.Fatalf("pending request changed status=%q err=%v", row.Status, err)
	}
}
