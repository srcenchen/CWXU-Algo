package data

import (
	"errors"
	"testing"

	"cwxu-algo/app/user/internal/data/model"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func postgresIndexMockDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB, PreferSimpleProtocol: true}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	return db, mock
}

func matchingPendingIndexRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"index_exists", "valid", "is_unique", "user_id_only", "predicate"}).
		AddRow(true, true, true, true, "((status)::text = 'pending'::text)")
}

func TestEnsureBlogImagePendingUniqueIndexReconcilesAndEnforces(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE blog_image_upload_requests (
		id integer PRIMARY KEY AUTOINCREMENT, created_at datetime, updated_at datetime,
		user_id integer NOT NULL, reason text NOT NULL, status text NOT NULL,
		review_note text, reviewer_id integer, reviewed_at datetime
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO blog_image_upload_requests(user_id, reason, status) VALUES
		(7, 'old', 'pending'), (7, 'new', 'pending')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := ensureBlogImagePendingUniqueIndex(db); err != nil {
		t.Fatal(err)
	}
	var pending int64
	if err := db.Model(&model.BlogImageUploadRequest{}).Where("user_id = ? AND status = ?", 7, model.BlogImageUploadPending).Count(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending count=%d, want 1", pending)
	}
	err = db.Create(&model.BlogImageUploadRequest{UserID: 7, Reason: "race", Status: model.BlogImageUploadPending}).Error
	if err == nil {
		t.Fatal("partial unique index did not reject a second pending request")
	}
	if err := db.Create(&model.BlogImageUploadRequest{UserID: 7, Reason: "history", Status: model.BlogImageUploadRejected}).Error; err != nil {
		t.Fatalf("partial index rejected historical request: %v", err)
	}
}

func TestBlogImagePendingIndexStateRequiresExactProductionDefinition(t *testing.T) {
	validPredicates := []string{
		"(status = 'pending'::text)",
		"((status)::text = 'pending'::text)",
		"status = 'pending'",
	}
	for _, predicate := range validPredicates {
		state := blogImagePendingIndexState{Exists: true, Valid: true, Unique: true, UserIDOnly: true, Predicate: predicate}
		if !state.matchesProductionDefinition() {
			t.Fatalf("valid predicate rejected: %q", predicate)
		}
	}
	bad := []blogImagePendingIndexState{
		{Exists: true, Valid: false, Unique: true, UserIDOnly: true, Predicate: "status = 'pending'"},
		{Exists: true, Valid: true, Unique: false, UserIDOnly: true, Predicate: "status = 'pending'"},
		{Exists: true, Valid: true, Unique: true, UserIDOnly: false, Predicate: "status = 'pending'"},
		{Exists: true, Valid: true, Unique: true, UserIDOnly: true, Predicate: "status = 'approved'"},
	}
	for _, state := range bad {
		if state.matchesProductionDefinition() {
			t.Fatalf("mismatched index accepted: %+v", state)
		}
	}
}

func TestEnsurePostgresBlogImagePendingUniqueIndexAcceptsOnlyMatchingDefinition(t *testing.T) {
	db, mock := postgresIndexMockDB(t)
	mock.ExpectExec(`(?s)UPDATE blog_image_upload_requests SET status = 'rejected'.*GROUP BY user_id`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
		WithArgs("uniq_blog_img_req_pending_user").WillReturnRows(matchingPendingIndexRows())
	if err := ensurePostgresBlogImagePendingUniqueIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsurePostgresBlogImagePendingUniqueIndexRetriesDuplicateWindow(t *testing.T) {
	db, mock := postgresIndexMockDB(t)
	// Attempt 1: no index, a concurrent duplicate makes CREATE fail and leaves an invalid index.
	mock.ExpectExec(`(?s)UPDATE blog_image_upload_requests SET status = 'rejected'.*GROUP BY user_id`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
		WithArgs("uniq_blog_img_req_pending_user").
		WillReturnRows(sqlmock.NewRows([]string{"index_exists", "valid", "is_unique", "user_id_only", "predicate"}))
	mock.ExpectExec(`(?s)CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_blog_img_req_pending_user`).
		WillReturnError(errors.New("duplicate key during concurrent index build"))
	// Attempt 2: dedupe again, drop the invalid index, recreate, and verify exact definition.
	mock.ExpectExec(`(?s)UPDATE blog_image_upload_requests SET status = 'rejected'.*GROUP BY user_id`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
		WithArgs("uniq_blog_img_req_pending_user").
		WillReturnRows(sqlmock.NewRows([]string{"index_exists", "valid", "is_unique", "user_id_only", "predicate"}).
			AddRow(true, false, true, true, "((status)::text = 'pending'::text)"))
	mock.ExpectExec(`DROP INDEX CONCURRENTLY IF EXISTS uniq_blog_img_req_pending_user`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_blog_img_req_pending_user`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
		WithArgs("uniq_blog_img_req_pending_user").WillReturnRows(matchingPendingIndexRows())

	if err := ensurePostgresBlogImagePendingUniqueIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsurePostgresBlogImagePendingUniqueIndexReplacesMismatchedDefinition(t *testing.T) {
	db, mock := postgresIndexMockDB(t)
	mock.ExpectExec(`(?s)UPDATE blog_image_upload_requests SET status = 'rejected'.*GROUP BY user_id`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
		WithArgs("uniq_blog_img_req_pending_user").
		WillReturnRows(sqlmock.NewRows([]string{"index_exists", "valid", "is_unique", "user_id_only", "predicate"}).
			AddRow(true, true, false, true, "status = 'pending'"))
	mock.ExpectExec(`DROP INDEX CONCURRENTLY IF EXISTS uniq_blog_img_req_pending_user`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`(?s)CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_blog_img_req_pending_user`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
		WithArgs("uniq_blog_img_req_pending_user").WillReturnRows(matchingPendingIndexRows())
	if err := ensurePostgresBlogImagePendingUniqueIndex(db); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsurePostgresBlogImagePendingUniqueIndexFailsAfterBoundedRetries(t *testing.T) {
	db, mock := postgresIndexMockDB(t)
	for attempt := 0; attempt < 3; attempt++ {
		mock.ExpectExec(`(?s)UPDATE blog_image_upload_requests SET status = 'rejected'.*GROUP BY user_id`).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
			WithArgs("uniq_blog_img_req_pending_user").
			WillReturnRows(sqlmock.NewRows([]string{"index_exists", "valid", "is_unique", "user_id_only", "predicate"}))
		mock.ExpectExec(`(?s)CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uniq_blog_img_req_pending_user`).
			WillReturnError(errors.New("concurrent duplicate"))
	}
	if err := ensurePostgresBlogImagePendingUniqueIndex(db); err == nil {
		t.Fatal("exhausted concurrent index builds must fail startup migration")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
