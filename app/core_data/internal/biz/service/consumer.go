package service

import (
	"encoding/json"
	"sync"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/utils/mqconsume"
	"cwxu-algo/app/core_data/internal/spidermetrics"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

// defaultSpiderConcurrency 默认 4；可用 CWXU_SPIDER_CONCURRENCY 覆盖（1–32）
const defaultSpiderConcurrency = 4

type Consumer struct {
	mq         *event.RabbitMQ
	spider     *SpiderUseCase
	spiderTask *task.SpiderTask
	stopCh     chan struct{}
	stopOnce   sync.Once
}

func NewConsumer(mq *event.RabbitMQ, spider *SpiderUseCase, spiderTask *task.SpiderTask) *Consumer {
	return &Consumer{
		mq:         mq,
		spider:     spider,
		spiderTask: spiderTask,
		stopCh:     make(chan struct{}),
	}
}

func (c *Consumer) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *Consumer) Consume() {
	conc := mqconsume.ConcurrencyFromEnv("CWXU_SPIDER_CONCURRENCY", defaultSpiderConcurrency)
	log.Infof("spider consumer 循环启动 concurrency=%d", conc)
	_ = mqconsume.Run(c.mq, mqconsume.Options{
		Name:             "spider",
		Queue:            "spider",
		Concurrency:      conc,
		MaxRetry:         3,
		DeclareOnMissing: true,
		Stop:             c.stopCh,
		Handler: func(body []byte, _ amqp.Table) error {
			msg := event.SpiderEvent{}
			if err := json.Unmarshal(body, &msg); err != nil {
				// 坏消息重试也不会成功：直接 Ack 丢弃，避免空占重试轮次
				log.Warnf("RabbitMQ(Spider): 解析json失败，丢弃消息: %v", err)
				return nil
			}
			if c.spiderTask != nil {
				c.spiderTask.MarkInflight(msg.UserId, msg.Platform)
				defer c.spiderTask.ClearInflight(msg.UserId, msg.Platform)
			}
			start := spidermetrics.RecordStart(msg.NeedAll)
			err := c.spider.LoadData(msg.UserId, msg.NeedAll, msg.Platform)
			spidermetrics.RecordEnd(start, err)
			if err != nil {
				log.Errorf("RabbitMQ(Spider): %v", err)
				// 记录平台级失败，供资料页展示（仍 return err 走 MQ 重试）
				if c.spiderTask != nil {
					c.spiderTask.MarkLastFail(msg.UserId, msg.Platform, err.Error())
				}
				return err
			}
			// 成功：更新用户级 + 平台级「上次同步」
			if c.spiderTask != nil {
				c.spiderTask.MarkLastOK(msg.UserId, msg.Platform)
			}
			return nil
		},
	})
}
