package service

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func profileEvidencePurgeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.UserACProblem{}, &model.Platform{}, &model.AbilityModelState{},
		&model.AbilityMaintenancePending{}, &model.ProblemTag{},
	); err != nil {
		t.Fatal(err)
	}
	if err := dal.InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestContainsProfileEvidenceSourceTable(t *testing.T) {
	for _, table := range []string{"submit_logs", "user_ac_problems", "platforms"} {
		if !containsProfileEvidenceSourceTable([]string{"problem_ability_stats", table}) {
			t.Fatalf("table %s was not recognized as profile evidence", table)
		}
	}
	if containsProfileEvidenceSourceTable([]string{"problem_ability_stats", "ability_model_state"}) {
		t.Fatal("ability-only tables were recognized as profile evidence sources")
	}
}

func TestDeleteFallbackWhitelistCoversAbilityDerivedTables(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.UserProblemStatus{}, &model.UserTagAC{}, &model.UserTagACSnapshot{}, &model.ProblemAbilityStat{},
		&model.AbilityModelState{}, &model.AbilityProfileScheduleRun{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Create(&model.UserProblemStatus{UserID: 1, ProblemID: 1, Status: model.UserProblemStatusAC, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserTagAC{UserID: 1, Tag: "dp", Count: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserTagACSnapshot{UserID: 1, ScoreVersion: 1, ModelVersion: 1, RowCount: 1, PublishedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemAbilityStat{ModelVersion: 1, ProblemID: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityProfileScheduleRun{Period: "2026-08-29", ModelVersion: 1, CompletedAt: now}).Error; err != nil {
		t.Fatal(err)
	}

	for _, table := range []string{
		"user_problem_status", "user_tag_ac", "user_tag_ac_snapshots", "problem_ability_stats",
		"ability_model_state", "ability_profile_schedule_runs",
	} {
		if _, err := deleteAllInBatches(context.Background(), db, table, 2); err != nil {
			t.Fatalf("fallback table %s: %v", table, err)
		}
		var n int64
		if err := db.Table(table).Count(&n).Error; err != nil || n != 0 {
			t.Fatalf("fallback table %s count=%d err=%v", table, n, err)
		}
	}
	if _, err := deleteAllInBatches(context.Background(), db, "not_allowed", 2); err == nil {
		t.Fatal("unknown table was not rejected")
	}
}

func TestPurgeSubmitDataTruncateSuccessRestoresMonotonicState(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "ability_model_state" WHERE id = $1 ORDER BY "ability_model_state"."id" LIMIT $2 FOR UPDATE`)).
		WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "active_version", "last_scheduled_refresh_period", "built_at", "updated_at"}).AddRow(1, 12, "2026-08-29", time.Now(), time.Now()))
	mock.ExpectExec(regexp.QuoteMeta(`TRUNCATE TABLE problem_ability_stats, ability_profile_schedule_runs, ability_model_state RESTART IDENTITY`)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO "ability_model_state" .*RETURNING "id"`).
		WithArgs(uint64(13), "", sqlmock.AnyArg(), sqlmock.AnyArg(), 1).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	mock.ExpectCommit()

	if _, err := purgeSubmitData(context.Background(), db, []string{"problem_ability_stats", "ability_profile_schedule_runs", "ability_model_state"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeSubmitDataTruncateSuccessBumpsProfileDataset(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT * FROM "ability_model_state" WHERE id = $1 ORDER BY "ability_model_state"."id" LIMIT $2 FOR UPDATE`)).
		WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows([]string{"id", "active_version", "last_scheduled_refresh_period", "built_at", "updated_at"}))
	mock.ExpectExec(regexp.QuoteMeta(`TRUNCATE TABLE submit_logs RESTART IDENTITY`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE profile_evidence_dataset_state`).
		WithArgs(1, dal.CurrentProfileEvidenceSchemaVersion).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	outcome, err := purgeSubmitData(context.Background(), db, []string{"submit_logs"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != spiderMaintenanceCommitted {
		t.Fatalf("outcome=%v want committed", outcome)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeSubmitDataPostgresTruncateFailureFallsBackTransactionally(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	abilityStateColumns := []string{"id", "active_version", "last_scheduled_refresh_period", "built_at", "updated_at"}
	abilityStateQuery := regexp.QuoteMeta(`SELECT * FROM "ability_model_state" WHERE id = $1 ORDER BY "ability_model_state"."id" LIMIT $2 FOR UPDATE`)
	deleteBatch := `(?s)DELETE FROM submit_logs\s+WHERE ctid IN \(\s+SELECT ctid FROM submit_logs LIMIT 5000\s+\)`

	mock.ExpectBegin()
	mock.ExpectQuery(abilityStateQuery).WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows(abilityStateColumns))
	mock.ExpectExec(regexp.QuoteMeta(`TRUNCATE TABLE submit_logs RESTART IDENTITY`)).
		WillReturnError(errors.New("truncate unavailable"))
	mock.ExpectRollback()

	mock.ExpectBegin()
	mock.ExpectQuery(abilityStateQuery).WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows(abilityStateColumns))
	mock.ExpectExec(deleteBatch).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(deleteBatch).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE profile_evidence_dataset_state`).
		WithArgs(1, dal.CurrentProfileEvidenceSchemaVersion).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	outcome, err := purgeSubmitData(context.Background(), db, []string{"submit_logs"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != spiderMaintenanceCommitted {
		t.Fatalf("outcome=%v want committed", outcome)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeSubmitDataFallbackBumpsProfileDataset(t *testing.T) {
	db := profileEvidencePurgeTestDB(t)
	ctx := context.Background()
	if err := db.Create(&model.SubmitLog{
		UserID: 31, Platform: "LuoGu", SubmitID: "purge-me", Status: "AC", Time: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	before, err := dal.ReadProfileEvidenceIdentity(ctx, db, 31)
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := purgeSubmitData(ctx, db, []string{"submit_logs"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != spiderMaintenanceCommitted {
		t.Fatalf("outcome=%v want committed", outcome)
	}
	var count int64
	if err := db.Model(&model.SubmitLog{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("submit_logs count=%d want 0", count)
	}
	after, err := dal.ReadProfileEvidenceIdentity(ctx, db, 31)
	if err != nil {
		t.Fatal(err)
	}
	if after.DatasetRevision != before.DatasetRevision+1 {
		t.Fatalf("dataset revision before=%d after=%d want +1", before.DatasetRevision, after.DatasetRevision)
	}
}

func TestPurgeSubmitDataBumpFailureRollsBackFacts(t *testing.T) {
	db := profileEvidencePurgeTestDB(t)
	ctx := context.Background()
	if err := db.Create(&model.SubmitLog{
		UserID: 32, Platform: "LuoGu", SubmitID: "keep-me", Status: "WA", Time: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ProfileEvidenceDatasetState{}).Where("id = ?", 1).Update("ready", false).Error; err != nil {
		t.Fatal(err)
	}

	outcome, err := purgeSubmitData(ctx, db, []string{"submit_logs"}, nil, nil)
	if err == nil {
		t.Fatal("unready dataset state allowed purge to commit")
	}
	if outcome != spiderMaintenanceRolledBack {
		t.Fatalf("outcome=%v want rolled back", outcome)
	}
	var count int64
	if err := db.Model(&model.SubmitLog{}).Where("submit_id = ?", "keep-me").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("submit_logs count=%d want 1 after bump rollback", count)
	}
}

func TestPurgeSubmitDataPendingCommitsFactsAndDatasetBumpTogether(t *testing.T) {
	db := profileEvidencePurgeTestDB(t)
	ctx := context.Background()
	if err := db.Create(&model.SubmitLog{
		UserID: 33, Platform: "LuoGu", SubmitID: "pending-purge", Status: "AC", Time: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	before, err := dal.ReadProfileEvidenceIdentity(ctx, db, 33)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := prepareSpiderMaintenancePending(ctx, db, spiderPurgeGlobalMaintenanceScope, spiderMaintenancePurgeGlobal, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimSpiderMaintenancePending(ctx, db, pending, "owner-success"); err != nil {
		t.Fatal(err)
	}

	outcome, err := purgeSubmitData(ctx, db, []string{"submit_logs"}, nil, pending)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != spiderMaintenanceCommitted {
		t.Fatalf("outcome=%v want committed", outcome)
	}
	var count int64
	if err := db.Model(&model.SubmitLog{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("submit_logs count=%d err=%v", count, err)
	}
	after, err := dal.ReadProfileEvidenceIdentity(ctx, db, 33)
	if err != nil {
		t.Fatal(err)
	}
	if after.DatasetRevision != before.DatasetRevision+1 {
		t.Fatalf("dataset revision before=%d after=%d want +1", before.DatasetRevision, after.DatasetRevision)
	}
	stored, err := loadSpiderMaintenancePending(ctx, db, pending.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Phase != "facts" || stored.Revision != 3 || stored.LeaseOwner != "owner-success" {
		t.Fatalf("stored pending=%+v want facts revision=3 owner-success", stored)
	}
	if pending.Phase != "facts" || pending.Revision != 3 {
		t.Fatalf("in-memory pending=%+v want facts revision=3", pending)
	}
}

func TestPurgeSubmitDataPendingMarkFailureRollsBackFactsAndDatasetBump(t *testing.T) {
	db := profileEvidencePurgeTestDB(t)
	ctx := context.Background()
	if err := db.Create(&model.SubmitLog{
		UserID: 34, Platform: "LuoGu", SubmitID: "pending-keep", Status: "WA", Time: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	before, err := dal.ReadProfileEvidenceIdentity(ctx, db, 34)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := prepareSpiderMaintenancePending(ctx, db, spiderPurgeGlobalMaintenanceScope, spiderMaintenancePurgeGlobal, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimSpiderMaintenancePending(ctx, db, pending, "owner-fail"); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail_purge_facts_mark", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "ability_maintenance_pending" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if ok && updates["phase"] == "facts" {
			tx.AddError(gorm.ErrInvalidTransaction)
		}
	}); err != nil {
		t.Fatal(err)
	}

	outcome, err := purgeSubmitData(ctx, db, []string{"submit_logs"}, nil, pending)
	if err == nil {
		t.Fatal("injected facts mark failure allowed purge to commit")
	}
	if outcome != spiderMaintenanceRolledBack {
		t.Fatalf("outcome=%v want rolled back", outcome)
	}
	var count int64
	if err := db.Model(&model.SubmitLog{}).Where("submit_id = ?", "pending-keep").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("submit_logs count=%d err=%v want preserved row", count, err)
	}
	after, err := dal.ReadProfileEvidenceIdentity(ctx, db, 34)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("profile evidence changed across rollback: before=%+v after=%+v", before, after)
	}
	stored, err := loadSpiderMaintenancePending(ctx, db, pending.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Phase != "intent" || stored.Revision != 2 || stored.LeaseOwner != "owner-fail" {
		t.Fatalf("stored pending=%+v want claimed intent revision=2", stored)
	}
	if pending.Phase != "intent" || pending.Revision != 2 {
		t.Fatalf("in-memory pending=%+v want unchanged claimed intent", pending)
	}
}

func TestPurgeSubmitDataFallbackRestoresMonotonicActiveStateAndClearsDailyMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.UserProblemStatus{}, &model.UserTagAC{}, &model.ProblemAbilityStat{},
		&model.AbilityModelState{}, &model.AbilityProfileScheduleRun{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 7, LastScheduledRefreshPeriod: "2026-08-29", BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityProfileScheduleRun{Period: "2026-08-29", ModelVersion: 7, CompletedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemAbilityStat{ModelVersion: 7, ProblemID: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	tables := []string{"problem_ability_stats", "ability_profile_schedule_runs", "ability_model_state"}
	if _, err := purgeSubmitData(context.Background(), db, tables, nil, nil); err != nil {
		t.Fatal(err)
	}
	var state model.AbilityModelState
	if err := db.First(&state, 1).Error; err != nil {
		t.Fatal(err)
	}
	if state.ActiveVersion != 8 || state.LastScheduledRefreshPeriod != "" {
		t.Fatalf("restored state=%+v want active=8 empty period", state)
	}
	for _, table := range []string{"problem_ability_stats", "ability_profile_schedule_runs"} {
		var n int64
		if err := db.Table(table).Count(&n).Error; err != nil || n != 0 {
			t.Fatalf("table=%s count=%d err=%v", table, n, err)
		}
	}
}

func TestPurgeSubmitDataRejectsAbilityVersionOverflow(t *testing.T) {
	maxVersion := ^uint64(0)
	if _, err := nextAbilityModelVersion(maxVersion); err == nil {
		t.Fatal("version overflow was reported as a successful purge")
	}
}
