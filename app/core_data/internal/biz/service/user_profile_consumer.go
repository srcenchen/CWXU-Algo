package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/utils/mqconsume"
	"cwxu-algo/app/core_data/internal/data/dal"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

// 画像重 JOIN 限并发，避免拖垮 DB
const userProfileConcurrency = 1

// profileInvalidationRecoveryInterval 孤儿失效围栏的巡检间隔。围栏正常由
// 持有者心跳续约；进程崩溃/重启会遗留奇数 generation + 无租约，需尽快重开，
// 否则该用户（或全局）的画像重建会持续失败。
const profileInvalidationRecoveryInterval = 2 * time.Minute

type userProfileBuilder interface {
	BuildAndCacheUserProfile(int64, bool) error
	ConfirmAbilityMaintenanceTarget(context.Context, string, int64) error
	MarkAbilityMaintenanceTargetDue(context.Context, string, int64) error
}

// userProfileInvalidationRecoverer 可选能力：由真实 ProblemUseCase 实现。
// 用接口断言而非扩展 userProfileBuilder，避免测试桩被迫实现。
type userProfileInvalidationRecoverer interface {
	RecoverOrphanedProfileInvalidations(context.Context) error
}

// isTransientProfileBuildError 判断画像构建失败是否属于“等依赖就绪后重试即可”
// 的瞬态协作状态（失效围栏进行中、题库异步打标未完成、模型/证据正在切换）。
// 这类失败不应按 ERROR 处理，也不该烧掉 MQ 重试次数。
func isTransientProfileBuildError(err error) bool {
	switch {
	case errors.Is(err, ErrUserProfileInvalidationInProgress),
		errors.Is(err, dal.ErrUserTagAbilityIncomplete),
		errors.Is(err, dal.ErrUserTagAbilityModelChanged),
		errors.Is(err, dal.ErrUserTagAbilityEvidenceChanged):
		return true
	default:
		return false
	}
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
	// 启动即修复上次进程遗留的孤儿围栏，之后周期巡检。
	c.recoverOrphanedInvalidations()
	go c.runInvalidationRecovery()
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

func (c *UserProfileConsumer) recoverOrphanedInvalidations() {
	recoverer, ok := c.problem.(userProfileInvalidationRecoverer)
	if !ok {
		return
	}
	if err := recoverer.RecoverOrphanedProfileInvalidations(context.Background()); err != nil {
		log.Warnf("user_profile invalidation recovery: %v", err)
	}
}

func (c *UserProfileConsumer) runInvalidationRecovery() {
	ticker := time.NewTicker(profileInvalidationRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			c.recoverOrphanedInvalidations()
		}
	}
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
		if isTransientProfileBuildError(err) {
			// 依赖尚未就绪（失效围栏/异步打标/模型切换）：不做 ERROR 与
			// 无效重试，直接按兜底路径重新排队（普通事件清 pending、维护
			// 意图标记 due），依赖就绪后由维护队列重建。
			log.Warnf("user_profile deferred user=%d: %v", msg.UserId, err)
			return c.handleExhausted(body)
		}
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
