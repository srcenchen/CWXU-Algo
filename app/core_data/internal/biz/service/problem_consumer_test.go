package service

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/streadway/amqp"
)

func TestProblemFetchConsumerEarlyPauseIgnoresMessagePlatform(t *testing.T) {
	pipelineControl.SetFetchPaused(false)
	t.Cleanup(func() { pipelineControl.SetFetchPaused(false) })

	if problemFetchConsumerPaused() {
		t.Fatal("全局爬取未暂停时 consumer 不应提前拦截消息，平台暂停交给 ProcessFetch 按 DB 平台判断")
	}
}

func TestClassifyProblemFetchPauseSeparatesGlobalAndPlatform(t *testing.T) {
	if got := classifyProblemFetchPause(errProblemFetchPaused); got != problemFetchPauseGlobal {
		t.Fatalf("全局暂停分类错误: %v", got)
	}
	if got := classifyProblemFetchPause(errProblemPlatformPaused); got != problemFetchPausePlatform {
		t.Fatalf("平台暂停分类错误: %v", got)
	}
	if got := classifyProblemFetchPause(errors.New("fetch failed")); got != problemFetchPauseNone {
		t.Fatalf("普通错误不应归为暂停: %v", got)
	}
}

type fakeProblemFetchPausedBroker struct {
	declareName    string
	declareDurable bool
	declareArgs    amqp.Table
	publishKey     string
	publishing     amqp.Publishing
	declareErr     error
	publishErr     error
}

type fakeProblemFetchAcknowledgement struct {
	acked       bool
	nacked      bool
	nackRequeue bool
}

func (f *fakeProblemFetchAcknowledgement) Ack(_ uint64, _ bool) error {
	f.acked = true
	return nil
}

func (f *fakeProblemFetchAcknowledgement) Nack(_ uint64, _ bool, requeue bool) error {
	f.nacked = true
	f.nackRequeue = requeue
	return nil
}

func (f *fakeProblemFetchAcknowledgement) Reject(_ uint64, _ bool) error {
	return nil
}

func (f *fakeProblemFetchPausedBroker) QueueDeclare(name string, durable, _, _, _ bool, args amqp.Table) (amqp.Queue, error) {
	f.declareName = name
	f.declareDurable = durable
	f.declareArgs = args
	return amqp.Queue{Name: name}, f.declareErr
}

func (f *fakeProblemFetchPausedBroker) Publish(_, key string, _, _ bool, publishing amqp.Publishing) error {
	f.publishKey = key
	f.publishing = publishing
	return f.publishErr
}

func TestPublishProblemFetchPausedDeclaresDurableBrokerDelayQueue(t *testing.T) {
	fake := &fakeProblemFetchPausedBroker{}
	delivery := amqp.Delivery{
		Headers:         amqp.Table{"trace": "abc"},
		ContentType:     "application/json",
		ContentEncoding: "utf-8",
		DeliveryMode:    amqp.Transient,
		Priority:        7,
		CorrelationId:   "correlation",
		ReplyTo:         "reply",
		Expiration:      "1234",
		MessageId:       "message",
		Timestamp:       time.Unix(123, 0),
		Type:            "problem",
		UserId:          "user",
		AppId:           "app",
		Body:            []byte(`{"problemId":1}`),
	}

	if err := publishProblemFetchPaused(fake, delivery); err != nil {
		t.Fatalf("publishProblemFetchPaused() error = %v", err)
	}
	if fake.declareName != problemFetchPausedDelayQueue || !fake.declareDurable {
		t.Fatalf("delay queue declaration = name %q durable %v", fake.declareName, fake.declareDurable)
	}
	wantArgs := amqp.Table{
		"x-message-ttl":             int32(30000),
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": problemFetchQueue,
	}
	if !reflect.DeepEqual(fake.declareArgs, wantArgs) {
		t.Fatalf("delay queue args = %#v, want %#v", fake.declareArgs, wantArgs)
	}
	if _, exists := fake.declareArgs["x-max-priority"]; exists {
		t.Fatal("delay queue must not declare x-max-priority")
	}
	if fake.publishKey != problemFetchPausedDelayQueue {
		t.Fatalf("publish routing key = %q", fake.publishKey)
	}
	wantPublishing := problemFetchPausedPublishing(delivery)
	if !reflect.DeepEqual(fake.publishing, wantPublishing) {
		t.Fatalf("publishing = %#v, want %#v", fake.publishing, wantPublishing)
	}
}

func TestPublishProblemFetchPausedDoesNotPublishWhenDeclareFails(t *testing.T) {
	fake := &fakeProblemFetchPausedBroker{declareErr: errors.New("declare failed")}

	if err := publishProblemFetchPaused(fake, amqp.Delivery{Body: []byte("body")}); err == nil {
		t.Fatal("declare failure must be returned")
	}
	if fake.publishKey != "" {
		t.Fatal("must not publish after declaration failure")
	}
}

func TestSettleProblemFetchPausedAcksOnlyAfterConfirmedPublish(t *testing.T) {
	fakeBroker := &fakeProblemFetchPausedBroker{}
	fakeAck := &fakeProblemFetchAcknowledgement{}
	delivery := amqp.Delivery{Acknowledger: fakeAck, DeliveryTag: 10}

	settleProblemFetchPaused(fakeBroker, delivery)

	if !fakeAck.acked || fakeAck.nacked {
		t.Fatalf("confirmed publish must Ack only: ack=%v nack=%v", fakeAck.acked, fakeAck.nacked)
	}
}

func TestSettleProblemFetchPausedNacksAndRequeuesOnBrokerFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		declareErr error
		publishErr error
	}{
		{name: "declare", declareErr: errors.New("declare failed")},
		{name: "publish", publishErr: errors.New("publish failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeBroker := &fakeProblemFetchPausedBroker{
				declareErr: test.declareErr,
				publishErr: test.publishErr,
			}
			fakeAck := &fakeProblemFetchAcknowledgement{}
			delivery := amqp.Delivery{Acknowledger: fakeAck, DeliveryTag: 10}

			settleProblemFetchPaused(fakeBroker, delivery)

			if fakeAck.acked || !fakeAck.nacked || !fakeAck.nackRequeue {
				t.Fatalf("broker failure must Nack(false, true): ack=%v nack=%v requeue=%v", fakeAck.acked, fakeAck.nacked, fakeAck.nackRequeue)
			}
		})
	}
}
