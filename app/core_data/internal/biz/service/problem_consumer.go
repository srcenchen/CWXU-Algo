package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/mqconsume"
	"cwxu-algo/app/core_data/internal/loadgate"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/redis/go-redis/v9"
	"github.com/streadway/amqp"
)

const (
	// 默认各 4；三个消费队列均可由站点运行时配置调整。
	defaultProblemFetchConcurrency = 4
	problemMaxRetry                = 5
	// problemRetryBaseDelay 重投退避基数：按重试次数指数递增，上限 problemRetryMaxDelay
	problemRetryBaseDelay = 2 * time.Second
	problemRetryMaxDelay  = 30 * time.Second
	// pipelineRequeueDelay 流水线暂停时拉长等待再重投，避免空转
	pipelineRequeueDelay = 30 * time.Second
	problemFetchQueue    = "problem_fetch"
)

// 进程内解析一次，供 consumer 与 progress 面板共用
var (
	problemFetchConcurrency = mqconsume.ConcurrencyFromEnv("CWXU_PROBLEM_FETCH_CONCURRENCY", defaultProblemFetchConcurrency)
)

type concurrencySetting struct {
	envKey string
	value  func(*sitesettings.Runtime) int
}

var spiderConcurrencySetting = concurrencySetting{
	envKey: "CWXU_SPIDER_CONCURRENCY",
	value:  func(rt *sitesettings.Runtime) int { return rt.SpiderConcurrency },
}

var problemFetchConcurrencySetting = concurrencySetting{
	envKey: "CWXU_PROBLEM_FETCH_CONCURRENCY",
	value:  func(rt *sitesettings.Runtime) int { return rt.ProblemFetchConcurrency },
}

var analyzeConcurrencySetting = concurrencySetting{
	envKey: "CWXU_PROBLEM_ANALYZE_CONCURRENCY",
	value:  func(rt *sitesettings.Runtime) int { return rt.ProblemAnalyzeConcurrency },
}

func runtimeConcurrency(ctx context.Context, rdb *redis.Client, setting concurrencySetting) int {
	rt, err := sitesettings.LoadFromRedis(ctx, rdb)
	if err == nil && rt != nil {
		if value := setting.value(rt); value >= 1 && value <= 32 {
			return value
		}
	}
	if errors.Is(err, redis.Nil) {
		return mqconsume.ConcurrencyFromEnv(setting.envKey, 4)
	}
	return 4
}

func runtimeConcurrencySource(rdb *redis.Client, setting concurrencySetting) func() int {
	var mu sync.Mutex
	last := 0
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		rt, err := sitesettings.LoadFromRedis(context.Background(), rdb)
		if err == nil && rt != nil {
			if value := setting.value(rt); value >= 1 && value <= 32 {
				last = value
				return value
			}
			last = mqconsume.ConcurrencyFromEnv(setting.envKey, 4)
			return last
		}
		if errors.Is(err, redis.Nil) {
			last = mqconsume.ConcurrencyFromEnv(setting.envKey, 4)
		} else if last == 0 {
			last = 4
		}
		return last
	}
}

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

// profile invalidation conflicts are coordination races, not analysis failures.
// They must remain retryable until the durable maintenance owner is available.
func isProblemAnalyzeCoordinationError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "profile invalidation intent changed") || strings.Contains(message, "profile invalidation already in progress") || strings.Contains(message, "profile invalidation ownership changed")
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

func problemFetchConsumerPaused() bool {
	return pipelineControl.IsFetchPaused()
}

func problemFetchEventPaused(ev event.ProblemFetchEvent) bool {
	return problemFetchConsumerPaused() && !ev.BypassFetchPause
}

type problemFetchPauseKind uint8

const (
	problemFetchPauseNone problemFetchPauseKind = iota
	problemFetchPauseGlobal
)

func classifyProblemFetchPause(err error) problemFetchPauseKind {
	switch {
	case errors.Is(err, errProblemFetchPaused):
		return problemFetchPauseGlobal
	default:
		return problemFetchPauseNone
	}
}

// ProblemFetchConsumer 消费 problem_fetch：仅爬取
type ProblemFetchConsumer struct {
	mq       *event.RabbitMQ
	problem  *ProblemUseCase
	stopCh   chan struct{}
	stopOnce sync.Once
}

func (c *ProblemFetchConsumer) redis() *redis.Client {
	if c == nil || c.problem == nil || c.problem.data == nil {
		return nil
	}
	return c.problem.data.RDB
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
		concurrencySource := runtimeConcurrencySource(c.redis(), problemFetchConcurrencySetting)
		if err := c.consumeOnce(concurrencySource(), concurrencySource); err != nil {
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

func (c *ProblemFetchConsumer) consumeOnce(concurrency int, concurrencySource func() int) error {
	if err := c.problem.declareProblemQueue(problemFetchQueue); err != nil {
		return fmt.Errorf("declare queue %s: %w", problemFetchQueue, err)
	}
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

	if err := ch.Qos(concurrency, 0, false); err != nil {
		return err
	}
	// consumer tag 留空，避免多实例/重连 tag 冲突
	msgs, err := ch.Consume("problem_fetch", "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Infof("problem_fetch consumer 已就绪 concurrency=%d queue=problem_fetch", concurrency)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			_ = ch.Close()
			wg.Wait()
			return nil
		case <-ticker.C:
			if concurrencySource() != concurrency {
				_ = ch.Close()
				wg.Wait()
				return nil
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
				// 平台暂停必须由 ProcessFetch 读取题目后按 DB 平台判断，不能信任可能陈旧的消息平台。
				if problemFetchEventPaused(msg) {
					log.Warnf("problem_fetch id=%d requeue: fetch paused", msg.ProblemID)
					sleepOrStop(c.stopCh, pipelineRequeueDelay)
					_ = d.Nack(false, true)
					return
				}
				// 系统过载时先退避，给在线访问留 CPU
				loadgate.Global().Wait(nil, 30*time.Second)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				if err := c.problem.ProcessFetch(ctx, msg); err != nil {
					switch classifyProblemFetchPause(err) {
					case problemFetchPauseGlobal:
						log.Warnf("RabbitMQ(problem_fetch) id=%d requeue paused: %v", msg.ProblemID, err)
						sleepOrStop(c.stopCh, pipelineRequeueDelay)
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
	}
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
	concurrencySource := runtimeConcurrencySource(c.redis(), analyzeConcurrencySetting)
	for {
		select {
		case <-c.stopCh:
			log.Infof("problem_analyze consumer 已停止")
			return
		default:
		}
		concurrency := concurrencySource()
		if changed, err := c.consumeOnce(concurrency, concurrencySource); changed {
			continue
		} else if err != nil {
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

func (c *ProblemAnalyzeConsumer) consumeOnce(concurrency int, concurrencySource func() int) (bool, error) {
	if err := c.problem.declareProblemQueue("problem_analyze"); err != nil {
		return false, fmt.Errorf("declare queue problem_analyze: %w", err)
	}
	ch, err := c.mq.OpenChannel()
	if err != nil {
		return false, err
	}
	defer ch.Close()
	if err := ch.Qos(concurrency, 0, false); err != nil {
		return false, err
	}
	consumerTag := fmt.Sprintf("problem-analyze-%d", time.Now().UnixNano())
	msgs, err := ch.Consume("problem_analyze", consumerTag, false, false, false, false, nil)
	if err != nil {
		return false, err
	}
	log.Infof("problem_analyze consumer 已就绪 concurrency=%d queue=problem_analyze", concurrency)

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			_ = ch.Cancel(consumerTag, false)
			wg.Wait()
			return false, nil
		case <-ticker.C:
			if concurrencySource() != concurrency {
				_ = ch.Cancel(consumerTag, false)
				wg.Wait()
				return true, nil
			}
		case d, ok := <-msgs:
			if !ok {
				wg.Wait()
				return false, nil
			}
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
					sleepOrStop(c.stopCh, pipelineRequeueDelay)
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
						sleepOrStop(c.stopCh, pipelineRequeueDelay)
						_ = d.Nack(false, true)
						return
					}
					if isProblemAnalyzeCoordinationError(err) {
						log.Warnf("RabbitMQ(problem_analyze) id=%d requeue coordination conflict: %v", msg.ProblemID, err)
						sleepOrStop(c.stopCh, pipelineRequeueDelay)
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
	}
}

func (c *ProblemAnalyzeConsumer) redis() *redis.Client {
	if c == nil || c.problem == nil || c.problem.data == nil {
		return nil
	}
	return c.problem.data.RDB
}
