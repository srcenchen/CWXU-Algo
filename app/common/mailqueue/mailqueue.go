// Package mailqueue 异步邮件投递：HTTP 路径严禁同步 SMTP，发送统一入 mail 队列，
// 由 core_data 的 MailConsumer 消费后异步发信。SMTP 慢/半开不再拖慢业务接口。
package mailqueue

import (
	"encoding/json"
	"strings"

	"cwxu-algo/app/common/event"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

// Queue mail 队列名
const Queue = "mail"

// Event 异步邮件任务：正文由生产端组装好，consumer 只负责 SMTP 发送。
type Event struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	HTML    string `json:"html"`
}

// Enqueue 投递异步邮件（PublishAsync 不阻塞调用方）。
// 返回 true 表示已成功入队；失败仅记日志（感谢信/确认信等非关键邮件可接受丢失）。
func Enqueue(mq *event.RabbitMQ, to, subject, html string) bool {
	to = strings.TrimSpace(to)
	subject = strings.TrimSpace(subject)
	if mq == nil || to == "" || subject == "" {
		return false
	}
	if err := declareQueue(mq); err != nil {
		log.Warnf("mailqueue: declare queue=%s: %v", Queue, err)
		return false
	}
	body, err := json.Marshal(Event{To: to, Subject: subject, HTML: html})
	if err != nil {
		log.Warnf("mailqueue: marshal: %v", err)
		return false
	}
	mq.PublishAsync("", Queue, false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
	})
	return true
}

// declareQueue 幂等声明队列：已存在直接返回；缺失才声明。
// 队列无 priority 需求，统一 nil args，避免与其它声明参数不一致触发 PRECONDITION。
func declareQueue(mq *event.RabbitMQ) error {
	if _, err := mq.QueueInspect(Queue); err == nil {
		return nil
	}
	if _, err := mq.QueueDeclare(Queue, true, false, false, false, nil); err != nil {
		return err
	}
	return nil
}
