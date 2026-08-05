package service

import (
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

// UserProfileConsumer 消费 user_profile 队列，预计算写入 Redis
type UserProfileConsumer struct {
	mq           *event.RabbitMQ
	problem      *ProblemUseCase
	profileTask  *task.UserProfileTask
	stopCh       chan struct{}
	stopOnce     sync.Once
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
		Handler: func(body []byte, _ amqp.Table) error {
			msg := event.UserProfileEvent{}
			if err := json.Unmarshal(body, &msg); err != nil {
				return fmt.Errorf("bad json: %w", err)
			}
		if msg.UserId <= 0 {
			return nil
		}
		start := time.Now()
		err := c.problem.BuildAndCacheUserProfile(msg.UserId, msg.Force)
			if err != nil {
				log.Errorf("user_profile build user=%d: %v", msg.UserId, err)
				// 失败不释放 pending，避免重试期间重复入队；TTL 到期后可再预热
				return err
			}
			if c.profileTask != nil {
				c.profileTask.ClearPending(msg.UserId)
			}
			log.Infof("user_profile built user=%d cost=%s", msg.UserId, time.Since(start).Round(time.Millisecond))
			return nil
		},
	})
}
