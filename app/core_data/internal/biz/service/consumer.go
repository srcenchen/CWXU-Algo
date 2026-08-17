package service

import (
	"encoding/json"
	"sync"
	"time"

	"cwxu-algo/app/common/event"
	"cwxu-algo/app/common/utils/mqconsume"
	"cwxu-algo/app/core_data/internal/loadgate"
	"cwxu-algo/app/core_data/internal/spidermetrics"
	"cwxu-algo/app/core_data/task"

	"github.com/go-kratos/kratos/v2/log"
	"github.com/streadway/amqp"
)

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
	rdb := c.spider.data.RDB
	concurrencySource := runtimeConcurrencySource(rdb, spiderConcurrencySetting)
	conc := concurrencySource()
	log.Infof("spider consumer 循环启动 concurrency=%d", conc)
	_ = mqconsume.Run(c.mq, mqconsume.Options{
		Name:              "spider",
		Queue:             "spider",
		Concurrency:       conc,
		ConcurrencySource: concurrencySource,
		MaxRetry:          3,
		DeclareOnMissing:  true,
		Stop:              c.stopCh,
		Handler: func(body []byte, _ amqp.Table) error {
			msg := event.SpiderEvent{}
			if err := json.Unmarshal(body, &msg); err != nil {
				// 坏消息重试也不会成功：直接 Ack 丢弃，避免空占重试轮次
				log.Warnf("RabbitMQ(Spider): 解析json失败，丢弃消息: %v", err)
				return nil
			}
			// 站管已暂停该 OJ：直接 Ack 丢弃（消息可能是暂停前入队的）
			if c.spiderTask != nil && c.spiderTask.IsPlatformPaused(msg.Platform) {
				log.Infof("RabbitMQ(Spider): skip platform=%s user=%d (paused by ops)", msg.Platform, msg.UserId)
				return nil
			}
			// 系统过载时先退避，把 CPU 让给在线访问（最多等 30s 再继续）
			loadgate.Global().Wait(nil, 30*time.Second)
			if c.spiderTask != nil {
				c.spiderTask.MarkInflight(msg.UserId, msg.Platform)
				defer c.spiderTask.ClearInflight(msg.UserId, msg.Platform)
			}
			start := spidermetrics.RecordStart(msg.Platform, msg.NeedAll)
			err := c.spider.LoadData(msg.UserId, msg.NeedAll, msg.Platform)
			if err != nil {
				userSide := task.IsUserSideSpiderErr(msg.Platform, err)
				// 用户侧失败（绑定用户名错误等）不算平台异常：不计入今日失败，直接 Ack 不重试
				if !userSide {
					spidermetrics.RecordEnd(msg.Platform, start, err)
				}
				log.Errorf("RabbitMQ(Spider): %v", err)
				// 记录平台级失败：写入用户可读短文案（完整 err 只进日志）
				if c.spiderTask != nil {
					c.spiderTask.MarkLastFail(msg.UserId, msg.Platform, task.FormatSpiderLastError(msg.Platform, err), userSide)
				}
				if userSide {
					return nil
				}
				return err
			}
			spidermetrics.RecordEnd(msg.Platform, start, nil)
			// 成功：更新用户级 + 平台级「上次同步」
			if c.spiderTask != nil {
				c.spiderTask.MarkLastOK(msg.UserId, msg.Platform)
			}
			return nil
		},
	})
}
