package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/mailqueue"
	"cwxu-algo/app/common/sitesettings"
	"cwxu-algo/app/common/utils/mqconsume"
	"cwxu-algo/app/core_data/internal/data"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

const (
	// 邮件为低频弱任务，并发 2 足够；CWXU_MAIL_CONCURRENCY 可覆盖
	defaultMailConcurrency = 2
	// SMTP 瞬断重试 3 次后丢弃，避免坏收件人/永久失败占队
	mailMaxRetry = 3
)

// 进程内解析一次
var mailConcurrency = mqconsume.ConcurrencyFromEnv("CWXU_MAIL_CONCURRENCY", defaultMailConcurrency)

// MailConsumer 消费 mail 队列：异步 SMTP 发送。
// HTTP 路径只入队（mailqueue.Enqueue），SMTP 慢/半开不再拖接口。
type MailConsumer struct {
	mq       *event.RabbitMQ
	data     *data.Data
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewMailConsumer(mq *event.RabbitMQ, data *data.Data) *MailConsumer {
	return &MailConsumer{
		mq:     mq,
		data:   data,
		stopCh: make(chan struct{}),
	}
}

// Stop 优雅停机：关闭消费通道，让 Consume 循环退出
func (c *MailConsumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *MailConsumer) Consume() {
	log.Infof("mail consumer 循环启动")
	for {
		select {
		case <-c.stopCh:
			log.Infof("mail consumer 已停止")
			return
		default:
		}
		if err := c.consumeOnce(); err != nil {
			log.Errorf("mail consumer 退出: %v，5s 后重连", err)
		} else {
			log.Warnf("mail consumer 通道关闭，5s 后重连")
		}
		select {
		case <-c.stopCh:
			log.Infof("mail consumer 已停止")
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *MailConsumer) consumeOnce() error {
	if c.mq == nil {
		return fmt.Errorf("mq not ready")
	}
	// 队列由发布侧声明（mailqueue.Enqueue）；此处禁止 QueueDeclare（args 不一致会 PRECONDITION 杀 channel）
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

	if err := ch.Qos(mailConcurrency, 0, false); err != nil {
		return err
	}
	// consumer tag 留空，避免多实例/重连 tag 冲突
	msgs, err := ch.Consume(mailqueue.Queue, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	log.Infof("mail consumer 已就绪 concurrency=%d queue=%s", mailConcurrency, mailqueue.Queue)

	sem := make(chan struct{}, mailConcurrency)
	var wg sync.WaitGroup
	for d := range msgs {
		sem <- struct{}{}
		wg.Add(1)
		go func(d amqp.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					log.Errorf("RabbitMQ(mail): panic: %v", r)
					requeueWithRetry(c.mq, mailqueue.Queue, d, mailMaxRetry)
				}
			}()
			var msg mailqueue.Event
			if err := json.Unmarshal(d.Body, &msg); err != nil {
				log.Errorf("RabbitMQ(mail): json %v", err)
				_ = d.Nack(false, false)
				return
			}
			if err := c.sendMail(msg); err != nil {
				log.Errorf("RabbitMQ(mail) to=%s: %v", msg.To, err)
				requeueWithRetry(c.mq, mailqueue.Queue, d, mailMaxRetry)
				return
			}
			_ = d.Ack(false)
		}(d)
	}
	wg.Wait()
	return nil
}

// sendMail 从站点配置取 SMTP 发送；失败返回 error 走重试。
// SMTP 未配置返回错误（有限次重试后丢弃）：配置上线后新消息正常发出。
func (c *MailConsumer) sendMail(msg mailqueue.Event) error {
	to := strings.TrimSpace(msg.To)
	subject := strings.TrimSpace(msg.Subject)
	if to == "" || subject == "" {
		// 无效消息直接 Ack
		return nil
	}
	// site_configs 在 user 库；core_data 先读 Redis，miss 时回源 user 库
	rt := sitesettings.Load(context.Background(), c.data.RDB, c.data.UserDB)
	sender := rt.MailSender()
	if sender == nil || !sender.Configured() {
		return fmt.Errorf("SMTP not configured")
	}
	if !strings.HasPrefix(subject, "[GoAlgo]") {
		subject = "[GoAlgo] " + subject
	}
	html := msg.HTML
	if strings.TrimSpace(html) == "" {
		html = "<p></p>"
	}
	if err := sender.Send(to, subject, html); err != nil {
		return fmt.Errorf("smtp send: %w", err)
	}
	return nil
}
