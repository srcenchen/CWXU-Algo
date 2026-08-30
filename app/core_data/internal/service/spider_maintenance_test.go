package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bizservice "cwxu-algo/app/core_data/internal/biz/service"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type afterProfileValidationHook struct {
	once atomic.Bool
	run  func()
}

func (h *afterProfileValidationHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *afterProfileValidationHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if err == nil && cmd.Name() == "eval" && len(args) > 1 {
			script := fmt.Sprint(args[1])
			if strings.Contains(script, `ARGV[4]`) && strings.Contains(script, `redis.call("PEXPIRE", KEYS[2], ARGV[3])`) && h.once.CompareAndSwap(false, true) {
				h.run()
			}
		}
		return err
	}
}

func (h *afterProfileValidationHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func newSpiderMaintenanceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.AbilityMaintenancePending{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRecoverSpiderMaintenanceFactsFinalizesAllDurableOperations(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	if err := db.AutoMigrate(&model.Platform{}, &model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Platform{UserID: 7, Platform: "LuoGu", Username: "new-user"}).Error; err != nil {
		t.Fatal(err)
	}
	setPending, err := prepareSpiderMaintenancePending(context.Background(), db, spiderSetMaintenanceScope(7, "LuoGu"), spiderMaintenanceSetBinding, `{"userId":7,"platform":"LuoGu","username":"new-user"}`)
	if err != nil {
		t.Fatal(err)
	}
	purgeUserPending, err := prepareSpiderMaintenancePending(context.Background(), db, spiderPurgeUserMaintenanceScope(8), spiderMaintenancePurgeUser, `{"userId":8}`)
	if err != nil {
		t.Fatal(err)
	}
	globalPending, err := prepareSpiderMaintenancePending(context.Background(), db, spiderPurgeGlobalMaintenanceScope, spiderMaintenancePurgeGlobal, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, pending := range []*model.AbilityMaintenancePending{setPending, purgeUserPending, globalPending} {
		if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Update("phase", "facts").Error; err != nil {
			t.Fatal(err)
		}
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	service := &SpiderService{db: db, rdb: rdb, spider: task.NewSpiderTask(nil, rdb, db)}
	service.recoverPendingSpiderMaintenance(context.Background())

	var remaining int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("operation IN ?", []string{
		spiderMaintenanceSetBinding, spiderMaintenancePurgeUser, spiderMaintenancePurgeGlobal,
	}).Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining durable spider maintenance=%d", remaining)
	}
	for _, key := range []string{
		"problem:user_profile:generation:global",
		fmt.Sprintf("problem:user_profile:generation:user:%d", 7),
		fmt.Sprintf("problem:user_profile:generation:user:%d", 8),
	} {
		value, err := rdb.Get(context.Background(), key).Int64()
		if err != nil {
			t.Fatalf("generation %s: %v", key, err)
		}
		if value%2 != 0 {
			t.Fatalf("generation %s=%d remained odd", key, value)
		}
	}
}

func TestSpiderMaintenanceRecoveryRotatesFailedOldestBatch(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for i := 0; i < 51; i++ {
		createdAt := base.Add(time.Duration(i) * time.Second)
		pending := model.AbilityMaintenancePending{
			Scope: fmt.Sprintf("spider:starvation:%02d", i), OperationID: fmt.Sprintf("spider-starvation-%02d", i),
			Revision: 1, Phase: "intent", Operation: spiderMaintenanceSetBinding, Payload: "{", CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if err := db.Create(&pending).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := &SpiderService{db: db}
	firstAttempt := time.Now()
	service.recoverPendingSpiderMaintenance(context.Background())
	var first, late model.AbilityMaintenancePending
	if err := db.First(&first, "scope = ?", "spider:starvation:00").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&late, "scope = ?", "spider:starvation:50").Error; err != nil {
		t.Fatal(err)
	}
	if first.UpdatedAt.Before(firstAttempt) {
		t.Fatalf("first failed attempt was not rotated: %v", first.UpdatedAt)
	}
	if !late.UpdatedAt.Equal(base.Add(50 * time.Second)) {
		t.Fatalf("late intent entered first batch: %v", late.UpdatedAt)
	}
	secondAttempt := time.Now()
	service.recoverPendingSpiderMaintenance(context.Background())
	if err := db.First(&late, "scope = ?", "spider:starvation:50").Error; err != nil {
		t.Fatal(err)
	}
	if late.UpdatedAt.Before(secondAttempt) {
		t.Fatalf("late intent remained starved: %v", late.UpdatedAt)
	}
}

func TestExecuteSetSpiderMaintenanceBeginFailureRetainsDurableIntent(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	if err := db.AutoMigrate(&model.Platform{}, &model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	pending, err := prepareSpiderMaintenancePending(context.Background(), db, spiderSetMaintenanceScope(11, "LuoGu"), spiderMaintenanceSetBinding, `{"userId":11,"platform":"LuoGu","username":"new-user"}`)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	if err := rdb.Set(context.Background(), "problem:user_profile:generation:user:11", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), "problem:user_profile:generation:user:11:current_intent", "another-intent", 0).Err(); err != nil {
		t.Fatal(err)
	}
	service := &SpiderService{db: db, rdb: rdb, spider: task.NewSpiderTask(nil, rdb, db)}
	if err := service.executeSetSpiderMaintenance(context.Background(), pending); err == nil {
		t.Fatal("foreign odd intent did not block Begin")
	}
	stored, err := loadSpiderMaintenancePending(context.Background(), db, pending.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.OperationID != pending.OperationID || stored.Phase != "intent" {
		t.Fatalf("durable intent was lost or mutated: %+v", stored)
	}
}

func TestExecuteSetSpiderMaintenanceHoldsPlatformLockDuringBindingFactsWrite(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	if err := db.AutoMigrate(&model.Platform{}, &model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pending, err := prepareSpiderMaintenancePending(ctx, db, spiderSetMaintenanceScope(41, "LuoGu"), spiderMaintenanceSetBinding, `{"userId":41,"platform":"LuoGu","username":"new-user"}`)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	var rivalAcquired atomic.Bool
	var probes atomic.Int32
	probeLock := func(tx *gorm.DB) {
		probes.Add(1)
		key := "spider:writelock:41:LuoGu"
		ok, lockErr := rdb.SetNX(ctx, key, "rival", time.Minute).Result()
		if lockErr != nil {
			tx.AddError(lockErr)
			return
		}
		if ok {
			rivalAcquired.Store(true)
			_ = rdb.Del(ctx, key).Err()
		}
	}
	if err := db.Callback().Create().Before("gorm:create").Register("test:observe_set_binding_platform_lock", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "platforms" {
			return
		}
		probeLock(tx)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Update().After("gorm:update").Register("test:observe_set_facts_phase_platform_lock", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "ability_maintenance_pending" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok || updates["phase"] != "facts" {
			return
		}
		probeLock(tx)
	}); err != nil {
		t.Fatal(err)
	}
	service := &SpiderService{db: db, rdb: rdb, spider: task.NewSpiderTask(nil, rdb, db)}
	if err := service.executeSetSpiderMaintenance(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if rivalAcquired.Load() {
		t.Fatal("set binding facts write ran without the platform lock")
	}
	if probes.Load() < 2 {
		t.Fatalf("set binding lock probes=%d want create and facts-phase probes", probes.Load())
	}
}

func TestExecuteSetSpiderMaintenanceReleasesPlatformLockBeforeFenceAndRecrawlTail(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	if err := db.AutoMigrate(&model.Platform{}, &model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pending, err := prepareSpiderMaintenancePending(ctx, db, spiderSetMaintenanceScope(42, "LuoGu"), spiderMaintenanceSetBinding, `{"userId":42,"platform":"LuoGu","username":"new-user"}`)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	var lockLeaked atomic.Bool
	if err := db.Callback().Update().Before("gorm:update").Register("test:observe_set_fence_platform_lock", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "ability_maintenance_pending" {
			return
		}
		updates, ok := tx.Statement.Dest.(map[string]interface{})
		if !ok || updates["phase"] != "fence_finalized" {
			return
		}
		if rdb.Exists(ctx, "spider:writelock:42:LuoGu").Val() != 0 {
			lockLeaked.Store(true)
		}
	}); err != nil {
		t.Fatal(err)
	}
	service := &SpiderService{db: db, rdb: rdb, spider: task.NewSpiderTask(nil, rdb, db)}
	if err := service.executeSetSpiderMaintenance(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if lockLeaked.Load() {
		t.Fatal("set binding platform lock was retained through fence/recrawl tail")
	}
	if got := rdb.Exists(ctx, task.ProfileRebuildAfterBindingKey(42, "LuoGu")).Val(); got != 1 {
		t.Fatalf("replacement crawl did not receive a platform-scoped profile marker: exists=%d", got)
	}
}

func TestSpiderMaintenanceStaleScannerClaimFailureAbandonsFence(t *testing.T) {
	tests := []struct {
		name      string
		userID    int64
		scope     string
		operation string
		payload   string
		global    bool
		execute   func(*SpiderService, context.Context, *model.AbilityMaintenancePending) error
	}{
		{
			name: "set", userID: 21, scope: spiderSetMaintenanceScope(21, "LuoGu"), operation: spiderMaintenanceSetBinding,
			payload: `{"userId":21,"platform":"LuoGu","username":"new-user"}`,
			execute: func(s *SpiderService, ctx context.Context, pending *model.AbilityMaintenancePending) error {
				return s.executeSetSpiderMaintenance(ctx, pending)
			},
		},
		{
			name: "purge-user", userID: 22, scope: spiderPurgeUserMaintenanceScope(22), operation: spiderMaintenancePurgeUser,
			payload: `{"userId":22}`,
			execute: func(s *SpiderService, ctx context.Context, pending *model.AbilityMaintenancePending) error {
				return s.executePurgeUserMaintenance(ctx, pending)
			},
		},
		{
			name: "purge-global", scope: spiderPurgeGlobalMaintenanceScope, operation: spiderMaintenancePurgeGlobal, payload: `{}`, global: true,
			execute: func(s *SpiderService, ctx context.Context, pending *model.AbilityMaintenancePending) error {
				return s.executePurgeGlobalMaintenance(ctx, pending)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := newSpiderMaintenanceTestDB(t)
			pending, err := prepareSpiderMaintenancePending(ctx, db, tc.scope, tc.operation, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			stale := *pending
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			var token bizservice.ProfileInvalidationToken
			if tc.global {
				token, err = bizservice.BeginGlobalProfileInvalidationForIntent(ctx, rdb, pending.OperationID)
			} else {
				token, err = bizservice.BeginUserProfileInvalidationForIntent(ctx, rdb, tc.userID, pending.OperationID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := claimSpiderMaintenancePending(ctx, db, pending, token.Owner); err != nil {
				t.Fatal(err)
			}
			if err := db.Transaction(func(tx *gorm.DB) error {
				return markSpiderMaintenanceFacts(ctx, tx, pending)
			}); err != nil {
				t.Fatal(err)
			}
			pending.Phase = "facts"
			pending.Revision++
			if tc.global {
				err = bizservice.AbandonGlobalProfileInvalidation(ctx, rdb, token)
			} else {
				err = bizservice.AbandonUserProfileInvalidation(ctx, rdb, tc.userID, token)
			}
			if err != nil {
				t.Fatal(err)
			}
			service := &SpiderService{db: db, rdb: rdb}
			if err := tc.execute(service, ctx, &stale); err == nil {
				t.Fatal("stale scanner unexpectedly claimed facts-committed pending")
			}
			generationKey := "problem:user_profile:generation:global"
			if !tc.global {
				generationKey = fmt.Sprintf("problem:user_profile:generation:user:%d", tc.userID)
			}
			generation, parseErr := rdb.Get(ctx, generationKey).Int64()
			if parseErr != nil || generation%2 == 0 || rdb.Get(ctx, generationKey+":current_intent").Val() != pending.OperationID {
				t.Fatalf("claim failure prematurely finished fence generation=%d parse=%v current=%q", generation, parseErr, rdb.Get(ctx, generationKey+":current_intent").Val())
			}
			stored, err := loadSpiderMaintenancePending(ctx, db, pending.Scope)
			if err != nil || stored == nil || stored.Phase != "facts" || stored.LeaseOwner != token.Owner {
				t.Fatalf("facts-committed pending changed: pending=%+v err=%v", stored, err)
			}
			var retry bizservice.ProfileInvalidationToken
			if tc.global {
				retry, err = bizservice.BeginGlobalProfileInvalidationForIntent(ctx, rdb, pending.OperationID)
			} else {
				retry, err = bizservice.BeginUserProfileInvalidationForIntent(ctx, rdb, tc.userID, pending.OperationID)
			}
			if err != nil {
				t.Fatalf("next scanner could not reclaim abandoned fence: %v", err)
			}
			if tc.global {
				_ = bizservice.AbandonGlobalProfileInvalidation(ctx, rdb, retry)
			} else {
				_ = bizservice.AbandonUserProfileInvalidation(ctx, rdb, tc.userID, retry)
			}
		})
	}
}

func TestSpiderMaintenanceGenerationBumpUsesCurrentProfileOwner(t *testing.T) {
	tests := []struct {
		name      string
		userID    int64
		scope     string
		operation string
		payload   string
		platform  string
		execute   func(*SpiderService, context.Context, *model.AbilityMaintenancePending) error
	}{
		{
			name: "set", userID: 31, scope: spiderSetMaintenanceScope(31, "LuoGu"), operation: spiderMaintenanceSetBinding,
			payload: `{"userId":31,"platform":"LuoGu","username":"new-user"}`, platform: "LuoGu",
			execute: func(s *SpiderService, ctx context.Context, pending *model.AbilityMaintenancePending) error {
				return s.executeSetSpiderMaintenance(ctx, pending)
			},
		},
		{
			name: "purge-user", userID: 32, scope: spiderPurgeUserMaintenanceScope(32), operation: spiderMaintenancePurgeUser,
			payload: `{"userId":32}`, platform: purgeUserPlatforms[0],
			execute: func(s *SpiderService, ctx context.Context, pending *model.AbilityMaintenancePending) error {
				return s.executePurgeUserMaintenance(ctx, pending)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := newSpiderMaintenanceTestDB(t)
			pending, err := prepareSpiderMaintenancePending(ctx, db, tc.scope, tc.operation, tc.payload)
			if err != nil {
				t.Fatal(err)
			}
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			takeoverRDB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = takeoverRDB.Close() })
			generationKey := task.GenerationKey(tc.userID, tc.platform)
			var takeoverErr error
			var startedGeneration int64
			rdb.AddHook(&afterProfileValidationHook{run: func() {
				mr.FastForward(31 * time.Minute)
				stored, loadErr := loadSpiderMaintenancePending(ctx, db, pending.Scope)
				if loadErr != nil {
					takeoverErr = loadErr
					return
				}
				ownerB, beginErr := bizservice.BeginUserProfileInvalidationForIntent(ctx, takeoverRDB, tc.userID, pending.OperationID)
				if beginErr != nil {
					takeoverErr = beginErr
					return
				}
				if claimErr := claimSpiderMaintenancePending(ctx, db, stored, ownerB.Owner); claimErr != nil {
					takeoverErr = claimErr
					return
				}
				startedGeneration, takeoverErr = bizservice.BumpUserProfileOwnedGeneration(ctx, takeoverRDB, tc.userID, ownerB, generationKey, 7*24*time.Hour)
				if takeoverErr == nil {
					takeoverErr = bizservice.FinishUserProfileInvalidation(ctx, takeoverRDB, tc.userID, ownerB)
				}
			}})
			service := &SpiderService{db: db, rdb: rdb, spider: task.NewSpiderTask(nil, rdb, db)}
			if err := tc.execute(service, ctx, pending); err == nil {
				t.Fatal("lost owner continued maintenance after takeover")
			}
			if takeoverErr != nil || startedGeneration == 0 {
				t.Fatalf("takeover failed generation=%d err=%v", startedGeneration, takeoverErr)
			}
			storedGeneration, err := rdb.Get(ctx, generationKey).Int64()
			if err != nil || storedGeneration != startedGeneration {
				t.Fatalf("lost owner bumped takeover generation stored=%d started=%d err=%v", storedGeneration, startedGeneration, err)
			}
		})
	}
}

func TestExecuteSetSpiderMaintenanceRollbackFinishesAndCancelsIntent(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	if err := db.AutoMigrate(&model.Platform{}, &model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	pending, err := prepareSpiderMaintenancePending(context.Background(), db, spiderSetMaintenanceScope(12, "LuoGu"), spiderMaintenanceSetBinding, `{"userId":12,"platform":"LuoGu","username":"new-user"}`)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	service := &SpiderService{db: db, rdb: rdb}
	if err := service.executeSetSpiderMaintenance(context.Background(), pending); err == nil {
		t.Fatal("missing spider task did not abort before facts")
	}
	stored, err := loadSpiderMaintenancePending(context.Background(), db, pending.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if stored != nil {
		t.Fatalf("rolled-back intent was retained: %+v", stored)
	}
	generation, err := rdb.Get(context.Background(), "problem:user_profile:generation:user:12").Int64()
	if err != nil {
		t.Fatal(err)
	}
	if generation%2 != 0 {
		t.Fatalf("rolled-back generation=%d remained odd", generation)
	}
}

func TestPrepareSpiderMaintenancePersistsStableIntentBeforeFence(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	pending, err := prepareSpiderMaintenancePending(context.Background(), db, "spider:set:7:LuoGu", spiderMaintenanceSetBinding, `{"username":"first"}`)
	if err != nil {
		t.Fatal(err)
	}
	if pending.OperationID == "" || pending.Phase != "intent" || pending.Revision != 1 {
		t.Fatalf("pending=%+v", pending)
	}
	var stored model.AbilityMaintenancePending
	if err := db.First(&stored, "scope = ?", pending.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if stored.OperationID != pending.OperationID {
		t.Fatalf("stored intent=%q want %q", stored.OperationID, pending.OperationID)
	}

	retry, err := prepareSpiderMaintenancePending(context.Background(), db, pending.Scope, spiderMaintenanceSetBinding, pending.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if retry.OperationID != pending.OperationID {
		t.Fatalf("retry intent=%q want stable %q", retry.OperationID, pending.OperationID)
	}
	if _, err := prepareSpiderMaintenancePending(context.Background(), db, pending.Scope, spiderMaintenanceSetBinding, `{"username":"second"}`); err == nil {
		t.Fatal("conflicting payload borrowed the active intent")
	}
}

func TestSpiderMaintenanceFactsPhaseRollsBackWithMutation(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	pending, err := prepareSpiderMaintenancePending(context.Background(), db, "spider:purge-user:9", spiderMaintenancePurgeUser, `{"userId":9}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimSpiderMaintenancePending(context.Background(), db, pending, "owner-1"); err != nil {
		t.Fatal(err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := markSpiderMaintenanceFacts(context.Background(), tx, pending); err != nil {
			return err
		}
		return gorm.ErrInvalidTransaction
	})
	if err == nil {
		t.Fatal("forced rollback succeeded")
	}
	var stored model.AbilityMaintenancePending
	if err := db.First(&stored, "scope = ?", pending.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Phase != "intent" || stored.Revision != 2 {
		t.Fatalf("stored after rollback=%+v", stored)
	}
}

func TestExecuteSetSpiderMaintenanceAdjudicatesCommittedLostResponse(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	if err := db.AutoMigrate(&model.Platform{}, &model.SpiderRepairState{}); err != nil {
		t.Fatal(err)
	}
	pending, err := prepareSpiderMaintenancePending(context.Background(), db, spiderSetMaintenanceScope(13, "LuoGu"), spiderMaintenanceSetBinding, `{"userId":13,"platform":"LuoGu","username":"new-user"}`)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	service := &SpiderService{db: db, rdb: rdb, spider: task.NewSpiderTask(nil, rdb, db)}
	original := spiderMaintenanceTransaction
	spiderMaintenanceTransaction = func(db *gorm.DB, fn func(*gorm.DB) error) error {
		if err := db.Transaction(fn); err != nil {
			return err
		}
		return errors.New("injected lost set commit response")
	}
	t.Cleanup(func() { spiderMaintenanceTransaction = original })
	if err := service.executeSetSpiderMaintenance(context.Background(), pending); err != nil {
		t.Fatalf("committed set was treated as rollback: %v", err)
	}
	var binding model.Platform
	if err := db.First(&binding, "user_id = ? AND platform = ?", 13, "LuoGu").Error; err != nil || binding.Username != "new-user" {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
	if stored, err := loadSpiderMaintenancePending(context.Background(), db, pending.Scope); err != nil || stored != nil {
		t.Fatalf("set tail pending=%+v err=%v", stored, err)
	}
}

func TestExecutePurgeUserMaintenanceAdjudicatesCommittedLostResponse(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	if err := db.AutoMigrate(
		&model.Platform{}, &model.SubmitLog{}, &model.ContestLog{}, &model.UserACProblem{},
		&model.UserACProblemDay{}, &model.UserProblemStatus{}, &model.UserTagAC{}, &model.SpiderRepairState{},
	); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Platform{UserID: 14, Platform: "LuoGu", Username: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareSpiderMaintenancePending(context.Background(), db, spiderPurgeUserMaintenanceScope(14), spiderMaintenancePurgeUser, `{"userId":14}`)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	service := &SpiderService{db: db, rdb: rdb, spider: task.NewSpiderTask(nil, rdb, db)}
	original := spiderMaintenanceTransaction
	spiderMaintenanceTransaction = func(db *gorm.DB, fn func(*gorm.DB) error) error {
		if err := db.Transaction(fn); err != nil {
			return err
		}
		return errors.New("injected lost purge-user commit response")
	}
	t.Cleanup(func() { spiderMaintenanceTransaction = original })
	if err := service.executePurgeUserMaintenance(context.Background(), pending); err != nil {
		t.Fatalf("committed purge-user was treated as rollback: %v", err)
	}
	var count int64
	if err := db.Model(&model.Platform{}).Where("user_id = ?", 14).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("purge-user platform count=%d err=%v", count, err)
	}
	if stored, err := loadSpiderMaintenancePending(context.Background(), db, pending.Scope); err != nil || stored != nil {
		t.Fatalf("purge-user tail pending=%+v err=%v", stored, err)
	}
}

func TestExecutePurgeGlobalDoesNotFallbackAfterCommittedLostResponse(t *testing.T) {
	db := newSpiderMaintenanceTestDB(t)
	if err := db.AutoMigrate(&model.SubmitLog{}, &model.Platform{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SubmitLog{UserID: 15, Platform: "Codeforces", SubmitID: "lost-global", Status: "AC", Time: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	pending, err := prepareSpiderMaintenancePending(context.Background(), db, spiderPurgeGlobalMaintenanceScope, spiderMaintenancePurgeGlobal, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	service := &SpiderService{db: db, rdb: rdb, spider: task.NewSpiderTask(nil, rdb, db)}
	original := spiderMaintenanceTransaction
	calls := 0
	spiderMaintenanceTransaction = func(tx *gorm.DB, fn func(*gorm.DB) error) error {
		calls++
		if calls == 1 {
			if err := db.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model.SubmitLog{}).Error; err != nil {
				return err
			}
			res := db.Model(&model.AbilityMaintenancePending{}).
				Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ?", pending.Scope, pending.OperationID, pending.LeaseOwner, pending.Revision).
				Updates(map[string]interface{}{"phase": "facts", "revision": gorm.Expr("revision + 1")})
			if res.Error != nil || res.RowsAffected != 1 {
				return fmt.Errorf("inject global facts rows=%d err=%v", res.RowsAffected, res.Error)
			}
			return errors.New("injected lost truncate commit response")
		}
		return tx.Transaction(fn)
	}
	t.Cleanup(func() { spiderMaintenanceTransaction = original })
	if err := service.executePurgeGlobalMaintenance(context.Background(), pending); err != nil {
		t.Fatalf("committed global purge was treated as rollback: %v", err)
	}
	if calls != 1 {
		t.Fatalf("UNKNOWN/committed truncate incorrectly ran fallback calls=%d", calls)
	}
	if stored, err := loadSpiderMaintenancePending(context.Background(), db, pending.Scope); err != nil || stored != nil {
		t.Fatalf("global tail pending=%+v err=%v", stored, err)
	}
}
