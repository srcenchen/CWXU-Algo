package service

import (
	"encoding/json"
	"fmt"
	"sync"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/utils/mqconsume"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

// 定时邮件（日报/周报）：单 worker，避免并发打爆 SMTP 与 LLM。
const summaryConcurrency = 1

type Consumer struct {
	mq       *event.RabbitMQ
	summary  *SummaryUseCase
	stopCh   chan struct{}
	stopOnce sync.Once
}

func NewConsumer(mq *event.RabbitMQ, summary *SummaryUseCase) *Consumer {
	return &Consumer{
		mq:      mq,
		summary: summary,
		stopCh:  make(chan struct{}),
	}
}

func (c *Consumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *Consumer) Consume() {
	c.consumeQueue(event.SummaryMailQueue)
}

func (c *Consumer) consumeQueue(queue string) {
	log.Infof("summary consumer 循环启动 queue=%s", queue)
	_ = mqconsume.Run(c.mq, mqconsume.Options{
		Name:             queue,
		Queue:            queue,
		Concurrency:      summaryConcurrency,
		MaxRetry:         5,
		DeclareOnMissing: true,
		Stop:             c.stopCh,
		Handler: func(body []byte, _ amqp.Table) error {
			msg := event.SummaryEvent{}
			if err := json.Unmarshal(body, &msg); err != nil {
				log.Errorf("RabbitMQ(Summary): 解析json出错 %s", err.Error())
				return fmt.Errorf("bad json: %w", err)
			}
			var runErr error
			switch msg.Type {
			case "PersonalLastDay":
				runErr = c.summary.PersonalLastDay(msg.UserId)
			case "PersonalDailyRule":
				runErr = c.summary.PersonalDailyRule(msg.UserId)
			case "WeeklyStaff", "WeeklyReportForCoach":
				runErr = c.summary.WeeklyStaff(msg.UserId)
			default:
				log.Errorf("RabbitMQ(Summary): 未知类型 %s", msg.Type)
				return fmt.Errorf("unknown type %s", msg.Type)
			}
			if runErr != nil {
				log.Errorf("RabbitMQ(Summary) user=%d type=%s: %v", msg.UserId, msg.Type, runErr)
				return runErr
			}
			return nil
		},
	})
}
