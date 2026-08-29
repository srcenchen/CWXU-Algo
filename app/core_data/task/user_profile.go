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
	userProfileQueue         = "user_profile"
	userProfilePendingTTL    = 30 * time.Minute
	userProfilePendingPref   = "user_profile:pending:"
	userProfilePendingNormal = "normal"
	userProfilePendingForce  = "force"
)

var userProfileClaimScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
local desired = ARGV[1]
local tentative = desired .. ':publishing:' .. ARGV[3]
if not current then
  redis.call('PSETEX', KEYS[1], ARGV[2], tentative)
  return 1
end
if desired == 'force' then
  if current == 'force' or current == 'force:published' or string.find(current, 'force:published:', 1, true) == 1 then
    return 0
  end
  redis.call('PSETEX', KEYS[1], ARGV[2], tentative)
  return 2
end
if desired == 'normal' and string.find(current, 'normal:publishing:', 1, true) == 1 then
  redis.call('PSETEX', KEYS[1], ARGV[2], tentative)
  return 2
end
if desired == 'normal' and string.find(current, 'force:publishing:', 1, true) == 1 then
  redis.call('PSETEX', KEYS[1], ARGV[2], 'force:publishing:' .. ARGV[3])
  return 3
end
return 0
`)

var userProfileReleaseClaimScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

var userProfilePublishClaimScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PSETEX', KEYS[1], ARGV[3], ARGV[2])
  return 1
end
return 0
`)

var userProfileClearClaimScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 0 end
local state = ARGV[1]
local token = ARGV[2]
if token == '' then
  if current == state .. ':published' then
    return redis.call('DEL', KEYS[1])
  end
  return 0
end
if current == state .. ':publishing:' .. token or current == state .. ':published:' .. token then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

var userProfileClaimSeq atomic.Uint64

// UserProfileTask 画像预计算入队（去重 + 持久化 MQ）
type UserProfileTask struct {
	mq         userProfilePublisher
	rdb        *redis.Client
	queueReady atomic.Bool
}

type userProfilePublisher interface {
	QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error)
	Publish(string, string, bool, bool, amqp.Publishing) error
}

func NewUserProfileTask(mq *event.RabbitMQ, rdb *redis.Client) *UserProfileTask {
	return NewUserProfileTaskWithPublisher(mq, rdb)
}

// NewUserProfileTaskWithPublisher keeps the queue protocol testable and allows
// alternate publishers without coupling pending-state semantics to RabbitMQ.
func NewUserProfileTaskWithPublisher(mq userProfilePublisher, rdb *redis.Client) *UserProfileTask {
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

// DoMaintenanceForce publishes a durable-maintenance rebuild.  The caller owns
// deduplication and recovery in SQL, so this deliberately does not claim the
// normal Redis pending key: a consumer drop must never suppress a DB retry.
func (t *UserProfileTask) DoMaintenanceForce(userID int64, intentID string) UserProfileEnqueueResult {
	if userID <= 0 || intentID == "" || t.mq == nil {
		return UserProfileEnqueueResult{Failed: true}
	}
	t.ensureQueue()
	body, err := json.Marshal(event.UserProfileEvent{UserId: userID, Force: true, IntentID: intentID})
	if err != nil {
		return UserProfileEnqueueResult{Failed: true}
	}
	if err := t.mq.Publish("", userProfileQueue, false, false, amqp.Publishing{
		ContentType: "application/json", Body: body, DeliveryMode: amqp.Persistent,
	}); err != nil {
		log.Errorf("UserProfileTask: maintenance publish user=%d intent=%s: %v", userID, intentID, err)
		t.queueReady.Store(false)
		return UserProfileEnqueueResult{Failed: true}
	}
	return UserProfileEnqueueResult{Published: true}
}

func (t *UserProfileTask) do(userID int64, force bool) UserProfileEnqueueResult {
	if userID <= 0 || t.mq == nil {
		return UserProfileEnqueueResult{Failed: true}
	}
	pendingState := userProfilePendingNormal
	if force {
		pendingState = userProfilePendingForce
	}
	publishForce := force
	claimToken := fmt.Sprintf("%d-%d", time.Now().UnixNano(), userProfileClaimSeq.Add(1))
	if t.rdb != nil {
		claim, err := userProfileClaimScript.Run(
			context.Background(), t.rdb, []string{userProfilePendingKey(userID)},
			pendingState, userProfilePendingTTL.Milliseconds(), claimToken,
		).Int64()
		if err != nil {
			log.Warnf("UserProfileTask: pending claim user=%d: %v", userID, err)
			// Redis 故障仍尝试入队
		} else if claim == 0 {
			return UserProfileEnqueueResult{Deduped: true}
		} else if claim == 3 {
			// A normal request cannot trust a tentative force publish. Take it over
			// as force so a failing earlier publisher cannot lose both requests.
			pendingState = userProfilePendingForce
			publishForce = true
		}
	}
	claimState := pendingState + ":publishing:" + claimToken
	t.ensureQueue()
	body, err := json.Marshal(event.UserProfileEvent{UserId: userID, Force: publishForce, ClaimToken: claimToken})
	if err != nil {
		t.releaseClaim(userID, claimState)
		return UserProfileEnqueueResult{Failed: true}
	}
	if err := t.mq.Publish("", userProfileQueue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
	}); err != nil {
		log.Errorf("UserProfileTask: Publish user=%d: %v", userID, err)
		t.queueReady.Store(false)
		t.releaseClaim(userID, claimState)
		return UserProfileEnqueueResult{Failed: true}
	}
	t.publishClaim(userID, claimState, pendingState+":published:"+claimToken)
	return UserProfileEnqueueResult{Published: true}
}

// ClearPending consumer 成功或最终耗尽后释放，允许再次入队。
// 只释放消息自身的 tentative/published claim；legacy 消息仅匹配旧 published 状态。
// 返回 Redis 错误，以便耗尽恢复在释放失败时保留 MQ 消息；无匹配视为幂等成功。
func (t *UserProfileTask) ClearPending(userID int64, force bool, claimToken string) error {
	if t.rdb == nil || userID <= 0 {
		return nil
	}
	pendingState := userProfilePendingNormal
	if force {
		pendingState = userProfilePendingForce
	}
	return userProfileClearClaimScript.Run(
		context.Background(), t.rdb, []string{userProfilePendingKey(userID)}, pendingState, claimToken,
	).Err()
}

func (t *UserProfileTask) releaseClaim(userID int64, state string) {
	if t.rdb == nil || userID <= 0 || state == "" {
		return
	}
	_ = userProfileReleaseClaimScript.Run(
		context.Background(), t.rdb, []string{userProfilePendingKey(userID)}, state,
	).Err()
}

func (t *UserProfileTask) publishClaim(userID int64, claimState, publishedState string) {
	if t.rdb == nil || userID <= 0 || claimState == "" || publishedState == "" {
		return
	}
	_ = userProfilePublishClaimScript.Run(
		context.Background(), t.rdb, []string{userProfilePendingKey(userID)},
		claimState, publishedState, userProfilePendingTTL.Milliseconds(),
	).Err()
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
