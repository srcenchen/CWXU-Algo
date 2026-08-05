package service

import (
	"context"
	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/utils/mqconsume"
	"cwxu-algo/app/core_data/internal/loadgate"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

const (
	// 默认各 4；可用 CWXU_PROBLEM_FETCH_CONCURRENCY / CWXU_PROBLEM_ANALYZE_CONCURRENCY 覆盖
	defaultProblemFetchConcurrency   = 4
	defaultProblemAnalyzeConcurrency = 4
	problemMaxRetry                  = 5
	// problemRetryBaseDelay 重投退避基数：按重试次数指数递增，上限 problemRetryMaxDelay
	problemRetryBaseDelay = 2 * time.Second
	problemRetryMaxDelay  = 30 * time.Second
	// problemPausedRequeueDelay 流水线暂停时拉长等待再重投，避免「取出-sleep-requeue」空转
	problemPausedRequeueDelay = 30 * time.Second
)

// 进程内解析一次，供 consumer 与 progress 面板共用
var (
	problemFetchConcurrency   = mqconsume.ConcurrencyFromEnv("CWXU_PROBLEM_FETCH_CONCURRENCY", defaultProblemFetchConcurrency)
	problemAnalyzeConcurrency = mqconsume.ConcurrencyFromEnv("CWXU_PROBLEM_ANALYZE_CONCURRENCY", defaultProblemAnalyzeConcurrency)
)

func retryCount(h amqp.Table) int {
	if h == nil {
		return 0
	}
	v, ok := h[mqconsume.RetryHeader]
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
	default:
		return 0
	}
}

func requeueWithRetry(mq *event.RabbitMQ, queue string, d amqp.Delivery, max int) {
	n := retryCount(d.Headers)
	if n >= max {
		log.Errorf("queue=%s 超过最大重试次数 %d，丢弃消息 body=%s", queue, n, truncateBody(d.Body, 200))
		_ = d.Nack(false, false)
		return
	}
	// 退避：按已重试次数指数递增，避免失败消息立刻回队空转
	if delay := retryBackoff(n); delay > 0 {
		time.Sleep(delay)
	}
	headers := amqp.Table{}
	for k, v := range d.Headers {
		headers[k] = v
	}
	headers[mqconsume.RetryHeader] = n + 1
	if err := mq.Publish("", queue, false, false, amqp.Publishing{
		ContentType:  d.ContentType,
		Body:         d.Body,
		DeliveryMode: amqp.Persistent,
		Headers:      headers,
	}); err != nil {
		log.Errorf("queue=%s requeue publish failed: %v", queue, err)
		_ = d.Nack(false, true)
		return
	}
	_ = d.Ack(false)
}

// retryBackoff 第 n 次重试前的等待（指数退避，封顶）
func retryBackoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := problemRetryBaseDelay << (n - 1)
	if d > problemRetryMaxDelay || d <= 0 {
		d = problemRetryMaxDelay
	}
	return d
}

// sleepOrStop 可被停机信号打断的等待
func sleepOrStop(stop <-chan struct{}, d time.Duration) {
	select {
	case <-stop:
	case <-time.After(d):
	}
}

// ProblemFetchConsumer 消费 problem_fetch：仅爬取
type ProblemFetchConsumer struct {
	mq       *event.RabbitMQ
	problem  *ProblemUseCase
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewProblemFetchConsumer(mq *event.RabbitMQ, problem *ProblemUseCase) *ProblemFetchConsumer {
	return &ProblemFetchConsumer{
		mq:      mq,
		problem: problem,
		stopCh:  make(chan struct{}),
	}
}

// Stop 优雅停机：关闭消费通道，让 Consume 循环退出
func (c *ProblemFetchConsumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *ProblemFetchConsumer) Consume() {
	log.Infof("problem_fetch consumer 循环启动")
	for {
		select {
		case <-c.stopCh:
			log.Infof("problem_fetch consumer 已停止")
			return
		default:
		}
		if err := c.consumeOnce(); err != nil {
			log.Errorf("problem_fetch consumer 退出: %v，5s 后重连", err)
		} else {
			log.Warnf("problem_fetch consumer 通道关闭，5s 后重连")
		}
		select {
		case <-c.stopCh:
			log.Infof("problem_fetch consumer 已停止")
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *ProblemFetchConsumer) consumeOnce() error {
	// 队列由发布侧创建；此处禁止 QueueDeclare（args 不一致会 PRECONDITION 杀 channel）
	ch, err := c.mq.OpenChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	// 停机时主动关 channel，使下方 range msgs 结束
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-c.stopCh:
			_ = ch.Close()
		case <-done:
		}
	}()

	if err := ch.Qos(problemFetchConcurrency, 0, false); err != nil {
		return err
	}
	// consumer tag 留空，避免多实例/重连 tag 冲突
	msgs, err := ch.Consume("problem_fetch", "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Infof("problem_fetch consumer 已就绪 concurrency=%d queue=problem_fetch", problemFetchConcurrency)

	sem := make(chan struct{}, problemFetchConcurrency)
	var wg sync.WaitGroup
	for d := range msgs {
		sem <- struct{}{}
		wg.Add(1)
		go func(d amqp.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					// panic 也走带上限的重投，避免坏消息 Nack(requeue) 无限循环
					log.Errorf("RabbitMQ(problem_fetch): panic: %v", r)
					requeueWithRetry(c.mq, "problem_fetch", d, problemMaxRetry)
				}
			}()
			var msg event.ProblemFetchEvent
			if err := json.Unmarshal(d.Body, &msg); err != nil {
				log.Errorf("RabbitMQ(problem_fetch): json %v", err)
				_ = d.Nack(false, false)
				return
			}
			if pipelineControl.IsFetchPaused() {
				log.Warnf("problem_fetch id=%d requeue: fetch paused", msg.ProblemID)
				sleepOrStop(c.stopCh, problemPausedRequeueDelay)
				_ = d.Nack(false, true)
				return
			}
			// 系统过载时先退避，给在线访问留 CPU
			loadgate.Global().Wait(nil, 30*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := c.problem.ProcessFetch(ctx, msg); err != nil {
				if strings.Contains(err.Error(), "paused") {
					log.Warnf("RabbitMQ(problem_fetch) id=%d requeue paused: %v", msg.ProblemID, err)
					sleepOrStop(c.stopCh, problemPausedRequeueDelay)
					_ = d.Nack(false, true)
					return
				}
				log.Errorf("RabbitMQ(problem_fetch) id=%d: %v", msg.ProblemID, err)
				requeueWithRetry(c.mq, "problem_fetch", d, problemMaxRetry)
				return
			}
			_ = d.Ack(false)
		}(d)
	}
	wg.Wait()
	return nil
}

// ProblemAnalyzeConsumer 消费 problem_analyze：仅 AI
type ProblemAnalyzeConsumer struct {
	mq       *event.RabbitMQ
	problem  *ProblemUseCase
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewProblemAnalyzeConsumer(mq *event.RabbitMQ, problem *ProblemUseCase) *ProblemAnalyzeConsumer {
	return &ProblemAnalyzeConsumer{
		mq:      mq,
		problem: problem,
		stopCh:  make(chan struct{}),
	}
}

// Stop 优雅停机：关闭消费通道，让 Consume 循环退出
func (c *ProblemAnalyzeConsumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *ProblemAnalyzeConsumer) Consume() {
	log.Infof("problem_analyze consumer 循环启动")
	for {
		select {
		case <-c.stopCh:
			log.Infof("problem_analyze consumer 已停止")
			return
		default:
		}
		if err := c.consumeOnce(); err != nil {
			log.Errorf("problem_analyze consumer 退出: %v，5s 后重连", err)
		} else {
			log.Warnf("problem_analyze consumer 通道关闭，5s 后重连")
		}
		select {
		case <-c.stopCh:
			log.Infof("problem_analyze consumer 已停止")
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *ProblemAnalyzeConsumer) consumeOnce() error {
	// 队列由发布侧创建；此处禁止 QueueDeclare（args 不一致会 PRECONDITION 杀 channel）
	ch, err := c.mq.OpenChannel()
	if err != nil {
		return err
	}
	defer ch.Close()
	// 停机时主动关 channel，使下方 range msgs 结束
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-c.stopCh:
			_ = ch.Close()
		case <-done:
		}
	}()

	if err := ch.Qos(problemAnalyzeConcurrency, 0, false); err != nil {
		return err
	}
	msgs, err := ch.Consume("problem_analyze", "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Infof("problem_analyze consumer 已就绪 concurrency=%d queue=problem_analyze", problemAnalyzeConcurrency)

	sem := make(chan struct{}, problemAnalyzeConcurrency)
	var wg sync.WaitGroup
	for d := range msgs {
		sem <- struct{}{}
		wg.Add(1)
		go func(d amqp.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					// panic 也走带上限的重投，避免坏消息 Nack(requeue) 无限循环
					log.Errorf("RabbitMQ(problem_analyze): panic: %v", r)
					requeueWithRetry(c.mq, "problem_analyze", d, problemMaxRetry)
				}
			}()
			var msg event.ProblemAnalyzeEvent
			if err := json.Unmarshal(d.Body, &msg); err != nil {
				log.Errorf("RabbitMQ(problem_analyze): json %v", err)
				_ = d.Nack(false, false)
				return
			}
			if pipelineControl.IsAnalyzePaused() {
				log.Warnf("problem_analyze id=%d requeue: AI paused", msg.ProblemID)
				sleepOrStop(c.stopCh, problemPausedRequeueDelay)
				_ = d.Nack(false, true)
				return
			}
			// 流式 AI：整体上限 10 分钟，避免 worker 永久占用；过载先退避让 CPU
			loadgate.Global().Wait(nil, 30*time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			err := c.problem.ProcessAnalyze(ctx, msg)
			cancel()
			if err != nil {
				if strings.Contains(err.Error(), "paused") {
					log.Warnf("RabbitMQ(problem_analyze) id=%d requeue paused: %v", msg.ProblemID, err)
					sleepOrStop(c.stopCh, problemPausedRequeueDelay)
					_ = d.Nack(false, true)
					return
				}
				log.Errorf("RabbitMQ(problem_analyze) id=%d: %v", msg.ProblemID, err)
				requeueWithRetry(c.mq, "problem_analyze", d, problemMaxRetry)
				return
			}
			_ = d.Ack(false)
		}(d)
	}
	wg.Wait()
	return nil
}
