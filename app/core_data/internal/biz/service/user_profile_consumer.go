package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/utils/mqconsume"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

// 画像重 JOIN 限并发，避免拖垮 DB
const userProfileConcurrency = 1

type userProfileBuilder interface {
	BuildAndCacheUserProfile(int64, bool) error
	ConfirmAbilityMaintenanceTarget(context.Context, string, int64) error
	MarkAbilityMaintenanceTargetDue(context.Context, string, int64) error
}

// UserProfileConsumer 消费 user_profile 队列，预计算写入 Redis
type UserProfileConsumer struct {
	mq          *event.RabbitMQ
	problem     userProfileBuilder
	profileTask *task.UserProfileTask
	stopCh      chan struct{}
	stopOnce    sync.Once
}

func NewUserProfileConsumer(mq *event.RabbitMQ, problem *ProblemUseCase, profileTask *task.UserProfileTask) *UserProfileConsumer {
	return &UserProfileConsumer{
		mq:          mq,
		problem:     problem,
		profileTask: profileTask,
		stopCh:      make(chan struct{}),
	}
}

func (c *UserProfileConsumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *UserProfileConsumer) Consume() {
	log.Infof("user_profile consumer 循环启动")
	// 确保队列存在
	if c.mq != nil {
		_, _ = c.mq.QueueDeclare("user_profile", true, false, false, false, nil)
	}
	_ = mqconsume.Run(c.mq, mqconsume.Options{
		Name:             "user_profile",
		Queue:            "user_profile",
		Concurrency:      userProfileConcurrency,
		MaxRetry:         3,
		DeclareOnMissing: true,
		Stop:             c.stopCh,
		Handler:          func(body []byte, _ amqp.Table) error { return c.handle(body) },
		OnExhausted:      func(body []byte, _ amqp.Table) error { return c.handleExhausted(body) },
	})
}

func (c *UserProfileConsumer) handleExhausted(body []byte) error {
	msg := event.UserProfileEvent{}
	if err := json.Unmarshal(body, &msg); err != nil {
		// Poison messages have no reliable intent to recover. Let the consumer
		// drop them after its bounded retries instead of creating a hot loop.
		log.Errorf("user_profile exhausted poison message: %v", err)
		return nil
	}
	if msg.UserId <= 0 {
		return nil
	}
	if msg.IntentID == "" {
		if c.profileTask == nil {
			return fmt.Errorf("user_profile pending dependency unavailable")
		}
		if err := c.profileTask.ClearPending(msg.UserId, msg.Force, msg.ClaimToken); err != nil {
			return fmt.Errorf("user_profile clear exhausted pending user=%d: %w", msg.UserId, err)
		}
		return nil
	}
	if c.problem == nil {
		return fmt.Errorf("user_profile builder dependency unavailable")
	}
	return c.problem.MarkAbilityMaintenanceTargetDue(context.Background(), msg.IntentID, msg.UserId)
}

func (c *UserProfileConsumer) handle(body []byte) error {
	msg := event.UserProfileEvent{}
	if err := json.Unmarshal(body, &msg); err != nil {
		return fmt.Errorf("bad json: %w", err)
	}
	if msg.UserId <= 0 {
		return nil
	}
	if c.problem == nil {
		return fmt.Errorf("user_profile builder dependency unavailable")
	}
	if msg.IntentID == "" && c.profileTask == nil {
		return fmt.Errorf("user_profile pending dependency unavailable")
	}
	start := time.Now()
	if err := c.problem.BuildAndCacheUserProfile(msg.UserId, msg.Force); err != nil {
		log.Errorf("user_profile build user=%d: %v", msg.UserId, err)
		return err
	}
	if msg.IntentID != "" {
		if err := c.problem.ConfirmAbilityMaintenanceTarget(context.Background(), msg.IntentID, msg.UserId); err != nil {
			return fmt.Errorf("user_profile confirm intent=%s user=%d: %w", msg.IntentID, msg.UserId, err)
		}
	} else if c.profileTask != nil {
		if err := c.profileTask.ClearPending(msg.UserId, msg.Force, msg.ClaimToken); err != nil {
			return fmt.Errorf("user_profile clear pending user=%d: %w", msg.UserId, err)
		}
	}
	log.Infof("user_profile built user=%d cost=%s", msg.UserId, time.Since(start).Round(time.Millisecond))
	return nil
}
