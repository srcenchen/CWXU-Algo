package task

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"cwxu-algo/app/common/event"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
)

const (
	userProfileQueue       = "user_profile"
	userProfilePendingTTL  = 30 * time.Minute
	userProfilePendingPref = "user_profile:pending:"
)

// UserProfileTask 画像预计算入队（去重 + 持久化 MQ）
type UserProfileTask struct {
	mq         *event.RabbitMQ
	rdb        *redis.Client
	queueReady atomic.Bool
}

func NewUserProfileTask(mq *event.RabbitMQ, rdb *redis.Client) *UserProfileTask {
	return &UserProfileTask{mq: mq, rdb: rdb}
}

func (t *UserProfileTask) ensureQueue() {
	if t.queueReady.Load() {
		return
	}
	if t.mq == nil {
		return
	}
	if _, err := t.mq.QueueDeclare(userProfileQueue, true, false, false, false, nil); err != nil {
		log.Warnf("UserProfileTask: QueueDeclare: %v", err)
		return
	}
	t.queueReady.Store(true)
}

func userProfilePendingKey(userID int64) string {
	return fmt.Sprintf("%s%d", userProfilePendingPref, userID)
}

// EnqueueResult 单次入队结果
type UserProfileEnqueueResult struct {
	Published bool
	Deduped   bool
	Failed    bool
}

func (r UserProfileEnqueueResult) KeepClaim() bool {
	return r.Published || r.Deduped
}

// Do 为用户入队画像重建；已在途则 dedup
func (t *UserProfileTask) Do(userID int64) UserProfileEnqueueResult {
	return t.do(userID, false)
}

// DoForce 强制重建（每日全量 / 空雷达补刷），跳过指纹去重
func (t *UserProfileTask) DoForce(userID int64) UserProfileEnqueueResult {
	return t.do(userID, true)
}

func (t *UserProfileTask) do(userID int64, force bool) UserProfileEnqueueResult {
	if userID <= 0 || t.mq == nil {
		return UserProfileEnqueueResult{Failed: true}
	}
	if t.rdb != nil {
		ok, err := t.rdb.SetNX(context.Background(), userProfilePendingKey(userID), "1", userProfilePendingTTL).Result()
		if err != nil {
			log.Warnf("UserProfileTask: pending SetNX user=%d: %v", userID, err)
			// Redis 故障仍尝试入队
		} else if !ok {
			return UserProfileEnqueueResult{Deduped: true}
		}
	}
	t.ensureQueue()
	body, err := json.Marshal(event.UserProfileEvent{UserId: userID, Force: force})
	if err != nil {
		t.clearPending(userID)
		return UserProfileEnqueueResult{Failed: true}
	}
	if err := t.mq.Publish("", userProfileQueue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
	}); err != nil {
		log.Errorf("UserProfileTask: Publish user=%d: %v", userID, err)
		t.queueReady.Store(false)
		t.clearPending(userID)
		return UserProfileEnqueueResult{Failed: true}
	}
	return UserProfileEnqueueResult{Published: true}
}

// ClearPending consumer 成功/失败后释放，允许再次入队
func (t *UserProfileTask) ClearPending(userID int64) {
	t.clearPending(userID)
}

func (t *UserProfileTask) clearPending(userID int64) {
	if t.rdb == nil || userID <= 0 {
		return
	}
	_ = t.rdb.Del(context.Background(), userProfilePendingKey(userID)).Err()
}

// DoBatch 批量入队（cron 预热）；force=true 强制重建（跳过指纹）。返回 published 数
func (t *UserProfileTask) DoBatch(userIDs []int64, force bool) (published, deduped, failed int) {
	for _, uid := range userIDs {
		r := t.do(uid, force)
		switch {
		case r.Published:
			published++
		case r.Deduped:
			deduped++
		default:
			failed++
		}
	}
	return
}
