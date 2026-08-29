package dal

import (
	"context"
	"strings"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func userACFingerprintTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SubmitLog{}, &model.UserACProblem{}, &model.UserACProblemDay{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func fingerprintFor(t *testing.T, db *gorm.DB, userID int64) string {
	t.Helper()
	fp, err := UserACFingerprint(context.Background(), db, userID)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func createFingerprintSubmit(t *testing.T, db *gorm.DB, row model.SubmitLog) model.SubmitLog {
	t.Helper()
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestUserACFingerprintTracksRealTerminalProcessEvidence(t *testing.T) {
	db := userACFingerprintTestDB(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	base := createFingerprintSubmit(t, db, model.SubmitLog{
		UserID: 1, Platform: "Codeforces", SubmitID: "100", Status: "AC", IsAC: true,
		Problem: "A", Time: now,
	})
	before := fingerprintFor(t, db, 1)

	createFingerprintSubmit(t, db, model.SubmitLog{
		UserID: 1, Platform: "Codeforces", SubmitID: "101", Status: "WA",
		Problem: "B", Time: now.Add(time.Minute),
	})
	afterFailure := fingerprintFor(t, db, 1)
	if afterFailure == before {
		t.Fatal("a new terminal failed submit must change evidence")
	}

	createFingerprintSubmit(t, db, model.SubmitLog{
		UserID: 1, Platform: "AtCoder", SubmitID: "old-1", Status: "WA",
		Problem: "old", Time: now.Add(-365 * 24 * time.Hour),
	})
	afterHistory := fingerprintFor(t, db, 1)
	if afterHistory == afterFailure {
		t.Fatal("a late historical terminal submit must change evidence")
	}

	pid := uint(99)
	if err := db.Model(&model.SubmitLog{}).Where("id = ?", base.ID).Update("problem_id", pid).Error; err != nil {
		t.Fatal(err)
	}
	afterBinding := fingerprintFor(t, db, 1)
	if afterBinding == afterHistory {
		t.Fatal("binding a real submit must change evidence")
	}

	createFingerprintSubmit(t, db, model.SubmitLog{
		UserID: 2, Platform: "Codeforces", SubmitID: "other", Status: "WA",
		Problem: "X", Time: now,
	})
	if got := fingerprintFor(t, db, 1); got != afterBinding {
		t.Fatalf("another user's submit changed evidence: before=%q after=%q", afterBinding, got)
	}
}

func TestUserACFingerprintTracksCanonicalACKeyPromotion(t *testing.T) {
	db := userACFingerprintTestDB(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	if err := db.Create(&model.UserACProblem{
		UserID: 7, ProblemKey: "e:Codeforces:100A", Platform: "Codeforces", FirstACAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	before := fingerprintFor(t, db, 7)
	if err := PromoteUserACKeysToProblemID(context.Background(), db, 7, []string{"e:Codeforces:100A"}, 42); err != nil {
		t.Fatal(err)
	}
	after := fingerprintFor(t, db, 7)
	if after == before {
		t.Fatal("canonical AC key promotion must change evidence")
	}
}

func TestUserACFingerprintIgnoresNonTerminalOrEmptySubmitIDs(t *testing.T) {
	db := userACFingerprintTestDB(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	before := fingerprintFor(t, db, 8)
	createFingerprintSubmit(t, db, model.SubmitLog{
		UserID: 8, Platform: "Codeforces", SubmitID: "pending", Status: "TESTING", Time: now,
	})
	createFingerprintSubmit(t, db, model.SubmitLog{
		UserID: 8, Platform: "AtCoder", SubmitID: "", Status: "WA", Time: now,
	})
	if got := fingerprintFor(t, db, 8); got != before {
		t.Fatalf("pending/empty submit IDs must not be process evidence: before=%q after=%q", before, got)
	}
}

func TestUserACFingerprintTracksTerminalEvidenceContentChanges(t *testing.T) {
	db := userACFingerprintTestDB(t)
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	pid1 := uint(1)
	row := createFingerprintSubmit(t, db, model.SubmitLog{
		UserID: 9, Platform: "Codeforces", SubmitID: "900", Status: "WA", IsAC: false,
		ProblemID: &pid1, ExternalID: "900A", Problem: "A", Time: now,
	})
	before := fingerprintFor(t, db, 9)
	if err := db.Model(&model.SubmitLog{}).Where("id = ?", row.ID).
		Updates(map[string]interface{}{"status": "AC", "is_ac": true}).Error; err != nil {
		t.Fatal(err)
	}
	afterVerdict := fingerprintFor(t, db, 9)
	if afterVerdict == before {
		t.Fatal("terminal verdict/is_ac correction must change evidence")
	}
	if err := db.Model(&model.SubmitLog{}).Where("id = ?", row.ID).Update("time", now.Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	afterTime := fingerprintFor(t, db, 9)
	if afterTime == afterVerdict {
		t.Fatal("terminal submit time correction must change evidence")
	}
	pid2 := uint(2)
	if err := db.Model(&model.SubmitLog{}).Where("id = ?", row.ID).Update("problem_id", pid2).Error; err != nil {
		t.Fatal(err)
	}
	if got := fingerprintFor(t, db, 9); got == afterTime {
		t.Fatal("terminal submit reassignment between non-zero problem IDs must change evidence")
	}
}

func TestUserACFingerprintPostgresUsesTerminalContentDigest(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)MD5\(COALESCE\(STRING_AGG\(CONCAT_WS\(CHR\(31\),.*id::text.*platform.*submit_id.*status.*is_ac.*time AT TIME ZONE 'UTC'.*problem_id.*external_id.*CHR\(30\) ORDER BY id\), ''\)\)`).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"terminal_count", "max_id", "bound_count", "content_hash"}).
			AddRow(2, 99, 1, "postgres-content-digest"))
	mock.ExpectQuery(`SELECT .*problem_key.* FROM "user_ac_problems" WHERE user_id = \$1`).
		WithArgs(int64(10)).
		WillReturnRows(sqlmock.NewRows([]string{"problem_key"}).AddRow("p:1"))
	mock.ExpectCommit()

	fp, err := UserACFingerprint(context.Background(), db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fp, "_spostgres-content-digest_") {
		t.Fatalf("postgres content digest missing from fingerprint: %q", fp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
