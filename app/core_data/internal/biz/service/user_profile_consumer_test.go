package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"cwxu-algo/app/common/event"
	profiletask "cwxu-algo/app/core_data/task"

	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
)

func recordedServiceProfileEvent(t *testing.T, publisher *serviceProfilePublisher, index int) event.UserProfileEvent {
	t.Helper()
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if index < 0 || index >= len(publisher.events) {
		t.Fatalf("published event index=%d count=%d", index, len(publisher.events))
	}
	return publisher.events[index]
}

func TestUserProfileConsumerExhaustedDropsPoisonAndInvalidMessages(t *testing.T) {
	consumer := &UserProfileConsumer{}
	if err := consumer.handleExhausted([]byte("{")); err != nil {
		t.Fatalf("bad JSON must remain droppable after retries: %v", err)
	}
	if err := consumer.handleExhausted([]byte(`{"user_id":0,"force":true}`)); err != nil {
		t.Fatalf("invalid user must remain droppable after retries: %v", err)
	}
}

func TestUserProfileConsumerOrdinaryRequiresPendingDependency(t *testing.T) {
	body := []byte(`{"user_id":9,"force":true,"claim_token":"claim-9"}`)
	consumer := &UserProfileConsumer{problem: &flakyMaintenanceProfileBuilder{}}
	if err := consumer.handle(body); err == nil || !strings.Contains(err.Error(), "dependency unavailable") {
		t.Fatalf("successful ordinary build without profile task error=%v", err)
	}
	if err := consumer.handleExhausted(body); err == nil || !strings.Contains(err.Error(), "dependency unavailable") {
		t.Fatalf("exhausted ordinary build without profile task error=%v", err)
	}
}

func requireUserProfileDependencyError(t *testing.T, fn func() error) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("valid user profile message panicked instead of returning a dependency error: %v", recovered)
		}
	}()
	if err := fn(); err == nil || !strings.Contains(err.Error(), "dependency unavailable") {
		t.Fatalf("dependency error=%v", err)
	}
}

func TestUserProfileConsumerValidMessageRequiresBuilderDependency(t *testing.T) {
	profileTask := profiletask.NewUserProfileTaskWithPublisher(&serviceProfilePublisher{}, nil)
	t.Run("ordinary handle", func(t *testing.T) {
		consumer := &UserProfileConsumer{profileTask: profileTask}
		requireUserProfileDependencyError(t, func() error {
			return consumer.handle([]byte(`{"user_id":10,"claim_token":"claim-10"}`))
		})
	})
	t.Run("maintenance handle", func(t *testing.T) {
		consumer := &UserProfileConsumer{}
		requireUserProfileDependencyError(t, func() error {
			return consumer.handle([]byte(`{"user_id":10,"force":true,"intent_id":"maintenance-10"}`))
		})
	})
	t.Run("maintenance exhausted", func(t *testing.T) {
		consumer := &UserProfileConsumer{}
		requireUserProfileDependencyError(t, func() error {
			return consumer.handleExhausted([]byte(`{"user_id":10,"force":true,"intent_id":"maintenance-10"}`))
		})
	})
}

func TestUserProfileConsumerExhaustedOrdinaryClearsPendingAndAllowsReenqueue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
	}{
		{name: "normal"},
		{name: "force", force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rdb := profileTestRedis(t)
			publisher := &serviceProfilePublisher{}
			profileTask := profiletask.NewUserProfileTaskWithPublisher(publisher, rdb)
			builder := &flakyMaintenanceProfileBuilder{failures: 4}
			const userID = int64(901)
			enqueue := func() profiletask.UserProfileEnqueueResult {
				if tc.force {
					return profileTask.DoForce(userID)
				}
				return profileTask.Do(userID)
			}

			if result := enqueue(); !result.Published || result.Deduped || result.Failed {
				t.Fatalf("initial enqueue=%+v", result)
			}
			body, err := json.Marshal(recordedServiceProfileEvent(t, publisher, 0))
			if err != nil {
				t.Fatal(err)
			}
			pendingKey := fmt.Sprintf("user_profile:pending:%d", userID)
			wantPending := rdb.Get(t.Context(), pendingKey).Val()
			wantPrefix := "normal:published"
			if tc.force {
				wantPrefix = "force:published"
			}
			if !strings.HasPrefix(wantPending, wantPrefix) {
				t.Fatalf("published pending=%q want prefix=%q", wantPending, wantPrefix)
			}
			consumer := &UserProfileConsumer{problem: builder, profileTask: profileTask}
			for attempt := 0; attempt < 4; attempt++ {
				if err := consumer.handle(body); err == nil {
					t.Fatalf("build attempt %d unexpectedly succeeded", attempt+1)
				}
				if got := rdb.Get(t.Context(), pendingKey).Val(); got != wantPending {
					t.Fatalf("build attempt %d changed pending=%q want=%q", attempt+1, got, wantPending)
				}
			}
			if err := consumer.handleExhausted(body); err != nil {
				t.Fatalf("exhausted cleanup: %v", err)
			}
			if got := rdb.Exists(t.Context(), pendingKey).Val(); got != 0 {
				t.Fatalf("exhausted %s event retained pending key", tc.name)
			}
			if result := enqueue(); !result.Published || result.Deduped || result.Failed {
				t.Fatalf("reenqueue after exhausted cleanup=%+v", result)
			}
		})
	}
}

func TestUserProfileConsumerSuccessfulOrdinaryClearsPendingAndAllowsReenqueue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
	}{
		{name: "normal"},
		{name: "force", force: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rdb := profileTestRedis(t)
			publisher := &serviceProfilePublisher{}
			profileTask := profiletask.NewUserProfileTaskWithPublisher(publisher, rdb)
			const userID = int64(910)
			enqueue := func() profiletask.UserProfileEnqueueResult {
				if tc.force {
					return profileTask.DoForce(userID)
				}
				return profileTask.Do(userID)
			}
			if result := enqueue(); !result.Published {
				t.Fatalf("initial enqueue=%+v", result)
			}
			body, err := json.Marshal(recordedServiceProfileEvent(t, publisher, 0))
			if err != nil {
				t.Fatal(err)
			}
			consumer := &UserProfileConsumer{problem: &flakyMaintenanceProfileBuilder{}, profileTask: profileTask}
			if err := consumer.handle(body); err != nil {
				t.Fatal(err)
			}
			pendingKey := fmt.Sprintf("user_profile:pending:%d", userID)
			if got := rdb.Exists(t.Context(), pendingKey).Val(); got != 0 {
				t.Fatalf("successful %s event retained pending", tc.name)
			}
			if result := enqueue(); !result.Published || result.Deduped || result.Failed {
				t.Fatalf("reenqueue after successful cleanup=%+v", result)
			}
		})
	}
}

func TestUserProfileConsumerExhaustedOrdinaryReturnsPendingCleanupError(t *testing.T) {
	mr, rdb := profileTestRedis(t)
	publisher := &serviceProfilePublisher{}
	profileTask := profiletask.NewUserProfileTaskWithPublisher(publisher, rdb)
	const userID = int64(902)
	if result := profileTask.DoForce(userID); !result.Published {
		t.Fatalf("initial force enqueue=%+v", result)
	}
	if err := rdb.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(recordedServiceProfileEvent(t, publisher, 0))
	if err != nil {
		t.Fatal(err)
	}
	consumer := &UserProfileConsumer{profileTask: profileTask}
	if err := consumer.handleExhausted(body); err == nil {
		t.Fatal("pending cleanup failure must keep the exhausted MQ message retryable")
	}
	if pendingKey := fmt.Sprintf("user_profile:pending:%d", userID); !mr.Exists(pendingKey) {
		t.Fatal("failed pending cleanup removed the recovery claim")
	}
}

func TestUserProfileConsumerSuccessfulBuildReturnsPendingCleanupError(t *testing.T) {
	mr, rdb := profileTestRedis(t)
	publisher := &serviceProfilePublisher{}
	profileTask := profiletask.NewUserProfileTaskWithPublisher(publisher, rdb)
	const userID = int64(904)
	if result := profileTask.DoForce(userID); !result.Published {
		t.Fatalf("initial force enqueue=%+v", result)
	}
	if err := rdb.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(recordedServiceProfileEvent(t, publisher, 0))
	if err != nil {
		t.Fatal(err)
	}
	builder := &flakyMaintenanceProfileBuilder{}
	consumer := &UserProfileConsumer{problem: builder, profileTask: profileTask}
	if err := consumer.handle(body); err == nil {
		t.Fatal("successful build with failed pending cleanup must keep the MQ message retryable")
	}
	if builder.builds != 1 {
		t.Fatalf("builds=%d want=1", builder.builds)
	}
	if pendingKey := fmt.Sprintf("user_profile:pending:%d", userID); !mr.Exists(pendingKey) {
		t.Fatal("failed pending cleanup removed the recovery claim")
	}
}

func TestUserProfileConsumerOldDeliveryDoesNotClearNewClaim(t *testing.T) {
	for _, newClaim := range []string{"force:publishing:new-claim", "force:published:new-claim"} {
		t.Run(newClaim, func(t *testing.T) {
			_, rdb := profileTestRedis(t)
			profileTask := profiletask.NewUserProfileTaskWithPublisher(&serviceProfilePublisher{}, rdb)
			const userID = int64(905)
			pendingKey := fmt.Sprintf("user_profile:pending:%d", userID)
			if err := rdb.Set(t.Context(), pendingKey, newClaim, 0).Err(); err != nil {
				t.Fatal(err)
			}
			consumer := &UserProfileConsumer{profileTask: profileTask}
			body := []byte(`{"user_id":905,"force":true,"claim_token":"expired-old-claim"}`)
			if err := consumer.handleExhausted(body); err != nil {
				t.Fatalf("non-owner cleanup must be an idempotent success: %v", err)
			}
			if got := rdb.Get(t.Context(), pendingKey).Val(); got != newClaim {
				t.Fatalf("old delivery cleared new claim: got=%q want=%q", got, newClaim)
			}
		})
	}
}

func TestUserProfileConsumerOldNormalDeliveryDoesNotClearForceUpgrade(t *testing.T) {
	_, rdb := profileTestRedis(t)
	publisher := &serviceProfilePublisher{}
	profileTask := profiletask.NewUserProfileTaskWithPublisher(publisher, rdb)
	const userID = int64(906)
	if result := profileTask.Do(userID); !result.Published {
		t.Fatalf("normal enqueue=%+v", result)
	}
	oldNormalBody, err := json.Marshal(recordedServiceProfileEvent(t, publisher, 0))
	if err != nil {
		t.Fatal(err)
	}
	if result := profileTask.DoForce(userID); !result.Published {
		t.Fatalf("force upgrade=%+v", result)
	}
	pendingKey := fmt.Sprintf("user_profile:pending:%d", userID)
	forceClaim := rdb.Get(t.Context(), pendingKey).Val()
	if !strings.HasPrefix(forceClaim, "force:published") {
		t.Fatalf("force upgrade pending=%q", forceClaim)
	}
	consumer := &UserProfileConsumer{profileTask: profileTask}
	if err := consumer.handleExhausted(oldNormalBody); err != nil {
		t.Fatal(err)
	}
	if got := rdb.Get(t.Context(), pendingKey).Val(); got != forceClaim {
		t.Fatalf("old normal delivery cleared force upgrade: got=%q want=%q", got, forceClaim)
	}
}

type consumeDuringPublishProfilePublisher struct {
	rdb       *redis.Client
	userID    int64
	consume   func([]byte) error
	observed  string
	publishes int
}

func (*consumeDuringPublishProfilePublisher) QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error) {
	return amqp.Queue{}, nil
}

func (p *consumeDuringPublishProfilePublisher) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	p.publishes++
	p.observed = p.rdb.Get(context.Background(), fmt.Sprintf("user_profile:pending:%d", p.userID)).Val()
	return p.consume(msg.Body)
}

func TestUserProfileConsumerCanClearTentativeClaimBeforePublisherFinalizes(t *testing.T) {
	_, rdb := profileTestRedis(t)
	const userID = int64(907)
	publisher := &consumeDuringPublishProfilePublisher{rdb: rdb, userID: userID}
	profileTask := profiletask.NewUserProfileTaskWithPublisher(publisher, rdb)
	consumer := &UserProfileConsumer{
		problem:     &flakyMaintenanceProfileBuilder{},
		profileTask: profileTask,
	}
	publisher.consume = consumer.handle

	if result := profileTask.DoForce(userID); !result.Published || result.Deduped || result.Failed {
		t.Fatalf("enqueue consumed before publish finalization=%+v", result)
	}
	if publisher.publishes != 1 || !strings.HasPrefix(publisher.observed, "force:publishing:") {
		t.Fatalf("publisher observed pending=%q publishes=%d", publisher.observed, publisher.publishes)
	}
	if got := rdb.Exists(t.Context(), fmt.Sprintf("user_profile:pending:%d", userID)).Val(); got != 0 {
		t.Fatal("consumer did not clear its tentative claim before publish finalization")
	}
}

func TestUserProfileConsumerLegacyDeliveryClearsOnlyMatchingLegacyState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		force   bool
		state   string
		cleared bool
	}{
		{name: "normal match", state: "normal:published", cleared: true},
		{name: "force match", force: true, state: "force:published", cleared: true},
		{name: "normal must not clear force", state: "force:published"},
		{name: "force must not clear normal", force: true, state: "normal:published"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rdb := profileTestRedis(t)
			profileTask := profiletask.NewUserProfileTaskWithPublisher(&serviceProfilePublisher{}, rdb)
			const userID = int64(908)
			pendingKey := fmt.Sprintf("user_profile:pending:%d", userID)
			if err := rdb.Set(t.Context(), pendingKey, tc.state, 0).Err(); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(event.UserProfileEvent{UserId: userID, Force: tc.force})
			if err != nil {
				t.Fatal(err)
			}
			consumer := &UserProfileConsumer{profileTask: profileTask}
			if err := consumer.handleExhausted(body); err != nil {
				t.Fatal(err)
			}
			if tc.cleared {
				if got := rdb.Exists(t.Context(), pendingKey).Val(); got != 0 {
					t.Fatalf("matching legacy state was not cleared: %q", tc.state)
				}
			} else if got := rdb.Get(t.Context(), pendingKey).Val(); got != tc.state {
				t.Fatalf("mismatched legacy state changed: got=%q want=%q", got, tc.state)
			}
		})
	}
}

type exhaustedMaintenanceProfileBuilder struct {
	intentID string
	userID   int64
	marks    int
	builds   int
	confirms int
}

func (b *exhaustedMaintenanceProfileBuilder) BuildAndCacheUserProfile(int64, bool) error {
	b.builds++
	return nil
}

func (b *exhaustedMaintenanceProfileBuilder) ConfirmAbilityMaintenanceTarget(context.Context, string, int64) error {
	b.confirms++
	return nil
}

func (b *exhaustedMaintenanceProfileBuilder) MarkAbilityMaintenanceTargetDue(_ context.Context, intentID string, userID int64) error {
	b.intentID = intentID
	b.userID = userID
	b.marks++
	return nil
}

func TestUserProfileConsumerExhaustedMaintenanceDoesNotClearOrdinaryPending(t *testing.T) {
	_, rdb := profileTestRedis(t)
	publisher := &serviceProfilePublisher{}
	profileTask := profiletask.NewUserProfileTaskWithPublisher(publisher, rdb)
	const userID = int64(903)
	if result := profileTask.DoForce(userID); !result.Published {
		t.Fatalf("initial force enqueue=%+v", result)
	}
	pendingKey := fmt.Sprintf("user_profile:pending:%d", userID)
	wantPending := rdb.Get(t.Context(), pendingKey).Val()
	builder := &exhaustedMaintenanceProfileBuilder{}
	consumer := &UserProfileConsumer{problem: builder, profileTask: profileTask}
	body, err := json.Marshal(event.UserProfileEvent{UserId: userID, Force: true, IntentID: "maintenance-903"})
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.handle(body); err != nil {
		t.Fatal(err)
	}
	if got := rdb.Get(t.Context(), pendingKey).Val(); got != wantPending {
		t.Fatalf("maintenance success changed ordinary pending=%q want=%q", got, wantPending)
	}
	if err := consumer.handleExhausted(body); err != nil {
		t.Fatal(err)
	}
	if got := rdb.Get(t.Context(), pendingKey).Val(); got != wantPending {
		t.Fatalf("maintenance exhaustion changed ordinary pending=%q want=%q", got, wantPending)
	}
	if builder.marks != 1 || builder.intentID != "maintenance-903" || builder.userID != userID {
		t.Fatalf("maintenance recovery marks=%d intent=%q user=%d", builder.marks, builder.intentID, builder.userID)
	}
}

func TestUserProfileConsumerMaintenanceDoesNotRequireOrdinaryPendingDependency(t *testing.T) {
	builder := &exhaustedMaintenanceProfileBuilder{}
	consumer := &UserProfileConsumer{problem: builder}
	body := []byte(`{"user_id":909,"force":true,"intent_id":"maintenance-909"}`)
	if err := consumer.handle(body); err != nil {
		t.Fatalf("maintenance handle without profile task: %v", err)
	}
	if err := consumer.handleExhausted(body); err != nil {
		t.Fatalf("maintenance exhaustion without profile task: %v", err)
	}
	if builder.builds != 1 || builder.confirms != 1 || builder.marks != 1 {
		t.Fatalf("maintenance calls builds=%d confirms=%d marks=%d", builder.builds, builder.confirms, builder.marks)
	}
}

type flakyMaintenanceProfileBuilder struct {
	confirm  *ProblemUseCase
	failures int
	builds   int
	confirms int
}

func (b *flakyMaintenanceProfileBuilder) BuildAndCacheUserProfile(_ int64, _ bool) error {
	b.builds++
	if b.builds <= b.failures {
		return errors.New("forced profile build failure")
	}
	return nil
}

func (b *flakyMaintenanceProfileBuilder) ConfirmAbilityMaintenanceTarget(ctx context.Context, intentID string, userID int64) error {
	b.confirms++
	return b.confirm.ConfirmAbilityMaintenanceTarget(ctx, intentID, userID)
}

func (b *flakyMaintenanceProfileBuilder) MarkAbilityMaintenanceTargetDue(ctx context.Context, intentID string, userID int64) error {
	return b.confirm.MarkAbilityMaintenanceTargetDue(ctx, intentID, userID)
}
