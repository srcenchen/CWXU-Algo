package mqconsume

import (
	"errors"
	"testing"
	"time"

	"github.com/streadway/amqp"
)

type recordingAcknowledger struct {
	acks    int
	nacks   int
	requeue bool
}

func (a *recordingAcknowledger) Ack(uint64, bool) error { a.acks++; return nil }
func (a *recordingAcknowledger) Nack(_ uint64, _ bool, requeue bool) error {
	a.nacks++
	a.requeue = requeue
	return nil
}
func (a *recordingAcknowledger) Reject(uint64, bool) error { return nil }

type recordingRetryBroker struct {
	declared  string
	args      amqp.Table
	published amqp.Publishing
	err       error
}

func (b *recordingRetryBroker) QueueDeclare(name string, _ bool, _ bool, _ bool, _ bool, args amqp.Table) (amqp.Queue, error) {
	b.declared, b.args = name, args
	return amqp.Queue{}, b.err
}
func (b *recordingRetryBroker) Publish(_ string, _ string, _ bool, _ bool, msg amqp.Publishing) error {
	b.published = msg
	return b.err
}

func TestHandleFailDropsOnlyAfterExhaustedRecoveryPersists(t *testing.T) {
	ack := &recordingAcknowledger{}
	delivery := amqp.Delivery{Acknowledger: ack, DeliveryTag: 1, Body: []byte(`{"intent_id":"i"}`), Headers: amqp.Table{RetryHeader: int32(3)}}
	called := false
	err := handleFail(nil, Options{MaxRetry: 3, OnExhausted: func(body []byte, _ amqp.Table) error {
		called = string(body) == `{"intent_id":"i"}`
		return nil
	}}, delivery, errors.New("build failed"), false)
	if err != nil || !called || ack.nacks != 1 || ack.requeue {
		t.Fatalf("exhausted success err=%v called=%t nacks=%d requeue=%t", err, called, ack.nacks, ack.requeue)
	}
}

func TestHandleFailRequeuesWhenExhaustedRecoveryCannotPersist(t *testing.T) {
	ack := &recordingAcknowledger{}
	delivery := amqp.Delivery{Acknowledger: ack, DeliveryTag: 1, Headers: amqp.Table{RetryHeader: int32(3)}}
	err := handleFail(nil, Options{MaxRetry: 3, OnExhausted: func([]byte, amqp.Table) error {
		return errors.New("database unavailable")
	}, Sleep: func(time.Duration) {}}, delivery, errors.New("build failed"), false)
	if err != nil || ack.nacks != 1 || !ack.requeue {
		t.Fatalf("exhausted persistence failure err=%v nacks=%d requeue=%t", err, ack.nacks, ack.requeue)
	}
}

func TestHandleFailBacksOffBeforeEachExhaustedRecoveryRequeue(t *testing.T) {
	ack := &recordingAcknowledger{}
	broker := &recordingRetryBroker{}
	delivery := amqp.Delivery{Acknowledger: ack, DeliveryTag: 1, ContentType: "application/json", MessageId: "id", Body: []byte("body"), Headers: amqp.Table{RetryHeader: int32(3), "keep": "value"}}
	var delays []time.Duration
	opts := Options{
		MaxRetry: 3,
		OnExhausted: func([]byte, amqp.Table) error {
			return errors.New("database unavailable")
		},
		Queue: "user_profile",
		ExhaustedRetryBackoff: func(retry int) time.Duration {
			delays = append(delays, 37*time.Millisecond)
			return 37 * time.Millisecond
		},
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := handleFail(broker, opts, delivery, errors.New("build failed"), false); err != nil {
			t.Fatal(err)
		}
	}
	if ack.acks != 3 || ack.nacks != 0 || len(delays) != 3 {
		t.Fatalf("acks=%d nacks=%d delays=%v", ack.acks, ack.nacks, delays)
	}
	for _, delay := range delays {
		if delay != 37*time.Millisecond {
			t.Fatalf("delay=%s", delay)
		}
	}
	if broker.declared != "user_profile.exhausted.37ms" || broker.args["x-dead-letter-routing-key"] != "user_profile" || broker.published.MessageId != "id" || broker.published.Headers["keep"] != "value" || broker.published.Headers[exhaustedRetryHeader] != 1 {
		t.Fatalf("delay message lost broker contract queue=%q args=%+v msg=%+v", broker.declared, broker.args, broker.published)
	}
}

func TestExhaustedRetryBackoffIsBoundedExponential(t *testing.T) {
	for attempts, want := range map[int]time.Duration{0: 250 * time.Millisecond, 1: 500 * time.Millisecond, 2: time.Second, 3: 2 * time.Second, 9: 2 * time.Second} {
		if got := exhaustedRetryBackoff(Options{}, attempts); got != want {
			t.Fatalf("attempts=%d delay=%s want=%s", attempts, got, want)
		}
	}
}
