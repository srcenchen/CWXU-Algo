package dal

import (
	"context"
	"strings"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func profileEvidenceRevisionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.UserACProblem{}, &model.Platform{}, &model.ProblemTag{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestProfileEvidenceRevisionTracksGlobalProblemTagFacts(t *testing.T) {
	db := profileEvidenceRevisionTestDB(t)
	if err := InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatal(err)
	}
	before := readProfileEvidenceRevision(t, db, 700)
	tag := model.ProblemTag{ProblemID: 70, Tag: "dp"}
	if err := db.Create(&tag).Error; err != nil {
		t.Fatal(err)
	}
	afterInsert := readProfileEvidenceRevision(t, db, 700)
	if afterInsert.DatasetRevision <= before.DatasetRevision || afterInsert.UserRevision != before.UserRevision {
		t.Fatalf("problem tag insert must bump only dataset identity: before=%+v after=%+v", before, afterInsert)
	}
	if err := db.Delete(&tag).Error; err != nil {
		t.Fatal(err)
	}
	afterDelete := readProfileEvidenceRevision(t, db, 700)
	if afterDelete.DatasetRevision <= afterInsert.DatasetRevision || afterDelete.UserRevision != afterInsert.UserRevision {
		t.Fatalf("problem tag delete must bump only dataset identity: before=%+v after=%+v", afterInsert, afterDelete)
	}
}

func readProfileEvidenceRevision(t *testing.T, db *gorm.DB, userID int64) ProfileEvidenceIdentity {
	t.Helper()
	identity, err := ReadProfileEvidenceIdentity(context.Background(), db, userID)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func requireProfileEvidenceRevisionAdvance(t *testing.T, before, after ProfileEvidenceIdentity) {
	t.Helper()
	if after.DatasetRevision != before.DatasetRevision || after.UserRevision <= before.UserRevision {
		t.Fatalf("evidence revision did not advance user only: before=%+v after=%+v", before, after)
	}
}

func TestProfileEvidenceRevisionTriggersTrackAllUserEvidenceWrites(t *testing.T) {
	db := profileEvidenceRevisionTestDB(t)
	ctx := context.Background()
	// Existing evidence must receive a durable zero revision only after the
	// triggers are installed, so no migration-window write can be missed.
	if err := db.Create(&model.SubmitLog{UserID: 72, Platform: "Luogu", SubmitID: "old", Status: "WA", Time: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatal(err)
	}
	old := readProfileEvidenceRevision(t, db, 72)
	if old.UserRevision != 0 || old.String() != "d0:u0" || old.CacheKey() != old.String() {
		t.Fatalf("backfilled zero identity=%+v string=%q", old, old.String())
	}

	base := readProfileEvidenceRevision(t, db, 7)
	log := model.SubmitLog{UserID: 7, Platform: "Luogu", SubmitID: "1", Status: "WA", ExternalID: "P1000", Time: time.Now()}
	if err := db.Create(&log).Error; err != nil {
		t.Fatal(err)
	}
	afterInsert := readProfileEvidenceRevision(t, db, 7)
	requireProfileEvidenceRevisionAdvance(t, base, afterInsert)

	problemID := uint(9)
	if err := db.Model(&log).Updates(map[string]interface{}{
		"status": "AC", "problem_id": problemID, "time": time.Now().Add(time.Minute), "external_id": "P1001",
	}).Error; err != nil {
		t.Fatal(err)
	}
	afterCorrection := readProfileEvidenceRevision(t, db, 7)
	requireProfileEvidenceRevisionAdvance(t, afterInsert, afterCorrection)
	beforeMovedOld := afterCorrection
	beforeMovedNew := readProfileEvidenceRevision(t, db, 8)
	if err := db.Model(&log).Update("user_id", 8).Error; err != nil {
		t.Fatal(err)
	}
	afterMovedOld := readProfileEvidenceRevision(t, db, 7)
	afterMovedNew := readProfileEvidenceRevision(t, db, 8)
	requireProfileEvidenceRevisionAdvance(t, beforeMovedOld, afterMovedOld)
	requireProfileEvidenceRevisionAdvance(t, beforeMovedNew, afterMovedNew)
	if err := db.Model(&log).Update("user_id", 7).Error; err != nil {
		t.Fatal(err)
	}
	afterCorrection = readProfileEvidenceRevision(t, db, 7)

	key := model.UserACProblem{UserID: 7, ProblemKey: "e:Luogu:P1001", Platform: "Luogu", FirstACAt: time.Now()}
	if err := db.Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	afterKeyInsert := readProfileEvidenceRevision(t, db, 7)
	requireProfileEvidenceRevisionAdvance(t, afterCorrection, afterKeyInsert)
	if err := db.Delete(&key).Error; err != nil {
		t.Fatal(err)
	}
	afterKeyPromotionDelete := readProfileEvidenceRevision(t, db, 7)
	requireProfileEvidenceRevisionAdvance(t, afterKeyInsert, afterKeyPromotionDelete)
	if err := db.Create(&model.UserACProblem{UserID: 7, ProblemKey: "p:9", Platform: "Luogu", FirstACAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	afterKeyPromotionInsert := readProfileEvidenceRevision(t, db, 7)
	requireProfileEvidenceRevisionAdvance(t, afterKeyPromotionDelete, afterKeyPromotionInsert)

	platform := model.Platform{UserID: 7, Platform: "Luogu", Username: "u", ClientSyncCompletedAt: ptrTime(time.Now())}
	if err := db.Create(&platform).Error; err != nil {
		t.Fatal(err)
	}
	afterPlatformInsert := readProfileEvidenceRevision(t, db, 7)
	requireProfileEvidenceRevisionAdvance(t, afterKeyPromotionInsert, afterPlatformInsert)
	if err := db.Model(&platform).Update("client_sync_head_submit_id", "999").Error; err != nil {
		t.Fatal(err)
	}
	afterPlatformUpdate := readProfileEvidenceRevision(t, db, 7)
	requireProfileEvidenceRevisionAdvance(t, afterPlatformInsert, afterPlatformUpdate)
	if err := db.Delete(&log).Error; err != nil {
		t.Fatal(err)
	}
	afterTombstone := readProfileEvidenceRevision(t, db, 7)
	requireProfileEvidenceRevisionAdvance(t, afterPlatformUpdate, afterTombstone)
	var tombstones int64
	if err := db.Model(&model.UserProfileEvidenceVersion{}).Where("user_id = ?", 7).Count(&tombstones).Error; err != nil || tombstones != 1 {
		t.Fatalf("user revision tombstone rows=%d err=%v", tombstones, err)
	}

	beforeRollback := afterTombstone
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&model.SubmitLog{UserID: 7, Platform: "Codeforces", SubmitID: "rollback", Status: "WA", Time: time.Now()}).Error; err != nil {
			return err
		}
		return gorm.ErrInvalidTransaction
	})
	if err == nil {
		t.Fatal("rollback transaction unexpectedly succeeded")
	}
	if afterRollback := readProfileEvidenceRevision(t, db, 7); afterRollback != beforeRollback {
		t.Fatalf("rolled-back write bumped durable identity: before=%+v after=%+v", beforeRollback, afterRollback)
	}

	queries := make([]string, 0, 3)
	captureSQL := func(tx *gorm.DB) { queries = append(queries, tx.Statement.SQL.String()) }
	if err := db.Callback().Raw().After("gorm:raw").Register("capture_profile_evidence_identity_raw", captureSQL); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().After("gorm:query").Register("capture_profile_evidence_identity_query", captureSQL); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Row().After("gorm:row").Register("capture_profile_evidence_identity_row", captureSQL); err != nil {
		t.Fatal(err)
	}
	_ = readProfileEvidenceRevision(t, db, 7)
	if len(queries) == 0 {
		t.Fatal("identity read did not hit an observable SQL callback")
	}
	joinedRevisionState := false
	for _, query := range queries {
		normalized := strings.ToLower(query)
		if strings.Contains(normalized, "profile_evidence_dataset_state") && strings.Contains(normalized, "user_profile_evidence_versions") {
			joinedRevisionState = true
		}
		for _, facts := range []string{"submit_logs", "user_ac_problems", "platforms"} {
			if strings.Contains(normalized, facts) {
				t.Fatalf("identity read scanned evidence table %q: %s", facts, query)
			}
		}
	}
	if !joinedRevisionState {
		t.Fatalf("identity read did not execute the revision-state SELECT: %v", queries)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestProfileEvidenceRevisionInstallationDDLAndDatasetFailure(t *testing.T) {
	db := profileEvidenceRevisionTestDB(t)
	if err := InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatalf("SQLite trigger installation must execute: %v", err)
	}
	if got := model.ProfileEvidencePostgresDatasetTriggerDDL(); !strings.Contains(got, "AFTER TRUNCATE") || !strings.Contains(got, "profile_evidence_dataset_state") ||
		!strings.Contains(got, "REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows") || !strings.Contains(got, "FOR EACH STATEMENT") || !strings.Contains(got, "SELECT DISTINCT user_id") {
		t.Fatalf("PostgreSQL dataset trigger DDL=%q", got)
	}

	missing, err := gorm.Open(sqlite.Open("file:"+t.Name()+"-missing?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.AutoMigrate(&model.UserProfileEvidenceVersion{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProfileEvidenceIdentity(context.Background(), missing, 8); err == nil {
		t.Fatal("missing dataset state must fail closed")
	}
	t.Run("unready", func(t *testing.T) {
		unready := profileEvidenceRevisionTestDB(t)
		if err := unready.AutoMigrate(&model.UserProfileEvidenceVersion{}, &model.ProfileEvidenceDatasetState{}); err != nil {
			t.Fatal(err)
		}
		if err := unready.Create(&model.ProfileEvidenceDatasetState{ID: 1}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := ReadProfileEvidenceIdentity(context.Background(), unready, 8); err == nil {
			t.Fatal("unready dataset state must fail closed")
		}
	})
}

func TestBumpProfileEvidenceDatasetIsTransactional(t *testing.T) {
	db := profileEvidenceRevisionTestDB(t)
	if err := InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatal(err)
	}
	before := readProfileEvidenceRevision(t, db, 13)
	if err := BumpProfileEvidenceDataset(context.Background(), db); err == nil {
		t.Fatal("root database must not be accepted as a dataset-bump transaction")
	}
	if afterRootAttempt := readProfileEvidenceRevision(t, db, 13); afterRootAttempt != before {
		t.Fatalf("root dataset bump changed identity: before=%+v after=%+v", before, afterRootAttempt)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return BumpProfileEvidenceDataset(context.Background(), tx)
	}); err != nil {
		t.Fatal(err)
	}
	after := readProfileEvidenceRevision(t, db, 13)
	if after.DatasetRevision != before.DatasetRevision+1 || after.UserRevision != before.UserRevision {
		t.Fatalf("explicit dataset bump before=%+v after=%+v", before, after)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := BumpProfileEvidenceDataset(context.Background(), tx); err != nil {
			return err
		}
		return gorm.ErrInvalidTransaction
	}); err == nil {
		t.Fatal("rollback dataset bump unexpectedly succeeded")
	}
	if rolledBack := readProfileEvidenceRevision(t, db, 13); rolledBack != after {
		t.Fatalf("rolled-back dataset bump persisted: before=%+v after=%+v", after, rolledBack)
	}
	prepared := db.Session(&gorm.Session{PrepareStmt: true})
	if err := prepared.Transaction(func(tx *gorm.DB) error {
		return BumpProfileEvidenceDataset(context.Background(), tx)
	}); err != nil {
		t.Fatalf("prepared transaction must be accepted: %v", err)
	}
	if afterPrepared := readProfileEvidenceRevision(t, db, 13); afterPrepared.DatasetRevision != after.DatasetRevision+1 {
		t.Fatalf("prepared transaction did not bump dataset: before=%+v after=%+v", after, afterPrepared)
	}
}

func ptrTime(v time.Time) *time.Time { return &v }
