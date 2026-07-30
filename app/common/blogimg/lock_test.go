package blogimg

import (
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestUserImageReferenceLockSerializesSameUser(t *testing.T) {
	db := gcTestDB(t)
	enteredFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	enteredSecond := make(chan struct{})
	done := make(chan error, 2)

	go func() {
		done <- WithUserImageReferenceTx(db, 61, func(tx *gorm.DB) error {
			close(enteredFirst)
			<-releaseFirst
			return nil
		})
	}()
	<-enteredFirst
	go func() {
		done <- WithUserImageReferenceTx(db, 61, func(tx *gorm.DB) error {
			close(enteredSecond)
			return nil
		})
	}()

	select {
	case <-enteredSecond:
		t.Fatal("same-user writer entered before the first transaction released its lock")
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-enteredSecond:
	case <-time.After(time.Second):
		t.Fatal("second writer did not enter after lock release")
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdminImageReferenceLockWaitsForAnyUserWriter(t *testing.T) {
	db := gcTestDB(t)
	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	adminEntered := make(chan struct{})
	done := make(chan error, 2)

	go func() {
		done <- WithUserImageReferenceTx(db, 71, func(tx *gorm.DB) error {
			close(writerEntered)
			<-releaseWriter
			return nil
		})
	}()
	<-writerEntered
	go func() {
		done <- WithAdminImageReferenceTx(db, func(tx *gorm.DB) error {
			close(adminEntered)
			return nil
		})
	}()

	select {
	case <-adminEntered:
		t.Fatal("admin cleanup entered while a user reference write was active")
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseWriter)
	select {
	case <-adminEntered:
	case <-time.After(time.Second):
		t.Fatal("admin cleanup did not enter after the user writer released")
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestGCSnapshotWaitsForWriterAndSeesCommittedReference(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(62)
	asset := imageAssetRow{UserID: userID, ObjectKey: "/blog/62/keep.webp", URL: "/blog/62/keep.webp", CreatedAt: gcOld()}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, "https://zhiyuansofts.cn", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids, snapshot := BuildOrphanSnapshot(userID, orphans)

	writerEntered := make(chan struct{})
	releaseWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
			if err := tx.Create(&articleRefRow{UserID: userID, Content: "![keep](/blog/62/keep.webp)"}).Error; err != nil {
				return err
			}
			close(writerEntered)
			<-releaseWriter
			return nil
		})
	}()
	<-writerEntered

	del := &fakeDeleter{base: "https://zhiyuansofts.cn"}
	gcDone := make(chan error, 1)
	go func() {
		_, err := GCUserImagesSnapshot(db, del, userID, ids, snapshot)
		gcDone <- err
	}()
	select {
	case err := <-gcDone:
		t.Fatalf("GC returned before writer committed: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(releaseWriter)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-gcDone; !errors.Is(err, ErrGCStaleSnapshot) {
		t.Fatalf("GC error=%v want stale snapshot after committed reference", err)
	}
	if len(del.Deleted()) != 0 {
		t.Fatalf("referenced image was deleted: %v", del.Deleted())
	}
}

type blockingDeleter struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingDeleter) Configured() bool      { return true }
func (b *blockingDeleter) PublicBaseURL() string { return "https://zhiyuansofts.cn" }
func (b *blockingDeleter) Delete(string) error {
	close(b.started)
	<-b.release
	return nil
}

func TestGCSnapshotHoldsLockUntilRemoteAndDatabaseDeleteFinish(t *testing.T) {
	db := gcTestDB(t)
	userID := uint(63)
	asset := imageAssetRow{UserID: userID, ObjectKey: "/blog/63/gone.webp", URL: "/blog/63/gone.webp", CreatedAt: gcOld()}
	if err := db.Create(&asset).Error; err != nil {
		t.Fatal(err)
	}
	orphans, err := ListUserImageOrphansCheckedAt(db, userID, "https://zhiyuansofts.cn", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ids, snapshot := BuildOrphanSnapshot(userID, orphans)
	del := &blockingDeleter{started: make(chan struct{}), release: make(chan struct{})}
	gcDone := make(chan error, 1)
	go func() {
		_, err := GCUserImagesSnapshot(db, del, userID, ids, snapshot)
		gcDone <- err
	}()
	<-del.started

	writerEntered := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- WithUserImageReferenceTx(db, userID, func(tx *gorm.DB) error {
			close(writerEntered)
			return tx.Create(&articleRefRow{UserID: userID, Content: "![late](/blog/63/gone.webp)"}).Error
		})
	}()
	select {
	case <-writerEntered:
		t.Fatal("writer entered while GC still held the user reference lock")
	case <-time.After(40 * time.Millisecond):
	}
	close(del.release)
	if err := <-gcDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
}
