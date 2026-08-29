package data

import (
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"

	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/DATA-DOG/go-sqlmock"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func abilityLookupIndexSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.UserACProblem{}, &model.Problem{}, &model.SubmitLog{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func abilityLookupIndexPostgresMockDB(t *testing.T) (*gorm.DB, sqlmock.Sqlmock) {
	t.Helper()
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
	return db, mock
}

func missingAbilityLookupIndexRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"index_exists", "valid"})
}

func expectMissingPostgresAbilityLookupIndex(mock sqlmock.Sqlmock, spec abilityLookupIndexSpec, result driver.Result) {
	mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
		WithArgs(spec.name).WillReturnRows(missingAbilityLookupIndexRows())
	mock.ExpectExec(regexp.QuoteMeta(spec.postgresDDL())).WillReturnResult(result)
}

func TestAbilityLookupIndexDefinitionsCoverRequiredKeyPaths(t *testing.T) {
	postgresEmbeddedPlatform := "BTRIM(SUBSTRING(problem_key FROM 3 FOR GREATEST(POSITION(':' IN SUBSTRING(problem_key FROM 3)) - 1, 0)))"
	postgresExternalID := "BTRIM(SUBSTRING(problem_key FROM 3 + POSITION(':' IN SUBSTRING(problem_key FROM 3))))"
	postgresExternalPredicate := "LEFT(problem_key, 2) = 'e:' AND POSITION(':' IN SUBSTRING(problem_key FROM 3)) > 0" +
		" AND " + postgresEmbeddedPlatform + " <> '' AND " + postgresExternalID + " <> ''" +
		" AND NOT (LOWER(" + postgresEmbeddedPlatform + ") = 'leetcode' AND LEFT(LOWER(" + postgresExternalID + "), 3) = 'ac-')"
	want := []string{
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_uac_problem_key ON user_ac_problems (problem_key)",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_uac_ability_platform_key ON user_ac_problems (LOWER(TRIM(platform)), problem_key)",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_uac_ability_external_identity ON user_ac_problems (LOWER(BTRIM(platform)), LOWER(" + postgresEmbeddedPlatform + "), " + postgresExternalID + ") WHERE " + postgresExternalPredicate,
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_problem_ability_external_lookup ON problems (LOWER(TRIM(platform)), TRIM(external_id))",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_submit_user_problem ON submit_logs (user_id, problem_id)",
		"CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_submit_user_ability_external ON submit_logs (user_id, LOWER(TRIM(platform)), TRIM(external_id))",
	}
	if len(abilityLookupIndexes) != len(want) {
		t.Fatalf("ability lookup indexes=%d want=%d", len(abilityLookupIndexes), len(want))
	}
	for i, spec := range abilityLookupIndexes {
		if got := spec.postgresDDL(); got != want[i] {
			t.Fatalf("index %d DDL=%q want=%q", i, got, want[i])
		}
	}
}

func TestAbilityExternalIdentityIndexUsesMatchingSQLiteExpressions(t *testing.T) {
	sqliteEmbeddedPlatform := "TRIM(SUBSTR(problem_key, 3, INSTR(SUBSTR(problem_key, 3), ':') - 1))"
	sqliteExternalID := "TRIM(SUBSTR(problem_key, 3 + INSTR(SUBSTR(problem_key, 3), ':')))"
	sqliteExternalPredicate := "SUBSTR(problem_key, 1, 2) = 'e:' AND INSTR(SUBSTR(problem_key, 3), ':') > 0" +
		" AND " + sqliteEmbeddedPlatform + " <> '' AND " + sqliteExternalID + " <> ''" +
		" AND NOT (LOWER(" + sqliteEmbeddedPlatform + ") = 'leetcode' AND SUBSTR(LOWER(" + sqliteExternalID + "), 1, 3) = 'ac-')"
	want := "CREATE INDEX IF NOT EXISTS idx_uac_ability_external_identity ON user_ac_problems (LOWER(TRIM(platform)), LOWER(" + sqliteEmbeddedPlatform + "), " + sqliteExternalID + ") WHERE " + sqliteExternalPredicate
	for _, spec := range abilityLookupIndexes {
		if spec.name == "idx_uac_ability_external_identity" {
			if got := spec.portableDDL(); got != want {
				t.Fatalf("SQLite external identity DDL=%q want=%q", got, want)
			}
			return
		}
	}
	t.Fatal("missing external identity lookup index")
}

func TestMigrateAbilityLookupIndexesSQLiteCreatesAllDefinedIndexesOutsideModels(t *testing.T) {
	db := abilityLookupIndexSQLiteDB(t)
	if err := migrateAbilityLookupIndexes(db); err != nil {
		t.Fatal(err)
	}
	for _, spec := range abilityLookupIndexes {
		var count int64
		if err := db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, spec.name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("controlled migration did not create %s", spec.name)
		}
	}
}

func TestMigrateAbilityLookupIndexesPostgresUsesConcurrentDDLForAllDefinedIndexes(t *testing.T) {
	db, mock := abilityLookupIndexPostgresMockDB(t)
	for _, spec := range abilityLookupIndexes {
		expectMissingPostgresAbilityLookupIndex(mock, spec, sqlmock.NewResult(0, 0))
	}
	if err := migrateAbilityLookupIndexes(db); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsurePostgresAbilityLookupIndexDropsInvalidBeforeCreate(t *testing.T) {
	db, mock := abilityLookupIndexPostgresMockDB(t)
	spec := abilityLookupIndexes[0]
	mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
		WithArgs(spec.name).
		WillReturnRows(sqlmock.NewRows([]string{"index_exists", "valid"}).AddRow(true, false))
	mock.ExpectExec(regexp.QuoteMeta("DROP INDEX CONCURRENTLY IF EXISTS " + spec.name)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(spec.postgresDDL())).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := ensurePostgresAbilityLookupIndex(db, spec); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateAbilityLookupIndexesReportsFailureAndContinues(t *testing.T) {
	db, mock := abilityLookupIndexPostgresMockDB(t)
	injected := errors.New("injected concurrent build failure")
	for i, spec := range abilityLookupIndexes {
		mock.ExpectQuery(`(?s)SELECT TRUE AS index_exists.*WHERE c.relname = \$1`).
			WithArgs(spec.name).WillReturnRows(missingAbilityLookupIndexRows())
		expect := mock.ExpectExec(regexp.QuoteMeta(spec.postgresDDL()))
		if i == 0 {
			expect.WillReturnError(injected)
		} else {
			expect.WillReturnResult(sqlmock.NewResult(0, 0))
		}
	}
	err := migrateAbilityLookupIndexes(db)
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), abilityLookupIndexes[0].name) {
		t.Fatalf("optional migration error must identify the failed index: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
