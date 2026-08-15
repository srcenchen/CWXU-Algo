package task

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cwxu-algo/app/common/event"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
)

const summaryPendingTTL = 20 * time.Minute

type SummaryTask struct {
	mq  *event.RabbitMQ
	rdb *redis.Client
}

func NewSummaryTask(mq *event.RabbitMQ, rdb *redis.Client) *SummaryTask {
	return &SummaryTask{mq: mq, rdb: rdb}
}

func summaryPendingKey(userId int64, typ string) string {
	return fmt.Sprintf("summary:pending:%s:%d", typ, userId)
}

// SummaryEnqueueResult 总结入队结果
type SummaryEnqueueResult struct {
	Published bool
	Deduped   bool
	Failed    bool
}

// KeepClaim 成功入队或已在途时保留周期 claim
func (r SummaryEnqueueResult) KeepClaim() bool {
	return r.Published || r.Deduped
}

// Do 入队 AI 总结任务。返回是否 published / dedup / failed。
func (t *SummaryTask) Do(userId int64, typ string) SummaryEnqueueResult {
	if t.mq == nil {
		log.Errorf("SummaryTask: mq not ready")
		return SummaryEnqueueResult{Failed: true}
	}
	if t.rdb != nil {
		ctx := context.Background()
		ok, err := t.rdb.SetNX(ctx, summaryPendingKey(userId, typ), "1", summaryPendingTTL).Result()
		if err == nil && !ok {
			log.Debugf("SummaryTask: dedup skip user=%d type=%s", userId, typ)
			return SummaryEnqueueResult{Deduped: true}
		}
	}
	queue := event.SummaryMailQueue
	if _, err := t.mq.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		log.Errorf("SummaryTask: QueueDeclare %s failed: %v", queue, err)
		t.clearPending(userId, typ)
		return SummaryEnqueueResult{Failed: true}
	}
	e := event.SummaryEvent{UserId: userId, Type: typ}
	body, err := json.Marshal(e)
	if err != nil {
		log.Errorf("SummaryTask: json.Marshal failed: %v", err)
		t.clearPending(userId, typ)
		return SummaryEnqueueResult{Failed: true}
	}
	if err := t.mq.Publish("", queue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
	}); err != nil {
		log.Errorf("SummaryTask: Publish %s failed: %v", queue, err)
		t.clearPending(userId, typ)
		return SummaryEnqueueResult{Failed: true}
	}
	return SummaryEnqueueResult{Published: true}
}

func (t *SummaryTask) clearPending(userId int64, typ string) {
	if t.rdb == nil {
		return
	}
	_ = t.rdb.Del(context.Background(), summaryPendingKey(userId, typ)).Err()
}
