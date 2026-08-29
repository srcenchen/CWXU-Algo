package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	profiletask "cwxu-algo/app/core_data/task"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type afterNthProfileValidationHook struct {
	want int32
	seen atomic.Int32
	run  func()
}

func (h *afterNthProfileValidationHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *afterNthProfileValidationHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		args := cmd.Args()
		if err == nil && cmd.Name() == "evalsha" && len(args) > 1 && fmt.Sprint(args[1]) == profileValidateInvalidationScript.Hash() && h.seen.Add(1) == h.want {
			h.run()
		}
		return err
	}
}

func (h *afterNthProfileValidationHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func problemFactsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:facts_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.Problem{}, &model.ProblemTag{}, &model.UserACProblem{}, &model.UserTagAC{}, &model.UserTagACSnapshot{},
		&model.UserProblemStatus{}, &model.AbilityModelState{}, &model.ProblemAbilityStat{}, &model.SubmitLog{}, &model.Platform{},
		&model.UserProfileEvidenceVersion{}, &model.ProfileEvidenceDatasetState{},
		&model.AbilityMaintenancePending{},
		&model.AbilityMaintenanceTarget{},
	); err != nil {
		t.Fatal(err)
	}
	if err := model.InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestCreateManualProblemPersistsTagInvertedIndex(t *testing.T) {
	db := problemFactsTestDB(t)
	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	p, err := uc.CreateManualProblem(0, "manual", "statement", "", []string{"dp", "graph"})
	if err != nil {
		t.Fatal(err)
	}
	var tags []model.ProblemTag
	if err := db.Where("problem_id = ?", p.ID).Order("tag").Find(&tags).Error; err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0].Tag != "dp" || tags[1].Tag != "graph" {
		t.Fatalf("manual problem tags not synchronized: %+v", tags)
	}
}

func TestApplyProblemFieldsTagSyncFailureRollsBackProblemJSON(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Manual", ExternalID: "rollback", Title: "rollback", Tags: model.StringArray{"old"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_new_problem_tag BEFORE INSERT ON problem_tags WHEN NEW.tag = 'new' BEGIN SELECT RAISE(FAIL, 'forced tag sync failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}}

	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err == nil {
		t.Fatal("tag sync failure was reported as success")
	}
	var after model.Problem
	if err := db.First(&after, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(after.Tags) != 1 || after.Tags[0] != "old" {
		t.Fatalf("problem JSON committed without inverted index: %+v", after.Tags)
	}
}

func TestApplyProblemDifficultyRefreshesBeforeForcePublishingAllACUsers(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "1A", Title: "A", Difficulty: "简单"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	for i, uid := range []int64{101, 102} {
		if err := db.Create(&model.UserACProblem{UserID: uid, ProblemKey: "p:" + string(rune('1'+i)), Platform: "Codeforces", FirstACAt: time.Now()}).Error; err != nil {
			t.Fatal(err)
		}
	}
	pub := &rebuildProfilesPublisher{}
	refresher := &fakeAdminAbilityStatsRefresher{version: 8}
	refresher.hook = func() {
		if len(pub.snapshot()) != 0 {
			t.Fatal("profile published before posterior refresh")
		}
		if err := db.Where("id = ?", 1).Assign(model.AbilityModelState{ActiveVersion: 8, BuiltAt: time.Now(), UpdatedAt: time.Now()}).FirstOrCreate(&model.AbilityModelState{ID: 1}).Error; err != nil {
			t.Fatal(err)
		}
	}
	uc := &ProblemUseCase{
		data: &data.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}

	if _, err := uc.ApplyProblemFields(p.ID, false, nil, false, "", "", true, "困难"); err != nil {
		t.Fatal(err)
	}
	events := pub.snapshot()
	if refresher.calls != 1 || refresher.mode != profiletask.AbilityStatsForceNew || len(events) != 2 {
		t.Fatalf("refresh calls=%d mode=%d events=%+v", refresher.calls, refresher.mode, events)
	}
	for _, got := range events {
		if !got.Force {
			t.Fatalf("difficulty profile event not forced: %+v", got)
		}
		if err := uc.ConfirmAbilityMaintenanceTarget(context.Background(), got.IntentID, got.UserId); err != nil {
			t.Fatal(err)
		}
	}
	uc.recoverAbilityMaintenancePending(context.Background())

	pub.events = nil
	refresher.err = errors.New("refresh unavailable")
	if _, err := uc.ApplyProblemFields(p.ID, false, nil, false, "", "", true, "中等"); err == nil {
		t.Fatal("posterior refresh failure was reported as complete")
	}
	if events := pub.snapshot(); len(events) != 0 {
		t.Fatalf("refresh failure published profiles: %+v", events)
	}
	var dirty model.Problem
	if err := db.First(&dirty, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if dirty.Status != model.ProblemStatusTagging || !strings.HasPrefix(dirty.ErrorMsg, problemFactsDirtyPrefix) {
		t.Fatalf("refresh failure was not persisted as retryable dirty facts: status=%s error=%q", dirty.Status, dirty.ErrorMsg)
	}
	refresher.err = nil
	if _, err := uc.ApplyProblemFields(p.ID, false, nil, false, "", "", true, "中等"); err != nil {
		t.Fatalf("same-value retry did not recover dirty facts: %v", err)
	}
	if refresher.calls != 3 {
		t.Fatalf("dirty retry did not force posterior refresh: calls=%d", refresher.calls)
	}
	if events := pub.snapshot(); len(events) != 2 {
		t.Fatalf("dirty retry did not republish all canonical AC users: %+v", events)
	}
}

func TestProcessAnalyzeRecoversDirtyCompletedFactsBeforeSkip(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{
		Platform: "Codeforces", ExternalID: "2A", Title: "A", Difficulty: "困难",
		Tags: model.StringArray{"dp"}, Status: model.ProblemStatusCompleted,
		ErrorMsg: problemFactsDirtyPrefix + "difficulty",
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{UserID: 111, ProblemKey: "p:1", Platform: "Codeforces", FirstACAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	refresher := &fakeAdminAbilityStatsRefresher{version: 9}
	refresher.hook = func() {
		_ = db.Where("id = ?", 1).Assign(model.AbilityModelState{ActiveVersion: 9, BuiltAt: time.Now(), UpdatedAt: time.Now()}).FirstOrCreate(&model.AbilityModelState{ID: 1}).Error
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher}

	if err := uc.ProcessAnalyze(context.Background(), event.ProblemAnalyzeEvent{ProblemID: p.ID}); err != nil {
		t.Fatal(err)
	}
	var after model.Problem
	if err := db.First(&after, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 1 || strings.HasPrefix(after.ErrorMsg, problemFactsDirtyPrefix) || len(pub.snapshot()) != 1 {
		t.Fatalf("dirty completed recovery calls=%d problem=%+v events=%+v", refresher.calls, after, pub.snapshot())
	}
}

func TestApplyProblemTagsUsesGlobalFenceButPublishesOnlyCanonicalACUsers(t *testing.T) {
	db := problemFactsTestDB(t)
	now := time.Now()
	p := model.Problem{Platform: "Codeforces", ExternalID: "3A", Title: "A", Tags: model.StringArray{"old"}}
	other := model.Problem{Platform: "Codeforces", ExternalID: "3B", Title: "B", Tags: model.StringArray{"other"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.ProblemTag{{ProblemID: p.ID, Tag: "old"}, {ProblemID: other.ID, Tag: "other"}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 3, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.UserACProblem{
		{UserID: 121, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: now},
		{UserID: 122, ProblemKey: fmt.Sprintf("p:%d", other.ID), Platform: other.Platform, FirstACAt: now},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserProblemStatus{UserID: 123, ProblemID: p.ID, Status: model.UserProblemStatusTried, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&[]model.UserTagAC{
		{UserID: 121, Tag: "legacy", Count: 99, Weight: 99, ScoreVersion: 0, ModelVersion: 0},
		{UserID: 121, Tag: "old", Count: 1, Weight: 1, ScoreVersion: 1, ModelVersion: 3},
		{UserID: 122, Tag: "other", Count: 1, Weight: 1, ScoreVersion: 1, ModelVersion: 3},
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	ctx := context.Background()
	if err := rdb.Set(ctx, userProfileLatestKey(121), "old", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, userProfileFpKey(121), "old", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, userProfileLatestKey(122), "keep", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb)}

	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err != nil {
		t.Fatal(err)
	}
	events := pub.snapshot()
	if len(events) != 1 || events[0].UserId != 121 || !events[0].Force {
		t.Fatalf("tag change profile scope=%+v", events)
	}
	var changed []model.UserTagAC
	if err := db.Where("user_id = ?", 121).Find(&changed).Error; err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].Tag != "new" || changed[0].ScoreVersion != 1 || changed[0].ModelVersion != 3 {
		t.Fatalf("tag rows were not full version-one replacement: %+v", changed)
	}
	if rdb.Exists(ctx, userProfileLatestKey(121), userProfileFpKey(121)).Val() != 0 {
		t.Fatal("affected user's exact/latest/fingerprint identity was not invalidated")
	}
	if rdb.Get(ctx, userProfileLatestKey(122)).Val() != "" {
		t.Fatal("global tag fence did not invalidate unrelated stale cache")
	}
}

func TestProblemFactsCommitResponseAmbiguityDoesNotCancelCommittedIntent(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "ambiguous", Title: "A", Tags: model.StringArray{"old"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{UserID: 124, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: time.Now(), UpdatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb)}
	original := problemFactsTransaction
	problemFactsTransaction = func(db *gorm.DB, fn func(*gorm.DB) error) error {
		if err := db.Transaction(fn); err != nil {
			return err
		}
		return errors.New("injected lost commit response")
	}
	t.Cleanup(func() { problemFactsTransaction = original })
	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err != nil {
		t.Fatalf("committed facts were cancelled after ambiguous response: %v", err)
	}
	var after model.Problem
	if err := db.First(&after, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(after.Tags) != 1 || after.Tags[0] != "new" || len(pub.snapshot()) != 1 {
		t.Fatalf("committed intent did not recover: problem=%+v events=%+v", after, pub.snapshot())
	}
	generation, parseErr := strconv.ParseInt(rdb.Get(context.Background(), profileGlobalGenerationKey).Val(), 10, 64)
	if parseErr != nil || generation%2 != 0 {
		t.Fatalf("ambiguous commit left unsafe generation=%d err=%v", generation, parseErr)
	}
}

func TestProblemFactsRollbackAdjudicationNeverClearsTakeoverOwner(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "takeover-adjudication", Title: "A", Tags: model.StringArray{"old"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	mr, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(&rebuildProfilesPublisher{}, rdb)}
	original := problemFactsTransaction
	var takeover ProfileInvalidationToken
	problemFactsTransaction = func(tx *gorm.DB, fn func(*gorm.DB) error) error {
		injected := errors.New("injected rollback response")
		_ = tx.Transaction(func(inner *gorm.DB) error {
			if err := fn(inner); err != nil {
				return err
			}
			return injected
		})
		mr.FastForward(profileInvalidationLeaseTTL + time.Second)
		var err error
		takeover, err = beginGlobalProfileInvalidationForIntent(context.Background(), rdb, mustProblemMaintenanceIntent(t, db, p.ID))
		if err != nil {
			t.Fatal(err)
		}
		stored, err := loadAbilityMaintenancePending(context.Background(), db, problemMaintenanceScope(p.ID))
		if err != nil {
			t.Fatal(err)
		}
		if err := claimAbilityMaintenancePending(context.Background(), db, stored, takeover.Owner); err != nil {
			t.Fatal(err)
		}
		if err := advanceAbilityMaintenancePhase(context.Background(), db, stored, "facts"); err != nil {
			t.Fatal(err)
		}
		return injected
	}
	t.Cleanup(func() {
		problemFactsTransaction = original
		_ = AbandonGlobalProfileInvalidation(context.Background(), rdb, takeover)
	})
	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err == nil {
		t.Fatal("takeover ambiguity was reported complete")
	}
	var stored model.AbilityMaintenancePending
	if err := db.First(&stored, "scope = ?", problemMaintenanceScope(p.ID)).Error; err != nil {
		t.Fatalf("old owner deleted takeover pending: %v", err)
	}
	if stored.LeaseOwner != takeover.Owner || stored.Phase != "facts" {
		t.Fatalf("takeover pending changed: %+v token=%+v", stored, takeover)
	}
}

func TestProblemFactsRollbackFinishLossRetainsImmutablePending(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "rollback-finish-loss", Title: "A", Tags: model.StringArray{"old"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	mr, rdb := profileTestRedis(t)
	if err := profileValidateInvalidationScript.Load(context.Background(), rdb).Err(); err != nil {
		t.Fatal(err)
	}
	takeoverRDB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = takeoverRDB.Close() })
	var takeover ProfileInvalidationToken
	var takeoverErr error
	rdb.AddHook(&afterNthProfileValidationHook{want: 2, run: func() {
		mr.FastForward(profileInvalidationLeaseTTL + time.Second)
		takeover, takeoverErr = beginGlobalProfileInvalidationForIntent(context.Background(), takeoverRDB, mustProblemMaintenanceIntent(t, db, p.ID))
	}})
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(&rebuildProfilesPublisher{}, rdb)}
	original := problemFactsTransaction
	problemFactsTransaction = func(tx *gorm.DB, fn func(*gorm.DB) error) error {
		injected := errors.New("injected rollback before finish takeover")
		_ = tx.Transaction(func(inner *gorm.DB) error {
			if err := fn(inner); err != nil {
				return err
			}
			return injected
		})
		return injected
	}
	t.Cleanup(func() {
		problemFactsTransaction = original
		if takeover.Owner != "" {
			_ = AbandonGlobalProfileInvalidation(context.Background(), takeoverRDB, takeover)
		}
	})
	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err == nil {
		t.Fatal("rollback takeover was reported complete")
	}
	if takeoverErr != nil || takeover.Owner == "" {
		t.Fatalf("takeover failed: token=%+v err=%v", takeover, takeoverErr)
	}
	var stored model.AbilityMaintenancePending
	if err := db.First(&stored, "scope = ?", problemMaintenanceScope(p.ID)).Error; err != nil {
		t.Fatalf("Finish ownership loss cleared immutable pending: %v", err)
	}
	if stored.OperationID != takeover.IntentID || stored.Phase != "intent" {
		t.Fatalf("rollback pending changed after Finish ownership loss: %+v", stored)
	}
	generation, err := rdb.Get(context.Background(), profileGlobalGenerationKey).Int64()
	if err != nil || generation%2 == 0 || rdb.Get(context.Background(), profileGlobalGenerationKey+":current_intent").Val() != takeover.IntentID {
		t.Fatalf("takeover fence changed generation=%d current=%q err=%v", generation, rdb.Get(context.Background(), profileGlobalGenerationKey+":current_intent").Val(), err)
	}
	if err := claimAbilityMaintenancePending(context.Background(), db, &stored, takeover.Owner); err != nil {
		t.Fatalf("takeover owner could not claim retained pending: %v", err)
	}
}

func mustProblemMaintenanceIntent(t *testing.T, db *gorm.DB, problemID uint) string {
	t.Helper()
	var pending model.AbilityMaintenancePending
	if err := db.First(&pending, "scope = ?", problemMaintenanceScope(problemID)).Error; err != nil {
		t.Fatal(err)
	}
	return pending.OperationID
}

func TestRecoverTagOnlyDerivedReadyFinishesOriginalGlobalFence(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "recover-tag-global", Title: "A", Tags: model.StringArray{"new"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(problemMaintenancePayload{Updates: map[string]interface{}{"tags": model.StringArray{"new"}}, Tags: []string{"new"}, TagsChanged: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: problemMaintenanceScope(p.ID), OperationID: "recover-tag-global-intent", Revision: 4,
		Phase: "derived_ready", LeaseOwner: "dead-owner", ProblemID: p.ID, Operation: "problem",
		Payload: string(payload), TagsChanged: true, CreatedAt: now, UpdatedAt: now,
	}
	target := model.AbilityMaintenanceTarget{
		IntentID: pending.OperationID, UserID: 127, Revision: 2, State: "outbox_ready",
		MessagePayload: `{"userId":127,"force":true}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	crashed, err := beginGlobalProfileInvalidationForIntent(context.Background(), rdb, pending.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := AbandonGlobalProfileInvalidation(context.Background(), rdb, crashed); err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb)}
	if err := uc.recoverProblemMaintenance(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	if err := uc.ConfirmAbilityMaintenanceTarget(context.Background(), pending.OperationID, target.UserID); err != nil {
		t.Fatal(err)
	}
	uc.recoverAbilityMaintenancePending(context.Background())
	generation, parseErr := strconv.ParseInt(rdb.Get(context.Background(), profileGlobalGenerationKey).Val(), 10, 64)
	if parseErr != nil || generation%2 != 0 || rdb.Exists(context.Background(), profileGlobalGenerationKey+":current_intent").Val() != 0 {
		t.Fatalf("tag recovery left global fence generation=%d parse=%v current=%q", generation, parseErr, rdb.Get(context.Background(), profileGlobalGenerationKey+":current_intent").Val())
	}
	var pendingCount, targetCount int64
	_ = db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&pendingCount).Error
	_ = db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error
	if pendingCount != 0 || targetCount != 0 || len(pub.snapshot()) != 1 {
		t.Fatalf("tag recovery pending=%d targets=%d events=%+v", pendingCount, targetCount, pub.snapshot())
	}
}

func TestProblemTagTargetsIncludeCanonicalUserCommittedAfterFacts(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "tag-boundary", Title: "A", Tags: model.StringArray{"old"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: time.Now(), UpdatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb)}
	original := problemFactsTransaction
	problemFactsTransaction = func(tx *gorm.DB, fn func(*gorm.DB) error) error {
		if err := tx.Transaction(fn); err != nil {
			return err
		}
		return db.Create(&model.UserACProblem{UserID: 125, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: time.Now()}).Error
	}
	t.Cleanup(func() { problemFactsTransaction = original })
	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err != nil {
		t.Fatal(err)
	}
	events := pub.snapshot()
	if len(events) != 1 || events[0].UserId != 125 || !events[0].Force {
		t.Fatalf("post-facts canonical user missed durable target: %+v", events)
	}
}

func TestProblemTagLeaseLossAfterFactsHasNoDerivedSideEffects(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "lease-loss", Title: "A", Tags: model.StringArray{"old"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{UserID: 126, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: time.Now(), UpdatedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	mr, rdb := profileTestRedis(t)
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb)}
	original := problemFactsTransaction
	problemFactsTransaction = func(tx *gorm.DB, fn func(*gorm.DB) error) error {
		if err := tx.Transaction(fn); err != nil {
			return err
		}
		mr.FastForward(profileInvalidationLeaseTTL + time.Second)
		return nil
	}
	t.Cleanup(func() { problemFactsTransaction = original })
	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err == nil {
		t.Fatal("expired profile lease was reported complete")
	}
	var derivedCount, targetCount int64
	_ = db.Model(&model.UserTagAC{}).Where("user_id = ?", 126).Count(&derivedCount).Error
	_ = db.Model(&model.AbilityMaintenanceTarget{}).Where("user_id = ?", 126).Count(&targetCount).Error
	if derivedCount != 0 || targetCount != 0 || len(pub.snapshot()) != 0 {
		t.Fatalf("lease loss leaked derived side effects rows=%d targets=%d events=%+v", derivedCount, targetCount, pub.snapshot())
	}
}

func TestLostOwnerCannotRewriteDirtyMarkerAfterTakeoverCompletes(t *testing.T) {
	db := problemFactsTestDB(t)
	now := time.Now()
	p := model.Problem{Platform: "Codeforces", ExternalID: "dirty-takeover", Title: "A", Tags: model.StringArray{"old"}, Status: model.ProblemStatusCompleted}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{UserID: 128, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	mr, rdb := profileTestRedis(t)
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb)}
	entered := make(chan struct{})
	release := make(chan struct{})
	var blocked atomic.Bool
	if err := db.Callback().Query().Before("gorm:query").Register("test:block_old_owner_rebuild", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "ability_model_state" && blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	aDone := make(chan error, 1)
	go func() {
		_, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, "")
		aDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old owner did not reach rebuild kill point")
	}
	mr.FastForward(profileInvalidationLeaseTTL + time.Second)
	pending, err := loadAbilityMaintenancePending(context.Background(), db, problemMaintenanceScope(p.ID))
	if err != nil || pending == nil {
		t.Fatalf("load takeover pending=%+v err=%v", pending, err)
	}
	if err := uc.recoverProblemMaintenance(context.Background(), pending); err != nil {
		t.Fatalf("takeover recovery: %v", err)
	}
	close(release)
	if err := <-aDone; err == nil {
		t.Fatal("lost old owner was reported complete")
	}
	var after model.Problem
	if err := db.First(&after, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(after.ErrorMsg, problemFactsDirtyPrefix) || after.Status == model.ProblemStatusTagging {
		t.Fatalf("lost owner rewrote completed problem marker: %+v", after)
	}
	var target model.AbilityMaintenanceTarget
	if err := db.First(&target, "intent_id = ?", pending.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := uc.ConfirmAbilityMaintenanceTarget(context.Background(), target.IntentID, target.UserID); err != nil {
		t.Fatal(err)
	}
	uc.recoverAbilityMaintenancePending(context.Background())
	var pendingCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", problemMaintenanceScope(p.ID)).Count(&pendingCount).Error; err != nil || pendingCount != 0 {
		t.Fatalf("takeover completion pending=%d err=%v", pendingCount, err)
	}
}

func TestResetAllPersistsPreparedIntentBeforeAnalyzePurge(t *testing.T) {
	db := problemFactsTestDB(t)
	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	if _, _, _, _, err := uc.ResetAll(true); err == nil {
		t.Fatal("nil MQ purge unexpectedly succeeded")
	}
	var pending model.AbilityMaintenancePending
	if err := db.First(&pending, "scope = ?", "global:reset").Error; err != nil {
		t.Fatalf("purge failure had no durable reset intent: %v", err)
	}
	if pending.Phase != "intent" || pending.Payload != `{"requeue":true}` {
		t.Fatalf("invalid prepared reset intent: %+v", pending)
	}
}

func TestResetFactsUnknownCommitOutcomeAbandonsWithoutFinishingFence(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "reset-unknown", Title: "A", ContentMD: "statement", Status: model.ProblemStatusCompleted}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: "global:reset", OperationID: "reset-unknown-intent", Revision: 1, Phase: "queue_purged",
		Operation: "reset", Payload: `{"requeue":false}`, TagsChanged: true, DifficultyChanged: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}}
	original := problemFactsTransaction
	var failReload atomic.Bool
	problemFactsTransaction = func(tx *gorm.DB, fn func(*gorm.DB) error) error {
		if err := tx.Transaction(fn); err != nil {
			return err
		}
		failReload.Store(true)
		return errors.New("injected lost reset commit response")
	}
	if err := db.Callback().Query().Before("gorm:query").Register("test:fail_reset_adjudication_reload", func(tx *gorm.DB) {
		if failReload.Load() && tx.Statement != nil && tx.Statement.Table == "ability_maintenance_pending" {
			tx.AddError(errors.New("injected reset reload failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { problemFactsTransaction = original })
	if _, _, _, _, err := uc.ResetAll(false); err == nil {
		t.Fatal("unknown reset outcome was reported complete")
	}
	failReload.Store(false)
	generation, parseErr := strconv.ParseInt(rdb.Get(context.Background(), profileGlobalGenerationKey).Val(), 10, 64)
	if parseErr != nil || generation%2 == 0 || rdb.Get(context.Background(), profileGlobalGenerationKey+":current_intent").Val() != pending.OperationID {
		t.Fatalf("unknown reset outcome prematurely finished fence generation=%d parse=%v current=%q", generation, parseErr, rdb.Get(context.Background(), profileGlobalGenerationKey+":current_intent").Val())
	}
}

func seedFenceFinalizedProblemMaintenance(t *testing.T, db *gorm.DB, externalID string) (model.Problem, model.AbilityMaintenancePending) {
	t.Helper()
	p := model.Problem{
		Platform: "Codeforces", ExternalID: externalID, Title: "A", Tags: model.StringArray{"new"},
		Status: model.ProblemStatusTagging, ErrorMsg: problemFactsDirtyPrefix + "tags",
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: problemMaintenanceScope(p.ID), OperationID: externalID + "-intent", Revision: 7,
		Phase: "fence_finalized", LeaseOwner: "final-owner", ProblemID: p.ID,
		Operation: "problem", TagsChanged: true, CreatedAt: now, UpdatedAt: now,
	}
	target := model.AbilityMaintenanceTarget{
		IntentID: pending.OperationID, UserID: 129, Revision: 3, State: "outbox_ready",
		MessagePayload: `{"userId":129,"force":true}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	return p, pending
}

func TestProblemMaintenanceFenceFinalizedRecoveryCompletesDurableCacheTail(t *testing.T) {
	db := problemFactsTestDB(t)
	p, pending := seedFenceFinalizedProblemMaintenance(t, db, "tail-after-fence")
	_, rdb := profileTestRedis(t)
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb)}
	if err := uc.recoverProblemMaintenance(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	var after model.Problem
	if err := db.First(&after, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(after.ErrorMsg, problemFactsDirtyPrefix) {
		t.Fatalf("fence-finalized recovery left dirty marker: %+v", after)
	}
	if rdb.Get(context.Background(), problemTagsVerKey).Val() != "1" || rdb.Get(context.Background(), problemListVerKey).Val() != "1" {
		t.Fatalf("fence-finalized recovery missed cache tail tags=%q list=%q", rdb.Get(context.Background(), problemTagsVerKey).Val(), rdb.Get(context.Background(), problemListVerKey).Val())
	}
}

func TestProblemMaintenanceRelayFailureDoesNotRepeatCompletedCacheTail(t *testing.T) {
	db := problemFactsTestDB(t)
	_, pending := seedFenceFinalizedProblemMaintenance(t, db, "tail-before-relay")
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{
		data:        &data.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(failingRebuildProfilesPublisher{}, rdb),
	}
	if err := uc.recoverProblemMaintenance(context.Background(), &pending); err == nil {
		t.Fatal("relay failure was reported complete")
	}
	var stored model.AbilityMaintenancePending
	if err := db.First(&stored, "scope = ?", pending.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Phase != "cache_tail_done" {
		t.Fatalf("completed cache tail was not durable before relay: %+v", stored)
	}
	firstTags := rdb.Get(context.Background(), problemTagsVerKey).Val()
	firstList := rdb.Get(context.Background(), problemListVerKey).Val()
	uc.profileTask = profiletask.NewUserProfileTaskWithPublisher(&rebuildProfilesPublisher{}, rdb)
	if err := uc.recoverProblemMaintenance(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	if rdb.Get(context.Background(), problemTagsVerKey).Val() != firstTags || rdb.Get(context.Background(), problemListVerKey).Val() != firstList {
		t.Fatalf("relay retry repeated cache tail tags=%q->%q list=%q->%q", firstTags, rdb.Get(context.Background(), problemTagsVerKey).Val(), firstList, rdb.Get(context.Background(), problemListVerKey).Val())
	}
}

func TestResetProblemFactsClearsInvertedAndVersionOneDerivedRows(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{
		Platform: "Codeforces", ExternalID: "4A", Title: "A", ContentMD: "statement",
		Tags: model.StringArray{"dp"}, Difficulty: "困难", Status: model.ProblemStatusCompleted,
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "dp"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserTagAC{UserID: 131, Tag: "dp", Count: 1, Weight: 1, ScoreVersion: 1, ModelVersion: 2}).Error; err != nil {
		t.Fatal(err)
	}

	reset, err := resetProblemFacts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if reset != 1 {
		t.Fatalf("reset=%d want 1", reset)
	}
	var after model.Problem
	if err := db.First(&after, p.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(after.Tags) != 0 || after.Difficulty != "" || after.Status != model.ProblemStatusTagging {
		t.Fatalf("problem facts not reset: %+v", after)
	}
	for _, table := range []string{"problem_tags", "user_tag_ac"} {
		var n int64
		if err := db.Table(table).Count(&n).Error; err != nil || n != 0 {
			t.Fatalf("table=%s count=%d err=%v", table, n, err)
		}
	}
}

func TestProblemFactsRefreshFailurePersistsRetryOutbox(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "5A", Title: "A", Difficulty: "简单"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	refresher := &fakeAdminAbilityStatsRefresher{err: errors.New("refresh unavailable")}
	uc := &ProblemUseCase{data: &data.Data{DB: db}, abilityStats: refresher}
	if _, err := uc.ApplyProblemFields(p.ID, false, nil, false, "", "", true, "困难"); err == nil {
		t.Fatal("expected refresh failure")
	}
	var count int64
	if err := db.Table("ability_maintenance_pending").Where("scope = ?", fmt.Sprintf("problem:%d", p.ID)).Count(&count).Error; err != nil {
		t.Fatalf("pending outbox was not persisted with facts: %v", err)
	}
	if count != 1 {
		t.Fatalf("pending outbox count=%d want 1", count)
	}
}

func TestProblemFactsIntentPersistsBeforeRedisFence(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "intent", Title: "A", Difficulty: "简单"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()
	uc := &ProblemUseCase{data: &data.Data{DB: db, RDB: rdb}}
	if _, err := uc.ApplyProblemFields(p.ID, false, nil, false, "", "", true, "困难"); err == nil {
		t.Fatal("expected Redis fence failure")
	}
	var pending model.AbilityMaintenancePending
	if err := db.First(&pending, "scope = ?", problemMaintenanceScope(p.ID)).Error; err != nil {
		t.Fatalf("durable intent was not committed before Redis fence: %v", err)
	}
	if pending.Phase != "intent" || pending.OperationID == "" {
		t.Fatalf("invalid pre-fence intent: %+v", pending)
	}
}

func TestAbilityMaintenanceRecoveryRotatesFailedOldestBatch(t *testing.T) {
	db := problemFactsTestDB(t)
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	for i := 0; i < 51; i++ {
		createdAt := base.Add(time.Duration(i) * time.Second)
		pending := model.AbilityMaintenancePending{
			Scope: fmt.Sprintf("problem:starvation:%02d", i), OperationID: fmt.Sprintf("problem-starvation-%02d", i),
			Revision: 1, Phase: "intent", Operation: "problem", Payload: "{", CreatedAt: createdAt, UpdatedAt: createdAt,
		}
		if err := db.Create(&pending).Error; err != nil {
			t.Fatal(err)
		}
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	firstAttempt := time.Now()
	uc.recoverAbilityMaintenancePending(context.Background())
	var first, late model.AbilityMaintenancePending
	if err := db.First(&first, "scope = ?", "problem:starvation:00").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&late, "scope = ?", "problem:starvation:50").Error; err != nil {
		t.Fatal(err)
	}
	if first.UpdatedAt.Before(firstAttempt) {
		t.Fatalf("first failed attempt was not rotated: %v", first.UpdatedAt)
	}
	if !late.UpdatedAt.Equal(base.Add(50 * time.Second)) {
		t.Fatalf("late intent entered first batch: %v", late.UpdatedAt)
	}
	secondAttempt := time.Now()
	uc.recoverAbilityMaintenancePending(context.Background())
	if err := db.First(&late, "scope = ?", "problem:starvation:50").Error; err != nil {
		t.Fatal(err)
	}
	if late.UpdatedAt.Before(secondAttempt) {
		t.Fatalf("late intent remained starved: %v", late.UpdatedAt)
	}
}

func TestAbilityMaintenanceIntentConflictIsImmutableAndClearUsesRevisionCAS(t *testing.T) {
	db := problemFactsTestDB(t)
	ctx := context.Background()
	first, created, err := ensureAbilityMaintenancePending(ctx, db, model.AbilityMaintenancePending{
		Scope: "problem:immutable", Operation: "problem", Payload: `{"request":"a"}`, TagsChanged: true,
	})
	if err != nil || !created {
		t.Fatalf("first intent created=%v err=%v", created, err)
	}
	second, created, err := ensureAbilityMaintenancePending(ctx, db, model.AbilityMaintenancePending{
		Scope: "problem:immutable", Operation: "problem", Payload: `{"request":"b"}`, DifficultyChanged: true,
	})
	if err != nil || created {
		t.Fatalf("conflicting intent created=%v err=%v", created, err)
	}
	if second.OperationID != first.OperationID || second.Payload != first.Payload || second.DifficultyChanged {
		t.Fatalf("conflicting request overwrote active intent: first=%+v second=%+v", first, second)
	}
	if err := claimAbilityMaintenancePending(ctx, db, first, "owner-a"); err != nil {
		t.Fatal(err)
	}
	stale := *first
	if err := advanceAbilityMaintenancePhase(ctx, db, first, "facts"); err != nil {
		t.Fatal(err)
	}
	if err := clearAbilityMaintenancePending(ctx, db, &stale); err == nil {
		t.Fatal("stale revision cleared a newer intent phase")
	}
	var count int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", first.Scope).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("stale clear removed active intent: count=%d err=%v", count, err)
	}
}

func TestProblemFactsAffectedUsersRequireProfilePublisher(t *testing.T) {
	db := problemFactsTestDB(t)
	now := time.Now()
	p := model.Problem{Platform: "Codeforces", ExternalID: "publisher", Title: "A", Tags: model.StringArray{"old"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{UserID: 151, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err == nil {
		t.Fatal("affected users were accepted without a profile publisher")
	}
	var count int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", problemMaintenanceScope(p.ID)).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("publisher failure did not retain pending: count=%d err=%v", count, err)
	}
}

func TestResetMaintenanceUsesImmutableRequeuePayload(t *testing.T) {
	db := problemFactsTestDB(t)
	ctx := context.Background()
	encoded, err := json.Marshal(resetMaintenancePayload{Requeue: true})
	if err != nil {
		t.Fatal(err)
	}
	pending, created, err := ensureAbilityMaintenancePending(ctx, db, model.AbilityMaintenancePending{
		Scope: "global:reset", Operation: "reset", Payload: string(encoded), TagsChanged: true, DifficultyChanged: true,
	})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	requeue, err := resetMaintenanceRequeue(pending, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !requeue {
		t.Fatal("scanner request false overwrote durable requeue=true")
	}
}

func TestResetMaintenancePartialAnalyzePublishRetainsPendingUntilReplay(t *testing.T) {
	db := problemFactsTestDB(t)
	now := time.Now()
	problems := []model.Problem{
		{Platform: "Codeforces", ExternalID: "reset-tail-a", Title: "A", ContentMD: "statement", Status: model.ProblemStatusTagging},
		{Platform: "Codeforces", ExternalID: "reset-tail-b", Title: "B", ContentMD: "statement", Status: model.ProblemStatusTagging},
	}
	if err := db.Create(&problems).Error; err != nil {
		t.Fatal(err)
	}
	for i := range problems {
		pid := problems[i].ID
		if err := db.Create(&model.SubmitLog{
			Platform: "Codeforces", UserID: 1, SubmitID: fmt.Sprintf("reset-tail-%d", i),
			ProblemID: &pid, Time: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	pending := model.AbilityMaintenancePending{
		Scope: "global:reset", OperationID: "reset-tail-intent", Revision: 1,
		Phase: "fence_finalized", LeaseOwner: "reset-owner", Operation: "reset",
		Payload: `{"requeue":true}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	var firstAttempt []uint
	_, err := uc.completeResetMaintenanceTail(context.Background(), &pending, true, func(problemID uint) error {
		firstAttempt = append(firstAttempt, problemID)
		if len(firstAttempt) == 2 {
			return errors.New("injected analyze publish failure")
		}
		return nil
	})
	if err == nil {
		t.Fatal("partial analyze publish was reported complete")
	}
	var count int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("partial tail cleared pending count=%d err=%v", count, err)
	}
	var replay []uint
	enqueued, err := uc.completeResetMaintenanceTail(context.Background(), &pending, true, func(problemID uint) error {
		replay = append(replay, problemID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 2 || len(replay) != 2 {
		t.Fatalf("replay enqueued=%d ids=%v", enqueued, replay)
	}
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("successful replay pending=%d err=%v", count, err)
	}
}

func TestResetProblemFactsPersistsGlobalPendingInSameTransaction(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "6A", Title: "A", ContentMD: "x", Status: model.ProblemStatusCompleted}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := resetProblemFacts(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("ability_maintenance_pending").Where("scope = ?", "global:reset").Count(&count).Error; err != nil {
		t.Fatalf("global reset pending missing: %v", err)
	}
	if count != 1 {
		t.Fatalf("global reset pending count=%d want 1", count)
	}
}

func TestDirtyTagRecoveryWithoutUpdateTagsKeepsProblemTagIndex(t *testing.T) {
	db := problemFactsTestDB(t)
	now := time.Now()
	p := model.Problem{Platform: "Codeforces", ExternalID: "7A", Title: "A", ContentMD: "old", Tags: model.StringArray{"dp"}, ErrorMsg: problemFactsDirtyPrefix + "tags", Status: model.ProblemStatusTagging}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "dp"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	if _, err := uc.ApplyProblemFields(p.ID, false, nil, true, "new content", "", false, ""); err != nil {
		t.Fatal(err)
	}
	var rows []model.ProblemTag
	if err := db.Where("problem_id = ?", p.ID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Tag != "dp" {
		t.Fatalf("dirty recovery cleared tag index: %+v", rows)
	}
}

func TestDifficultyRefreshReenumeratesCanonicalUsersAfterRefresh(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "8A", Title: "A", Difficulty: "简单"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{UserID: 141, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	refresher := &fakeAdminAbilityStatsRefresher{version: 2}
	refresher.hook = func() {
		now := time.Now()
		_ = db.Where("id = ?", 1).Assign(model.AbilityModelState{ActiveVersion: 2, BuiltAt: now, UpdatedAt: now}).FirstOrCreate(&model.AbilityModelState{ID: 1}).Error
		_ = db.Create(&model.UserACProblem{UserID: 142, ProblemKey: "p:999", Platform: "Codeforces", FirstACAt: now}).Error
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}, abilityStats: refresher, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}
	if _, err := uc.ApplyProblemFields(p.ID, false, nil, false, "", "", true, "困难"); err != nil {
		t.Fatal(err)
	}
	events := pub.snapshot()
	if len(events) != 2 {
		t.Fatalf("post-refresh canonical user was missed: %+v", events)
	}
}

func TestApplyProblemFactUpdateRequiresExactlyOneProblemRow(t *testing.T) {
	db := problemFactsTestDB(t)
	p := model.Problem{Platform: "Codeforces", ExternalID: "9A", Title: "A", Tags: model.StringArray{"old"}}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(fmt.Sprintf(`CREATE TRIGGER ignore_problem_fact_update BEFORE UPDATE ON problems WHEN OLD.id = %d BEGIN SELECT RAISE(IGNORE); END`, p.ID)).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	if _, err := uc.ApplyProblemFields(p.ID, true, []string{"new"}, false, "", "", false, ""); err == nil {
		t.Fatal("zero-row problem update was treated as success")
	}
	var tags []model.ProblemTag
	if err := db.Where("problem_id = ?", p.ID).Find(&tags).Error; err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Tag != "old" {
		t.Fatalf("tag index changed after zero-row problem update: %+v", tags)
	}
}

func TestNormalizeEditTags(t *testing.T) {
	got := normalizeEditTags([]string{" 动态规划 ", "前缀和", "动态规划", "", "  "})
	if len(got) != 2 || got[0] != "动态规划" || got[1] != "前缀和" {
		t.Fatalf("unexpected tags: %#v", got)
	}
}

func TestProblemEditPendingSummaryIncludesChangedFields(t *testing.T) {
	req := &model.ProblemEditRequest{
		HasTags:           true,
		HasContent:        true,
		ProposedTags:      model.StringArray{"动态规划", "前缀和"},
		ProposedContentMD: "这是一段新题面",
		ProposedTitle:     "新的题目标题",
		Note:              "修正样例与标签",
	}
	body := problemEditPendingSummary("原题目", req)
	for _, want := range []string{"原题目", "标题改为", "题面内容", "动态规划、前缀和", "修正样例与标签"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body=%q missing %q", body, want)
		}
	}
}

func TestProblemEditApprovalThankYouListsApprovedFields(t *testing.T) {
	req := &model.ProblemEditRequest{
		HasTags:       true,
		HasContent:    true,
		ProposedTitle: "新标题",
	}
	body := problemEditApprovalThankYou(nil, req)
	for _, want := range []string{"题目标题", "题面内容", "题目标签", "感谢你为 GoAlgo 作出贡献"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body=%q missing %q", body, want)
		}
	}
}

func TestHtmlEscapePlain(t *testing.T) {
	got := htmlEscapePlain(`a <b> & "x"`)
	if !strings.Contains(got, "&lt;") || !strings.Contains(got, "&amp;") {
		t.Fatalf("got %q", got)
	}
}

func TestListProblemContributorsDistinctByFirstApprove(t *testing.T) {
	dsn := "file:problem_contrib_" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.ProblemEditRequest{}); err != nil {
		t.Fatal(err)
	}
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	rows := []model.ProblemEditRequest{
		{ProblemID: 9, UserID: 2, Status: model.ProblemEditApproved, UpdatedAt: t2},
		{ProblemID: 9, UserID: 1, Status: model.ProblemEditApproved, UpdatedAt: t1},
		// 同一用户再次通过：仍只出现一次
		{ProblemID: 9, UserID: 1, Status: model.ProblemEditApproved, UpdatedAt: t3},
		{ProblemID: 9, UserID: 3, Status: model.ProblemEditRejected, UpdatedAt: t1},
		{ProblemID: 8, UserID: 1, Status: model.ProblemEditApproved, UpdatedAt: t1},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	uc := &ProblemUseCase{data: &data.Data{DB: db}}
	ids, err := uc.ListProblemContributors(9)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("ids=%v want [1 2] by first approve time", ids)
	}
}

func TestNonEmptyTags(t *testing.T) {
	if len(nonEmptyTags(model.StringArray{})) != 0 {
		t.Fatal("empty should be empty")
	}
	if len(nonEmptyTags(model.StringArray{"", "  "})) != 0 {
		t.Fatal("blank tags should be empty")
	}
	if len(nonEmptyTags(model.StringArray{"图论"})) != 1 {
		t.Fatal("expected one tag")
	}
}
