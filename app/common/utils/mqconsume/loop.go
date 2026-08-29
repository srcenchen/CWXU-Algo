package mqconsume

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/common/event"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

const RetryHeader = "x-retry"
const exhaustedRetryHeader = "x-exhausted-retry"

type retryBroker interface {
	QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error)
	Publish(string, string, bool, bool, amqp.Publishing) error
}

// Options 受控消费循环配置。
type Options struct {
	Name        string
	Queue       string
	Concurrency int
	// ConcurrencySource 可选；值变化时停止拉取新消息，排空在途任务后重建 channel/QoS。
	ConcurrencySource func() int
	ReloadInterval    time.Duration
	// MaxRetry 失败后最大重试次数（不含首次）。超过则 drop（Nack requeue=false）。
	MaxRetry int
	// DeclareOnMissing 启动消费前声明持久队列，并在消费失败时再次声明后重试。
	DeclareOnMissing bool
	// Handler 返回 error 则按重试策略处理；nil 则 Ack。
	Handler func(body []byte, headers amqp.Table) error
	// ShouldRequeue 可选：返回 true 表示立即 requeue 不计入重试（如 pipeline pause）。
	ShouldRequeue func(err error) bool
	// OnExhausted 在最后一次失败且消息即将丢弃前运行。返回 error 时原消息会
	// requeue，防止外部持久恢复状态尚未写入就丢失消息。
	OnExhausted func(body []byte, headers amqp.Table) error
	// ExhaustedRetryBackoff controls the bounded delay before requeueing an
	// exhausted message whose durable recovery callback failed.
	ExhaustedRetryBackoff func(retry int) time.Duration
	// Sleep makes retry waits testable. Nil uses time.Sleep.
	Sleep func(time.Duration)
	// Stop 可选：关闭时退出循环。
	Stop <-chan struct{}
}

var errConcurrencyChanged = errors.New("consumer concurrency changed")

// ConcurrencyFromEnv 读取正整数环境变量；空/非法时返回 def（def≤0 时回落为 1）。
// 用于 2c4g 默认低并发，强机可用 CWXU_*_CONCURRENCY 覆盖。
func ConcurrencyFromEnv(key string, def int) int {
	if def <= 0 {
		def = 1
	}
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	if n > 32 {
		return 32
	}
	return n
}

// Run 阻塞直到 channel 关闭或 Stop。每次消息在有限 worker 池中处理。
func Run(mq *event.RabbitMQ, opts Options) error {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	if opts.MaxRetry <= 0 {
		opts.MaxRetry = 3
	}

	for {
		if opts.ConcurrencySource != nil {
			opts.Concurrency = normalizeConcurrency(opts.ConcurrencySource(), opts.Concurrency)
		}
		if opts.Stop != nil {
			select {
			case <-opts.Stop:
				return nil
			default:
			}
		}
		err := runOnce(mq, opts)
		if errors.Is(err, errConcurrencyChanged) {
			continue
		}
		if err == nil {
			// channel closed normally
		} else {
			log.Errorf("%s consumer: %v，5s 后重连", opts.Name, err)
		}
		if opts.Stop != nil {
			select {
			case <-opts.Stop:
				return nil
			case <-time.After(5 * time.Second):
			}
		} else {
			time.Sleep(5 * time.Second)
		}
	}
}

func runOnce(mq *event.RabbitMQ, opts Options) error {
	if opts.DeclareOnMissing {
		if _, err := mq.QueueDeclare(opts.Queue, true, false, false, false, nil); err != nil {
			return err
		}
	}
	ch, err := mq.OpenChannel()
	if err != nil {
		return err
	}
	defer ch.Close()

	if err := ch.Qos(opts.Concurrency, 0, false); err != nil {
		return err
	}
	consumerTag := fmt.Sprintf("%s-%d", opts.Name, time.Now().UnixNano())
	msgs, err := ch.Consume(opts.Queue, consumerTag, false, false, false, false, nil)
	if err != nil {
		if !opts.DeclareOnMissing {
			return err
		}
		_ = ch.Close()
		ch, err = mq.OpenChannel()
		if err != nil {
			return err
		}
		defer ch.Close()
		if _, err := ch.QueueDeclare(opts.Queue, true, false, false, false, nil); err != nil {
			return err
		}
		if err := ch.Qos(opts.Concurrency, 0, false); err != nil {
			return err
		}
		msgs, err = ch.Consume(opts.Queue, consumerTag, false, false, false, false, nil)
		if err != nil {
			return err
		}
	}

	log.Infof("%s consumer 已就绪 concurrency=%d queue=%s", opts.Name, opts.Concurrency, opts.Queue)

	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	var reload <-chan time.Time
	var ticker *time.Ticker
	if opts.ConcurrencySource != nil {
		interval := opts.ReloadInterval
		if interval <= 0 {
			interval = 2 * time.Second
		}
		ticker = time.NewTicker(interval)
		defer ticker.Stop()
		reload = ticker.C
	}

	for {
		select {
		case <-opts.Stop:
			_ = ch.Cancel(consumerTag, false)
			wg.Wait()
			return nil
		case <-reload:
			if normalizeConcurrency(opts.ConcurrencySource(), opts.Concurrency) != opts.Concurrency {
				_ = ch.Cancel(consumerTag, false)
				wg.Wait()
				return errConcurrencyChanged
			}
		case d, ok := <-msgs:
			if !ok {
				wg.Wait()
				return nil
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(d amqp.Delivery) {
				defer wg.Done()
				defer func() { <-sem }()
				defer func() {
					if r := recover(); r != nil {
						log.Errorf("%s: panic recovered: %v", opts.Name, r)
						_ = handleFail(mq, opts, d, nil, true)
					}
				}()
				err := opts.Handler(d.Body, d.Headers)
				if err == nil {
					_ = d.Ack(false)
					return
				}
				if opts.ShouldRequeue != nil && opts.ShouldRequeue(err) {
					time.Sleep(2 * time.Second)
					_ = d.Nack(false, true)
					return
				}
				_ = handleFail(mq, opts, d, err, false)
			}(d)
		}
	}
}

func normalizeConcurrency(value, fallback int) int {
	if value < 1 {
		return fallback
	}
	if value > 32 {
		return 32
	}
	return value
}

func handleFail(mq retryBroker, opts Options, d amqp.Delivery, err error, fromPanic bool) error {
	retry := headerRetry(d.Headers)
	if err != nil {
		log.Errorf("%s fail retry=%d/%d: %v", opts.Name, retry, opts.MaxRetry, err)
	} else if fromPanic {
		log.Errorf("%s panic retry=%d/%d", opts.Name, retry, opts.MaxRetry)
	}
	if retry >= opts.MaxRetry {
		// 超过上限：丢弃，避免 poison 无限循环
		if opts.OnExhausted != nil {
			if exhaustedErr := opts.OnExhausted(d.Body, d.Headers); exhaustedErr != nil {
				log.Errorf("%s exhausted recovery failed, delaying original: %v", opts.Name, exhaustedErr)
				exhaustedRetries := headerRetry(amqp.Table{RetryHeader: d.Headers[exhaustedRetryHeader]})
				delay := exhaustedRetryBackoff(opts, exhaustedRetries)
				delayErr := settleExhaustedRetry(mq, opts.Queue, d, delay)
				if delayErr == nil {
					return d.Ack(false)
				}
				log.Errorf("%s exhausted delay publish failed, requeueing original: %v", opts.Name, delayErr)
				return d.Nack(false, true)
			}
		}
		return d.Nack(false, false)
	}
	// 重新入队并递增重试计数，然后 Ack 原消息
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers[RetryHeader] = retry + 1
	pubErr := mq.Publish("", opts.Queue, false, false, amqp.Publishing{
		ContentType:  d.ContentType,
		Body:         d.Body,
		DeliveryMode: amqp.Persistent,
		Headers:      headers,
	})
	if pubErr != nil {
		log.Errorf("%s requeue publish failed: %v", opts.Name, pubErr)
		return d.Nack(false, true)
	}
	return d.Ack(false)
}

func settleExhaustedRetry(mq retryBroker, queue string, d amqp.Delivery, delay time.Duration) error {
	if mq == nil || queue == "" {
		return fmt.Errorf("exhausted retry broker unavailable")
	}
	if delay <= 0 {
		delay = time.Millisecond
	}
	delayQueue := fmt.Sprintf("%s.exhausted.%dms", queue, delay.Milliseconds())
	args := amqp.Table{
		"x-message-ttl":             int32(delay / time.Millisecond),
		"x-dead-letter-exchange":    "",
		"x-dead-letter-routing-key": queue,
	}
	if _, err := mq.QueueDeclare(delayQueue, true, false, false, false, args); err != nil {
		return err
	}
	headers := amqp.Table{}
	for k, value := range d.Headers {
		headers[k] = value
	}
	headers[exhaustedRetryHeader] = headerRetry(amqp.Table{RetryHeader: headers[exhaustedRetryHeader]}) + 1
	return mq.Publish("", delayQueue, false, false, amqp.Publishing{
		ContentType: d.ContentType, ContentEncoding: d.ContentEncoding, DeliveryMode: amqp.Persistent,
		Priority: d.Priority, CorrelationId: d.CorrelationId, ReplyTo: d.ReplyTo, Expiration: d.Expiration,
		MessageId: d.MessageId, Timestamp: d.Timestamp, Type: d.Type, UserId: d.UserId, AppId: d.AppId,
		Headers: headers, Body: append([]byte(nil), d.Body...),
	})
}

func exhaustedRetryBackoff(opts Options, retry int) time.Duration {
	if opts.ExhaustedRetryBackoff != nil {
		return opts.ExhaustedRetryBackoff(retry)
	}
	// The exhausted-retry header is durable across delay-queue round trips and
	// yields exponential delay (250ms, 500ms, …),
	// capped at two seconds so an unacked Rabbit delivery is never held long.
	delay := 250 * time.Millisecond
	for step := 0; step < retry && delay < 2*time.Second; step++ {
		delay *= 2
	}
	if delay > 2*time.Second {
		return 2 * time.Second
	}
	return delay
}

func headerRetry(h amqp.Table) int {
	if h == nil {
		return 0
	}
	v, ok := h[RetryHeader]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case int:
		return t
	case int32:
		return int(t)
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	case []byte:
		n, _ := strconv.Atoi(string(t))
		return n
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	default:
		return 0
	}
}
