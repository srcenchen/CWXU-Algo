package dal

import (
	"context"
	"strings"
	"sync"
	"testing"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func profileCacheIdentityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.AbilityModelState{}, &model.UserProfileEvidenceVersion{}, &model.ProfileEvidenceDatasetState{},
	); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestReadProfileCacheIdentityUsesOneReadOnlyStateQuery(t *testing.T) {
	db := profileCacheIdentityTestDB(t)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 17}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProfileEvidenceDatasetState{
		ID: 1, Revision: 23, SchemaVersion: CurrentProfileEvidenceSchemaVersion, Ready: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserProfileEvidenceVersion{UserID: 41, Revision: 29}).Error; err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	statements := make([]string, 0, 1)
	writes := 0
	recordRead := func(tx *gorm.DB) {
		mu.Lock()
		defer mu.Unlock()
		if sql := strings.TrimSpace(tx.Statement.SQL.String()); sql != "" {
			statements = append(statements, strings.ToLower(sql))
		}
	}
	recordWrite := func(*gorm.DB) {
		mu.Lock()
		writes++
		mu.Unlock()
	}
	if err := db.Callback().Query().After("gorm:query").Register("test:profile_cache_identity_query", recordRead); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Row().After("gorm:row").Register("test:profile_cache_identity_row", recordRead); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Raw().After("gorm:raw").Register("test:profile_cache_identity_raw", recordRead); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Create().Before("gorm:create").Register("test:profile_cache_identity_create", recordWrite); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Update().Before("gorm:update").Register("test:profile_cache_identity_update", recordWrite); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Delete().Before("gorm:delete").Register("test:profile_cache_identity_delete", recordWrite); err != nil {
		t.Fatal(err)
	}

	identity, err := ReadProfileCacheIdentity(context.Background(), db, 41)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ModelVersion != 17 || identity.Evidence != (ProfileEvidenceIdentity{DatasetRevision: 23, UserRevision: 29}) {
		t.Fatalf("identity=%+v", identity)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(statements) != 1 {
		t.Fatalf("identity statements=%d want 1: %v", len(statements), statements)
	}
	if writes != 0 {
		t.Fatalf("identity read executed %d writes", writes)
	}
	for _, forbidden := range []string{"submit_logs", "user_ac_problems", "platforms", "insert ", "update ", "delete "} {
		if strings.Contains(statements[0], forbidden) {
			t.Fatalf("identity query contains forbidden %q: %s", forbidden, statements[0])
		}
	}
	for _, required := range []string{"ability_model_state", "profile_evidence_dataset_state", "user_profile_evidence_versions", "left join"} {
		if !strings.Contains(statements[0], required) {
			t.Fatalf("identity query missing %q: %s", required, statements[0])
		}
	}
}

func TestReadProfileCacheIdentityTreatsMissingUserRevisionAsZeroWithoutWrite(t *testing.T) {
	db := profileCacheIdentityTestDB(t)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 3}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProfileEvidenceDatasetState{
		ID: 1, Revision: 5, SchemaVersion: CurrentProfileEvidenceSchemaVersion, Ready: true,
	}).Error; err != nil {
		t.Fatal(err)
	}

	identity, err := ReadProfileCacheIdentity(context.Background(), db, 99)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ModelVersion != 3 || identity.Evidence != (ProfileEvidenceIdentity{DatasetRevision: 5}) {
		t.Fatalf("identity=%+v", identity)
	}
	var rows int64
	if err := db.Model(&model.UserProfileEvidenceVersion{}).Where("user_id = ?", 99).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("identity hot read inserted %d user revision rows", rows)
	}
}

func TestReadProfileCacheIdentityFailsClosedOnIncompleteState(t *testing.T) {
	for _, tt := range []struct {
		name string
		seed func(*testing.T, *gorm.DB)
	}{
		{
			name: "missing model",
			seed: func(t *testing.T, db *gorm.DB) {
				t.Helper()
				if err := db.Create(&model.ProfileEvidenceDatasetState{ID: 1, SchemaVersion: CurrentProfileEvidenceSchemaVersion, Ready: true}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing dataset",
			seed: func(t *testing.T, db *gorm.DB) {
				t.Helper()
				if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "zero model",
			seed: func(t *testing.T, db *gorm.DB) {
				t.Helper()
				if err := db.Create(&model.AbilityModelState{ID: 1}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Create(&model.ProfileEvidenceDatasetState{ID: 1, SchemaVersion: CurrentProfileEvidenceSchemaVersion, Ready: true}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unready dataset",
			seed: func(t *testing.T, db *gorm.DB) {
				t.Helper()
				if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Create(&model.ProfileEvidenceDatasetState{ID: 1, SchemaVersion: CurrentProfileEvidenceSchemaVersion}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "schema mismatch",
			seed: func(t *testing.T, db *gorm.DB) {
				t.Helper()
				if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Create(&model.ProfileEvidenceDatasetState{ID: 1, SchemaVersion: CurrentProfileEvidenceSchemaVersion + 1, Ready: true}).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := profileCacheIdentityTestDB(t)
			tt.seed(t, db)
			if _, err := ReadProfileCacheIdentity(context.Background(), db, 7); err == nil {
				t.Fatal("incomplete cache identity state must fail closed")
			}
		})
	}
}
