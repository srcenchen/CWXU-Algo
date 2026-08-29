package dal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func abilityMaintenanceRecoveryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.AbilityMaintenancePending{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestLoadAbilityMaintenanceRecoveryBatchRotatesFailedOldestFifty(t *testing.T) {
	tests := []struct {
		name       string
		operations []string
		operation  string
	}{
		{name: "general", operations: []string{"problem", "rebuild", "reset"}, operation: "problem"},
		{name: "spider", operations: []string{"spider_set", "spider_purge_user", "spider_purge_global"}, operation: "spider_set"},
		{name: "luogu", operations: []string{"luogu_cleanup"}, operation: "luogu_cleanup"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := abilityMaintenanceRecoveryTestDB(t)
			ctx := context.Background()
			base := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
			for i := 0; i < 51; i++ {
				createdAt := base.Add(time.Duration(i) * time.Second)
				pending := model.AbilityMaintenancePending{
					Scope: fmt.Sprintf("%s:%02d", tc.name, i), OperationID: fmt.Sprintf("%s-intent-%02d", tc.name, i),
					Revision: 1, Phase: "intent", Operation: tc.operation, CreatedAt: createdAt, UpdatedAt: createdAt,
				}
				if err := db.Create(&pending).Error; err != nil {
					t.Fatal(err)
				}
			}

			first, err := LoadAbilityMaintenanceRecoveryBatch(ctx, db, tc.operations, 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(first) != 50 || first[0].Scope != fmt.Sprintf("%s:%02d", tc.name, 0) || first[49].Scope != fmt.Sprintf("%s:%02d", tc.name, 49) {
				t.Fatalf("first batch=%d first=%q last=%q", len(first), first[0].Scope, first[len(first)-1].Scope)
			}
			attemptedAt := base.Add(time.Hour)
			for i := range first {
				claimed, err := TouchAbilityMaintenanceRecoveryAttempt(ctx, db, &first[i], attemptedAt)
				if err != nil || !claimed {
					t.Fatalf("touch %s claimed=%v err=%v", first[i].Scope, claimed, err)
				}
			}

			second, err := LoadAbilityMaintenanceRecoveryBatch(ctx, db, tc.operations, 50)
			if err != nil {
				t.Fatal(err)
			}
			if len(second) != 50 || second[0].Scope != fmt.Sprintf("%s:%02d", tc.name, 50) {
				t.Fatalf("second batch=%d first=%q want=%s:50", len(second), second[0].Scope, tc.name)
			}
		})
	}
}

func TestLoadAbilityMaintenanceRecoveryBatchIsolatesUnknownOperations(t *testing.T) {
	db := abilityMaintenanceRecoveryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		pending := model.AbilityMaintenancePending{
			Scope: fmt.Sprintf("unknown:%02d", i), OperationID: fmt.Sprintf("unknown-intent-%02d", i),
			Revision: 1, Phase: "intent", Operation: "future_operation", CreatedAt: base, UpdatedAt: base,
		}
		if err := db.Create(&pending).Error; err != nil {
			t.Fatal(err)
		}
	}
	known := model.AbilityMaintenancePending{
		Scope: "known:problem", OperationID: "known-intent", Revision: 1, Phase: "intent", Operation: "problem",
		CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour),
	}
	if err := db.Create(&known).Error; err != nil {
		t.Fatal(err)
	}

	batch, err := LoadAbilityMaintenanceRecoveryBatch(ctx, db, []string{"problem", "rebuild", "reset"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || batch[0].OperationID != known.OperationID {
		t.Fatalf("known batch=%+v", batch)
	}
	unknown, err := ListUnknownAbilityMaintenanceOperations(ctx, db, []string{
		"problem", "rebuild", "reset", "spider_set", "spider_purge_user", "spider_purge_global", "luogu_cleanup",
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 1 || unknown[0].Operation != "future_operation" || unknown[0].Count != 50 {
		t.Fatalf("unknown summary=%+v", unknown)
	}
}

func TestTouchAbilityMaintenanceRecoveryAttemptDoesNotTouchReplacementIntent(t *testing.T) {
	db := abilityMaintenanceRecoveryTestDB(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	stale := model.AbilityMaintenancePending{
		Scope: "problem:replacement", OperationID: "old-intent", Revision: 1, Phase: "intent", Operation: "problem",
		CreatedAt: base, UpdatedAt: base,
	}
	if err := db.Create(&stale).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&stale).Error; err != nil {
		t.Fatal(err)
	}
	replacementUpdatedAt := base.Add(time.Minute)
	replacement := model.AbilityMaintenancePending{
		Scope: stale.Scope, OperationID: "new-intent", Revision: 1, Phase: "intent", Operation: "problem",
		CreatedAt: replacementUpdatedAt, UpdatedAt: replacementUpdatedAt,
	}
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}

	claimed, err := TouchAbilityMaintenanceRecoveryAttempt(ctx, db, &stale, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if claimed {
		t.Fatal("stale scanner touched replacement intent")
	}
	var stored model.AbilityMaintenancePending
	if err := db.First(&stored, "scope = ?", stale.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if stored.OperationID != replacement.OperationID || !stored.UpdatedAt.Equal(replacementUpdatedAt) || stored.Revision != replacement.Revision || stored.Phase != replacement.Phase {
		t.Fatalf("replacement changed: %+v", stored)
	}
}
