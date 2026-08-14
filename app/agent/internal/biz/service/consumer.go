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

// 2c4g：交互总结与定时邮件各单 worker，避免日报排在 PersonalRecent 积压后面。
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
	var wg sync.WaitGroup
	for _, queue := range []string{event.SummaryQueue, event.SummaryMailQueue} {
		queue := queue
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.consumeQueue(queue)
		}()
	}
	wg.Wait()
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
			case "PersonalRecent":
				runErr = c.summary.PersonalRecent(msg.UserId)
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
