package task

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"cwxu-algo/app/common/event"
	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/internal/data/model"

	"github.com/streadway/amqp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type cronAbilityProfilePublisher struct {
	mu       sync.Mutex
	events   []event.UserProfileEvent
	failNext bool
}

func (p *cronAbilityProfilePublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *cronAbilityProfilePublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	p.mu.Lock()
	if p.failNext {
		p.failNext = false
		p.mu.Unlock()
		return errors.New("controlled profile publish failure")
	}
	p.mu.Unlock()
	var profileEvent event.UserProfileEvent
	if err := json.Unmarshal(msg.Body, &profileEvent); err != nil {
		return err
	}
	p.mu.Lock()
	p.events = append(p.events, profileEvent)
	p.mu.Unlock()
	return nil
}

func (p *cronAbilityProfilePublisher) snapshot() []event.UserProfileEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]event.UserProfileEvent(nil), p.events...)
}

type fakeCronAbilityStatsRefresher struct {
	mode    AbilityStatsRefreshMode
	version uint64
	err     error
	hook    func()
	calls   int
}

func (r *fakeCronAbilityStatsRefresher) Refresh(_ context.Context, mode AbilityStatsRefreshMode) (uint64, error) {
	r.calls++
	r.mode = mode
	if r.hook != nil {
		r.hook()
	}
	return r.version, r.err
}

func cronAbilityTestDB(t *testing.T, complete bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	models := []any{
		&model.UserACProblem{}, &model.UserTagAC{}, &model.UserTagACSnapshot{}, &model.AbilityModelState{},
		&model.UserProfileEvidenceVersion{}, &model.ProfileEvidenceDatasetState{},
		&model.AbilityProfileScheduleRun{},
	}
	if complete {
		models = append(models,
			&model.SubmitLog{}, &model.Problem{}, &model.Platform{}, &model.ProblemAbilityStat{},
		)
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ProfileEvidenceDatasetState{
		ID: 1, SchemaVersion: dal.CurrentProfileEvidenceSchemaVersion, Ready: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func addCronAbilityCandidate(t *testing.T, db *gorm.DB, userID int64) {
	t.Helper()
	if err := db.Create(&model.UserACProblem{
		UserID: userID, ProblemKey: "p:1", Platform: "Codeforces", FirstACAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestRunUserProfilePrewarmFullRefreshesBeforeEnqueue(t *testing.T) {
	db := cronAbilityTestDB(t, true)
	addCronAbilityCandidate(t, db, 101)
	pub := &cronAbilityProfilePublisher{}
	cronTask := &CronTask{
		db: db, profile: NewUserProfileTaskWithPublisher(pub, nil),
		abilityStats: NewProblemAbilityStatsRefresher(&coredata.Data{DB: db}),
	}

	cronTask.runUserProfilePrewarm(profilePrewarmDaily)

	var state model.AbilityModelState
	if err := db.WithContext(context.Background()).First(&state, 1).Error; err != nil {
		t.Fatalf("full prewarm published without first creating an active ability model: %v", err)
	}
	if state.ActiveVersion == 0 {
		t.Fatal("full prewarm did not publish a new ability model")
	}
	events := pub.snapshot()
	if len(events) != 1 || events[0].UserId != 101 || !events[0].Force {
		t.Fatalf("events=%+v", events)
	}
}

func TestRunUserProfilePrewarmStartupForcesNewStatsBeforeEnqueue(t *testing.T) {
	db := cronAbilityTestDB(t, false)
	pub := &cronAbilityProfilePublisher{}
	refresher := &fakeCronAbilityStatsRefresher{version: 3}
	refresher.hook = func() {
		if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 3}).Error; err != nil {
			t.Fatal(err)
		}
		addCronAbilityCandidate(t, db, 107)
	}
	cronTask := &CronTask{
		db: db, profile: NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}

	cronTask.runUserProfilePrewarm(profilePrewarmStartup)

	if refresher.calls != 1 || refresher.mode != AbilityStatsForceNew {
		t.Fatalf("startup refresh calls=%d mode=%d", refresher.calls, refresher.mode)
	}
	events := pub.snapshot()
	if len(events) != 1 || events[0].UserId != 107 || !events[0].Force {
		t.Fatalf("startup enumeration did not happen after forced refresh: %+v", events)
	}
}

func TestRunUserProfilePrewarmStartupRunsOnlyOnceAfterSuccess(t *testing.T) {
	db := cronAbilityTestDB(t, false)
	addCronAbilityCandidate(t, db, 108)
	pub := &cronAbilityProfilePublisher{}
	refresher := &fakeCronAbilityStatsRefresher{version: 4}
	cronTask := &CronTask{db: db, profile: NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher}

	cronTask.runUserProfilePrewarm(profilePrewarmStartup)
	cronTask.runUserProfilePrewarm(profilePrewarmStartup)

	if refresher.calls != 1 || len(pub.snapshot()) != 1 {
		t.Fatalf("one-time startup refresh calls=%d events=%+v", refresher.calls, pub.snapshot())
	}
	var marker model.AbilityProfileScheduleRun
	if err := db.First(&marker, "period = ?", oneTimeAbilityProfileMigration).Error; err != nil {
		t.Fatalf("missing one-time completion marker: %v", err)
	}
}

func TestRunUserProfilePrewarmStartupRetriesAfterPublishFailure(t *testing.T) {
	db := cronAbilityTestDB(t, false)
	addCronAbilityCandidate(t, db, 109)
	pub := &cronAbilityProfilePublisher{failNext: true}
	refresher := &fakeCronAbilityStatsRefresher{version: 5}
	cronTask := &CronTask{db: db, profile: NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher}

	cronTask.runUserProfilePrewarm(profilePrewarmStartup)
	cronTask.runUserProfilePrewarm(profilePrewarmStartup)

	if refresher.calls != 2 || len(pub.snapshot()) != 1 {
		t.Fatalf("failed startup was not retried calls=%d events=%+v", refresher.calls, pub.snapshot())
	}
}

func TestRunUserProfilePrewarmRefreshFailurePublishesNothing(t *testing.T) {
	db := cronAbilityTestDB(t, false)
	addCronAbilityCandidate(t, db, 102)
	pub := &cronAbilityProfilePublisher{}
	refreshErr := errors.New("refresh failed")
	refresher := &fakeCronAbilityStatsRefresher{err: refreshErr}
	cronTask := &CronTask{
		db: db, profile: NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}

	cronTask.runUserProfilePrewarm(profilePrewarmDaily)

	if events := pub.snapshot(); len(events) != 0 {
		t.Fatalf("refresh failure must publish zero profile jobs, got %+v", events)
	}
	if refresher.calls != 1 || refresher.mode != AbilityStatsScheduledDaily {
		t.Fatalf("refresh calls=%d mode=%d", refresher.calls, refresher.mode)
	}
}

func TestRunUserProfilePrewarmEmptyHealIncludesLegacyModelRows(t *testing.T) {
	db := cronAbilityTestDB(t, false)
	addCronAbilityCandidate(t, db, 103)
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.UserTagAC{
		UserID: 103, Tag: "legacy", Count: 8, Weight: 8,
		ScoreVersion: 1, ModelVersion: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	pub := &cronAbilityProfilePublisher{}
	refresher := &fakeCronAbilityStatsRefresher{version: 2}
	cronTask := &CronTask{
		db: db, profile: NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}

	cronTask.runUserProfilePrewarm(profilePrewarmEmptyHeal)

	events := pub.snapshot()
	if len(events) != 1 || events[0].UserId != 103 || !events[0].Force {
		t.Fatalf("legacy model row suppressed startup/6h empty-heal: %+v", events)
	}
	if refresher.calls != 1 || refresher.mode != AbilityStatsEnsureActive {
		t.Fatalf("empty-heal refresh calls=%d mode=%d", refresher.calls, refresher.mode)
	}
}

func TestRunUserProfilePrewarmFullCallsRefreshBeforeEnumeration(t *testing.T) {
	db := cronAbilityTestDB(t, false)
	pub := &cronAbilityProfilePublisher{}
	refresher := &fakeCronAbilityStatsRefresher{version: 1}
	refresher.hook = func() { addCronAbilityCandidate(t, db, 104) }
	cronTask := &CronTask{
		db: db, profile: NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}

	cronTask.runUserProfilePrewarm(profilePrewarmDaily)

	events := pub.snapshot()
	if len(events) != 1 || events[0].UserId != 104 || !events[0].Force {
		t.Fatalf("enumeration did not happen after refresh hook: %+v", events)
	}
}

func TestRunUserProfilePrewarmDBGateAvoidsDuplicateFullWithoutRedis(t *testing.T) {
	db := cronAbilityTestDB(t, false)
	addCronAbilityCandidate(t, db, 105)
	pub := &cronAbilityProfilePublisher{}
	refresher := &fakeCronAbilityStatsRefresher{version: 1}
	cronTask := &CronTask{
		db: db, profile: NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}

	cronTask.runUserProfilePrewarm(profilePrewarmDaily)
	cronTask.runUserProfilePrewarm(profilePrewarmDaily)

	if events := pub.snapshot(); len(events) != 1 {
		t.Fatalf("DB schedule gate did not suppress duplicate full enumeration/enqueue without Redis: %+v", events)
	}
}

func TestRunUserProfilePrewarmPartialPublishDoesNotCompleteDBPeriod(t *testing.T) {
	db := cronAbilityTestDB(t, false)
	addCronAbilityCandidate(t, db, 106)
	pub := &cronAbilityProfilePublisher{failNext: true}
	refresher := &fakeCronAbilityStatsRefresher{version: 1}
	cronTask := &CronTask{
		db: db, profile: NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}

	cronTask.runUserProfilePrewarm(profilePrewarmDaily)
	cronTask.runUserProfilePrewarm(profilePrewarmDaily)

	if events := pub.snapshot(); len(events) != 1 || events[0].UserId != 106 {
		t.Fatalf("partial publish was marked complete instead of allowing takeover: %+v", events)
	}
}
