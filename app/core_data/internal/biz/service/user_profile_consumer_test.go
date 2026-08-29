package service

import (
	"context"
	"errors"
	"testing"
)

func TestUserProfileConsumerExhaustedPoisonAndOrdinaryMessagesDoNotBlockDrop(t *testing.T) {
	consumer := &UserProfileConsumer{}
	if err := consumer.handleExhausted([]byte("{")); err != nil {
		t.Fatalf("bad JSON must remain droppable after retries: %v", err)
	}
	if err := consumer.handleExhausted([]byte(`{"user_id":9,"force":true}`)); err != nil {
		t.Fatalf("ordinary event must not require durable maintenance retry: %v", err)
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
