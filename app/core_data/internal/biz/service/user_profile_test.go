package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/utils"
	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"
	profiletask "cwxu-algo/app/core_data/task"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type serviceProfilePublisher struct {
	mu     sync.Mutex
	events []event.UserProfileEvent
}

type failProfileCacheSetHook struct {
	failed     atomic.Bool
	failLatest bool
}

type blockProfileFingerprintGetHook struct {
	key     string
	started chan struct{}
	release chan struct{}
	once    atomic.Bool
}

func (h *blockProfileFingerprintGetHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *blockProfileFingerprintGetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" {
			args := cmd.Args()
			if len(args) > 1 && args[1] == h.key && h.once.CompareAndSwap(false, true) {
				close(h.started)
				<-h.release
			}
		}
		return next(ctx, cmd)
	}
}

func (h *blockProfileFingerprintGetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *failProfileCacheSetHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *failProfileCacheSetHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if (cmd.Name() == "eval" || cmd.Name() == "evalsha") && len(cmd.Args()) > 2 && fmt.Sprint(cmd.Args()[2]) == "5" && h.failed.CompareAndSwap(false, true) {
			return errors.New("forced atomic profile cache publish failure")
		}
		if cmd.Name() == "set" {
			args := cmd.Args()
			if len(args) > 1 {
				if key, ok := args[1].(string); ok && strings.HasPrefix(key, "problem:user_profile:") {
					isLatest := strings.HasSuffix(key, ":latest")
					if isLatest == h.failLatest && h.failed.CompareAndSwap(false, true) {
						return errors.New("forced profile cache set failure")
					}
				}
			}
		}
		return next(ctx, cmd)
	}
}

func (h *failProfileCacheSetHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (p *serviceProfilePublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *serviceProfilePublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var e event.UserProfileEvent
	if err := json.Unmarshal(msg.Body, &e); err != nil {
		return err
	}
	p.events = append(p.events, e)
	return nil
}

func profileTestRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

func profileTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SubmitLog{}, &model.UserACProblem{}, &model.UserTagAC{}, &model.UserTagACSnapshot{}, &model.AbilityModelState{},
		&model.Problem{}, &model.ProblemTag{}, &model.ProblemAbilityStat{}, &model.Platform{},
		&model.UserProfileEvidenceVersion{}, &model.ProfileEvidenceDatasetState{},
	); err != nil {
		t.Fatal(err)
	}
	if err := dal.InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func profileTestCacheIdentity(t *testing.T, db *gorm.DB, userID int64) dal.ProfileCacheIdentity {
	t.Helper()
	identity, err := dal.ReadProfileCacheIdentity(context.Background(), db, userID)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func publishProfileTestTagSnapshot(t *testing.T, db *gorm.DB, userID int64, modelVersion uint64) dal.ProfileCacheIdentity {
	t.Helper()
	identity := profileTestCacheIdentity(t, db, userID)
	var rowCount int64
	if err := db.Model(&model.UserTagAC{}).
		Where("user_id = ? AND score_version = ? AND model_version = ? AND count > 0", userID, dal.CurrentUserTagAbilityScoreVersion, modelVersion).
		Count(&rowCount).Error; err != nil {
		t.Fatal(err)
	}
	header := model.UserTagACSnapshot{
		UserID: userID, ScoreVersion: dal.CurrentUserTagAbilityScoreVersion, ModelVersion: modelVersion,
		EvidenceDatasetRevision: identity.Evidence.DatasetRevision,
		EvidenceUserRevision:    identity.Evidence.UserRevision,
		RowCount:                rowCount,
		PublishedAt:             time.Now(),
	}
	if err := db.Save(&header).Error; err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestUserProfileExactCacheIdentityIncludesSchemaModelAndEvidence(t *testing.T) {
	key := userProfileExactCacheKey(42, 9, "process-7")
	for _, want := range []string{"s9", "u42", "m9", "eprocess-7"} {
		if !strings.Contains(key, want) {
			t.Fatalf("exact key %q does not contain %q", key, want)
		}
	}
	if key == userProfileExactCacheKey(42, 10, "process-7") {
		t.Fatal("model version must participate in exact cache identity")
	}
	if key == userProfileExactCacheKey(42, 9, "process-8") {
		t.Fatal("evidence version must participate in exact cache identity")
	}
}

func TestWriteProfileCachePersistsCapturedIdentity(t *testing.T) {
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{RDB: rdb}}
	snap := &UserProfileSnapshot{}
	if err := uc.writeProfileCache(context.Background(), 42, 9, "process-7", snap); err != nil {
		t.Fatal(err)
	}

	if snap.SchemaVersion != userProfileCacheSchema || snap.ModelVersion != 9 || snap.EvidenceVersion != "process-7" {
		t.Fatalf("snapshot identity not captured: %+v", snap)
	}
	b, err := rdb.Get(context.Background(), userProfileExactCacheKey(42, 9, "process-7")).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var cached UserProfileSnapshot
	if err := utils.GobDecoder(b, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.SchemaVersion != "9" || cached.ModelVersion != 9 || cached.EvidenceVersion != "process-7" {
		t.Fatalf("cached identity mismatch: %+v", cached)
	}
	if rdb.Exists(context.Background(), userProfileExactCacheKey(42, 10, "process-7")).Val() != 0 {
		t.Fatal("write used a re-read model version instead of the captured model")
	}
}

func TestComputeUserProfileUsesVersionedAbilityScore(t *testing.T) {
	db := profileTestDB(t)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 9}).Error; err != nil {
		t.Fatal(err)
	}
	row := model.UserTagAC{
		UserID: 1, Tag: "graph", Count: 4, Weight: 2.8,
		ScoreVersion: dal.CurrentUserTagAbilityScoreVersion, ModelVersion: 9,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	publishProfileTestTagSnapshot(t, db, 1, 9)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}}
	snap, _ := uc.computeUserProfile(1)
	if len(snap.Radar) != 1 {
		t.Fatalf("radar=%+v", snap.Radar)
	}
	want := dal.TagAbilityScore(row.Weight, int(row.Count))
	if math.Abs(snap.Radar[0].Score-want) > 1e-12 {
		t.Fatalf("score=%v want versioned score=%v", snap.Radar[0].Score, want)
	}
	if snap.BuiltAt < time.Now().Add(-time.Minute).Unix() {
		t.Fatalf("unexpected build time: %d", snap.BuiltAt)
	}
}

func TestComputeUserProfileKeepsEverySortedTagForAllTagStatistics(t *testing.T) {
	db := profileTestDB(t)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 10}).Error; err != nil {
		t.Fatal(err)
	}
	rows := make([]model.UserTagAC, 0, 25)
	for i := 0; i < 25; i++ {
		count := int64(i%5 + 1)
		rows = append(rows, model.UserTagAC{
			UserID: 2, Tag: fmt.Sprintf("tag-%02d", i), Count: count,
			Weight:       float64(count) * (0.2 + 0.06*float64(i)),
			ScoreVersion: dal.CurrentUserTagAbilityScoreVersion, ModelVersion: 10,
		})
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	publishProfileTestTagSnapshot(t, db, 2, 10)
	want := append([]model.UserTagAC(nil), rows...)
	sort.Slice(want, func(i, j int) bool {
		si := dal.TagAbilityScore(want[i].Weight, int(want[i].Count))
		sj := dal.TagAbilityScore(want[j].Weight, int(want[j].Count))
		if si != sj {
			return si > sj
		}
		if want[i].Count != want[j].Count {
			return want[i].Count > want[j].Count
		}
		return want[i].Tag < want[j].Tag
	})

	uc := &ProblemUseCase{data: &coredata.Data{DB: db}}
	snap, _ := uc.computeUserProfile(2)
	if len(snap.Radar) != len(want) {
		t.Fatalf("profile tag size=%d want all %d", len(snap.Radar), len(want))
	}
	for i := range snap.Radar {
		if snap.Radar[i].Tag != want[i].Tag || snap.Radar[i].Score != dal.TagAbilityScore(want[i].Weight, int(want[i].Count)) {
			t.Fatalf("radar[%d]=%+v want tag=%s score=%v", i, snap.Radar[i], want[i].Tag, dal.TagAbilityScore(want[i].Weight, int(want[i].Count)))
		}
	}
}

func seedProfileBuild(t *testing.T, db *gorm.DB, userID int64) model.SubmitLog {
	t.Helper()
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1}).Error; err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "Codeforces", ExternalID: "100A", Title: "A", Difficulty: "medium"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "graph"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{
		UserID: userID, ProblemKey: "p:" + fmt.Sprint(p.ID), Platform: "Codeforces", FirstACAt: time.Now().Add(-time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	row := model.SubmitLog{
		UserID: userID, Platform: "Codeforces", SubmitID: fmt.Sprintf("base-%d", userID),
		Status: "AC", IsAC: true, Problem: "A", ExternalID: "100A", Time: time.Now().Add(-time.Hour),
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func assertNoProfileWrites(t *testing.T, rdb *redis.Client, userID int64) {
	t.Helper()
	ctx := context.Background()
	if n := rdb.DBSize(ctx).Val(); n != 0 {
		keys := rdb.Keys(ctx, "*").Val()
		t.Fatalf("profile build wrote %d Redis keys after inconsistent build: %v", n, keys)
	}
	if rdb.Exists(ctx, userProfileFpKey(userID)).Val() != 0 {
		t.Fatal("inconsistent build wrote fingerprint")
	}
}

func TestBuildAndCacheUserProfileRebuildFailureWritesNothing(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(201)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	var acQueries atomic.Int32
	if err := db.Callback().Query().Before("gorm:query").Register("test:fail_rebuild_ac_query", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_ac_problems" && acQueries.Add(1) == 1 {
			tx.AddError(errors.New("forced rebuild failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}

	if err := uc.BuildAndCacheUserProfile(userID, true); err == nil {
		t.Fatal("rebuild failure must fail the build")
	}
	assertNoProfileWrites(t, rdb, userID)
}

func TestBuildAndCacheUserProfileComputeFailureWritesNothing(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(203)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	if err := db.Callback().Row().Before("gorm:row").Register("test:fail_profile_compute", func(tx *gorm.DB) {
		if strings.Contains(strings.ToLower(tx.Statement.SQL.String()), "user_tag_ac_snapshots") {
			tx.AddError(errors.New("forced profile component query failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}

	if err := uc.BuildAndCacheUserProfile(userID, true); err == nil {
		t.Fatal("profile component query failure must fail the build")
	}
	assertNoProfileWrites(t, rdb, userID)
}

func TestBuildAndCacheUserProfileCacheSetFailureDoesNotWriteFingerprint(t *testing.T) {
	for _, tt := range []struct {
		name       string
		failLatest bool
	}{
		{name: "exact"},
		{name: "latest", failLatest: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const userID = int64(204)
			_, rdb := profileTestRedis(t)
			rdb.AddHook(&failProfileCacheSetHook{failLatest: tt.failLatest})
			uc := &ProblemUseCase{data: &coredata.Data{RDB: rdb}}
			snap := &UserProfileSnapshot{}
			if err := uc.cacheBuiltProfile(context.Background(), userID, 1, "evidence", snap); err == nil {
				t.Fatal("cache set failure must fail the build")
			}
			if rdb.Exists(context.Background(), userProfileFpKey(userID)).Val() != 0 {
				t.Fatal("cache set failure wrote fingerprint and would permanently skip retry")
			}
		})
	}
}

func TestBuildAndCacheUserProfileModelFlipWritesNothing(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(202)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	var flipped atomic.Bool
	if err := db.Callback().Query().After("gorm:query").Register("test:flip_model", func(tx *gorm.DB) {
		if tx.Statement.Table == "problem_tags" && flipped.CompareAndSwap(false, true) {
			if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
				Model(&model.AbilityModelState{}).Where("id = ?", 1).Update("active_version", 2).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}

	err := uc.BuildAndCacheUserProfile(userID, true)
	if err == nil {
		t.Fatal("a model flip during build must return a retryable error")
	}
	assertNoProfileWrites(t, rdb, userID)
}

func TestBuildAndCacheUserProfileFinalModelPostcheckWritesNothing(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(205)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	var flipped atomic.Bool
	if err := db.Callback().Row().After("gorm:row").Register("test:flip_model_after_rebuild", func(tx *gorm.DB) {
		if strings.Contains(strings.ToLower(tx.Statement.SQL.String()), "user_tag_ac_snapshots") && flipped.CompareAndSwap(false, true) {
			if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
				Model(&model.AbilityModelState{}).Where("id = ?", 1).Update("active_version", 2).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	err := uc.BuildAndCacheUserProfile(userID, true)
	if err == nil || !strings.Contains(err.Error(), "changed during build") {
		t.Fatalf("final model postcheck error=%v", err)
	}
	assertNoProfileWrites(t, rdb, userID)
}

func TestBuildAndCacheUserProfileEvidenceChangesWriteNothing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*gorm.DB, int64, model.SubmitLog) error
	}{
		{
			name: "insert terminal submit",
			mutate: func(db *gorm.DB, userID int64, _ model.SubmitLog) error {
				return db.Create(&model.SubmitLog{
					UserID: userID, Platform: "AtCoder", SubmitID: fmt.Sprintf("late-%d", userID),
					Status: "WA", Problem: "late", Time: time.Now().Add(-24 * time.Hour),
				}).Error
			},
		},
		{
			name: "bind terminal submit",
			mutate: func(db *gorm.DB, _ int64, base model.SubmitLog) error {
				return db.Model(&model.SubmitLog{}).Where("id = ?", base.ID).Update("problem_id", 99).Error
			},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := profileTestDB(t)
			userID := int64(210 + i)
			base := seedProfileBuild(t, db, userID)
			_, rdb := profileTestRedis(t)
			uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
			var changed atomic.Bool
			if err := db.Callback().Row().After("gorm:row").Register("test:change_evidence", func(tx *gorm.DB) {
				if strings.Contains(strings.ToLower(tx.Statement.SQL.String()), "user_tag_ac_snapshots") && changed.CompareAndSwap(false, true) {
					if err := tt.mutate(tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}), userID, base); err != nil {
						tx.AddError(err)
					}
				}
			}); err != nil {
				t.Fatal(err)
			}

			if err := uc.BuildAndCacheUserProfile(userID, true); err == nil || !strings.Contains(err.Error(), "changed during build") {
				t.Fatal("evidence changed during build must return a retryable error")
			}
			assertNoProfileWrites(t, rdb, userID)
		})
	}
}

func TestBuildAndCacheUserProfileDatasetRevisionChangeWritesNothing(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(219)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	var changed atomic.Bool
	if err := db.Callback().Row().After("gorm:row").Register("test:change_profile_dataset_revision", func(tx *gorm.DB) {
		if strings.Contains(strings.ToLower(tx.Statement.SQL.String()), "user_tag_ac_snapshots") && changed.CompareAndSwap(false, true) {
			if err := tx.Session(&gorm.Session{NewDB: true, SkipHooks: true}).
				Model(&model.ProfileEvidenceDatasetState{}).Where("id = ?", 1).
				Update("revision", gorm.Expr("revision + 1")).Error; err != nil {
				tx.AddError(err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}

	if err := uc.BuildAndCacheUserProfile(userID, true); err == nil || !strings.Contains(err.Error(), "changed during build") {
		t.Fatalf("dataset revision changed during build error=%v", err)
	}
	assertNoProfileWrites(t, rdb, userID)
}

func TestBuildAndCacheUserProfileSkipFingerprintIncludesModelAndEvidence(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(220)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	evidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	if err := rdb.Set(context.Background(), userProfileFpKey(userID), userProfileBuildVersion(1, evidence), 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AbilityModelState{}).Where("id = ?", 1).Update("active_version", 2).Error; err != nil {
		t.Fatal(err)
	}
	var acQueries atomic.Int32
	if err := db.Callback().Query().Before("gorm:query").Register("test:prove_model_not_skipped", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_ac_problems" && acQueries.Add(1) == 1 {
			tx.AddError(errors.New("model change reached rebuild"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	if err := uc.BuildAndCacheUserProfile(userID, false); err == nil {
		t.Fatal("same evidence under a new model must not be skipped")
	}
}

func TestBuildAndCacheUserProfileForceWaitsForNormalSkipThenRebuilds(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(221)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	evidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	if err := rdb.Set(context.Background(), userProfileFpKey(userID), userProfileBuildVersion(1, evidence), 0).Err(); err != nil {
		t.Fatal(err)
	}
	hook := &blockProfileFingerprintGetHook{
		key: userProfileFpKey(userID), started: make(chan struct{}), release: make(chan struct{}),
	}
	rdb.AddHook(hook)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	normalResult := make(chan error, 1)
	go func() { normalResult <- uc.BuildAndCacheUserProfile(userID, false) }()
	<-hook.started
	forceCalling := make(chan struct{})
	forceResult := make(chan error, 1)
	go func() {
		close(forceCalling)
		forceResult <- uc.BuildAndCacheUserProfile(userID, true)
	}()
	<-forceCalling
	deadline := time.NewTimer(time.Second)
	poll := time.NewTicker(time.Millisecond)
	waiting := false
	for !waiting {
		userProfileBuildStateRegistryMu.Lock()
		if current, ok := userProfileBuildStates.Load(userID); ok {
			waiting = current.(*userProfileBuildState).refs == 2
		}
		userProfileBuildStateRegistryMu.Unlock()
		if waiting {
			break
		}
		select {
		case <-deadline.C:
			poll.Stop()
			close(hook.release)
			t.Fatal("force build did not enter the shared user build state")
		case <-poll.C:
		}
	}
	if !deadline.Stop() {
		<-deadline.C
	}
	poll.Stop()
	close(hook.release)
	if err := <-normalResult; err != nil {
		t.Fatalf("normal build: %v", err)
	}
	if err := <-forceResult; err != nil {
		t.Fatalf("force build: %v", err)
	}
	var rows int64
	if err := db.Model(&model.UserTagAC{}).Where("user_id = ?", userID).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("force joined the normal skip flight and never rebuilt tag aggregate")
	}
}

func TestBuildAndCacheUserProfileReleasesIdleBuildState(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(222)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	userProfileBuildStates.Delete(userID)
	t.Cleanup(func() { userProfileBuildStates.Delete(userID) })
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}

	if err := uc.BuildAndCacheUserProfile(userID, true); err != nil {
		t.Fatal(err)
	}
	if _, loaded := userProfileBuildStates.Load(userID); loaded {
		t.Fatal("completed build retained an idle per-user state")
	}
}

func TestUserProfileColdReadDoesNotPublishLaggingTagAggregate(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(301)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 2}).Error; err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "Codeforces", ExternalID: "301A", Title: "A", Difficulty: "medium"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: "graph"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{
		UserID: userID, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: "Codeforces", FirstACAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserTagAC{
		UserID: userID, Tag: "graph", Count: 1, Weight: 0.8,
		ScoreVersion: dal.CurrentUserTagAbilityScoreVersion, ModelVersion: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	publishProfileTestTagSnapshot(t, db, userID, 1)
	_, rdb := profileTestRedis(t)
	pub := &serviceProfilePublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}

	radars, _, _, _, err := uc.UserProfile(userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(radars) != 1 || radars[0].Tag != "graph" {
		t.Fatalf("lagging aggregate must remain readable as stale while rebuild is queued: %+v", radars)
	}
	if keys := rdb.Keys(context.Background(), "problem:user_profile:*").Val(); len(keys) != 0 {
		t.Fatalf("lagging aggregate was published as active model: %v", keys)
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	if len(events) != 1 || events[0].UserId != userID || !events[0].Force {
		t.Fatalf("lagging cold read must enqueue force rebuild: %+v", events)
	}
}

func TestUserProfileColdReadDoesNotPublishActiveModelRowsFromOldEvidence(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(304)
	base := seedProfileBuild(t, db, userID)
	if err := db.Create(&model.UserTagAC{
		UserID: userID, Tag: "graph", Count: 1, Weight: 0.8,
		ScoreVersion: dal.CurrentUserTagAbilityScoreVersion, ModelVersion: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	publishProfileTestTagSnapshot(t, db, userID, 1)
	_, rdb := profileTestRedis(t)
	oldEvidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	if err := rdb.Set(context.Background(), userProfileFpKey(userID), userProfileBuildVersion(1, oldEvidence), 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SubmitLog{
		UserID: userID, Platform: "AtCoder", SubmitID: fmt.Sprintf("new-failure-%d", base.ID),
		Status: "WA", Problem: "late", Time: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	pub := &serviceProfilePublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}

	radars, _, _, _, err := uc.UserProfile(userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(radars) != 0 {
		t.Fatalf("old-evidence aggregate leaked through the light read: %+v", radars)
	}
	if keys := rdb.Keys(context.Background(), "problem:user_profile:*").Val(); len(keys) != 0 {
		t.Fatalf("old-evidence aggregate was published under new evidence: %v", keys)
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	if len(events) != 1 || events[0].UserId != userID || !events[0].Force {
		t.Fatalf("old-evidence cold read must enqueue force rebuild: %+v", events)
	}
}

func TestUserProfileIdentityFailureFailsClosedBeforeLightRead(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(316)
	seedProfileBuild(t, db, userID)
	if err := db.Model(&model.ProfileEvidenceDatasetState{}).Where("id = ?", 1).Update("ready", false).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	var lightQueries atomic.Int32
	recordLightQuery := func(tx *gorm.DB) {
		sqlText := strings.ToLower(tx.Statement.SQL.String())
		if strings.Contains(sqlText, "user_tag_ac_snapshots") || strings.Contains(sqlText, "user_ac_problems") {
			lightQueries.Add(1)
		}
	}
	if err := db.Callback().Raw().Before("gorm:raw").Register("test:identity_failure_no_light_raw", recordLightQuery); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().Before("gorm:query").Register("test:identity_failure_no_light_query", recordLightQuery); err != nil {
		t.Fatal(err)
	}

	if _, _, _, _, err := uc.UserProfile(userID); err == nil {
		t.Fatal("unready durable identity must fail the HTTP profile read")
	}
	if got := lightQueries.Load(); got != 0 {
		t.Fatalf("identity failure still queried unverified light aggregates: %d", got)
	}
}

func TestUserProfileLightReadComponentFailurePropagates(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(317)
	seedProfileBuild(t, db, userID)
	if err := db.Migrator().DropTable(&model.UserACProblem{}); err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}

	if _, _, _, _, err := uc.UserProfile(userID); err == nil {
		t.Fatal("light aggregate query failure must propagate to the HTTP caller")
	}
}

func TestUserProfileColdReadNeverPublishesExactEvenWhenAggregateProofMatches(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(306)
	seedProfileBuild(t, db, userID)
	if err := db.Create(&model.UserTagAC{
		UserID: userID, Tag: "graph", Count: 1, Weight: 0.8,
		ScoreVersion: dal.CurrentUserTagAbilityScoreVersion, ModelVersion: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	publishProfileTestTagSnapshot(t, db, userID, 1)
	_, rdb := profileTestRedis(t)
	evidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	if err := rdb.Set(context.Background(), userProfileFpKey(userID), userProfileBuildVersion(1, evidence), 0).Err(); err != nil {
		t.Fatal(err)
	}
	pub := &serviceProfilePublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}
	radars, _, _, _, err := uc.UserProfile(userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(radars) != 1 {
		t.Fatalf("light compatibility radar=%+v", radars)
	}
	if keys := rdb.Keys(context.Background(), "problem:user_profile:*").Val(); len(keys) != 0 {
		t.Fatalf("HTTP light read published exact/latest and can race MQ rebuild: %v", keys)
	}
}

func TestUserProfileColdMissForcesAndRebuildsTagAggregate(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(305)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	evidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	if err := rdb.Set(context.Background(), userProfileFpKey(userID), userProfileBuildVersion(1, evidence), 0).Err(); err != nil {
		t.Fatal(err)
	}
	pub := &serviceProfilePublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}
	if _, _, _, _, err := uc.UserProfile(userID); err != nil {
		t.Fatal(err)
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	if len(events) != 1 || !events[0].Force {
		t.Fatalf("cold miss event=%+v want force", events)
	}
	if err := uc.BuildAndCacheUserProfile(userID, events[0].Force); err != nil {
		t.Fatalf("consume cold-miss force rebuild: %v", err)
	}
	var rows int64
	if err := db.Model(&model.UserTagAC{}).
		Where("user_id = ? AND model_version = ?", userID, 1).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("cold-miss force rebuild was skipped by matching identity")
	}
	if rdb.Exists(context.Background(), userProfileFpKey(userID)).Val() != 1 {
		t.Fatal("cold-miss force rebuild did not publish build identity")
	}
	radars, _, _, _, err := uc.UserProfile(userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(radars) == 0 {
		t.Fatal("cold-miss force rebuild did not publish readable radar cache")
	}
}

func TestUserProfileRejectsOldLatestAndRebuildsLightSnapshot(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(302)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 3}).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	pub := &serviceProfilePublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}
	old := &UserProfileSnapshot{SchemaVersion: userProfileCacheSchema, ModelVersion: 2, EvidenceVersion: "old", TotalAC: 77}
	b, err := utils.GobEncoder(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), userProfileLatestKey(userID), b, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	evidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	exactKey := userProfileExactCacheKey(userID, 3, evidence)

	_, _, _, total, err := uc.UserProfile(userID)
	if err != nil {
		t.Fatal(err)
	}
	if total == 77 {
		t.Fatalf("stale latest was exposed as current total=%d", total)
	}
	if rdb.Exists(context.Background(), exactKey).Val() != 0 {
		t.Fatal("old latest was copied or rewritten into the current exact key")
	}
	if got := rdb.Get(context.Background(), userProfileLatestKey(userID)).Val(); got != string(b) {
		t.Fatal("strict read unexpectedly rewrote latest snapshot")
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	if len(events) != 1 || events[0].UserId != userID || !events[0].Force {
		t.Fatalf("stale latest must enqueue a force rebuild: %+v", events)
	}
}

func TestUserProfileReturnsEvidenceCurrentStaleModelLatestWhileRebuilding(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(314)
	seedProfileBuild(t, db, userID)
	if err := db.Model(&model.AbilityModelState{}).Where("id = ?", 1).Update("active_version", 3).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	pub := &serviceProfilePublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}
	evidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	stale := &UserProfileSnapshot{TotalAC: 77}
	if err := uc.cacheBuiltProfile(context.Background(), userID, 2, evidence, stale); err != nil {
		t.Fatal(err)
	}

	_, _, _, total, err := uc.UserProfile(userID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 77 {
		t.Fatalf("evidence-current stale model snapshot must remain readable: total=%d", total)
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	if len(events) != 1 || events[0].UserId != userID || !events[0].Force {
		t.Fatalf("stale model latest must enqueue force rebuild: %+v", events)
	}
}

func TestUserProfileCacheHitDoesNotScanEvidenceHistory(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(315)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}
	evidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	snapshot := &UserProfileSnapshot{TotalAC: 88}
	snapshot.Radar = append(snapshot.Radar, struct {
		Tag     string
		Score   float64
		ACCount int64
	}{Tag: "graph", Score: 50, ACCount: 1})
	if err := uc.cacheBuiltProfile(context.Background(), userID, 1, evidence, snapshot); err != nil {
		t.Fatal(err)
	}

	var factQueries atomic.Int32
	factQueryObserved := make(chan struct{}, 1)
	recordFactQuery := func(tx *gorm.DB) {
		sql := strings.ToLower(tx.Statement.SQL.String())
		table := strings.ToLower(tx.Statement.Table)
		if table == "submit_logs" || table == "user_ac_problems" ||
			strings.Contains(sql, "submit_logs") || strings.Contains(sql, "user_ac_problems") {
			factQueries.Add(1)
			select {
			case factQueryObserved <- struct{}{}:
			default:
			}
		}
	}
	if err := db.Callback().Query().Before("gorm:query").Register("test:profile_cache_hit_query", recordFactQuery); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Row().Before("gorm:row").Register("test:profile_cache_hit_row", recordFactQuery); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Raw().Before("gorm:raw").Register("test:profile_cache_hit_raw", recordFactQuery); err != nil {
		t.Fatal(err)
	}

	_, _, _, total, err := uc.UserProfile(userID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 88 {
		t.Fatalf("cache hit total=%d want 88", total)
	}
	select {
	case <-factQueryObserved:
	case <-time.After(250 * time.Millisecond):
	}
	if got := factQueries.Load(); got != 0 {
		t.Fatalf("cache hit scanned evidence history queries=%d", got)
	}
}

func TestUserProfileExactEmptyCacheSelfHealsOnlyForTaggedACCandidates(t *testing.T) {
	for _, tt := range []struct {
		name        string
		userID      int64
		tags        []string
		wantRebuild bool
	}{
		{name: "tagged AC candidate", userID: 318, tags: []string{"graph"}, wantRebuild: true},
		{name: "legitimate empty radar", userID: 319},
		{name: "whitespace tag is legitimate empty", userID: 320, tags: []string{" \t "}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := profileTestDB(t)
			if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1}).Error; err != nil {
				t.Fatal(err)
			}
			p := model.Problem{Platform: "Codeforces", ExternalID: fmt.Sprintf("%dA", tt.userID), Difficulty: "medium"}
			if err := db.Create(&p).Error; err != nil {
				t.Fatal(err)
			}
			for _, tag := range tt.tags {
				if err := db.Create(&model.ProblemTag{ProblemID: p.ID, Tag: tag}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Create(&model.UserACProblem{
				UserID: tt.userID, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: time.Now(),
			}).Error; err != nil {
				t.Fatal(err)
			}
			_, rdb := profileTestRedis(t)
			pub := &serviceProfilePublisher{}
			uc := &ProblemUseCase{
				data:        &coredata.Data{DB: db, RDB: rdb},
				profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
			}
			identity := profileTestCacheIdentity(t, db, tt.userID)
			if err := uc.cacheBuiltProfile(context.Background(), tt.userID, identity.ModelVersion, identity.Evidence.String(), &UserProfileSnapshot{}); err != nil {
				t.Fatal(err)
			}
			var sourceReads atomic.Int32
			if err := db.Callback().Query().Before("gorm:query").Register("count_exact_empty_source_reads", func(tx *gorm.DB) {
				if tx.Statement.Table == "user_ac_problems" {
					sourceReads.Add(1)
				}
			}); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				radar, _, _, _, err := uc.UserProfile(tt.userID)
				if err != nil {
					t.Fatal(err)
				}
				if len(radar) != 0 {
					t.Fatalf("empty cache unexpectedly returned radar: %+v", radar)
				}
			}
			if got := sourceReads.Load(); got != 1 {
				t.Fatalf("two exact-empty requests performed %d authoritative source reads, want 1", got)
			}
			pub.mu.Lock()
			events := append([]event.UserProfileEvent(nil), pub.events...)
			pub.mu.Unlock()
			if tt.wantRebuild {
				if len(events) != 1 || events[0].UserId != tt.userID || !events[0].Force {
					t.Fatalf("invalid empty exact cache event=%+v, want one force rebuild", events)
				}
			} else if len(events) != 0 {
				t.Fatalf("legitimate empty exact cache entered a rebuild loop: %+v", events)
			}
		})
	}
}

func TestUserProfileVerifiedEmptyCacheSkipsAuthoritativeValidation(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(321)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1}).Error; err != nil {
		t.Fatal(err)
	}
	p := model.Problem{Platform: "Codeforces", ExternalID: "321A", Difficulty: "medium"}
	if err := db.Create(&p).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserACProblem{
		UserID: userID, ProblemKey: fmt.Sprintf("p:%d", p.ID), Platform: p.Platform, FirstACAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	pub := &serviceProfilePublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}
	if err := uc.BuildAndCacheUserProfile(userID, true); err != nil {
		t.Fatal(err)
	}

	var sourceReads atomic.Int32
	if err := db.Callback().Query().Before("gorm:query").Register("count_verified_empty_source_reads", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_ac_problems" {
			sourceReads.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		radar, _, _, _, err := uc.UserProfile(userID)
		if err != nil {
			t.Fatal(err)
		}
		if len(radar) != 0 {
			t.Fatalf("verified empty cache unexpectedly returned radar: %+v", radar)
		}
	}
	if got := sourceReads.Load(); got != 0 {
		t.Fatalf("verified empty cache performed %d authoritative source reads, want 0", got)
	}
	pub.mu.Lock()
	events := append([]event.UserProfileEvent(nil), pub.events...)
	pub.mu.Unlock()
	if len(events) != 0 {
		t.Fatalf("verified legitimate empty cache entered rebuild: %+v", events)
	}
}

func TestBuildUserProfileFailsClosedWhileUserGenerationIsOdd(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(307)
	seedProfileBuild(t, db, userID)
	_, rdb := profileTestRedis(t)
	if err := rdb.Set(context.Background(), "problem:user_profile:generation:user:307", 1, 0).Err(); err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db, RDB: rdb}}

	if err := uc.BuildAndCacheUserProfile(userID, true); err == nil {
		t.Fatal("builder published while user invalidation generation was odd")
	}
	if keys := rdb.Keys(context.Background(), "problem:user_profile:s*:u307:*").Val(); len(keys) != 0 {
		t.Fatalf("odd generation published profile keys: %v", keys)
	}
	if rdb.Exists(context.Background(), userProfileFpKey(userID)).Val() != 0 {
		t.Fatal("odd generation published fingerprint")
	}
}

func TestProfileGenerationChangeBetweenIdentityReadAndCacheReadRejectsSnapshot(t *testing.T) {
	_, rdb := profileTestRedis(t)
	const userID = int64(308)
	uc := &ProblemUseCase{data: &coredata.Data{RDB: rdb}}
	ctx := context.Background()
	gen, err := readProfileCacheGeneration(ctx, rdb, userID)
	if err != nil {
		t.Fatal(err)
	}
	snap := &UserProfileSnapshot{TotalAC: 99}
	if err := uc.writeProfileCacheAtGeneration(ctx, userID, 1, "old", gen, snap); err != nil {
		t.Fatal(err)
	}
	token, err := BeginUserProfileInvalidation(ctx, rdb, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := uc.readProfileCacheAtGeneration(ctx, userID, userProfileLatestKey(userID), gen); ok || got != nil {
		t.Fatalf("cache read returned snapshot after generation became odd: %+v", got)
	}
	if err := FinishUserProfileInvalidation(ctx, rdb, userID, token); err != nil {
		t.Fatal(err)
	}
	if err := uc.writeProfileCacheAtGeneration(ctx, userID, 1, "old", gen, &UserProfileSnapshot{TotalAC: 99}); err == nil {
		t.Fatal("pre-invalidation publisher wrote under the post-invalidation generation")
	}
	if rdb.Exists(ctx, userProfileLatestKey(userID)).Val() != 0 {
		t.Fatal("stale publisher restored latest after invalidation")
	}
}

func TestProfileInvalidationDoesNotShareOddGenerationOwnership(t *testing.T) {
	_, rdb := profileTestRedis(t)
	ctx := context.Background()
	first, err := BeginUserProfileInvalidation(ctx, rdb, 309)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BeginUserProfileInvalidation(ctx, rdb, 309)
	if err == nil {
		_ = FinishUserProfileInvalidation(ctx, rdb, 309, second)
		t.Fatalf("concurrent invalidator shared odd generation owner: first=%v second=%v", first, second)
	}
	if err := FinishUserProfileInvalidation(ctx, rdb, 309, first); err != nil {
		t.Fatal(err)
	}
}

func TestProfileInvalidationLeaseExpiryAllowsOwnerSafeTakeover(t *testing.T) {
	mr, rdb := profileTestRedis(t)
	ctx := context.Background()
	first, err := BeginUserProfileInvalidation(ctx, rdb, 310)
	if err != nil {
		t.Fatal(err)
	}
	mr.FastForward(profileInvalidationLeaseTTL + time.Second)
	second, err := beginUserProfileInvalidationForIntent(ctx, rdb, 310, first.IntentID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Owner == second.Owner || first.Generation != second.Generation {
		t.Fatalf("takeover tokens first=%+v second=%+v", first, second)
	}
	if err := FinishUserProfileInvalidation(ctx, rdb, 310, first); err == nil {
		t.Fatal("expired owner completed a replacement lease")
	}
	if err := FinishUserProfileInvalidation(ctx, rdb, 310, second); err != nil {
		t.Fatal(err)
	}
}

func TestBumpUserProfileOwnedGenerationRejectsLostOwnerAfterTakeoverStartsSync(t *testing.T) {
	mr, rdb := profileTestRedis(t)
	ctx := context.Background()
	const userID = int64(314)
	const intentID = "owned-generation-takeover"
	generationKey := fmt.Sprintf("spider:gen:%d:LuoGu", userID)
	ownerA, err := beginUserProfileInvalidationForIntent(ctx, rdb, userID, intentID)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerA.stopHeartbeat()
	if err := ValidateUserProfileInvalidation(ctx, rdb, userID, ownerA); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(profileInvalidationLeaseTTL + time.Second)
	ownerB, err := beginUserProfileInvalidationForIntent(ctx, rdb, userID, intentID)
	if err != nil {
		t.Fatal(err)
	}
	startedGeneration, err := BumpUserProfileOwnedGeneration(ctx, rdb, userID, ownerB, generationKey, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := FinishUserProfileInvalidation(ctx, rdb, userID, ownerB); err != nil {
		t.Fatal(err)
	}
	if _, err := BumpUserProfileOwnedGeneration(ctx, rdb, userID, ownerA, generationKey, 7*24*time.Hour); err == nil {
		t.Fatal("lost owner changed generation after takeover sync started")
	}
	stored, err := rdb.Get(ctx, generationKey).Int64()
	if err != nil || stored != startedGeneration {
		t.Fatalf("lost owner invalidated takeover sync generation=%d started=%d err=%v", stored, startedGeneration, err)
	}
	if ttl := rdb.TTL(ctx, generationKey).Val(); ttl <= 0 {
		t.Fatalf("owned generation TTL was not retained: %v", ttl)
	}
}

func TestProfileInvalidationHeartbeatPreventsLiveOwnerTakeover(t *testing.T) {
	mr, rdb := profileTestRedis(t)
	ctx := context.Background()
	first, err := beginProfileInvalidationForIntentWithTTL(ctx, rdb, profileUserGenerationKey(311), "heartbeat-test", 90*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		mr.FastForward(50 * time.Millisecond)
		time.Sleep(40 * time.Millisecond)
	}
	if second, err := beginUserProfileInvalidationForIntent(ctx, rdb, 311, first.IntentID); err == nil {
		_ = FinishUserProfileInvalidation(ctx, rdb, 311, second)
		t.Fatal("live owner lease expired while heartbeat was running")
	}
	first.stopHeartbeat()
	mr.FastForward(120 * time.Millisecond)
	second, err := beginUserProfileInvalidationForIntent(ctx, rdb, 311, first.IntentID)
	if err != nil {
		t.Fatalf("dead owner lease was not recoverable: %v", err)
	}
	if err := FinishUserProfileInvalidation(ctx, rdb, 311, first); err == nil {
		t.Fatal("old owner continued after replacement takeover")
	}
	if err := FinishUserProfileInvalidation(ctx, rdb, 311, second); err != nil {
		t.Fatal(err)
	}
}

func TestProfileInvalidationBeginRetryReusesIntentOwnerAndGeneration(t *testing.T) {
	_, rdb := profileTestRedis(t)
	ctx := context.Background()
	key := profileUserGenerationKey(312)
	first, err := beginProfileInvalidationForAttemptWithTTL(ctx, rdb, key, "intent-312", "owner-312", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	second, err := beginProfileInvalidationForAttemptWithTTL(ctx, rdb, key, "intent-312", "owner-312", time.Minute)
	if err != nil {
		t.Fatalf("same attempt could not recover ambiguous Begin: %v", err)
	}
	if first.Generation != second.Generation || first.IntentID != second.IntentID || first.Owner != second.Owner {
		t.Fatalf("ambiguous Begin changed identity: first=%+v second=%+v", first, second)
	}
	second.stopHeartbeat()
	if err := FinishUserProfileInvalidation(ctx, rdb, 312, first); err != nil {
		t.Fatal(err)
	}
}

func TestProfileInvalidationDifferentIntentCannotTakeExpiredOdd(t *testing.T) {
	mr, rdb := profileTestRedis(t)
	ctx := context.Background()
	first, err := beginProfileInvalidationForAttemptWithTTL(ctx, rdb, profileUserGenerationKey(313), "intent-a", "owner-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	first.stopHeartbeat()
	mr.FastForward(2 * time.Minute)
	if _, err := beginProfileInvalidationForAttemptWithTTL(ctx, rdb, profileUserGenerationKey(313), "intent-b", "owner-b", time.Minute); err == nil {
		t.Fatal("different intent stole an expired odd generation")
	}
	second, err := beginProfileInvalidationForAttemptWithTTL(ctx, rdb, profileUserGenerationKey(313), "intent-a", "owner-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := FinishUserProfileInvalidation(ctx, rdb, 313, second); err != nil {
		t.Fatal(err)
	}
}

func TestUserProfileRejectsMismatchedSnapshotAtExactKey(t *testing.T) {
	db := profileTestDB(t)
	const userID = int64(303)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 4}).Error; err != nil {
		t.Fatal(err)
	}
	_, rdb := profileTestRedis(t)
	pub := &serviceProfilePublisher{}
	uc := &ProblemUseCase{
		data:        &coredata.Data{DB: db, RDB: rdb},
		profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, rdb),
	}
	evidence := profileTestCacheIdentity(t, db, userID).Evidence.String()
	wrong := &UserProfileSnapshot{SchemaVersion: "6", ModelVersion: 3, EvidenceVersion: "wrong", TotalAC: 11}
	wrongBytes, err := utils.GobEncoder(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), userProfileExactCacheKey(userID, 4, evidence), wrongBytes, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	latest := &UserProfileSnapshot{SchemaVersion: userProfileCacheSchema, ModelVersion: 3, EvidenceVersion: "old", TotalAC: 22}
	latestBytes, err := utils.GobEncoder(latest)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(context.Background(), userProfileLatestKey(userID), latestBytes, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}

	_, _, _, total, err := uc.UserProfile(userID)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("mismatched exact/latest snapshot was exposed: total=%d", total)
	}
}
