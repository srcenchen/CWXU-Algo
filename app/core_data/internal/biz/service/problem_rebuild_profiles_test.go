package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cwxu-algo/app/common/event"
	coredata "cwxu-algo/app/core_data/internal/data"
	"cwxu-algo/app/core_data/internal/data/model"
	profiletask "cwxu-algo/app/core_data/task"

	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type rebuildProfilesPublisher struct {
	mu         sync.Mutex
	events     []event.UserProfileEvent
	onPublish  func(event.UserProfileEvent) error
	publishErr error
}

type failingRebuildProfilesPublisher struct{}

type staleRetryRebuildProfilesPublisher struct {
	db       *gorm.DB
	intentID string
	userID   int64
}

type blockingRebuildProfilesPublisher struct {
	mu      sync.Mutex
	events  []event.UserProfileEvent
	entered chan struct{}
	release chan struct{}
	calls   int
}

func (p *blockingRebuildProfilesPublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *blockingRebuildProfilesPublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	var profileEvent event.UserProfileEvent
	if err := json.Unmarshal(msg.Body, &profileEvent); err != nil {
		return err
	}
	p.mu.Lock()
	p.events = append(p.events, profileEvent)
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.entered)
		<-p.release
	}
	return nil
}

func (p *blockingRebuildProfilesPublisher) snapshot() []event.UserProfileEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]event.UserProfileEvent(nil), p.events...)
}

func (failingRebuildProfilesPublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (failingRebuildProfilesPublisher) Publish(string, string, bool, bool, amqp.Publishing) error {
	return errors.New("forced profile publish failure")
}

func (p *staleRetryRebuildProfilesPublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *staleRetryRebuildProfilesPublisher) Publish(string, string, bool, bool, amqp.Publishing) error {
	if err := p.db.Model(&model.AbilityMaintenanceTarget{}).
		Where("intent_id = ? AND user_id = ?", p.intentID, p.userID).
		Updates(map[string]interface{}{"revision": gorm.Expr("revision + 1"), "last_error": "new owner"}).Error; err != nil {
		return err
	}
	return errors.New("forced stale publish failure")
}

func (p *rebuildProfilesPublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *rebuildProfilesPublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	var profileEvent event.UserProfileEvent
	if err := json.Unmarshal(msg.Body, &profileEvent); err != nil {
		return err
	}
	if p.onPublish != nil {
		if err := p.onPublish(profileEvent); err != nil {
			return err
		}
	}
	p.mu.Lock()
	p.events = append(p.events, profileEvent)
	p.mu.Unlock()
	return p.publishErr
}

func (p *rebuildProfilesPublisher) snapshot() []event.UserProfileEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]event.UserProfileEvent(nil), p.events...)
}

type fakeAdminAbilityStatsRefresher struct {
	version uint64
	err     error
	hook    func()
	mode    profiletask.AbilityStatsRefreshMode
	calls   int
}

func (r *fakeAdminAbilityStatsRefresher) Refresh(_ context.Context, mode profiletask.AbilityStatsRefreshMode) (uint64, error) {
	r.calls++
	r.mode = mode
	if r.hook != nil {
		r.hook()
	}
	return r.version, r.err
}

func (r *fakeAdminAbilityStatsRefresher) RefreshForMaintenance(ctx context.Context, transition profiletask.AbilityStatsMaintenanceTransition) (uint64, error) {
	r.calls++
	r.mode = profiletask.AbilityStatsForceNew
	if r.hook != nil {
		r.hook()
	}
	if r.err != nil {
		return r.version, r.err
	}
	if err := transition(ctx, nil, r.version); err != nil {
		return 0, err
	}
	return r.version, nil
}

func rebuildProfilesTestDB(t *testing.T, complete bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	models := []any{
		&model.UserACProblem{}, &model.SubmitLog{}, &model.Platform{},
		&model.UserProfileEvidenceVersion{}, &model.ProfileEvidenceDatasetState{},
		&model.AbilityModelState{}, &model.AbilityMaintenancePending{},
		&model.AbilityMaintenanceTarget{},
		&model.UserTagAC{}, &model.UserTagACSnapshot{}, &model.ProblemTag{}, &model.Problem{}, &model.ProblemAbilityStat{},
	}
	if complete {
		models = append(models, &model.ProblemAbilityStat{})
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	if err := model.InstallProfileEvidenceRevisionTriggers(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func addRebuildProfilesCandidate(t *testing.T, db *gorm.DB, userID int64) {
	t.Helper()
	if err := db.Create(&model.UserACProblem{
		UserID: userID, ProblemKey: "p:1", Platform: "Codeforces", FirstACAt: time.Now(),
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestRebuildAllUserProfilesRefreshesBeforeForceBatch(t *testing.T) {
	db := rebuildProfilesTestDB(t, true)
	addRebuildProfilesCandidate(t, db, 201)
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{
		data:         &coredata.Data{DB: db},
		profileTask:  profiletask.NewUserProfileTaskWithPublisher(pub, nil),
		abilityStats: profiletask.NewProblemAbilityStatsRefresher(&coredata.Data{DB: db}),
	}

	candidates, published, rebuildErr := uc.RebuildAllUserProfiles(context.Background())
	if rebuildErr != nil {
		t.Fatal(rebuildErr)
	}

	var state model.AbilityModelState
	if err := db.WithContext(context.Background()).First(&state, 1).Error; err != nil || state.ActiveVersion == 0 {
		t.Fatalf("admin rebuild published without an active refreshed model: state=%+v err=%v", state, err)
	}
	if candidates != 1 || published != 1 {
		t.Fatalf("candidates=%d published=%d", candidates, published)
	}
}

func TestRebuildAllUserProfilesRefreshFailurePublishesNothing(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	addRebuildProfilesCandidate(t, db, 202)
	pub := &rebuildProfilesPublisher{}
	refreshErr := errors.New("refresh failed")
	refresher := &fakeAdminAbilityStatsRefresher{err: refreshErr}
	uc := &ProblemUseCase{
		data:         &coredata.Data{DB: db},
		profileTask:  profiletask.NewUserProfileTaskWithPublisher(pub, nil),
		abilityStats: refresher,
	}

	_, published, gotErr := uc.RebuildAllUserProfiles(context.Background())

	events := pub.snapshot()
	if !errors.Is(gotErr, refreshErr) || published != 0 || len(events) != 0 {
		t.Fatalf("refresh failure err=%v published=%d events=%+v", gotErr, published, events)
	}
	if refresher.calls != 1 || refresher.mode != profiletask.AbilityStatsForceNew {
		t.Fatalf("refresh calls=%d mode=%d", refresher.calls, refresher.mode)
	}
}

func TestRebuildAllUserProfilesCallsRefreshBeforeEnumeration(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	pub := &rebuildProfilesPublisher{}
	refresher := &fakeAdminAbilityStatsRefresher{version: 1}
	refresher.hook = func() {
		addRebuildProfilesCandidate(t, db, 203)
		_ = db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1}).Error
	}
	uc := &ProblemUseCase{
		data:         &coredata.Data{DB: db},
		profileTask:  profiletask.NewUserProfileTaskWithPublisher(pub, nil),
		abilityStats: refresher,
	}

	candidates, published, err := uc.RebuildAllUserProfiles(context.Background())

	events := pub.snapshot()
	if err != nil || candidates != 1 || published != 1 || len(events) != 1 || events[0].UserId != 203 {
		t.Fatalf("candidates=%d published=%d err=%v events=%+v", candidates, published, err, events)
	}
}

func TestRebuildAllUserProfilesPublishFailureReturnsErrorAndKeepsPending(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	addRebuildProfilesCandidate(t, db, 204)
	refresher := &fakeAdminAbilityStatsRefresher{version: 1}
	refresher.hook = func() { _ = db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 1}).Error }
	uc := &ProblemUseCase{
		data:         &coredata.Data{DB: db},
		profileTask:  profiletask.NewUserProfileTaskWithPublisher(failingRebuildProfilesPublisher{}, nil),
		abilityStats: refresher,
	}
	_, _, err := uc.RebuildAllUserProfiles(context.Background())
	if err == nil {
		t.Fatal("profile publish failure was reported as successful rebuild")
	}
	var count int64
	if queryErr := db.Table("ability_maintenance_pending").Where("scope = ?", "global:rebuild").Count(&count).Error; queryErr != nil {
		t.Fatalf("global rebuild pending missing: %v", queryErr)
	}
	if count != 1 {
		t.Fatalf("global rebuild pending count=%d want 1", count)
	}
}

func TestRebuildAllUserProfilesIntentPersistsBeforeRedisFence(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()
	uc := &ProblemUseCase{
		data: &coredata.Data{DB: db, RDB: rdb}, profileTask: profiletask.NewUserProfileTaskWithPublisher(&rebuildProfilesPublisher{}, nil),
		abilityStats: &fakeAdminAbilityStatsRefresher{version: 1},
	}
	if _, _, err := uc.RebuildAllUserProfiles(context.Background()); err == nil {
		t.Fatal("expected Redis fence failure")
	}
	var pending model.AbilityMaintenancePending
	if err := db.First(&pending, "scope = ?", "global:rebuild").Error; err != nil {
		t.Fatalf("global rebuild intent was not committed before Redis fence: %v", err)
	}
	if pending.Phase != "intent" || pending.OperationID == "" {
		t.Fatalf("invalid rebuild intent: %+v", pending)
	}
}

func TestAbilityMaintenanceFenceFinalizedRelayPublishesAndClears(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: "global:relay-test", OperationID: "relay-intent", Revision: 1,
		Phase: "fence_finalized", LeaseOwner: "relay-owner", Operation: "rebuild",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	target := model.AbilityMaintenanceTarget{
		IntentID: pending.OperationID, UserID: 205, Revision: 1, State: "outbox_ready",
		MessagePayload: `{"userId":205,"force":true}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := uc.relayAbilityMaintenanceTargets(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	events := pub.snapshot()
	if len(events) != 1 || events[0].UserId != 205 || !events[0].Force {
		t.Fatalf("relayed events=%+v", events)
	}
	if err := uc.ConfirmAbilityMaintenanceTarget(context.Background(), pending.OperationID, target.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.relayAbilityMaintenanceTargets(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	var pendingCount, targetCount int64
	_ = db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&pendingCount).Error
	_ = db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error
	if pendingCount != 0 || targetCount != 0 {
		t.Fatalf("relay did not atomically clear pending=%d targets=%d", pendingCount, targetCount)
	}
}

func TestAbilityMaintenanceRelayKeepsDeliveredTargetUntilConsumerConfirmation(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: "global:delivery-confirmation", OperationID: "delivery-confirmation-intent", Revision: 1,
		Phase: "fence_finalized", LeaseOwner: "delivery-owner", Operation: "rebuild",
		CreatedAt: now, UpdatedAt: now,
	}
	target := model.AbilityMaintenanceTarget{
		IntentID: pending.OperationID, UserID: 255, Revision: 1, State: "outbox_ready",
		MessagePayload: `{"userId":255,"force":true}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := uc.relayAbilityMaintenanceTargets(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}

	events := pub.snapshot()
	if len(events) != 1 || events[0].UserId != target.UserID || !events[0].Force || events[0].IntentID != target.IntentID {
		t.Fatalf("maintenance event=%+v", events)
	}
	var storedTarget model.AbilityMaintenanceTarget
	if err := db.First(&storedTarget, "intent_id = ? AND user_id = ?", target.IntentID, target.UserID).Error; err != nil {
		t.Fatalf("target must remain durable until consumer confirms: %v", err)
	}
	if storedTarget.State != "delivered" || storedTarget.PublishAttempts != 1 || storedTarget.NextRetryAt.IsZero() {
		t.Fatalf("unexpected delivered target: %+v", storedTarget)
	}
	var pendingCount int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&pendingCount).Error; err != nil || pendingCount != 1 {
		t.Fatalf("parent must remain for delivery confirmation count=%d err=%v", pendingCount, err)
	}
}

func TestRebuildFenceRelayClaimsParentBeforePublishing(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{Scope: "global:rebuild", OperationID: "rebuild-relay-claim", Revision: 1, Phase: "fence_finalized", Operation: "rebuild", CreatedAt: now, UpdatedAt: now}
	target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 260, Revision: 1, State: "outbox_ready", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	pub := &blockingRebuildProfilesPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	newUC := func() *ProblemUseCase {
		return &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil), abilityStats: &fakeAdminAbilityStatsRefresher{version: 1}}
	}
	first := make(chan error, 1)
	go func() { _, _, err := newUC().RebuildAllUserProfiles(context.Background()); first <- err }()
	<-pub.entered
	var leased model.AbilityMaintenancePending
	if err := db.First(&leased, "scope = ?", pending.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if leased.RelayLeaseOwner == "" || leased.Revision != pending.Revision+1 {
		t.Fatalf("rebuild relay lease lacks owner/revision fence: %+v", leased)
	}
	second := make(chan error, 1)
	go func() { _, _, err := newUC().RebuildAllUserProfiles(context.Background()); second <- err }()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("competing rebuild relay: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("competing rebuild relay did not return")
	}
	if events := pub.snapshot(); len(events) != 1 {
		t.Fatalf("competing rebuild published duplicate target: %+v", events)
	}
	close(pub.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	var stored model.AbilityMaintenancePending
	if err := db.First(&stored, "scope = ?", pending.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if stored.RelayLeaseOwner != "" || !stored.RelayLeaseUntil.IsZero() {
		t.Fatalf("relay lease was not released: %+v", stored)
	}
}

func TestProblemFenceRelayClaimsParentBeforePublishing(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{Scope: "problem:260", OperationID: "problem-relay-claim", Revision: 1, Phase: "cache_tail_done", Operation: "problem", CreatedAt: now, UpdatedAt: now}
	target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 261, Revision: 1, State: "outbox_ready", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	pub := &blockingRebuildProfilesPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	newUC := func() *ProblemUseCase {
		return &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}
	}
	first := make(chan error, 1)
	firstPending := pending
	go func() { first <- newUC().completeProblemMaintenanceTail(context.Background(), &firstPending) }()
	<-pub.entered
	var leased model.AbilityMaintenancePending
	if err := db.First(&leased, "scope = ?", pending.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if leased.RelayLeaseOwner == "" || leased.Revision != pending.Revision+1 {
		t.Fatalf("problem relay lease lacks owner/revision fence: %+v", leased)
	}
	secondPending := pending
	if err := newUC().completeProblemMaintenanceTail(context.Background(), &secondPending); err != nil {
		t.Fatalf("competing problem relay: %v", err)
	}
	if events := pub.snapshot(); len(events) != 1 {
		t.Fatalf("competing problem published duplicate target: %+v", events)
	}
	close(pub.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestResetFenceRelayClaimsParentBeforePublishing(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{Scope: "global:reset", OperationID: "reset-relay-claim", Revision: 1, Phase: "fence_finalized", Operation: "reset", Payload: `{"requeue":false}`, CreatedAt: now, UpdatedAt: now}
	target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 262, Revision: 1, State: "outbox_ready", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	pub := &blockingRebuildProfilesPublisher{entered: make(chan struct{}), release: make(chan struct{})}
	newUC := func() *ProblemUseCase {
		return &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}
	}
	first := make(chan error, 1)
	go func() { _, _, _, _, err := newUC().ResetAll(false); first <- err }()
	<-pub.entered
	var leased model.AbilityMaintenancePending
	if err := db.First(&leased, "scope = ?", pending.Scope).Error; err != nil {
		t.Fatal(err)
	}
	if leased.RelayLeaseOwner == "" || leased.Revision != pending.Revision+1 {
		t.Fatalf("reset relay lease lacks owner/revision fence: %+v", leased)
	}
	if _, _, _, _, err := newUC().ResetAll(false); err != nil {
		t.Fatalf("competing reset relay: %v", err)
	}
	if events := pub.snapshot(); len(events) != 1 {
		t.Fatalf("competing reset published duplicate target: %+v", events)
	}
	close(pub.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestRelayStopsBeforeNextTargetAfterLeaseTakeover(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{Scope: "global:relay-heartbeat", OperationID: "relay-heartbeat-intent", Revision: 1, Phase: "fence_finalized", Operation: "rebuild", CreatedAt: now, UpdatedAt: now}
	for _, userID := range []int64{263, 264} {
		if err := db.Create(&model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: userID, Revision: 1, State: "outbox_ready", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	published := 0
	pub := &rebuildProfilesPublisher{onPublish: func(event.UserProfileEvent) error {
		published++
		if published == 1 {
			res := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Updates(map[string]interface{}{
				"relay_lease_owner": "takeover", "relay_lease_until": time.Now().Add(time.Minute), "revision": gorm.Expr("revision + 1"),
			})
			if res.Error != nil || res.RowsAffected != 1 {
				return fmt.Errorf("takeover rows=%d err=%v", res.RowsAffected, res.Error)
			}
		}
		return nil
	}}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil)}
	if _, err := uc.relayAbilityMaintenanceTargets(context.Background(), &pending); err == nil {
		t.Fatal("lost relay owner reported success")
	}
	if events := pub.snapshot(); len(events) != 1 || events[0].UserId != 263 {
		t.Fatalf("lost owner published after takeover: %+v", events)
	}
}

func TestAbilityMaintenanceConsumerDropIsRepublishedAndConfirmedExactlyOnce(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{Scope: "global:rebuild", OperationID: "consumer-drop-intent", Revision: 1, Phase: "fence_finalized", Operation: "rebuild", CreatedAt: now, UpdatedAt: now}
	target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 256, Revision: 1, State: "outbox_ready", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil), abilityStats: &fakeAdminAbilityStatsRefresher{version: 1}}
	if _, err := uc.relayAbilityMaintenanceTargets(context.Background(), &pending); err != nil {
		t.Fatal(err)
	}
	events := pub.snapshot()
	if len(events) != 1 {
		t.Fatalf("initial events=%+v", events)
	}
	builder := &flakyMaintenanceProfileBuilder{confirm: uc, failures: 4}
	consumer := &UserProfileConsumer{problem: builder}
	body, err := json.Marshal(events[0])
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 4; attempt++ {
		if err := consumer.handle(body); err == nil {
			t.Fatalf("consumer attempt %d unexpectedly succeeded", attempt+1)
		}
	}
	var stored model.AbilityMaintenanceTarget
	if err := db.First(&stored, "intent_id = ? AND user_id = ?", pending.OperationID, target.UserID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != "delivered" || stored.NextRetryAt.Before(time.Now().Add(12*time.Hour)) {
		t.Fatalf("consumer drop lost durable target: %+v", stored)
	}
	if err := consumer.handleExhausted(body); err != nil {
		t.Fatal(err)
	}
	uc.recoverAbilityMaintenancePending(context.Background())
	events = pub.snapshot()
	if len(events) != 2 || events[1].IntentID != pending.OperationID {
		t.Fatalf("scanner did not republish durable intent: %+v", events)
	}
	body, err = json.Marshal(events[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.handle(body); err != nil {
		t.Fatal(err)
	}
	if err := consumer.handle(body); err != nil { // duplicate MQ delivery is an idempotent ack
		t.Fatal(err)
	}
	uc.recoverAbilityMaintenancePending(context.Background())
	var parentCount, targetCount int64
	_ = db.Model(&model.AbilityMaintenancePending{}).Where("operation_id = ?", pending.OperationID).Count(&parentCount).Error
	_ = db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error
	if parentCount != 0 || targetCount != 0 {
		t.Fatalf("confirmed maintenance not cleaned parent=%d target=%d", parentCount, targetCount)
	}
	if builder.builds != 6 || builder.confirms != 2 {
		t.Fatalf("unexpected consumer calls builds=%d confirms=%d", builder.builds, builder.confirms)
	}
}

func TestAbilityMaintenanceConfirmationMayRaceDeliveredTransition(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{Scope: "global:confirm-race", OperationID: "confirm-race-intent", Revision: 1, Phase: "fence_finalized", Operation: "rebuild", CreatedAt: now, UpdatedAt: now}
	target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 257, Revision: 1, State: "outbox_ready", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}}
	pub := &rebuildProfilesPublisher{onPublish: func(msg event.UserProfileEvent) error {
		return uc.ConfirmAbilityMaintenanceTarget(context.Background(), msg.IntentID, msg.UserId)
	}}
	uc.profileTask = profiletask.NewUserProfileTaskWithPublisher(pub, nil)
	completed, err := uc.relayAbilityMaintenanceTargets(context.Background(), &pending)
	if err != nil || !completed {
		t.Fatalf("publish/ack race completed=%t err=%v", completed, err)
	}
	var parentCount, targetCount int64
	_ = db.Model(&model.AbilityMaintenancePending{}).Where("operation_id = ?", pending.OperationID).Count(&parentCount).Error
	_ = db.Model(&model.AbilityMaintenanceTarget{}).Where("intent_id = ?", pending.OperationID).Count(&targetCount).Error
	if parentCount != 0 || targetCount != 0 {
		t.Fatalf("publish/ack race not cleaned parent=%d target=%d", parentCount, targetCount)
	}
}

func TestAbilityMaintenanceDeliveredRepublishFailureKeepsRetryableTarget(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{Scope: "global:delivered-failure", OperationID: "delivered-failure-intent", Revision: 1, Phase: "fence_finalized", Operation: "rebuild", CreatedAt: now, UpdatedAt: now}
	target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 258, Revision: 3, State: "delivered", PublishAttempts: 1, NextRetryAt: now.Add(-time.Second), CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(failingRebuildProfilesPublisher{}, nil)}
	if err := uc.publishAbilityMaintenanceTargets(context.Background(), &pending); err == nil || !strings.Contains(err.Error(), "profile publish failed") {
		t.Fatalf("delivered retry failure err=%v", err)
	}
	var stored model.AbilityMaintenanceTarget
	if err := db.First(&stored, "intent_id = ? AND user_id = ?", target.IntentID, target.UserID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != "delivered" || stored.PublishAttempts != 2 || stored.NextRetryAt.Before(time.Now()) {
		t.Fatalf("delivered retry was not durably retained: %+v", stored)
	}
}

func TestAbilityMaintenancePublishFailureAfterConsumerAckIsAccepted(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{Scope: "global:publish-ack-race", OperationID: "publish-ack-race-intent", Revision: 1, Phase: "fence_finalized", Operation: "rebuild", CreatedAt: now, UpdatedAt: now}
	target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 259, Revision: 1, State: "outbox_ready", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}}
	pub := &rebuildProfilesPublisher{publishErr: errors.New("ambiguous publish response")}
	pub.onPublish = func(msg event.UserProfileEvent) error {
		return uc.ConfirmAbilityMaintenanceTarget(context.Background(), msg.IntentID, msg.UserId)
	}
	uc.profileTask = profiletask.NewUserProfileTaskWithPublisher(pub, nil)
	completed, err := uc.relayAbilityMaintenanceTargets(context.Background(), &pending)
	if err != nil || !completed {
		t.Fatalf("ambiguous publish after ack completed=%t err=%v", completed, err)
	}
}

func TestAbilityMaintenanceScannerRecoversPreparedRebuildIntent(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	addRebuildProfilesCandidate(t, db, 206)
	pending := model.AbilityMaintenancePending{
		Scope: "global:rebuild", OperationID: "prepared-rebuild-intent", Revision: 1,
		Phase: "intent", Operation: "rebuild", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	refresher := &fakeAdminAbilityStatsRefresher{version: 1}
	refresher.hook = func() {
		_ = db.Where("id = ?", 1).Assign(model.AbilityModelState{ActiveVersion: 1, BuiltAt: time.Now(), UpdatedAt: time.Now()}).FirstOrCreate(&model.AbilityModelState{ID: 1}).Error
	}
	uc := &ProblemUseCase{
		data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}
	uc.recoverAbilityMaintenancePending(context.Background())
	if events := pub.snapshot(); len(events) != 1 || events[0].UserId != 206 || !events[0].Force {
		t.Fatalf("scanner events=%+v", events)
	}
	var count int64
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("scanner must retain unconfirmed pending=%d err=%v", count, err)
	}
	if err := uc.ConfirmAbilityMaintenanceTarget(context.Background(), pending.OperationID, 206); err != nil {
		t.Fatal(err)
	}
	uc.recoverAbilityMaintenancePending(context.Background())
	if err := db.Model(&model.AbilityMaintenancePending{}).Where("scope = ?", pending.Scope).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("scanner did not clear confirmed pending=%d err=%v", count, err)
	}
}

func TestRebuildAllUserProfilesResumesFromModelReadyWithoutRefreshingAgain(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	addRebuildProfilesCandidate(t, db, 207)
	now := time.Now()
	if err := db.Create(&model.AbilityModelState{ID: 1, ActiveVersion: 4, BuiltAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	pending := model.AbilityMaintenancePending{
		Scope: "global:rebuild", OperationID: "model-ready-rebuild", Revision: 1,
		Phase: "model_ready", Operation: "rebuild", TargetModelVersion: 4,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	pub := &rebuildProfilesPublisher{}
	refresher := &fakeAdminAbilityStatsRefresher{err: errors.New("refresh must not repeat")}
	uc := &ProblemUseCase{
		data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(pub, nil), abilityStats: refresher,
	}
	candidates, published, err := uc.RebuildAllUserProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if refresher.calls != 0 || candidates != 1 || published != 1 {
		t.Fatalf("refresh_calls=%d candidates=%d published=%d", refresher.calls, candidates, published)
	}
}

func TestAbilityMaintenanceTargetsPersistStableSetAndPerUserProgress(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: "global:progress", OperationID: "progress-intent", Revision: 1,
		Phase: "model_ready", LeaseOwner: "owner", Operation: "rebuild",
		TargetModelVersion: 7, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := prepareAbilityMaintenanceRebuildTargets(context.Background(), db, &pending, []int64{301, 302, 303}); err != nil {
		t.Fatal(err)
	}
	seen := make([]int64, 0, 3)
	err := rebuildPendingAbilityMaintenanceTargets(context.Background(), db, &pending, nil, func(userID int64) error {
		seen = append(seen, userID)
		if userID == 302 {
			return errors.New("injected second-user kill")
		}
		return nil
	})
	if err == nil {
		t.Fatal("injected per-user failure was reported complete")
	}
	var targets []model.AbilityMaintenanceTarget
	if err := db.Where("intent_id = ?", pending.OperationID).Order("user_id").Find(&targets).Error; err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 || targets[0].State != "rebuilt" || targets[1].State != "pending" || targets[2].State != "pending" {
		t.Fatalf("durable progress=%+v", targets)
	}
	seen = seen[:0]
	if err := rebuildPendingAbilityMaintenanceTargets(context.Background(), db, &pending, nil, func(userID int64) error {
		seen = append(seen, userID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != 302 || seen[1] != 303 {
		t.Fatalf("recovery repeated completed target: %v", seen)
	}
}

func TestAbilityMaintenanceOldOwnerCannotCommitTargetAfterParentTakeover(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: "global:target-takeover", OperationID: "target-takeover-intent", Revision: 3,
		Phase: "targets_ready", LeaseOwner: "owner-a", Operation: "rebuild", CreatedAt: now, UpdatedAt: now,
	}
	target := model.AbilityMaintenanceTarget{IntentID: pending.OperationID, UserID: 305, Revision: 1, State: "pending", CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	err := rebuildPendingAbilityMaintenanceTargets(context.Background(), db, &pending, nil, func(userID int64) error {
		if userID != target.UserID {
			t.Fatalf("unexpected target user=%d", userID)
		}
		res := db.Model(&model.AbilityMaintenancePending{}).
			Where("scope = ? AND operation_id = ? AND lease_owner = ? AND revision = ?", pending.Scope, pending.OperationID, pending.LeaseOwner, pending.Revision).
			Updates(map[string]interface{}{"lease_owner": "owner-b", "revision": gorm.Expr("revision + 1")})
		if res.Error != nil || res.RowsAffected != 1 {
			t.Fatalf("takeover rows=%d err=%v", res.RowsAffected, res.Error)
		}
		return nil
	})
	if err == nil {
		t.Fatal("old owner committed target progress after parent takeover")
	}
	var stored model.AbilityMaintenanceTarget
	if err := db.First(&stored, "intent_id = ? AND user_id = ?", target.IntentID, target.UserID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.State != "pending" || stored.Revision != 1 {
		t.Fatalf("old owner advanced target after takeover: %+v", stored)
	}
}

func TestAbilityMaintenancePublishFailureRetryMetadataUsesStrictCAS(t *testing.T) {
	db := rebuildProfilesTestDB(t, false)
	now := time.Now()
	pending := model.AbilityMaintenancePending{
		Scope: "global:retry-cas", OperationID: "retry-cas-intent", Revision: 2,
		Phase: "derived_ready", LeaseOwner: "owner", Operation: "rebuild", CreatedAt: now, UpdatedAt: now,
	}
	target := model.AbilityMaintenanceTarget{
		IntentID: pending.OperationID, UserID: 304, Revision: 5, State: "outbox_ready",
		MessagePayload: `{"userId":304,"force":true}`, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	publisher := &staleRetryRebuildProfilesPublisher{db: db, intentID: pending.OperationID, userID: target.UserID}
	uc := &ProblemUseCase{data: &coredata.Data{DB: db}, profileTask: profiletask.NewUserProfileTaskWithPublisher(publisher, nil)}
	if err := uc.publishAbilityMaintenanceTargets(context.Background(), &pending); err == nil || !strings.Contains(err.Error(), "retry owner changed") {
		t.Fatalf("stale retry CAS err=%v", err)
	}
	var stored model.AbilityMaintenanceTarget
	if err := db.First(&stored, "intent_id = ? AND user_id = ?", target.IntentID, target.UserID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 6 || stored.LastError != "new owner" || stored.PublishAttempts != 0 {
		t.Fatalf("stale retry metadata overwrote new owner: %+v", stored)
	}
}
